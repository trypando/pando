package proxy_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/proxy"
)

// throughAgent stands a host agent (O-45) between the proxy and the
// recording upstream, as on a multi-host install: the upstream's Dial opens a
// mutually authenticated connection to the agent, which carries it to the
// container. The agent's view of its host is faked — one app network on which
// the app's container is 10.213.0.20 — and its last hop dials the recording
// server on loopback.
func throughAgent(t *testing.T) func(string) proxy.Upstreams {
	return func(upstreamURL string) proxy.Upstreams {
		caPEM, err := hostagent.NewAuthority()
		require.NoError(t, err)
		as, err := hostagent.ParseAuthorities(caPEM)
		require.NoError(t, err)
		certPEM, keyPEM, err := as.IssueServer("app-host")
		require.NoError(t, err)
		serverTLS, err := hostagent.ServerTLS(certPEM, keyPEM, as.PoolPEM())
		require.NoError(t, err)

		backend := strings.TrimPrefix(upstreamURL, "http://")
		agent := &hostagent.Agent{
			TLS:  serverTLS,
			Pool: netip.MustParsePrefix("10.213.0.0/16"),
			Resolve: func(_ context.Context, name string) ([]netip.Addr, error) {
				if name == "pando-app_01HQ8-web" {
					return []netip.Addr{netip.MustParseAddr("10.213.0.20")}, nil
				}
				return nil, fmt.Errorf("no such host")
			},
			Interfaces: func() ([]netip.Prefix, error) {
				return []netip.Prefix{netip.MustParsePrefix("10.213.0.18/28")}, nil
			},
			Dial: func(ctx context.Context, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "tcp", backend)
			},
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		require.NoError(t, err)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { _ = agent.Serve(ctx, ln); close(done) }()
		t.Cleanup(func() { cancel(); <-done })

		client, err := as.IssueClient()
		require.NoError(t, err)
		clientTLS := hostagent.ClientTLS(as, client, "app-host")
		agentAddr := ln.Addr().String()
		return fixedUpstream{
			// What the multi-host adapter returns: the container's name, which
			// nothing on this machine resolves, and a Dial through the agent.
			addr: "http://pando-app_01HQ8-web:80",
			dial: func(ctx context.Context) (net.Conn, error) {
				return hostagent.Dial(ctx, agentAddr, clientTLS, "pando-app_01HQ8-web", 80)
			},
		}
	}
}

// TestR053_ForgedHeadersAreReplacedThroughAHostAgent asserts R-053 for an app
// on another host: the strip happens in the proxy before the connection to the
// agent is opened, so it holds whichever way the request travels.
func TestR053_ForgedHeadersAreReplacedThroughAHostAgent(t *testing.T) {
	front, _, _, got := harnessVia(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	}, throughAgent(t))
	forgedHeadersAreReplaced(t, front, got)
}

// TestR173_PandosOwnCookiesNeverReachAnAppThroughAHostAgent asserts R-173 for
// an app on another host.
func TestR173_PandosOwnCookiesNeverReachAnAppThroughAHostAgent(t *testing.T) {
	front, _, _, got := harnessVia(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	}, throughAgent(t))
	pandosCookiesAreStripped(t, front, got)
}

// TestR023_AnAppOnAnotherHostIsReachedOnlyAfterTheDecision asserts that the
// agent route is not a way around enforcement: a request the proxy denies
// never opens a connection to the agent, and one it allows reuses the
// connection it opened.
func TestR023_AnAppOnAnotherHostIsReachedOnlyAfterTheDecision(t *testing.T) {
	var dials atomic.Int32
	via := func(url string) proxy.Upstreams {
		inner := throughAgent(t)(url).(fixedUpstream)
		dial := inner.dial
		inner.dial = func(ctx context.Context) (net.Conn, error) {
			dials.Add(1)
			return dial(ctx)
		}
		return inner
	}

	// Denied: no grant, signed in.
	front, _, _, got := harnessVia(t, activeUser("usr_mallory"), nil, via)
	resp, err := http.Get(front.URL + "/")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.Zero(t, dials.Load(), "a denied request must not reach the agent")
	require.Nil(t, got.header)

	// Allowed, twice: one connection through the agent, reused.
	front, _, _, got = harnessVia(t, activeUser("usr_alice"), func(s *store) {
		s.owner[appID] = "usr_alice"
	}, via)
	for range 2 {
		resp, err := http.Get(front.URL + "/")
		require.NoError(t, err)
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "upstream ok", string(body))
	}
	require.Equal(t, int32(1), dials.Load(), "the connection through the agent is reused")
	require.NotNil(t, got.header)
}
