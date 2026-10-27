package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/hostagent"
)

// fakeRuntime is a Docker API on a unix socket with one running app
// container, counting the stops it is asked for. Not served until serve.
type fakeRuntime struct {
	socket  string
	stopped atomic.Int32
}

func newFakeRuntime(t *testing.T) *fakeRuntime {
	t.Helper()
	// Not under t.TempDir: a unix socket's path must be short.
	dir, err := os.MkdirTemp("", "pr")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return &fakeRuntime{socket: filepath.Join(dir, "d.sock")}
}

func (f *fakeRuntime) serve(t *testing.T) {
	t.Helper()
	ln, err := net.Listen("unix", f.socket)
	require.NoError(t, err)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.47")
		switch {
		case strings.HasSuffix(r.URL.Path, "/containers/json"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `[{"Id":"c1","State":"running","Labels":{"io.pando.managed":"true","io.pando.app":"app_1"}}]`)
		case strings.HasSuffix(r.URL.Path, "/stop"):
			f.stopped.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			_, _ = io.WriteString(w, "OK")
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
}

// TestR153_AnAppHostWhoseAgentFailsToStartStillFencesWhenCutOff asserts
// R-153 with R-421 on an app host whose full agent cannot start: it still
// runs its Monitor, which stops the host's app containers through its own
// runtime when it is cut off, so its standbys never run beside them; and it
// answers the cluster's pings only while that runtime answers, so it never
// says it is well while it could not stop its apps. Its refusal is still
// contact, so it is not taken for lost while it can be reached.
func TestR153_AnAppHostWhoseAgentFailsToStartStillFencesWhenCutOff(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	writeJoinedFiles(t, dir, authority, "edge-1")
	joined := hosts.Joined{HostID: appHostID, Name: "edge-1", Roles: []string{"app"}, Cluster: "https://127.0.0.1:1"}
	runtime := newFakeRuntime(t)
	agentAddr := freeAddr(t)
	t.Setenv(multidocker.EnvAgentRuntimeSocket, runtime.socket)
	t.Setenv(multidocker.EnvAgentPool, "everywhere") // the full agent cannot start
	t.Setenv(multidocker.EnvAgentListen, agentAddr)
	every := fenceGateEvery
	fenceGateEvery = func(liveness.Config) time.Duration { return 0 }
	t.Cleanup(func() { fenceGateEvery = every })

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runAppHost(ctx, dir, joined, liveness.Config{}, zap.NewNop()) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	ping := func() error {
		_, err := pingAs(t, authority, agentAddr, "edge-1")
		return err
	}
	var perr error
	require.Eventually(t, func() bool {
		perr = ping()
		return hostagent.Contacted(perr) && perr != nil && strings.Contains(perr.Error(), "refused the ping")
	}, 10*time.Second, 50*time.Millisecond, "its runtime is away: no view, and still contact")
	require.ErrorContains(t, perr, "cannot stop its own apps")

	runtime.serve(t)
	require.Eventually(t, func() bool { return ping() == nil }, 10*time.Second, 50*time.Millisecond,
		"its runtime answers: so does it")

	// Its Monitor, cut off from both other hosts, stops its app through
	// that runtime.
	require.NoError(t, hosts.WritePeers(filepath.Join(dir, hosts.PeersFile), []hosts.Peer{
		{ID: appHostID, Name: "edge-1"}, {ID: "host_b", Name: "edge-2", Address: "10.0.0.2:7443"}, {ID: "host_c", Name: "edge-3", Address: "10.0.0.3:7443"},
	}))
	_, monitor, err := appHostFallback(dir, joined, liveness.Config{}, zap.NewNop())
	require.NoError(t, err)
	clk := clock.NewFake(time.Time{})
	monitor.Clock = clk
	monitor.Probe = func(context.Context, liveness.Peer) ([]byte, error) { return nil, errors.New("no route to host") }
	interval := liveness.Config{}.Normalize().Interval
	for round := 0; round < 20 && runtime.stopped.Load() == 0; round++ {
		monitor.Round(ctx)
		monitor.Settle()
		clk.Advance(interval)
	}
	require.Positive(t, runtime.stopped.Load(), "the cut-off host stopped its app")
}

// TestAnAppHostFallbackStartsFromItsOwnCertificateAlone checks what the
// fallback needs: its own certificate, not its probe certificate, and that
// a host with no certificate of its own runs nothing.
func TestAnAppHostFallbackStartsFromItsOwnCertificateAlone(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	joined := hosts.Joined{HostID: appHostID, Name: "edge-1", Cluster: "https://127.0.0.1:1"}
	_, _, err = appHostFallback(dir, joined, liveness.Config{}, zap.NewNop())
	require.ErrorContains(t, err, "server.cluster_dir")

	writeJoinedFiles(t, dir, authority, "edge-1")
	require.NoError(t, os.Remove(filepath.Join(dir, hosts.HostClientKeyFile)))
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	agent, monitor, err := appHostFallback(dir, joined, liveness.Config{}, zap.NewNop())
	require.NoError(t, err)
	require.NotNil(t, monitor)
	require.Nil(t, agent.Liveness.Answer(""), "no runtime known: it reports nothing")
	n, err := monitor.Fencer.Fence(context.Background())
	require.Zero(t, n)
	require.ErrorContains(t, err, "could not be reached")
}
