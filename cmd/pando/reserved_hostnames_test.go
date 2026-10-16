package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
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
