package hosts_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
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

// TestR415_ARevokedHostStaysRefused asserts R-415's revocations on the
// hosts' side: a revoked ID or certificate is refused whatever list is heard
// after, revocations are only ever added to, and they are kept on disk and
// read back before any list, written again only when something was added or
// the last write failed.
func TestR415_ARevokedHostStaysRefused(t *testing.T) {
	m := &hosts.Members{}
	require.True(t, m.Admits("host_gone", "sha_gone"))
	file := filepath.Join(t.TempDir(), hosts.RevokedFile)
	require.NoError(t, m.KeepRevoked(file, state.Revoked{Hosts: []string{"host_gone"}, Certs: []string{"sha_gone"}}))
	_, _ = m.Track(func(context.Context) ([]hosts.Peer, error) {
		return []hosts.Peer{{ID: "host_gone"}, {ID: "host_a"}}, nil
	})(context.Background())
	require.False(t, m.Admits("host_gone", ""), "an older list naming it does not bring it back")
	require.False(t, m.Admits("host_a", "sha_gone"), "a revoked certificate, whoever it names")
	require.True(t, m.Admits("host_a", ""))
	require.False(t, m.Revoke(state.Revoked{Hosts: []string{"host_gone"}}), "nothing new")

	require.NoError(t, os.Remove(file))
	require.NoError(t, m.KeepRevoked(file, state.Revoked{}), "nothing new: not written")
	_, err := os.Stat(file)
	require.True(t, os.IsNotExist(err))

	bad := filepath.Join(t.TempDir(), "missing", hosts.RevokedFile)
	require.Error(t, m.KeepRevoked(bad, state.Revoked{Hosts: []string{"host_b"}}))
	require.NoError(t, m.KeepRevoked(file, state.Revoked{}), "the last write failed: written now")

	restarted := &hosts.Members{}
	restarted.LoadRevoked(filepath.Join(t.TempDir(), "none"))
	restarted.LoadRevoked(file)
	require.False(t, restarted.Admits("host_gone", ""))
	require.False(t, restarted.Admits("host_b", ""))
	require.False(t, restarted.Admits("host_a", "sha_gone"))
	require.ElementsMatch(t, []string{"host_gone", "host_b"}, restarted.Revoked().Hosts)
}

// TestR415_APeerListBringsWhatIsRevoked asserts that a joined host hears
// the hosts removed from the cluster with its peer list, and keeps them
// beside it.
func TestR415_APeerListBringsWhatIsRevoked(t *testing.T) {
	list := hosts.PeerList{Hosts: []hosts.Peer{{ID: "host_a"}}, Revoked: state.Revoked{Hosts: []string{"host_gone"}, Certs: []string{}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(list)
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	m := &hosts.Members{}
	sync := &hosts.PeerSync{Address: srv.URL, File: filepath.Join(dir, hosts.PeersFile), HTTP: srv.Client(), Members: m}
	got, err := sync.Peers(context.Background())
	require.NoError(t, err)
	require.Equal(t, list.Hosts, got)
	require.False(t, m.Admits("host_gone", ""))
	kept := &hosts.Members{}
	kept.LoadRevoked(filepath.Join(dir, hosts.RevokedFile))
	require.False(t, kept.Admits("host_gone", ""), "kept beside the peer list")

	require.NoError(t, os.Remove(filepath.Join(dir, hosts.RevokedFile)))
	require.NoError(t, os.Mkdir(filepath.Join(dir, hosts.RevokedFile), 0o700))
	list.Revoked.Hosts = append(list.Revoked.Hosts, "host_also_gone")
	_, err = sync.Peers(context.Background())
	require.ErrorContains(t, err, "could not be kept")
	require.False(t, m.Admits("host_also_gone", ""), "refused even when it cannot be kept")
}
