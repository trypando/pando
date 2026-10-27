package reconciler

import (
	"context"
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
type RouteSync struct {
	Apps interface {
		LiveApps(ctx context.Context, f planner.InventoryFilter) ([]planner.InventoryApp, error)
	}
	Registry Registry

	// ProxyUpstream is where every route points: this host's Pando.
	ProxyUpstream string

	// Hosts counts the cluster's hosts. A single host's routes are the
	// reconciler's alone, so the pass is skipped. Nil runs it always.
	Hosts func(ctx context.Context) (int, error)

	Interval time.Duration
	Clock    clock.Clock
	Logger   *zap.Logger
}

// RouteSyncInterval is how often a control host rewrites its routes [P].
const RouteSyncInterval = time.Minute

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
		case <-s.Clock.After(interval):
		}
	}
}

// Once ensures the route of every live app routed through an adapter, and
// returns how many it ensured. An app whose route fails is logged and the
// rest are still written.
func (s *RouteSync) Once(ctx context.Context) (int, error) {
	if s.Hosts != nil {
		if n, err := s.Hosts(ctx); err != nil || n < 2 {
			return 0, err
		}
	}
	apps, err := s.Apps.LiveApps(ctx, planner.InventoryFilter{All: true})
	if err != nil {
		return 0, err
	}
	ensured := 0
	for _, app := range apps {
		if s.ensure(ctx, app) {
			ensured++
		}
	}
	return ensured, nil
}

func (s *RouteSync) ensure(ctx context.Context, app planner.InventoryApp) bool {
	if app.Spec == nil || app.Spec.Routing.AdapterRef == "" || app.Spec.Routing.Mode == spec.RoutingPort {
		return false
	}
	routing, ok := s.Registry.Routing(app.Spec.Routing.AdapterRef)
	if !ok {
		return false
	}
	if _, err := routing.Ensure(ctx, api.RouteFor(app.AppID, app.Spec.Routing, s.ProxyUpstream)); err != nil {
		if s.Logger != nil {
			s.Logger.Warn("could not write an app's route on this host", zap.String("app_id", app.AppID), zap.Error(err))
		}
		return false
	}
	return true
}
