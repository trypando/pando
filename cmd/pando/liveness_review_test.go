package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/id"
)

// pingAs pings addr as the leader's collector, checking it is the host named
// name, and returns the View.
func pingAs(t *testing.T, authority *hosts.Authority, addr, name string) (liveness.View, error) {
	t.Helper()
	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	cert, err := as.IssueCollector()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	raw, err := hostagent.Ping(ctx, addr, hostagent.ClientTLS(as, cert, name), "")
	var v liveness.View
	if err == nil {
		err = json.Unmarshal(raw, &v)
	}
	return v, err
}

// TestR421_AControlHostAnswersPingsBeforeItReachesItsDatabase asserts R-421
// on a control host whose Pando is waiting for Postgres: from what it keeps
// in server.cluster_dir — who it is, and the peers it last read — it answers
// pings on its cluster listener and watches its peers, before any database.
// The first start ever, with nothing kept, waits for the database, and keeps
// what the next start needs.
func TestR421_AControlHostAnswersPingsBeforeItReachesItsDatabase(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	selfID := id.New(id.Host)
	peerID := id.New(id.Host)
	dir := filepath.Join(t.TempDir(), "cluster")
	addr := freeAddr(t)
	base := controlLiveness{Authority: authority, Dir: dir, Port: portOf(t, addr), IsSelf: hosts.InstallHost,
		Docker: &appDocker{}, Logger: zap.NewNop()}

	// Nothing kept: it waits for the database.
	waiting := base.start(ctx)
	time.Sleep(50 * time.Millisecond)
	require.Nil(t, waiting.current.Load())
	store := newRows(
		state.Host{ID: selfID, Name: "core-1", Roles: []string{state.HostRoleControl}, Address: "127.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)},
		state.Host{ID: peerID, Name: "app-1", Roles: []string{state.HostRoleApp}, Address: "10.0.0.9:7443", JoinedWith: "jt_1"},
	)
	p, err := controlLiveness{Store: store, Authority: authority, Dir: dir, Port: base.Port, IsSelf: hosts.InstallHost,
		Docker: &appDocker{}, Logger: zap.NewNop()}.setup(ctx)
	require.NoError(t, err)
	list, err := p.monitor.Peers(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)

	// The next start, the database away: it answers from what it kept.
	pings := base.start(ctx)
	require.Eventually(t, func() bool { return pings.current.Load() != nil }, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, selfID, pings.current.Load().monitor.Self)
	kept, err := pings.current.Load().monitor.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, list, kept, "the peers it last read")
	routes := &startingHandler{}
	srv, err := startClusterListener(&config.Config{Server: config.Server{ClusterAddr: addr}}, authority, routes, pings, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })
	v, err := pingAs(t, authority, addr, "core-1")
	require.NoError(t, err)
	require.Equal(t, selfID, v.Host)

	// Joins wait for the API; the database reached, peers come from it.
	w := httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cluster/join", nil))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	require.Contains(t, w.Body.String(), "is starting")
	routes.set(http.NotFoundHandler())
	w = httptest.NewRecorder()
	routes.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/cluster/join", nil))
	require.Equal(t, http.StatusNotFound, w.Code)

	pings.useStore(store)
	store.mu.Lock()
	store.down = true
	store.mu.Unlock()
	_, err = pings.current.Load().monitor.Peers(ctx)
	require.ErrorIs(t, err, errDatabaseAway, "the Monitor keeps its last list")

	// A joined control host knows itself from its join.
	joinedDir := filepath.Join(t.TempDir(), "cluster")
	joined := &hosts.Joined{HostID: peerID, Name: "core-2", Roles: []string{state.HostRoleControl}}
	jl := base
	jl.Dir, jl.Joined = joinedDir, joined
	jp, err := jl.setup(ctx)
	require.NoError(t, err)
	require.Equal(t, peerID, jp.monitor.Self)
	_, err = jp.monitor.Peers(ctx)
	require.Error(t, err, "no peers kept yet")
}

