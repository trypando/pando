package loopback_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/loopback"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

func portRoute(appID string, port int) api.RouteRequest {
	return api.RouteRequest{
		AppID: appID, Mode: spec.RoutingPort, Port: port,
		ProxyUpstream: "127.0.0.1:8080",
	}
}

func TestIdentity(t *testing.T) {
	a := loopback.New()
	require.Equal(t, loopback.Kind, a.Kind())
	require.Equal(t, api.CategoryRouting, a.Category())
	require.NoError(t, a.HealthCheck(context.Background()), "there is no external dependency to be unhealthy")
}

func TestConfigure(t *testing.T) {
	a := loopback.New()
	require.NoError(t, a.Configure(context.Background(), nil), "nothing to configure is the normal case")

	require.Error(t, a.Configure(context.Background(), json.RawMessage(`{`)))
	require.Equal(t, errs.ValidInvalid,
		errs.CodeOf(a.Configure(context.Background(), json.RawMessage(`{`))))

	require.NoError(t, a.Configure(context.Background(), json.RawMessage(`{"base_url":"http://127.0.0.1"}`)))
}

// R-254: capabilities are data, so the planner can refuse a subdomain app at
// plan time rather than producing an app nobody can reach.
func TestR254_CapabilitiesReportPortModeOnly(t *testing.T) {
	caps, err := loopback.New().Capabilities(context.Background())
	require.NoError(t, err)

	require.Equal(t, []api.RoutingMode{spec.RoutingPort}, caps.Modes)
	require.Equal(t, spec.RoutingMode(spec.RoutingPort), spec.RoutingMode(caps.DefaultMode))
	require.False(t, caps.SupportsTLS)
	require.False(t, caps.SupportsWildcardTLS)
	require.False(t, caps.RequiresPublicReachability, "this is a laptop")
}

func TestEnsureRecordsIntentAndObserveReportsIt(t *testing.T) {
	a := loopback.New()
	ctx := context.Background()

	handle, err := a.Ensure(ctx, portRoute("app_01HQ8", 3000))
	require.NoError(t, err)
	require.Equal(t, "app_01HQ8", handle.AppID)
	require.Equal(t, "port:3000", handle.Handle)

	state, err := a.Observe(ctx, handle)
	require.NoError(t, err)
	require.True(t, state.Present)
	require.Equal(t, "http://localhost:3000", state.Address)
}

// A base_url stored by an earlier version still loads, and changes nothing:
// the address a person sees comes from the host they reached Pando on
// (spec.Address, R-261), not from a setting on this adapter.
func TestR261_AStoredBaseURLLoadsAndChangesNoAddress(t *testing.T) {
	a := loopback.New()
	ctx := context.Background()
	require.NoError(t, a.Configure(ctx, json.RawMessage(`{"base_url":"http://dev.local"}`)))

	handle, err := a.Ensure(ctx, portRoute("app_01HQ8", 8443))
	require.NoError(t, err)

	state, err := a.Observe(ctx, handle)
	require.NoError(t, err)
	require.Equal(t, "http://localhost:8443", state.Address)
}

// The form offers nothing that does nothing, and the description promises
// only the mode Capabilities reports.
func TestR161_InfoDescribesPortModeAndOffersNoBaseURL(t *testing.T) {
	info := loopback.Info()
	require.Equal(t, api.CategoryRouting, info.Category)
	require.Equal(t, loopback.Kind, info.Kind)
	for _, f := range info.Fields {
		require.NotEqual(t, "base_url", f.Key, "base_url has no effect and must not be offered")
	}
	require.Contains(t, info.Description, "port")
	require.NotContains(t, info.Description, "path", "Capabilities report port mode only")

	caps, err := loopback.New().Capabilities(context.Background())
	require.NoError(t, err)
	require.Equal(t, []api.RoutingMode{spec.RoutingPort}, caps.Modes)
}

func TestWarningsNameAStoredBaseURL(t *testing.T) {
	w := loopback.Warnings(json.RawMessage(`{"base_url":"http://dev.local"}`))
	require.Len(t, w, 1)
	require.Contains(t, w[0], "base_url")

	require.Empty(t, loopback.Warnings(nil))
	require.Empty(t, loopback.Warnings(json.RawMessage(`{}`)))
	require.Empty(t, loopback.Warnings(json.RawMessage(`{"base_url":""}`)))
	require.Empty(t, loopback.Warnings(json.RawMessage(`{`)), "unreadable configuration is Configure's error, not a warning")
}

func TestObserveOfAnUnknownAppReportsAbsentRatherThanFailing(t *testing.T) {
	state, err := loopback.New().Observe(context.Background(), api.RouteHandle{AppID: "app_GONE"})
	require.NoError(t, err)
	require.False(t, state.Present)
	require.Empty(t, state.Address)
}

func TestRemoveIsIdempotent(t *testing.T) {
	a := loopback.New()
	ctx := context.Background()

	handle, err := a.Ensure(ctx, portRoute("app_01HQ8", 3000))
	require.NoError(t, err)

	require.NoError(t, a.Remove(ctx, handle))
	require.NoError(t, a.Remove(ctx, handle), "removing a route that is gone is not an error")

	state, err := a.Observe(ctx, handle)
	require.NoError(t, err)
	require.False(t, state.Present)
}

// The adapter states port mode only, so it must also refuse anything else
// rather than accepting a route it cannot serve.
func TestEnsureRefusesAModeItCannotServe(t *testing.T) {
	r := portRoute("app_01HQ8", 3000)
	r.Mode = spec.RoutingSubdomain

	_, err := loopback.New().Ensure(context.Background(), r)
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.NotEmpty(t, errs.As(err).Remedy)
}

// R-023: an adapter is told where to point. An empty upstream is a caller bug,
// and guessing is how a route ends up bypassing the proxy.
func TestR023_EnsureRefusesARouteWithNoProxyUpstream(t *testing.T) {
	r := portRoute("app_01HQ8", 3000)
	r.ProxyUpstream = ""

	_, err := loopback.New().Ensure(context.Background(), r)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
}
