package main

import (
	"context"
	"crypto/tls"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/id"
)

func newRows(list ...state.Host) *rows {
	return &rows{hosts: list, lost: map[string]bool{}, fences: map[string]string{}}
}

// TestR421_AControlHostTakesPartOnceItCan asserts R-421's wiring: a control
// host not yet recorded, or whose database is away, tries again until it
// answers pings, and refuses them by name until then; every other listener
// connection is served as before.
func TestR421_AControlHostTakesPartOnceItCan(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	store := newRows()
	store.down = true
	cl := controlLiveness{
		Store: store, Authority: authority, Dir: filepath.Join(t.TempDir(), "cluster"), Port: 7444,
		IsSelf: hosts.InstallHost, Docker: &appDocker{}, Logger: zap.NewNop(),
	}
	ctx := context.Background()
	_, err = cl.setup(ctx)
	require.ErrorIs(t, err, errDatabaseAway)
	store.down = false
	_, err = cl.setup(ctx)
	require.ErrorContains(t, err, "not recorded")

	store.hosts = []state.Host{{ID: id.New(id.Host), Name: "core-1", Address: "10.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)}}
	unwritable := cl
	unwritable.Dir = filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(unwritable.Dir, nil, 0o600))
	_, err = unwritable.setup(ctx)
	require.ErrorContains(t, err, "server.cluster_dir")

	pings := &controlPings{}
	_, err = pings.configFor(&tls.ClientHelloInfo{SupportedProtos: []string{hostagent.PingProtocol}})
	require.ErrorIs(t, err, errNotWatching)
	cfg, err := pings.configFor(&tls.ClientHelloInfo{SupportedProtos: []string{"http/1.1"}})
	require.NoError(t, err)
	require.Nil(t, cfg, "the listener's own configuration")
	pings.serve(nil, nil, nil) // nothing to answer with yet: left unanswered

	runCtx, cancel := context.WithCancel(ctx)
	started := cl.start(runCtx)
	require.Eventually(t, func() bool { return started.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	cancel()

	// A host that cannot take part yet keeps trying until it stops.
	stopped, stop := context.WithCancel(ctx)
	never := cl
	never.IsSelf = func(state.Host) bool { return false }
	waiting := never.start(stopped)
	stop()
	require.Nil(t, waiting.current.Load())
}

func TestStartControlLivenessPicksItsRowAndARuntime(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	joinedID := id.New(id.Host)
	store := newRows(
		state.Host{ID: id.New(id.Host), Name: "core-1", Address: "10.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)},
		state.Host{ID: joinedID, Name: "core-2", Address: "10.0.0.2:7443", CertSHA256: strings.Repeat("0", 64), JoinedWith: "jt_1"},
	)
	firstID := store.hosts[0].ID
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	t.Setenv("DOCKER_HOST", "unix:///nowhere/docker.sock")
	joined := startControlLiveness(ctx, filepath.Join(t.TempDir(), "cluster"), 7444, authority,
		hosts.Joined{HostID: joinedID}, true, zap.NewNop())
	joined.useStore(store)
	require.Eventually(t, func() bool { return joined.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, joinedID, joined.current.Load().monitor.Self)

	t.Setenv("DOCKER_HOST", "::not a host::")
	first := startControlLiveness(ctx, filepath.Join(t.TempDir(), "cluster"), 7444, authority,
		hosts.Joined{}, false, zap.NewNop())
	first.useStore(store)
	require.Eventually(t, func() bool { return first.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, firstID, first.current.Load().monitor.Self)
	_, err = first.current.Load().monitor.Fencer.Fence(ctx)
	require.ErrorContains(t, err, "could not be reached", "a runtime it cannot reach still leaves it answering pings")
}

func TestUnreachableRuntimeSaysWhy(t *testing.T) {
	u := unreachableRuntime{err: errors.New("no socket")}
	_, err := u.ContainerList(context.Background(), client.ContainerListOptions{})
	require.ErrorContains(t, err, "no socket")
	_, err = u.ContainerStop(context.Background(), "c1", client.ContainerStopOptions{})
	require.ErrorContains(t, err, "no socket")
}
