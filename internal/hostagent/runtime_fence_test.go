package hostagent

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

type switchFence struct{ on atomic.Bool }

func (f *switchFence) Fenced() bool { return f.on.Load() }

// dockerish is a Docker API on a unix socket that creates and starts
// anything, and counts what it was asked to.
func dockerish(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	dir, err := os.MkdirTemp("", "pf")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	var started atomic.Int32
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/create") || strings.HasSuffix(r.URL.Path, "/start") {
			started.Add(1)
			w.WriteHeader(http.StatusCreated)
		}
		_, _ = io.WriteString(w, "OK")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock, &started
}

// TestR421_AFencedHostsAgentStartsNothingTheControlPlaneAsks asserts R-421
// on a joined host: while its Monitor has fenced, its agent refuses every
// runtime call that would create or start a container, however the control
// plane's reconciler asks, on a connection opened before the fence too, with
// Docker's error shape; every other call still passes, and once the host is
// not fenced the calls pass again.
func TestR421_AFencedHostsAgentStartsNothingTheControlPlaneAsks(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	sock, started := dockerish(t)
	fence := &switchFence{}
	addr := agentOn(ctx, t, as, &Agent{RuntimeSocket: sock, Fence: fence})
	tlsConfig := clientFor(t, as)
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return DialRuntime(ctx, addr, tlsConfig) },
	}}
	t.Cleanup(hc.CloseIdleConnections)
	post := func(path string) (int, string) {
		resp, err := hc.Post("http://docker"+path, "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	code, _ := post("/v1.47/containers/create?name=web")
	require.Equal(t, http.StatusCreated, code, "not fenced: created")

	fence.on.Store(true) // the same kept connection, now fenced
	for _, path := range []string{"/v1.47/containers/create", "/containers/abc/start", "/v1.47/containers/abc/restart", "/v1.47/containers/abc/unpause"} {
		code, body := post(path)
		require.Equal(t, http.StatusConflict, code, path)
		require.Contains(t, body, `"message":"This host is cut off from most of the cluster`)
	}
	code, _ = post("/v1.47/containers/abc/stop")
	require.Equal(t, http.StatusOK, code, "stopping is never refused")
	resp, err := hc.Get("http://docker/_ping")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.EqualValues(t, 1, started.Load(), "nothing created or started while fenced")

	fence.on.Store(false)
	code, _ = post("/v1.47/containers/abc/start")
	require.Equal(t, http.StatusCreated, code)
	require.EqualValues(t, 2, started.Load())
}

// TestAFenceableRuntimeStreamClosesCleanly asserts a stream opened and
// closed with no call made leaves nothing open.
func TestAFenceableRuntimeStreamClosesCleanly(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	sock, _ := dockerish(t)
	addr := agentOn(ctx, t, as, &Agent{RuntimeSocket: sock, Fence: &switchFence{}})
	conn, err := DialRuntime(ctx, addr, clientFor(t, as))
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	l := &oneConn{closed: make(chan struct{})}
	require.Equal(t, "pipe", l.Addr().Network())
	require.Equal(t, "runtime", l.Addr().String())
}
