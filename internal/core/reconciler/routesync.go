package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/spec"
)

// RouteSync writes every app's route on this host, from the database (R-417).
//
// The reconciler writes an app's route on whichever control host holds its
// lease, which is the only edge that learns it; another control host's edge
// would send the app's hostname to its catch-all, without the app's own
// certificate. Each control host runs this, so its edge is generated from the
// pinned specs rather than shared as a directory. Every route points at
// Pando's proxy (R-023): a route this pass writes is the one the reconciler
// would, and one it leaves behind for a deleted app reaches Pando, which no
// longer knows the hostname.
//
// A pass writes only the routes that differ from what this host last wrote
// (written), and one not written for RouteSyncRefresh, in case its edge lost
// it: rewriting every route every minute reached its adapter — a file
// Traefik reloads, a provider's API — for nothing.
type RouteSync struct {
	Apps interface {
		LiveApps(ctx context.Context, f planner.InventoryFilter) ([]planner.InventoryApp, error)
	}
	Registry Registry

	// ProxyUpstream is where every route points: this host's Pando.
	ProxyUpstream string

	// ControlHosts counts the hosts holding the control role
	// (hosts.Service.ControlCount). With one, every edge is that host's and
	// its routes are the reconciler's alone, so the pass is skipped, however
	// many app hosts there are. Nil runs it always.
	ControlHosts func(ctx context.Context) (int, error)

	// Exclusive runs pass unless another replica on this host is running
	// one (state.Cluster.OnePerHost), and reports whether it ran: every
	// replica of a host writes to one edge, so one pass at a time is enough.
	// Nil runs it always.
	Exclusive func(ctx context.Context, pass func(context.Context)) (bool, error)

	Interval time.Duration
	Clock    clock.Clock
	Logger   *zap.Logger

	mu sync.Mutex
	// written is, by app, the route this host last wrote and when.
	written map[string]writtenRoute
}

type writtenRoute struct {
	digest string
	at     time.Time
}

// RouteSyncInterval is how often a control host checks its routes [P].
const RouteSyncInterval = time.Minute

// RouteSyncRefresh is how long a route this host wrote is trusted to be on
// its edge before it is written again [P].
const RouteSyncRefresh = time.Hour

// Run passes until ctx ends.
func (s *RouteSync) Run(ctx context.Context) {
	interval := s.Interval
	if interval <= 0 {
		interval = RouteSyncInterval
	}
	for {
		if _, err := s.Once(ctx); err != nil && s.Logger != nil {
			s.Logger.Warn("could not write this host's routes from the database", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-s.clock().After(interval):
		}
	}
}

// Once writes the route of every live app routed through an adapter whose
// route differs from the one this host last wrote, and returns how many it
// wrote. An app whose route fails is logged, tried again next pass, and the
// rest are still written.
func (s *RouteSync) Once(ctx context.Context) (int, error) {
	if s.ControlHosts != nil {
		if n, err := s.ControlHosts(ctx); err != nil || n < 2 {
			return 0, err
		}
	}
	if s.Exclusive == nil {
		return s.pass(ctx)
	}
	var wrote int
	var err error
	if _, lockErr := s.Exclusive(ctx, func(ctx context.Context) { wrote, err = s.pass(ctx) }); lockErr != nil {
		return 0, lockErr
	}
	return wrote, err
}

func (s *RouteSync) pass(ctx context.Context) (int, error) {
	apps, err := s.Apps.LiveApps(ctx, planner.InventoryFilter{All: true})
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	seen := make(map[string]writtenRoute, len(apps))
	wrote := 0
	for _, app := range apps {
		w, outcome := s.ensure(ctx, app, now)
		if outcome != routeNone {
			seen[app.AppID] = w
		}
		if outcome == routeWritten {
			wrote++
		}
	}
	// A deleted app's entry goes, so one created again is written.
	s.written = seen
	return wrote, nil
}

// routeOutcome is what ensure did with an app's route.
type routeOutcome int

const (
	// routeNone: no route to write, or its write failed.
	routeNone routeOutcome = iota
	// routeCurrent: this host wrote the same route within RouteSyncRefresh.
	routeCurrent
	routeWritten
)

// ensure writes app's route, at now, unless it is current, and returns what
// is on the edge for it.
func (s *RouteSync) ensure(ctx context.Context, app planner.InventoryApp, now time.Time) (writtenRoute, routeOutcome) {
	if app.Spec == nil || app.Spec.Routing.AdapterRef == "" || app.Spec.Routing.Mode == spec.RoutingPort {
		return writtenRoute{}, routeNone
	}
	routing, ok := s.Registry.Routing(app.Spec.Routing.AdapterRef)
	if !ok {
		return writtenRoute{}, routeNone
	}
	req := api.RouteFor(app.AppID, app.Spec.Routing, s.ProxyUpstream)
	digest := routeDigest(app.Spec.Routing.AdapterRef, req)
	if last, ok := s.written[app.AppID]; ok && last.digest == digest && now.Sub(last.at) < RouteSyncRefresh {
		return last, routeCurrent
	}
	if _, err := routing.Ensure(ctx, req); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("could not write an app's route on this host", zap.String("app_id", app.AppID), zap.Error(err))
		}
		return writtenRoute{}, routeNone
	}
	return writtenRoute{digest: digest, at: now}, routeWritten
}

// routeDigest is what a route written through the adapter ref says.
func routeDigest(ref string, req api.RouteRequest) string {
	body, _ := json.Marshal(req) // #nosec G104 -- strings, numbers and booleans
	sum := sha256.Sum256(append([]byte(ref+"\n"), body...))
	return hex.EncodeToString(sum[:])
}

func (s *RouteSync) clock() clock.Clock {
	if s.Clock == nil {
		return clock.System{}
	}
	return s.Clock
}

func (s *RouteSync) now() time.Time { return s.clock().Now() }
