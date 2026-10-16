package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

type routingCaps struct {
	adapterapi.RoutingAdapter
	caps adapterapi.RoutingCapabilities
	err  error
}

func (routingCaps) Kind() string                                     { return "stub" }
func (routingCaps) Category() adapterapi.Category                    { return adapterapi.CategoryRouting }
func (routingCaps) Configure(context.Context, json.RawMessage) error { return nil }
func (routingCaps) HealthCheck(context.Context) error                { return nil }
func (r routingCaps) Capabilities(context.Context) (adapterapi.RoutingCapabilities, error) {
	return r.caps, r.err
}

// TestR165_PandosOwnHostnamesAreReserved asserts the list R-165's reserved
// hostnames are read from: the external URL's host and every routing
// adapter's console hostname, read when asked — and not a base domain, whose
// apex an app may have when the console is elsewhere.
func TestR165_PandosOwnHostnamesAreReserved(t *testing.T) {
	reg := adapterapi.NewRegistry()
	require.NoError(t, reg.Register("rte_traefik", routingCaps{caps: adapterapi.RoutingCapabilities{
		Modes: []spec.RoutingMode{spec.RoutingSubdomain}, BaseDomain: "apps.example.com",
		ReservedHostnames: []string{"Console.Example.com."}}}))
	require.NoError(t, reg.Register("rte_broken", routingCaps{err: errors.New("unreachable")}))

	external, err := url.Parse("https://pando.example.com:8443")
	require.NoError(t, err)

	got := reservedHostnames(reg, external)(context.Background())
	require.ElementsMatch(t, []string{"pando.example.com", "console.example.com"}, got)

	require.Empty(t, reservedHostnames(nil, nil)(context.Background()))
}

// TestR165_StartupReservesTheExternalHostname asserts the wiring: the apps
// store is told Pando's hostname, from the external URL.
func TestR165_StartupReservesTheExternalHostname(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Addr = ":8080"
	cfg.Server.ExternalURL = "https://pando.example.com"
	apps := state.NewApps(nil)

	guardOwnAddresses(cfg, adapterapi.NewRegistry(), apps)
	require.NotNil(t, apps.ReservedHostnames)
	require.Equal(t, []string{"pando.example.com"}, apps.ReservedHostnames(context.Background()))
}
