package hosts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
)

// TestR415_APingFromAHostNoLongerListedIsRefused asserts R-415's removal on
// the hosts' side: once a peer list no longer has a host, Members refuses
// it; a failed read keeps the list last heard, and before any list every
// host is taken.
func TestR415_APingFromAHostNoLongerListedIsRefused(t *testing.T) {
	m := &hosts.Members{}
	require.True(t, m.Has("host_gone"), "nothing heard yet")
	lists := [][]hosts.Peer{{{ID: "host_a"}, {ID: "host_gone"}}, {{ID: "host_a"}}, nil}
	errsFor := []error{nil, nil, errors.New("the cluster is away")}
	i := 0
	peers := m.Track(func(context.Context) ([]hosts.Peer, error) {
		defer func() { i++ }()
		return lists[i], errsFor[i]
	})
	for range lists {
		_, _ = peers(context.Background())
	}
	require.True(t, m.Has("host_a"))
	require.False(t, m.Has("host_gone"), "removed, and the failed read did not bring it back")
}
