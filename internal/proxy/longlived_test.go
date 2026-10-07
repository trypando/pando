package proxy_test

import (
	"bufio"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

// liveAuth resolves a request's credentials to whatever the principal is now,
// the way the real authenticator reads the session and the user afresh on
// every call. calls counts how often it was asked.
type liveAuth struct {
	mu        sync.Mutex
	principal authz.Principal
	calls     atomic.Int32
}

func (l *liveAuth) Authenticate(*http.Request) (authz.Principal, error) {
	l.calls.Add(1)
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.principal, nil
}

func (l *liveAuth) set(p authz.Principal) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.principal = p
}

// upgradingUpstream answers every request by switching protocols and holding
// the connection open, as a websocket server does.
func upgradingUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = rw.Flush()
		_, _ = io.Copy(io.Discard, conn)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// openSocket connects through the proxy and completes the upgrade.
func openSocket(t *testing.T, front *httptest.Server) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", front.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "GET /socket HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nCookie: pando_session=ses_x\r\n\r\n")
	require.NoError(t, err)

	r := bufio.NewReader(conn)
	resp, err := http.ReadResponse(r, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	return conn, r
}

func appResolver() *resolver {
	return &resolver{
		app: state.App{ID: appID, Slug: "notes", State: state.StateRunning},
		spec: &spec.AppSpec{
			Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
			Workloads: []spec.Workload{{Name: "web", Primary: true,
				Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
		},
	}
}

// socketProxy is a proxy in front of an upgrading upstream, re-authorizing
// every interval.
func socketProxy(t *testing.T, auth proxy.Authenticator, s *store, interval time.Duration) *httptest.Server {
	t.Helper()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	p := &proxy.Proxy{
		Resolver:      appResolver(),
		Authenticator: auth,
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upgradingUpstream(t).URL},
		Logger:        zap.NewNop(),
	}
	p.SetReauthInterval(interval)
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)
	return front
}

// awaitClose reads until the proxy closes the connection, and returns the
// close frame's status code, or 0 if nothing arrived in time.
func awaitClose(t *testing.T, conn net.Conn, r *bufio.Reader, within time.Duration) uint16 {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(within))
	head := make([]byte, 2)
	if _, err := io.ReadFull(r, head); err != nil {
		return 0
	}
	require.Equal(t, byte(0x88), head[0], "a close frame")
	payload := make([]byte, head[1])
	_, err := io.ReadFull(r, payload)
	require.NoError(t, err)
	return binary.BigEndian.Uint16(payload)
}

// TestR048_RevokingASessionClosesItsOpenWebsocket asserts that a long-lived
// connection is re-authorized on its credentials as they are now, not on the
// principal they were when it opened: a session revoked since — which the
// authenticator now reads as nobody — closes the socket with a
// policy-violation close frame (design 06 §4.2, R-048).
func TestR048_RevokingASessionClosesItsOpenWebsocket(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	auth := &liveAuth{principal: activeUser("usr_alice")}
	front := socketProxy(t, auth, s, 30*time.Millisecond)

	conn, r := openSocket(t, front)
	require.Zero(t, awaitClose(t, conn, r, 150*time.Millisecond), "still allowed, still open")
	require.Greater(t, auth.calls.Load(), int32(1), "the credentials were authenticated again")

	// The session is revoked: the same cookie now authenticates as nobody.
	auth.set(authz.Anonymous())
	require.Equal(t, uint16(1008), awaitClose(t, conn, r, 2*time.Second))
}

// TestR049_SuspendingAUserClosesTheirOpenWebsocket asserts the same for a
// suspension: the session still exists, but the user it reads is suspended,
// and suspension ends access at once (R-049, R-048).
func TestR049_SuspendingAUserClosesTheirOpenWebsocket(t *testing.T) {
	s := newStore()
	s.data[appID] = []string{"usr_alice"}
	auth := &liveAuth{principal: activeUser("usr_alice")}
	front := socketProxy(t, auth, s, 30*time.Millisecond)

	conn, r := openSocket(t, front)
	suspended := activeUser("usr_alice")
	suspended.Status = "suspended"
	auth.set(suspended)
	require.Equal(t, uint16(1008), awaitClose(t, conn, r, 2*time.Second))
}

// TestR023_ProxiedRequestsReuseTheirUpstreamConnection asserts that the proxy
// keeps a connection to an app and reuses it: sequential requests to one app
// arrive over one connection, rather than one dialed — and left idle — per
// request.
func TestR023_ProxiedRequestsReuseTheirUpstreamConnection(t *testing.T) {
	var conns atomic.Int32
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	upstream.Config.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateNew {
			conns.Add(1)
		}
	}
	upstream.Start()
	t.Cleanup(upstream.Close)

	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	s := newStore()
	s.anonymous[appID] = true
	p := &proxy.Proxy{
		Resolver:      appResolver(),
		Authenticator: staticAuth{principal: authz.Anonymous()},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Logger:        zap.NewNop(),
	}
	front := httptest.NewServer(p)
	t.Cleanup(front.Close)

	for i := 0; i < 10; i++ {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, front.URL+"/", nil)
		require.NoError(t, err)
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
	require.Equal(t, int32(1), conns.Load(), "one upstream connection for ten requests")
}
