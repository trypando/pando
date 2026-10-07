package proxy

import (
	"context"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// StateResolver resolves apps from the state store.
type StateResolver struct {
	apps *state.Apps
}

func NewStateResolver(apps *state.Apps) *StateResolver { return &StateResolver{apps: apps} }

// ByHostname resolves an app by the hostname in its pinned spec.
func (r *StateResolver) ByHostname(ctx context.Context, hostname string) (state.App, *spec.AppSpec, bool, error) {
	return r.apps.ByRouting(ctx, "hostname", hostname)
}

// ByPath resolves a path-mode app by the longest path it holds that the
// request's path begins with.
func (r *StateResolver) ByPath(ctx context.Context, path string) (state.App, *spec.AppSpec, string, bool, error) {
	return r.apps.ByPath(ctx, path)
}

// BySlug resolves an app by its slug, for path mode.
func (r *StateResolver) BySlug(ctx context.Context, slug string) (state.App, *spec.AppSpec, bool, error) {
	return r.apps.ByRouting(ctx, "slug", slug)
}

// ByPort resolves an app by the host port it was given, for port mode.
func (r *StateResolver) ByPort(ctx context.Context, port int) (state.App, *spec.AppSpec, bool, error) {
	return r.apps.ByRouting(ctx, "port", strconv.Itoa(port))
}

// RuntimeUpstreams asks the runtime adapter where an app's primary workload is.
//
// The address comes from the adapter rather than being assembled here, because
// how a workload is addressed is exactly the provider vocabulary core must not
// learn (R-251).
type RuntimeUpstreams struct {
	registry *api.Registry
}

func NewRuntimeUpstreams(registry *api.Registry) *RuntimeUpstreams {
	return &RuntimeUpstreams{registry: registry}
}

// PrimaryAddress returns the URL of the app's primary workload.
func (u *RuntimeUpstreams) PrimaryAddress(ctx context.Context, app state.App, s *spec.AppSpec) (string, error) {
	upstream, err := u.Primary(ctx, app, s)
	return upstream.URL, err
}

// Primary returns how to reach the app's primary workload: its URL, and on a
// runtime whose workloads are on another host, the Dial that reaches it
// through that host's agent (O-45).
func (u *RuntimeUpstreams) Primary(ctx context.Context, app state.App, s *spec.AppSpec) (api.Upstream, error) {
	if s == nil {
		return api.Upstream{}, errs.New(errs.StateInvalid, "This app has no pinned spec.")
	}
	primary, ok := s.PrimaryWorkload()
	if !ok {
		return api.Upstream{}, errs.New(errs.ValidPrimaryWorkload, "This app has no primary workload.")
	}

	port := 0
	for _, p := range primary.Ports {
		if p.Protocol == "http" || p.Protocol == "" {
			port = p.Number
			break
		}
	}
	if port == 0 && len(primary.Ports) > 0 {
		port = primary.Ports[0].Number
	}
	if port == 0 {
		return api.Upstream{}, errs.New(errs.StateInvalid,
			"Pando doesn't know which port this app serves on.").
			WithRemedy("Add the port to the app's spec.")
	}

	// Which port is a question about the spec, and core answers it. Where that
	// port is reachable is a question about the runtime, and only the runtime
	// can answer it (R-251). The address was assembled here as a Docker
	// container name, which sent every request for an app on any other runtime
	// to a host that did not exist.
	if u.registry == nil {
		return api.Upstream{}, errs.New(errs.AdapterUnavailable, "Pando has no runtimes set up.")
	}
	runtime, ok := u.registry.Runtime(s.Runtime.AdapterRef)
	if !ok {
		return api.Upstream{}, errs.Newf(errs.PlanAdapterNotConfigured,
			"The runtime %q is not configured.", s.Runtime.AdapterRef)
	}

	// The bundle is the app: every deploy names it by the app's ID.
	return runtime.Upstream(ctx, api.WorkloadRef{BundleID: app.ID, Workload: primary.Name}, port)
}

// Counters is an in-memory request count, per app.
//
// Its purpose is evidential: R-023 says there is no bypass, and a counter that
// every request increments — allowed or denied, authenticated or anonymous — is
// how that claim is checked rather than asserted.
type Counters struct {
	mu     sync.RWMutex
	counts map[string]*appCounter
}

type appCounter struct {
	total     atomic.Int64
	allowed   atomic.Int64
	denied    atomic.Int64
	anonymous atomic.Int64
}

func NewCounters() *Counters { return &Counters{counts: map[string]*appCounter{}} }

func (c *Counters) Request(appID string, kind authz.PrincipalKind, allowed bool) {
	c.mu.Lock()
	counter, ok := c.counts[appID]
	if !ok {
		counter = &appCounter{}
		c.counts[appID] = counter
	}
	c.mu.Unlock()

	counter.total.Add(1)
	if allowed {
		counter.allowed.Add(1)
	} else {
		counter.denied.Add(1)
	}
	if kind == authz.KindAnonymous {
		counter.anonymous.Add(1)
	}
}

// Snapshot returns the counts for one app.
func (c *Counters) Snapshot(appID string) (total, allowed, denied, anonymous int64) {
	c.mu.RLock()
	counter, ok := c.counts[appID]
	c.mu.RUnlock()
	if !ok {
		return 0, 0, 0, 0
	}
	return counter.total.Load(), counter.allowed.Load(), counter.denied.Load(), counter.anonymous.Load()
}

var (
	_ Resolver  = (*StateResolver)(nil)
	_ Upstreams = (*RuntimeUpstreams)(nil)
	_ Metrics   = (*Counters)(nil)
)

// IsAppHostname reports whether a hostname belongs to an app.
//
// For the HTTP router, which has to decide between the console and the proxy
// before either runs. Deliberately only a yes or no: the caller uses it to pick
// a handler, and the proxy still resolves and authorizes the request itself.
func (r *StateResolver) IsAppHostname(ctx context.Context, hostname string) (bool, error) {
	if hostname == "" {
		return false, nil
	}
	_, _, found, err := r.apps.ByRouting(ctx, "hostname", hostname)
	return found, err
}
