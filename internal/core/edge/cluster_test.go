package edge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/secret"
)

// TestR417_EveryControlHostRunsItsOwnEdgeAndOnlyTheLeaderOrders asserts the
// edge pass in a cluster: every edge is told it is one of several, the
// leader's pass orders the certificates, and a host that does not lead runs
// its own edge with what the leader holds, through an issuer that orders
// nothing (O-68).
func TestR417_EveryControlHostRunsItsOwnEdgeAndOnlyTheLeaderOrders(t *testing.T) {
	issue := &api.CertificateIssue{Email: "ops@example.com", Challenge: api.ChallengeHTTP01,
		Orders: []api.CertificateOrder{{Name: "pando-tls-a.example.com", Domains: []string{"a.example.com"}}}}
	cert := api.EdgeCertificate{Name: "pando-tls-a.example.com", CertPEM: []byte("CERT"), KeyPEM: secret.New("KEY")}
	hosts := 2
	count := func(context.Context) (int, error) { return hosts, nil }

	// Host A leads.
	rtA, routeA := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}, &fakeRouting{needs: true, issue: issue}
	a := setup(t, rtA, map[string]*fakeRouting{"rte_traefik": routeA})
	ordering := &fakeIssuer{out: []api.EdgeCertificate{cert}}
	a.Certificates, a.Hosts = ordering, count
	require.NoError(t, a.Reconcile(context.Background()))
	require.True(t, routeA.got.Clustered)
	require.Equal(t, 1, ordering.runs)

	// Host B does not.
	rtB, routeB := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}, &fakeRouting{needs: true, issue: issue}
	b := setup(t, rtB, map[string]*fakeRouting{"rte_traefik": routeB})
	held := &fakeIssuer{out: []api.EdgeCertificate{cert}}
	b.Certificates, b.Hosts, b.Follower = held, count, true
	require.NoError(t, b.Reconcile(context.Background()))
	require.True(t, routeB.got.Clustered)
	require.Equal(t, rtA.edges["rte_traefik"].Certificates, rtB.edges["rte_traefik"].Certificates,
		"both edges serve one certificate")

	// One host again: an edge orders its own, as before.
	hosts = 1
	require.NoError(t, a.Reconcile(context.Background()))
	require.False(t, routeA.got.Clustered)
}

// TestR417_AFollowerLeavesARuntimeWithOneEdgeToTheLeader asserts that a host
// that does not lead applies nothing on a runtime whose one edge serves every
// host (Kubernetes), and that RunFollower passes only in a cluster and only
// while this replica does not lead.
func TestR417_AFollowerLeavesARuntimeWithOneEdgeToTheLeader(t *testing.T) {
	rt := &fakeRuntime{supports: true, edges: map[string]api.EdgePlan{}}
	routing := &fakeRouting{needs: true}
	s := setup(t, rt, map[string]*fakeRouting{"rte_traefik": routing})
	s.Follower = true
	s.Hosts = func(context.Context) (int, error) { return 2, nil }
	reg := s.Registry
	k8s := &edgeConfigRuntime{fakeRuntime: rt, config: []api.EdgeConfig{api.EdgeConfigKubernetesAPI}}
	require.NoError(t, reg.Register("rt_k8s", k8s))
	require.NoError(t, reg.SetDefault(api.CategoryRuntime, "rt_k8s"))
	require.NoError(t, s.Reconcile(context.Background()))
	require.Empty(t, rt.edges)
	require.NoError(t, reg.SetDefault(api.CategoryRuntime, "rt_docker"))

	leading := true
	s.Leading = func() bool { return leading }
	s.Clock = clock.System{}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s.RunFollower(ctx, 10*time.Millisecond)
	require.Empty(t, rt.edges, "the leader's own pass applies its host's edge")

	leading = false
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	s.RunFollower(ctx2, 10*time.Millisecond)
	require.Contains(t, rt.edges, "rte_traefik")

	s.Hosts = func(context.Context) (int, error) { return 0, errors.New("the database is down") }
	require.Error(t, s.Reconcile(context.Background()))
}

type edgeConfigRuntime struct {
	*fakeRuntime
	config []api.EdgeConfig
}

func (f *edgeConfigRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsEdge: true, EdgeConfig: f.config}, nil
}
