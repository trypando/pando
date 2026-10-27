package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/edge"
	"github.com/trypando/pando/internal/core/edgecert"
)

// TestR417_AControlHostThatDoesNotLeadServesTheLeadersCertificates asserts
// the follower's edge pass is wired as O-68 says: the same registry, proxy
// and host count as the leader's, an issuer that only reads what the leader
// holds, and none at all where the leader has none to issue with.
func TestR417_AControlHostThatDoesNotLeadServesTheLeadersCertificates(t *testing.T) {
	hosts := func(context.Context) (int, error) { return 2, nil }
	leading := func() bool { return false }
	leaderPass := &edge.Service{
		Registry: adapterapi.NewRegistry(), ProxyUpstream: "http://pando:8080", Clock: clock.System{},
		Hosts: hosts, Certificates: &edgecert.Issuer{},
	}
	f := edgeFollower(leaderPass, nil, leading)
	require.True(t, f.Follower)
	require.Same(t, leaderPass.Registry, f.Registry)
	require.Equal(t, leaderPass.ProxyUpstream, f.ProxyUpstream)
	require.NotNil(t, f.Hosts)
	require.NotNil(t, f.Leading)
	require.IsType(t, edgecert.Held{}, f.Certificates)

	leaderPass.Certificates = nil
	require.Nil(t, edgeFollower(leaderPass, nil, leading).Certificates)
}
