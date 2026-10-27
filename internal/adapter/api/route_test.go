package api_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// TestR169_EveryRouteAsksForTLSUnlessItIsAPort asserts the one route request
// the deploy, the reconciler and route sync all write: TLS for a hostname,
// none for a bare port, and always pointed at Pando's proxy (R-023).
func TestR169_EveryRouteAsksForTLSUnlessItIsAPort(t *testing.T) {
	sub := api.RouteFor("app_a", spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "a.example.com"}, "http://pando:8080")
	require.Equal(t, api.RouteRequest{
		AppID: "app_a", Mode: spec.RoutingSubdomain, Hostname: "a.example.com", ProxyUpstream: "http://pando:8080",
		TLS: api.TLSRequest{Enabled: true, Hostname: "a.example.com"},
	}, sub)

	path := api.RouteFor("app_b", spec.Routing{Mode: spec.RoutingPath, Hostname: "apps.example.com", PathPrefix: "/b"}, "u")
	require.Equal(t, "/b", path.PathPrefix)
	require.True(t, path.TLS.Enabled)

	port := api.RouteFor("app_c", spec.Routing{Mode: spec.RoutingPort, Port: 9000}, "u")
	require.Equal(t, 9000, port.Port)
	require.False(t, port.TLS.Enabled, "a bare port has no hostname to certify")
}
