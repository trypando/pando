package loopback

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "loopback"

// Adapter serves apps on localhost ports: the laptop default.
//
// It does nothing to the network — there is nothing to configure, because the
// proxy already listens and a port-mode app is reached by asking the proxy for
// it. Ensure therefore records intent rather than performing an action, and
// Observe reports what was recorded.
//
// That is not a degenerate case of the interface; it is the interface working.
// Ensure/Remove/Observe describe intent, which is why they fit both an adapter
// that writes config files and one that has no config at all.
type Adapter struct {
	mu     sync.RWMutex
	routes map[string]api.RouteRequest
	config Config
}

// Config is the adapter's configuration. There is nothing to set.
//
// BaseURL is accepted so a configuration stored by an earlier version still
// loads, and is otherwise ignored. It never changed an address anyone saw: the
// API renders a port-mode address from the host the request reached Pando on
// (spec.Address, R-261), and the proxy listens on each allocated port on that
// same host (design 03 §4.2), so any other host is an address nothing answers
// on. server.external_url already names where browsers reach the install; a
// second setting that could disagree with it would be a guess about DNS.
type Config struct {
	BaseURL string `json:"base_url,omitempty"`
}

// baseURLIgnored is the save-time warning for a stored base_url.
const baseURLIgnored = "The base_url setting has no effect and can be removed. " +
	"Apps on the built-in routing are reached on their own port, on whatever address you reach Pando's console at."

// Warnings returns what is worth telling someone saving this configuration,
// without refusing it. A stored base_url is the only one: it is accepted for
// compatibility but does nothing. Configuration that does not parse returns
// nothing here; Configure reports that as an error.
func Warnings(raw json.RawMessage) []string {
	var c Config
	if len(raw) == 0 || json.Unmarshal(raw, &c) != nil || c.BaseURL == "" {
		return nil
	}
	return []string{baseURLIgnored}
}

func New() *Adapter {
	return &Adapter{routes: map[string]api.RouteRequest{}}
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRouting }

func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, &a.config); err != nil {
		return errs.Wrap(errs.ValidInvalid, "The loopback routing configuration could not be read.", err)
	}
	return nil
}

// HealthCheck always succeeds: there is no external dependency to be unhealthy.
func (a *Adapter) HealthCheck(context.Context) error { return nil }

// Capabilities reports port mode only, and no TLS.
//
// Stated honestly so the planner refuses a subdomain app at plan time with
// PLAN_CAPABILITY_UNSUPPORTED, rather than accepting it and producing an app
// nobody can reach.
func (a *Adapter) Capabilities(context.Context) (api.RoutingCapabilities, error) {
	return api.RoutingCapabilities{
		Modes:       []api.RoutingMode{spec.RoutingPort},
		DefaultMode: spec.RoutingPort,

		SupportsTLS:         false,
		SupportsWildcardTLS: false,

		// Nothing needs to be publicly reachable — this is a laptop.
		RequiresPublicReachability: false,
	}, nil
}

// Ensure records that an app should be reachable on a port.
func (a *Adapter) Ensure(_ context.Context, r api.RouteRequest) (api.RouteHandle, error) {
	if r.Mode != spec.RoutingPort {
		return api.RouteHandle{}, errs.Newf(errs.PlanCapabilityUnsupported,
			"Loopback routing can only serve apps on a port.").
			WithDetail("requested", string(r.Mode)).
			WithRemedy("Use port mode for this app, or configure a routing option that supports hostnames.")
	}
	if r.ProxyUpstream == "" {
		// The contract is that an adapter is told where to point (R-023). An
		// empty upstream means the caller built the request wrongly, and
		// guessing would be how a route ends up bypassing the proxy.
		return api.RouteHandle{}, errs.New(errs.AdapterFailed,
			"No proxy address was given for this app's traffic.")
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.routes[r.AppID] = r

	return api.RouteHandle{AppID: r.AppID, Handle: fmt.Sprintf("port:%d", r.Port)}, nil
}

func (a *Adapter) Remove(_ context.Context, h api.RouteHandle) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.routes, h.AppID)
	return nil
}

func (a *Adapter) Observe(_ context.Context, h api.RouteHandle) (api.RouteState, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()

	r, ok := a.routes[h.AppID]
	if !ok {
		return api.RouteState{Present: false}, nil
	}
	// The adapter cannot name the host a browser uses, so this is the laptop
	// answer. It is not what anyone is shown: the API renders the address from
	// the request's host (spec.Address). Config.BaseURL is deliberately not
	// read here; see Config.
	return api.RouteState{Present: true, Address: fmt.Sprintf("http://localhost:%d", r.Port)}, nil
}

// Edge: none. Loopback is Pando's own listeners; nothing stands in front.
func (a *Adapter) Edge(context.Context, api.EdgeRequest) (api.EdgePlan, bool, error) {
	return api.EdgePlan{}, false, nil
}

var _ api.RoutingAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryRouting,
		Kind:        Kind,
		Name:        "Built in",
		Description: "Serves each app from Pando itself on its own port, with no TLS.",
		IDPrefix:    "rte_",
		// No fields: there is nothing to configure (see Config).
	}
}
