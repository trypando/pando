//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/edge"
	"github.com/trypando/pando/internal/core/state"
)

type edgeRouting struct{ adapterapi.RoutingAdapter }

func (edgeRouting) Kind() string                                     { return "traefik" }
func (edgeRouting) Category() adapterapi.Category                    { return adapterapi.CategoryRouting }
func (edgeRouting) Configure(context.Context, json.RawMessage) error { return nil }
func (edgeRouting) HealthCheck(context.Context) error                { return nil }
func (edgeRouting) Capabilities(context.Context) (adapterapi.RoutingCapabilities, error) {
	return adapterapi.RoutingCapabilities{}, nil
}
func (edgeRouting) Edge(context.Context, adapterapi.EdgeRequest) (adapterapi.EdgePlan, bool, error) {
	return adapterapi.EdgePlan{Image: "traefik:v3.2"}, true, nil
}

// noEdgeRuntime is a runtime that cannot run an edge.
type noEdgeRuntime struct{ adapterapi.RuntimeAdapter }

func (noEdgeRuntime) LargestFitFor(context.Context, string) (*adapterapi.Fit, error) {
	return nil, nil
}

func (noEdgeRuntime) Kind() string                                     { return "docker" }
func (noEdgeRuntime) Category() adapterapi.Category                    { return adapterapi.CategoryRuntime }
func (noEdgeRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (noEdgeRuntime) HealthCheck(context.Context) error                { return nil }
func (noEdgeRuntime) Capabilities(context.Context) (adapterapi.RuntimeCapabilities, error) {
	return adapterapi.RuntimeCapabilities{}, nil
}

// TestR174_TheAdaptersListSaysWhenAnEdgeIsNotRunning asserts R-174's other
// half: Pando runs the edge, so Pando says when it could not — and why, to
// whoever may change the adapter.
func TestR174_TheAdaptersListSaysWhenAnEdgeIsNotRunning(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	require.NoError(t, i.Adapters.Upsert(ctx, state.AdapterConfig{
		ID: "rte_traefik", Category: "routing", Kind: "traefik", Name: "Traefik", Enabled: true,
	}))
	reg := i.Server.Registry
	require.NoError(t, reg.Register("rte_traefik", edgeRouting{}))
	require.NoError(t, reg.Register("rt_docker", noEdgeRuntime{}))
	require.NoError(t, reg.SetDefault(adapterapi.CategoryRuntime, "rt_docker"))

	edges := &edge.Service{Registry: reg, ProxyUpstream: "http://pando:8080"}
	require.Error(t, edges.Reconcile(ctx), "a runtime that cannot run an edge refuses readably")
	i.Server.Edges = edges

	var listed struct {
		Adapters []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Edge   *struct {
				Running   bool   `json:"running"`
				Message   string `json:"message"`
				CheckedAt string `json:"checked_at"`
			} `json:"edge"`
		} `json:"adapters"`
	}
	got := i.do(i.admin(), http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &listed)

	var found bool
	for _, a := range listed.Adapters {
		if a.ID != "rte_traefik" {
			require.Nil(t, a.Edge, "%s has no edge", a.ID)
			continue
		}
		found = true
		require.NotNil(t, a.Edge)
		require.False(t, a.Edge.Running)
		require.Contains(t, a.Edge.Message, "cannot run one")
		require.NotEmpty(t, a.Edge.CheckedAt)
	}
	require.True(t, found)
}
