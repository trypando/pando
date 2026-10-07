package edge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// fakeRuntime records edges. Methods an edge never calls are the embedded
// interface's, and panic if reached.
type fakeRuntime struct {
	api.RuntimeAdapter
	supports bool
	edges    map[string]api.EdgePlan
	applyErr error
}

func (*fakeRuntime) Kind() string                                     { return "fake" }
func (*fakeRuntime) Category() api.Category                           { return api.CategoryRuntime }
func (*fakeRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (*fakeRuntime) HealthCheck(context.Context) error                { return nil }
func (f *fakeRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsEdge: f.supports}, nil
}
func (f *fakeRuntime) ApplyEdge(_ context.Context, p api.EdgePlan) error {
	if f.applyErr != nil {
		return f.applyErr
	}
	f.edges[p.Name] = p
	return nil
}
func (f *fakeRuntime) ObserveEdge(_ context.Context, name string) (api.EdgeState, error) {
	_, ok := f.edges[name]
	return api.EdgeState{Present: ok, Running: ok}, nil
}
func (f *fakeRuntime) RemoveEdge(_ context.Context, name string) error {
	delete(f.edges, name)
	return nil
}
func (f *fakeRuntime) Edges(context.Context) ([]string, error) {
	var names []string
	for n := range f.edges {
		names = append(names, n)
	}
	return names, nil
}

type fakeRouting struct {
	api.RoutingAdapter
	needs bool
	got   api.EdgeRequest
	issue *api.CertificateIssue
}

func (*fakeRouting) Kind() string                                     { return "fake" }
func (*fakeRouting) Category() api.Category                           { return api.CategoryRouting }
func (*fakeRouting) Configure(context.Context, json.RawMessage) error { return nil }
func (*fakeRouting) HealthCheck(context.Context) error                { return nil }
func (f *fakeRouting) Edge(_ context.Context, r api.EdgeRequest) (api.EdgePlan, bool, error) {
	f.got = r
	if !f.needs {
		return api.EdgePlan{}, false, nil
	}
	return api.EdgePlan{Image: "traefik:v3.2", Issue: f.issue}, true, nil
}

func setup(t *testing.T, rt *fakeRuntime, routes map[string]*fakeRouting) *Service {
	t.Helper()
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("rt_docker", rt))
	require.NoError(t, reg.SetDefault(api.CategoryRuntime, "rt_docker"))
	for ref, r := range routes {
		require.NoError(t, reg.Register(ref, r))
	}
	return &Service{Registry: reg, ProxyUpstream: "http://pando:8080"}
}

// TestR174_AnEdgeARoutingAdapterAsksForIsRun asserts R-174: the edge is a
// setting in Pando, and Pando runs it — named for the adapter, told where
// Pando's proxy is, and reaching it by the proxy's host name.
func TestR174_AnEdgeARoutingAdapterAsksForIsRun(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	traefik := &fakeRouting{needs: true}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": traefik, "rte_loopback": {}})

	require.NoError(t, s.Reconcile(context.Background()))

	require.Len(t, rt.edges, 1)
	plan := rt.edges["rte_traefik"]
	require.Equal(t, "pando", plan.ProxyAlias)
	require.Equal(t, "http://pando:8080", traefik.got.ProxyUpstream)

	st, ok := s.Status("rte_traefik")
	require.True(t, ok)
	require.True(t, st.Running)
	_, ok = s.Status("rte_loopback")
	require.False(t, ok, "an adapter without an edge has no edge status")
}

// TestR174_AnEdgeNothingAsksForIsRemoved covers switching Traefik to somebody
// else's, or removing the adapter.
func TestR174_AnEdgeNothingAsksForIsRemoved(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{"rte_old": {}}}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": {needs: false}})

	require.NoError(t, s.Reconcile(context.Background()))
	require.Empty(t, rt.edges)
}

// TestR254_AnEdgeOnARuntimeThatCannotRunOneIsRefusedWithAReason asserts
// R-254: the capability is data, and its absence is a readable refusal rather
// than an edge configured and never started.
func TestR254_AnEdgeOnARuntimeThatCannotRunOneIsRefusedWithAReason(t *testing.T) {
	rt := &fakeRuntime{supports: false, edges: map[string]api.EdgePlan{}}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": {needs: true}})

	err := s.Reconcile(context.Background())
	require.Error(t, err)
	st, ok := s.Status("rte_traefik")
	require.True(t, ok)
	require.False(t, st.Running)
	require.Contains(t, st.Message, "cannot run one")
}

func TestOneBrokenEdgeDoesNotStopAnother(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	rt.applyErr = errs.New(errs.AdapterFailed, "port 80 is taken.").WithRemedy("Free it.")
	s := setup(t, rt, map[string]*fakeRouting{"rte_a": {needs: true}, "rte_b": {needs: true}})

	err := s.Reconcile(context.Background())
	var e *errs.Error
	require.True(t, errors.As(err, &e))

	for _, ref := range []string{"rte_a", "rte_b"} {
		st, ok := s.Status(ref)
		require.True(t, ok, ref)
		require.Equal(t, "port 80 is taken. Free it.", st.Message, "both were tried")
	}
}
