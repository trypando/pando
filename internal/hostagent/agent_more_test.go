package hostagent

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func serverTLSFor(t *testing.T, as Authorities, host string) *tls.Config {
	t.Helper()
	certPEM, keyPEM, err := as.IssueServer(host)
	require.NoError(t, err)
	cfg, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	return cfg
}

func testAuthorities(t *testing.T) Authorities {
	t.Helper()
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	as, err := ParseAuthorities(caPEM)
	require.NoError(t, err)
	return as
}

// TestR023_AnAgentWillNotStartWithoutItsTwoChecks asserts that the agent
// refuses to serve unless it requires Pando's client certificate and knows
// the app range it may forward into: without either, it would be a way
// around Pando's proxy.
func TestR023_AnAgentWillNotStartWithoutItsTwoChecks(t *testing.T) {
	as := testAuthorities(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()

	err = (&Agent{Pool: testPool}).Serve(context.Background(), ln)
	require.ErrorContains(t, err, "client certificate")

	optional := serverTLSFor(t, as, "host-1")
	optional.ClientAuth = tls.VerifyClientCertIfGiven
	err = (&Agent{TLS: optional, Pool: testPool}).Serve(context.Background(), ln)
	require.ErrorContains(t, err, "client certificate")

	err = (&Agent{TLS: serverTLSFor(t, as, "host-1")}).Serve(context.Background(), ln)
	require.ErrorContains(t, err, "app network range")

	err = (&Agent{TLS: serverTLSFor(t, as, "host-1"), Pool: netip.MustParsePrefix("fd00::/64")}).Serve(context.Background(), ln)
	require.ErrorContains(t, err, "app network range")
}

func TestAnAgentStopsWhenItsListenerFails(t *testing.T) {
	as := testAuthorities(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	_ = ln.Close()
	err = (&Agent{TLS: serverTLSFor(t, as, "host-1"), Pool: testPool, Logger: zap.NewNop()}).Serve(context.Background(), ln)
	require.Error(t, err, "a closed listener is an error, not a shutdown")
}

// agentOn starts an agent with the given hooks and returns its address.
func agentOn(ctx context.Context, t *testing.T, as Authorities, agent *Agent) string {
	t.Helper()
	agent.TLS = serverTLSFor(t, as, "host-1")
	agent.Pool = testPool
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- agent.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done, "a canceled agent returns cleanly")
	})
	return ln.Addr().String()
}

func clientFor(t *testing.T, as Authorities) *tls.Config {
	t.Helper()
	cert, err := as.IssueClient()
	require.NoError(t, err)
	return ClientTLS(as, cert, "host-1")
}

func TestAnAgentSaysWhyItCouldNotReachTheTarget(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	appNet := []netip.Prefix{netip.MustParsePrefix("10.213.0.18/28")}
	web := []netip.Addr{netip.MustParseAddr("10.213.0.20")}

	t.Run("its own networks cannot be read", func(t *testing.T) {
		addr := agentOn(ctx, t, as, &Agent{
			Resolve:    func(context.Context, string) ([]netip.Addr, error) { return web, nil },
			Interfaces: func() ([]netip.Prefix, error) { return nil, io.ErrUnexpectedEOF },
		})
		_, err := Dial(ctx, addr, clientFor(t, as), "pando-app_1-web", 3000)
		require.ErrorContains(t, err, "could not read its own networks")
	})

	t.Run("the container does not accept", func(t *testing.T) {
		addr := agentOn(ctx, t, as, &Agent{
			Resolve:    func(context.Context, string) ([]netip.Addr, error) { return web, nil },
			Interfaces: func() ([]netip.Prefix, error) { return appNet, nil },
			Dial: func(context.Context, string) (net.Conn, error) {
				return nil, io.ErrClosedPipe
			},
		})
		_, err := Dial(ctx, addr, clientFor(t, as), "pando-app_1-web", 3000)
		require.ErrorContains(t, err, "did not accept a connection on port 3000")
	})

	t.Run("no address at all", func(t *testing.T) {
		addr := agentOn(ctx, t, as, &Agent{
			Resolve:    func(context.Context, string) ([]netip.Addr, error) { return nil, nil },
			Interfaces: func() ([]netip.Prefix, error) { return appNet, nil },
		})
		_, err := Dial(ctx, addr, clientFor(t, as), "pando-app_1-web", 3000)
		require.ErrorContains(t, err, "not on any app network")
	})

	t.Run("the system resolver, interfaces and dialer", func(t *testing.T) {
		// Nothing on the machine running the test is named like an app
		// container, so the system resolver finds nothing and nothing is
		// dialed.
		addr := agentOn(ctx, t, as, &Agent{})
		_, err := Dial(ctx, addr, clientFor(t, as), "pando-no-such-container-for-the-agent-test", 3000)
		require.ErrorContains(t, err, "refused")
	})
}