// TestR421_AnAppHostThatCannotStartItsAgentIsStillContact asserts R-421 on
// an app host whose Pando cannot start its agent in full, and knows no
// runtime to stop its apps through: rather than exit, it refuses the
// cluster's pings, which is contact, so it is never taken for lost while it
// can be reached, and reports no view it could not act on (R-153); it returns
// why it could not start once it is stopped.
func TestR421_AnAppHostThatCannotStartItsAgentIsStillContact(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	writeJoinedFiles(t, dir, authority, "edge-1")
	addr := freeAddr(t)
	t.Setenv(multidocker.EnvAgentListen, addr)
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	joined := hosts.Joined{HostID: appHostID, Name: "edge-1", Roles: []string{"app"}, Cluster: "https://127.0.0.1:1"}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runAppHost(ctx, dir, joined, liveness.Config{}, zap.NewNop()) }()
	require.Eventually(t, func() bool {
		_, err = pingAs(t, authority, addr, "edge-1")
		return err != nil && strings.Contains(err.Error(), "refused the ping")
	}, 10*time.Second, 50*time.Millisecond)
	require.True(t, hostagent.Contacted(err), "a refusal is contact")
	require.ErrorContains(t, err, "cannot stop its own apps")

	cancel()
	select {
	case err := <-done:
		require.ErrorContains(t, err, "does not know where its container runtime's socket is")
	case <-time.After(10 * time.Second):
		t.Fatal("the app host did not stop")
	}

	// Without even its own certificate, there is nothing to answer with.
	require.NoError(t, os.Remove(filepath.Join(dir, hosts.HostCertFile)))
	require.ErrorContains(t, runAppHost(context.Background(), dir, joined, liveness.Config{}, zap.NewNop()), "server.cluster_dir")
}

type sizer struct {
	n   int
	err error
}

func (s sizer) Count(context.Context) (int, error) { return s.n, s.err }

// TestR414_ASingleHostStartsWithoutItsClusterListener asserts R-414 on an
// install upgraded from before clusters: a cluster port another program
// holds stops nothing on a single host that never joined, which only needs
// the listener to be joined; a host of a cluster still stops. And the
// cluster authority's default is beside the API token key, wherever that is,
// so no new directory is needed.
func TestR414_ASingleHostStartsWithoutItsClusterListener(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = taken.Close() })
	_, listenErr := startClusterListener(&config.Config{Server: config.Server{ClusterAddr: taken.Addr().String()}},
		authority, http.NotFoundHandler(), &controlPings{}, zap.NewNop())
	require.ErrorContains(t, listenErr, "server.cluster_addr")

	ctx := context.Background()
	require.NoError(t, requireClusterListener(ctx, nil, true, sizer{n: 3}, zap.NewNop()))
	require.NoError(t, requireClusterListener(ctx, listenErr, false, sizer{n: 1}, zap.NewNop()), "a single host starts")
	require.ErrorIs(t, requireClusterListener(ctx, listenErr, false, sizer{n: 2}, zap.NewNop()), listenErr)
	require.ErrorIs(t, requireClusterListener(ctx, listenErr, true, sizer{n: 1}, zap.NewNop()), listenErr, "a joined host is in a cluster")
	require.ErrorIs(t, requireClusterListener(ctx, listenErr, false, sizer{err: errors.New("database away")}, zap.NewNop()), listenErr)

	keyDir := t.TempDir()
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando@127.0.0.1:1/pando")
	t.Setenv("PANDO_SERVER_TOKEN_KEY_PATH", filepath.Join(keyDir, "keys", "token.key"))
	cfg, err := config.Load("")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(keyDir, "keys", config.ClusterAuthorityFile), cfg.Server.ClusterAuthorityPath)
	_, err = hosts.LoadOrCreateAuthority(cfg.Server.ClusterAuthorityPath)
	require.NoError(t, err, "made where the token key is kept")

	t.Setenv("PANDO_SERVER_CLUSTER_AUTHORITY_PATH", filepath.Join(keyDir, "ca.pem"))
	cfg, err = config.Load("")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(keyDir, "ca.pem"), cfg.Server.ClusterAuthorityPath, "a path set is kept")
}

// TestUnreachedForWithoutAMonitor covers a control host not yet watching.
func TestUnreachedForWithoutAMonitor(t *testing.T) {
	_, ok := (&controlPings{}).unreachedFor("host_x")
	require.False(t, ok)
	_, err := controlLiveness{Dir: t.TempDir(), IsSelf: hosts.InstallHost}.self(context.Background())
	require.ErrorContains(t, err, "until it reaches the database")
}
