package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

type fenceRows struct {
	writes []bool
	err    error
}

func (f *fenceRows) SetHostFenced(_ context.Context, _ string, fenced bool) (bool, error) {
	if f.err != nil {
		return false, f.err
	}
	f.writes = append(f.writes, fenced)
	return true, nil
}

// TestR421_TheLeaderRecordsWhatEachHostSaysOfItsFence asserts what the
// reconciler on every replica reads to leave a fenced host's apps stopped:
// the leader's probes record each host's fence when it changes, and only
// then; a write that fails is made again; an answer that is not the host's
// view is ignored, as is a probe that failed.
func TestR421_TheLeaderRecordsWhatEachHostSaysOfItsFence(t *testing.T) {
	ctx := context.Background()
	rows := &fenceRows{}
	rec := &fenceRecorder{store: rows, logger: zap.NewNop()}
	answer := liveness.View{Host: "host_a"}
	var probeErr error
	probe := rec.wrap(func(context.Context, liveness.Peer) ([]byte, error) {
		raw, _ := json.Marshal(answer)
		return raw, probeErr
	})
	peer := liveness.Peer{ID: "host_a"}

	_, _ = probe(ctx, peer)
	_, _ = probe(ctx, peer)
	require.Equal(t, []bool{false}, rows.writes, "written once, then only on a change")
	answer.Fenced = true
	rows.err = errors.New("database away")
	_, _ = probe(ctx, peer)
	rows.err = nil
	_, _ = probe(ctx, peer)
	require.Equal(t, []bool{false, true}, rows.writes, "a failed write is made again")

	answer.Fenced = false
	probeErr = errors.New("unreached")
	_, _ = probe(ctx, peer)
	_, _ = probe(ctx, liveness.Peer{ID: "host_b"})
	require.Equal(t, []bool{false, true}, rows.writes, "an unreached host keeps what it last said")
}

// TestR421_AControlHostThatHasNotStartedWatchingHasNotFenced asserts the
// control host's own fence reads false until its Monitor runs, and a host
// joined with the control role alone runs no agent.
func TestR421_AControlHostThatHasNotStartedWatchingHasNotFenced(t *testing.T) {
	var none *controlPings
	require.False(t, none.fenced())
	pings := &controlPings{}
	require.False(t, pings.Fencing())
	pings.current.Store(&controlPing{monitor: &liveness.Monitor{}})
	require.False(t, pings.Fencing())

	// The control role alone: nothing is read from the (empty) directory.
	startJoinedAgent(context.Background(), t.TempDir(), hosts.Joined{Roles: []string{state.HostRoleControl}}, pings, zap.NewNop())
	// Both roles, nothing given at join: the agent is not started, and why
	// is logged rather than stopping the server.
	startJoinedAgent(context.Background(), t.TempDir(), hosts.Joined{Roles: []string{state.HostRoleControl, state.HostRoleApp}}, pings, zap.NewNop())
}

// TestR416_AHostWithBothRolesRunsItsAppsThroughItsAgent asserts R-416 for a
// host that joined with the control and app roles: beside its server it runs
// the agent the runtime reaches its apps through, at its agent port, which
// carries a control host's runtime calls to the host's own runtime.
func TestR416_AHostWithBothRolesRunsItsAppsThroughItsAgent(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	writeJoinedFiles(t, dir, authority, "core-2")

	sockDir, err := os.MkdirTemp("", "pb")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sock := filepath.Join(sockDir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	docker := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "OK")
	}), ReadHeaderTimeout: time.Second}
	go func() { _ = docker.Serve(ln) }()
	t.Cleanup(func() { _ = docker.Close() })

	agentAddr := freeAddr(t)
	t.Setenv(multidocker.EnvAgentRuntimeSocket, sock)
	t.Setenv(multidocker.EnvAgentListen, agentAddr)
	t.Setenv(multidocker.EnvAgentPool, "")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	joined := hosts.Joined{HostID: appHostID, Name: "core-2", Roles: []string{state.HostRoleControl, state.HostRoleApp}, Cluster: "https://127.0.0.1:1"}
	startJoinedAgent(ctx, dir, joined, &controlPings{}, zap.NewNop())

	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	control, err := as.IssueClient()
	require.NoError(t, err)
	tlsConfig := hostagent.ClientTLS(as, control, "core-2")
	require.Eventually(t, func() bool {
		conn, err := hostagent.DialRuntime(ctx, agentAddr, tlsConfig)
		if err != nil {
			return false
		}
		defer conn.Close()
		_, _ = io.WriteString(conn, "GET /_ping HTTP/1.1\r\nHost: docker\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			return false
		}
		defer resp.Body.Close()
		return resp.StatusCode == http.StatusOK
	}, 10*time.Second, 50*time.Millisecond, "the runtime is reached through the agent")
}