func TestTheDefaultDialerReachesThePermittedAddress(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_, _ = io.WriteString(c, "hi")
			_ = c.Close()
		}
	}()
	conn, err := (&Agent{}).dial(context.Background(), ln.Addr().String())
	require.NoError(t, err)
	b, _ := io.ReadAll(conn)
	require.Equal(t, "hi", string(b))
}

func TestSystemInterfacesIncludeLoopback(t *testing.T) {
	got, err := SystemInterfaces()
	require.NoError(t, err)
	require.Contains(t, got, netip.MustParsePrefix("127.0.0.1/8"))
	for _, p := range got {
		require.True(t, p.Addr().Is4(), "IPv4 only: %s", p)
	}
}

func TestAConnectionThatSaysNothingIsClosed(t *testing.T) {
	as := testAuthorities(t)
	addr := agentOn(context.Background(), t, as, &Agent{})
	conn, err := tls.Dial("tcp", addr, clientFor(t, as))
	require.NoError(t, err)
	// The handshake completes; then the client closes without a header.
	require.NoError(t, conn.CloseWrite())
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, _ := io.ReadAll(conn)
	require.Empty(t, string(b), "nothing is answered to a connection that named no target")
}

// fakeAgent answers the dialer's header line with reply, over TLS issued by
// as for host-1, without checking the client.
func fakeAgent(t *testing.T, as Authorities, reply string) string {
	t.Helper()
	cfg := serverTLSFor(t, as, "host-1")
	cfg.ClientAuth = tls.NoClientCert
	cfg.VerifyConnection = nil
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				buf := make([]byte, 128)
				_, _ = c.Read(buf)
				_, _ = io.WriteString(c, reply)
			}()
		}
	}()
	return ln.Addr().String()
}

func TestTheDialerReportsWhatTheAgentAnswered(t *testing.T) {
	as := testAuthorities(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := Dial(ctx, fakeAgent(t, as, "HTTP/1.1 400 Bad Request\n"), clientFor(t, as), "pando-a-web", 80)
	require.ErrorContains(t, err, `answered "HTTP/1.1 400 Bad Request"`)

	_, err = Dial(ctx, fakeAgent(t, as, "NO busy\n"), clientFor(t, as), "pando-a-web", 80)
	require.ErrorContains(t, err, "refused: busy")

	_, err = Dial(ctx, fakeAgent(t, as, "OK"), clientFor(t, as), "pando-a-web", 80)
	require.ErrorContains(t, err, "closed the connection")

	// A long answer is cut at the header limit.
	_, err = Dial(ctx, fakeAgent(t, as, strings.Repeat("x", maxHeaderLine+10)), clientFor(t, as), "pando-a-web", 80)
	require.ErrorContains(t, err, "answered")

	// Nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := ln.Addr().String()
	_ = ln.Close()
	_, err = Dial(context.Background(), closed, clientFor(t, as), "pando-a-web", 80)
	require.ErrorContains(t, err, "could not reach the host agent")

	// An agent whose certificate is for another host is not trusted.
	_, err = Dial(ctx, fakeAgent(t, as, "OK\n"), ClientTLS(as, mustClient(t, as), "host-2"), "pando-a-web", 80)
	require.ErrorContains(t, err, "could not reach the host agent")
}

func mustClient(t *testing.T, as Authorities) tls.Certificate {
	t.Helper()
	c, err := as.IssueClient()
	require.NoError(t, err)
	return c
}

func TestAnAgentCarriesAStreamBothWaysUntilBothSidesClose(t *testing.T) {
	as := testAuthorities(t)
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	go func() {
		c, err := backend.Accept()
		if err != nil {
			return
		}
		_, _ = io.Copy(c, c) // echo until the agent closes its write side
		_ = c.Close()
	}()
	addr := agentOn(context.Background(), t, as, &Agent{
		Resolve: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("10.213.0.20")}, nil
		},
		Interfaces: func() ([]netip.Prefix, error) {
			return []netip.Prefix{netip.MustParsePrefix("10.213.0.18/28")}, nil
		},
		Dial: func(ctx context.Context, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "tcp", backend.Addr().String())
		},
	})
	conn, err := Dial(context.Background(), addr, clientFor(t, as), "pando-app_1-web", 3000)
	require.NoError(t, err)
	_, err = io.WriteString(conn, "a websocket frame")
	require.NoError(t, err)
	require.NoError(t, conn.(*tls.Conn).CloseWrite())
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	b, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "a websocket frame", string(b))
}

