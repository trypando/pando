package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// TestR415_AControlHostRefusesARemovedHostItReadsFromTheDatabase asserts
// R-415 on a control host's cluster listener: the hosts removed from the
// cluster, read from the database with the peer list each round, are refused
// at once, by ID and by certificate, and kept, so that the host refuses them
// after a restart while its database is away too.
func TestR415_AControlHostRefusesARemovedHostItReadsFromTheDatabase(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	dir, err := os.MkdirTemp("", "pando-control-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	gone, other := id.New(id.Host), id.New(id.Host)
	revokedCert := strings.Repeat("a", 64)
	store := newRows(state.Host{ID: id.New(id.Host), Name: "core-1", Address: "10.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)},
		state.Host{ID: other, Name: "edge-2", Address: "10.0.0.3:7443", CertSHA256: strings.Repeat("1", 64), JoinedWith: "jt_1"})
	cl := controlLiveness{
		Store: store, Authority: authority, Dir: dir, Port: 7444,
		IsSelf: hosts.InstallHost, Docker: &appDocker{}, Logger: zap.NewNop(),
	}
	ctx := context.Background()
	p, err := cl.setup(ctx)
	require.NoError(t, err)
	require.True(t, p.members.Admits(gone, ""), "before any list, every host not removed")

	store.mu.Lock()
	store.revoked = state.Revoked{Hosts: []string{gone}, Certs: []string{revokedCert}}
	store.mu.Unlock()
	_, err = p.monitor.Peers(ctx)
	require.NoError(t, err)
	require.False(t, p.members.Admits(gone, ""), "removed: refused by its ID")
	require.False(t, p.members.Admits(other, revokedCert), "a revoked certificate is refused whoever it names")
	require.True(t, p.members.Admits(other, ""))

	store.mu.Lock()
	store.down = true
	store.mu.Unlock()
	restarted, err := cl.setup(ctx)
	require.NoError(t, err, "who it is was kept")
	require.False(t, restarted.members.Admits(gone, ""), "after a restart, before the database is reached")
}
