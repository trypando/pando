package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	kubernetesruntime "github.com/trypando/pando/internal/adapter/runtime/kubernetes"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/id"
)

// awayDocker is appDocker whose daemon can be away.
type awayDocker struct {
	appDocker
	away atomic.Bool
}

func (d *awayDocker) Reachable(context.Context) error {
	if d.away.Load() {
		return errors.New("the runtime is away")
	}
	return nil
}

// TestR153_AControlHostThatCannotReachItsRuntimeIsNeverTakenForLost
// asserts R-153 with R-421 on a control host: while the runtime its fence
// stops apps through does not answer, it refuses every ping, which is
// contact, so it is never taken for lost while it can be reached and no
// standby starts beside its apps; once the runtime answers, it answers
// again. A control host the install's runtime places no apps on — on
// Kubernetes or Incus, or holding the control role alone — has nothing of
// its own to fence and answers whatever its Docker does; what the runtime
// is, is kept for the next start, before the database.
func TestR153_AControlHostThatCannotReachItsRuntimeIsNeverTakenForLost(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	// Not under t.TempDir: the Monitor may still write its peer list as the
	// test stops it.
	dir, err := os.MkdirTemp("", "pando-control-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	every := fenceGateEvery
	fenceGateEvery = func(liveness.Config) time.Duration { return 0 }
	t.Cleanup(func() { fenceGateEvery = every })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	addr := freeAddr(t)
	store := newRows(state.Host{ID: id.New(id.Host), Name: "core-1", Roles: []string{state.HostRoleControl, state.HostRoleApp},
		Address: "127.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)})
	docker := &awayDocker{}
	cl := controlLiveness{Store: store, Authority: authority, Dir: dir, Port: portOf(t, addr), IsSelf: hosts.InstallHost,
		Docker: docker, Logger: zap.NewNop()}
	pings := cl.start(ctx)
	require.Eventually(t, func() bool { return pings.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	srv, err := startClusterListener(&config.Config{Server: config.Server{ClusterAddr: addr}}, authority, &startingHandler{}, pings, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	docker.away.Store(true)
	_, err = pingAs(t, authority, addr, "core-1")
	require.ErrorContains(t, err, "cannot stop its own apps", "the runtime is away: no view")
	require.True(t, hostagent.Contacted(err), "and still contact")
	docker.away.Store(false)
	_, err = pingAs(t, authority, addr, "core-1")
	require.NoError(t, err, "the runtime answers: so does it")

	// On Kubernetes nothing of its own is placed here.
	docker.away.Store(true)
	pings.useRuntime("kubernetes")
	_, err = pingAs(t, authority, addr, "core-1")
	require.NoError(t, err, "nothing to fence: it answers")

	// Kept for the next start, before the database is reached.
	restarted := cl
	restarted.Store = nil
	again := restarted.start(ctx)
	require.Eventually(t, func() bool { return again.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	require.NotNil(t, again.current.Load().answer.Answer(""), "kept: Kubernetes")

	// Docker, and this host holds the control role alone: no apps here.
	pings.useRuntime("docker")
	require.Nil(t, pings.current.Load().answer.Answer(""), "Docker places apps here")
	store.mu.Lock()
	store.hosts[0].Roles = []string{state.HostRoleControl}
	store.mu.Unlock()
	controlOnly, err := controlLiveness{Store: store, Authority: authority, Dir: dir, Port: cl.Port, IsSelf: hosts.InstallHost,
		Docker: docker, Logger: zap.NewNop()}.setup(ctx)
	require.NoError(t, err)
	require.NotNil(t, controlOnly.answer.Answer(""), "the control role alone: no apps here")
}

func TestTheDefaultRuntimesKindIsRead(t *testing.T) {
	registry := adapterapi.NewRegistry()
	require.Empty(t, defaultRuntimeKind(registry), "none")
	require.NoError(t, registry.Register("rt_k8s", kubernetesruntime.New()))
	require.NoError(t, registry.SetDefault(adapterapi.CategoryRuntime, "rt_k8s"))
	require.Equal(t, kubernetesruntime.Kind, defaultRuntimeKind(registry))
	require.False(t, placesApps(kubernetesruntime.Kind, true, []string{state.HostRoleControl, state.HostRoleApp}))
	require.True(t, placesApps("", false, []string{state.HostRoleApp}), "not known yet")
	require.False(t, placesApps("", true, []string{state.HostRoleApp}), "no runtime")
}