func TestAuthoritiesThatCannotBeUsedAreRefusedWithAReason(t *testing.T) {
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	certPart := caPEM[:strings.Index(string(caPEM), "-----BEGIN PRIVATE KEY")]
	keyPart := caPEM[strings.Index(string(caPEM), "-----BEGIN PRIVATE KEY"):]

	_, err = ParseAuthorities(certPart)
	require.ErrorContains(t, err, "no private key after it")
	_, err = ParseAuthorities(append(append([]byte{}, keyPart...), certPart...))
	require.ErrorContains(t, err, "comes before its certificate")
	_, err = ParseAuthorities([]byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"))
	require.ErrorContains(t, err, "could not be read")
	_, err = ParseAuthorities(append(append([]byte{}, certPart...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte{1, 2}})...))
	require.ErrorContains(t, err, "private key could not be read")

	// A leaf certificate is not an authority.
	as := testAuthorities(t)
	leafPEM, _, err := as.IssueServer("host-1")
	require.NoError(t, err)
	_, err = ParseAuthorities(leafPEM)
	require.ErrorContains(t, err, "not a certificate authority")

	// A key that cannot sign.
	x, err := ecdh.X25519().GenerateKey(rand.Reader)
	require.NoError(t, err)
	xDER, err := x509.MarshalPKCS8PrivateKey(x)
	require.NoError(t, err)
	_, err = ParseAuthorities(append(append([]byte{}, certPart...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: xDER})...))
	require.ErrorContains(t, err, "cannot sign")

	// An authority in SEC 1 form ("EC PRIVATE KEY") is read too.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	require.NoError(t, err)
	ecDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	sec1 := append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: ecDER})...)
	short, err := ParseAuthorities(sec1)
	require.NoError(t, err)

	// What it issues does not outlive it.
	leaf, err := short.IssueClient()
	require.NoError(t, err)
	require.False(t, leaf.Leaf.NotAfter.After(short[0].Cert.NotAfter))
	require.Less(t, time.Until(leaf.Leaf.NotAfter), 25*time.Hour)

	_, err = Authorities(nil).IssueClient()
	require.ErrorContains(t, err, "no agent authority")
	_, _, err = Authorities(nil).IssueServer("host-1")
	require.Error(t, err)
}

func TestServerTLSRefusesWhatItCannotUse(t *testing.T) {
	as := testAuthorities(t)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	_, err = ServerTLS(certPEM, []byte("not a key"), as.PoolPEM())
	require.ErrorContains(t, err, "certificate could not be read")
	_, err = ServerTLS(certPEM, keyPEM, nil)
	require.ErrorContains(t, err, "no authority")

	cfg, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	require.Error(t, cfg.VerifyConnection(tls.ConnectionState{}), "no verified chain, no connection")
}

// TestR023_AnAgentLogsTheConnectionsItRefuses asserts that a connection
// without Pando's certificate is refused and logged with where it came from.
func TestR023_AnAgentLogsTheConnectionsItRefuses(t *testing.T) {
	as := testAuthorities(t)
	core, logs := observer.New(zap.InfoLevel)
	addr := agentOn(context.Background(), t, as, &Agent{Logger: zap.New(core)})
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: as.Pool(), ServerName: ServerName("host-1")}
	_, err := Dial(context.Background(), addr, cfg, "pando-app_1-web", 3000)
	require.Error(t, err)
	require.Eventually(t, func() bool {
		return logs.FilterMessage("refused a connection without Pando's certificate").Len() == 1
	}, 5*time.Second, 10*time.Millisecond)
}
