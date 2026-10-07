// Package edge runs what routing adapters need in front of Pando (R-174).
//
// A routing adapter describes an edge — Traefik, cloudflared — as an
// api.EdgePlan; the default runtime adapter runs it. This package is the join,
// and the only place the two meet: neither adapter reaches into the other
// (design 03 §4.4).
package edge

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
)

// Status is the last thing Pando found out about one routing adapter's edge.
type Status struct {
	// Running is whether the edge was running when last looked at.
	Running bool `json:"running"`

	// Message says what is wrong, when something is. Operator detail — a
	// port, an image — so the handler shows it only to whoever may change
	// the adapter.
	Message string `json:"message,omitempty"`

	CheckedAt time.Time `json:"checked_at"`
}

// Service applies every routing adapter's edge through the default runtime.
type Service struct {
	Registry      *api.Registry
	ProxyUpstream string
	Logger        *zap.Logger
	Clock         clock.Clock

	mu     sync.RWMutex
	status map[string]Status
}

// Status reports the last result for a routing adapter's edge. false means
// that adapter has no edge.
func (s *Service) Status(ref string) (Status, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.status[ref]
	return st, ok
}

// Reconcile makes the edges that exist the edges the routing adapters ask for.
//
// Idempotent: a matching edge is left alone, a changed plan recreates it, and
// an edge nothing asks for any more is removed. One adapter's broken edge does
// not stop another's from being applied.
func (s *Service) Reconcile(ctx context.Context) error {
	rt, err := s.runtime()
	if err != nil {
		return err
	}
	caps, err := rt.Capabilities(ctx)
	if err != nil {
		return err
	}

	now := s.now()
	results := map[string]Status{}
	wanted := map[string]bool{}
	var firstErr error
	fail := func(ref string, err error) {
		results[ref] = Status{Message: messageOf(err), CheckedAt: now}
		s.logger().Warn("edge not applied", zap.String("adapter_id", ref), zap.Error(err))
		if firstErr == nil {
			firstErr = err
		}
	}

	refs := s.Registry.ByCategory(api.CategoryRouting)
	sort.Strings(refs)
	for _, ref := range refs {
		routing, ok := s.Registry.Routing(ref)
		if !ok {
			continue
		}
		plan, needs, err := routing.Edge(ctx, api.EdgeRequest{Ref: ref, ProxyUpstream: s.ProxyUpstream, EdgeConfig: caps.EdgeConfig})
		if err != nil {
			fail(ref, err)
			continue
		}
		if !needs {
			continue
		}
		if plan.Name == "" {
			plan.Name = ref
		}
		if plan.ProxyAlias == "" {
			plan.ProxyAlias = hostOf(s.ProxyUpstream)
		}
		wanted[plan.Name] = true

		if !caps.SupportsEdge {
			// Refused rather than configured and unreachable (R-254).
			fail(ref, errs.New(errs.PlanCapabilityUnsupported,
				fmt.Sprintf("The %s routing adapter needs to run a process in front of Pando, and the runtime adapter cannot run one.", ref)).
				WithRemedy("Use the Docker runtime adapter, or set the routing adapter so that something else runs its edge."))
			continue
		}
		if err := rt.ApplyEdge(ctx, plan); err != nil {
			fail(ref, err)
			continue
		}
		observed, err := rt.ObserveEdge(ctx, plan.Name)
		if err != nil {
			fail(ref, err)
			continue
		}
		results[ref] = Status{Running: observed.Running, Message: observed.Detail, CheckedAt: now}
	}

	if caps.SupportsEdge {
		existing, err := rt.Edges(ctx)
		if err != nil {
			return err
		}
		for _, name := range existing {
			if wanted[name] {
				continue
			}
			// An adapter removed, disabled, switched to somebody else's
			// Traefik, or failing to configure. Its certificate volume is
			// kept by the runtime, so switching back does not re-issue.
			if err := rt.RemoveEdge(ctx, name); err != nil {
				s.logger().Warn("edge not removed", zap.String("edge", name), zap.Error(err))
				continue
			}
			s.logger().Info("edge removed", zap.String("edge", name))
		}
	}

	s.mu.Lock()
	s.status = results
	s.mu.Unlock()
	return firstErr
}

// Run reconciles at start and then every interval until ctx ends.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Minute
	}
	for {
		if err := s.Reconcile(ctx); err != nil && ctx.Err() == nil {
			s.logger().Debug("edge reconcile incomplete", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.clock().After(interval):
		}
	}
}

func (s *Service) runtime() (api.RuntimeAdapter, error) {
	ref, ok := s.Registry.Default(api.CategoryRuntime)
	if !ok {
		if refs := s.Registry.ByCategory(api.CategoryRuntime); len(refs) > 0 {
			ref = refs[0]
		}
	}
	rt, ok := s.Registry.Runtime(ref)
	if !ok {
		return nil, errs.New(errs.AdapterUnavailable, "No runtime adapter is configured, so Pando cannot run an edge.")
	}
	return rt, nil
}

func (s *Service) logger() *zap.Logger {
	if s.Logger == nil {
		return zap.NewNop()
	}
	return s.Logger
}

func (s *Service) clock() clock.Clock {
	if s.Clock == nil {
		return clock.System{}
	}
	return s.Clock
}

func (s *Service) now() time.Time { return s.clock().Now() }

// messageOf is what an operator reads: the envelope's message and remedy, or
// a plain statement when the error came from somewhere without one.
func messageOf(err error) string {
	var e *errs.Error
	if errors.As(err, &e) {
		if e.Remedy != "" {
			return e.Message + " " + e.Remedy
		}
		return e.Message
	}
	return "Pando could not start the edge: " + err.Error()
}

// hostOf is the host part of an upstream URL — what the edge dials.
func hostOf(upstream string) string {
	u, err := url.Parse(upstream)
	if err != nil {
		return ""
	}
	return u.Hostname()
}
