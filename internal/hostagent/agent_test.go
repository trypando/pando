package hostagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

var testPool = netip.MustParsePrefix("10.213.0.0/16")

// agentFixture is an agent on loopback, joined (by its fake interfaces) to one
// app network, 10.213.0.16/28, on which "pando-app_1-web" is 10.213.0.20 and
// reaches app.
type agentFixture struct {
	addr string
	as   Authorities
}

func newAgent(t *testing.T, app http.Handler) agentFixture {
	t.Helper()
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	as, err := ParseAuthorities(caPEM)
	require.NoError(t, err)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	serverTLS, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)

	backend := httptest.NewServer(app)
	t.Cleanup(backend.Close)

	names := map[string][]netip.Addr{
		"pando-app_1-web":     {netip.MustParseAddr("10.213.0.20")},
		"pando-app_1-gateway": {netip.MustParseAddr("10.213.0.17")}, // the bridge, the host itself
		"pando-elsewhere":     {netip.MustParseAddr("10.213.0.40")}, // a network the agent is not on
		"pando-outside":       {netip.MustParseAddr("172.17.0.1")},  // outside the app range
	}
	agent := &Agent{
		TLS:  serverTLS,
		Pool: testPool,
		Resolve: func(_ context.Context, name string) ([]netip.Addr, error) {
			if a, ok := names[name]; ok {
				return a, nil
			}
			return nil, fmt.Errorf("no such host")
		},
		Interfaces: func() ([]netip.Prefix, error) {
			return []netip.Prefix{
				netip.MustParsePrefix("172.18.0.2/16"),  // the network its port is published on
				netip.MustParsePrefix("10.213.0.18/28"), // one app network
			}, nil
		},
		Dial: func(ctx context.Context, addr string) (net.Conn, error) {
			if addr != "10.213.0.20:3000" {
				return nil, fmt.Errorf("the test dials only the app")
			}
			var d net.Dialer
			return d.DialContext(ctx, "tcp", strings.TrimPrefix(backend.URL, "http://"))
		},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return agentFixture{addr: ln.Addr().String(), as: as}
}

func (f agentFixture) clientTLS(t *testing.T) *tls.Config {
	t.Helper()
	cert, err := f.as.IssueClient()
	require.NoError(t, err)
	return ClientTLS(f.as, cert, "host-1")
}

func get(t *testing.T, conn net.Conn) string {
	t.Helper()
	_, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return string(body)
}

func TestR023_AnAgentForwardsPandosConnectionToTheNamedContainer(t *testing.T) {
	f := newAgent(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello from the app")
	}))
	conn, err := Dial(context.Background(), f.addr, f.clientTLS(t), "pando-app_1-web", 3000)
	require.NoError(t, err)
	defer conn.Close()
	require.Equal(t, "hello from the app", get(t, conn))
}

// TestR023_AnAgentRefusesAConnectionWithoutPandosCertificate asserts R-023 on
// several hosts: the agent carries a connection only for a holder of Pando's
// client certificate (O-45, design 06 §4).
func TestR023_AnAgentRefusesAConnectionWithoutPandosCertificate(t *testing.T) {
	reached := false
	f := newAgent(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true }))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	t.Run("no certificate", func(t *testing.T) {
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.as.Pool(), ServerName: ServerName("host-1")}
		_, err := Dial(ctx, f.addr, cfg, "pando-app_1-web", 3000)
		require.Error(t, err)
	})

	t.Run("a certificate from another authority", func(t *testing.T) {
		otherPEM, err := NewAuthority()
		require.NoError(t, err)
		other, err := ParseAuthorities(otherPEM)
		require.NoError(t, err)
		cert, err := other.IssueClient() // the right name, the wrong issuer
		require.NoError(t, err)
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.as.Pool(), ServerName: ServerName("host-1"),
			Certificates: []tls.Certificate{cert}}
		_, err = Dial(ctx, f.addr, cfg, "pando-app_1-web", 3000)
		require.Error(t, err)
	})

	t.Run("another agent's certificate", func(t *testing.T) {
		// Same authority, but issued to an agent for server authentication:
		// a compromised app host's key does not open the others.
		certPEM, keyPEM, err := f.as.IssueServer("host-2")
		require.NoError(t, err)
		cert, err := tls.X509KeyPair(certPEM, keyPEM)
		require.NoError(t, err)
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.as.Pool(), ServerName: ServerName("host-1"),
			Certificates: []tls.Certificate{cert}}
		_, err = Dial(ctx, f.addr, cfg, "pando-app_1-web", 3000)
		require.Error(t, err)
	})

	t.Run("a client certificate from the authority that does not name Pando's proxy", func(t *testing.T) {
		cert, err := f.as.issue(pkix.Name{CommonName: "someone-else"}, nil, x509.ExtKeyUsageClientAuth)
		require.NoError(t, err)
		cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: f.as.Pool(), ServerName: ServerName("host-1"),
			Certificates: []tls.Certificate{cert}}
		_, err = Dial(ctx, f.addr, cfg, "pando-app_1-web", 3000)
		require.Error(t, err)
	})

	t.Run("plain TCP", func(t *testing.T) {
		conn, err := net.Dial("tcp", f.addr)
		require.NoError(t, err)
		defer conn.Close()
		_, _ = io.WriteString(conn, "PANDO-AGENT/1 pando-app_1-web 3000\n")
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		b, _ := io.ReadAll(conn)
		require.NotContains(t, string(b), "OK")
	})

	require.False(t, reached, "the app was reached without Pando's certificate")
}

// TestR023_AnAgentRefusesATargetOutsideItsAppNetworks asserts that the agent
// reaches nothing on its host but an app container on a Pando-managed network
// it was joined to (R-023, R-025).
func TestR023_AnAgentRefusesATargetOutsideItsAppNetworks(t *testing.T) {
	f := newAgent(t, http.NotFoundHandler())
	ctx := context.Background()
	for _, name := range []string{
		"pando-app_1-gateway", // the bridge address: the host
		"pando-elsewhere",     // inside the range, on a network the agent is not on
		"pando-outside",       // outside the range: the network its port is published on
		"pando-unknown",       // does not resolve
		"example.com",         // not an app container's name, never looked up
		"pando-app_1-web\n",   // not one line
	} {
		_, err := Dial(ctx, f.addr, f.clientTLS(t), name, 3000)
		require.Error(t, err, name)
	}
	_, err := Dial(ctx, f.addr, f.clientTLS(t), "pando-app_1-web", 0)
	require.Error(t, err)
}

func TestPermittedAddresses(t *testing.T) {
	own := []netip.Prefix{
		netip.MustParsePrefix("172.18.0.2/16"),
		netip.MustParsePrefix("10.213.0.18/28"),
	}
	for addr, ok := range map[string]bool{
		"10.213.0.20": true,
		"10.213.0.30": true,
		"10.213.0.16": false, // network
		"10.213.0.17": false, // bridge: the host
		"10.213.0.18": false, // the agent
		"10.213.0.31": false, // broadcast
		"10.213.0.33": false, // another app's network, not joined
		"172.18.0.3":  false, // outside the range
		"127.0.0.1":   false,
		"::1":         false,
	} {
		err := Permitted(netip.MustParseAddr(addr), testPool, own)
		require.Equal(t, ok, err == nil, "%s: %v", addr, err)
	}

	// An interface wider than the range — not an app network even though it
	// contains an address in the range — permits nothing.
	wide := []netip.Prefix{netip.MustParsePrefix("10.0.0.2/8")}
	require.Error(t, Permitted(netip.MustParseAddr("10.213.0.20"), testPool, wide))
}

func TestAuthoritiesRoundTripAndRefuseAKeyThatIsNotTheirs(t *testing.T) {
	a, err := NewAuthority()
	require.NoError(t, err)
	b, err := NewAuthority()
	require.NoError(t, err)
	both, err := ParseAuthorities(append(append([]byte{}, a...), b...))
	require.NoError(t, err)
	require.Len(t, both, 2)

	// a's certificate with b's key.
	certA := a[:strings.Index(string(a), "-----BEGIN PRIVATE KEY")]
	keyB := b[strings.Index(string(b), "-----BEGIN PRIVATE KEY"):]
	_, err = ParseAuthorities(append(append([]byte{}, certA...), keyB...))
	require.Error(t, err)

	_, err = ParseAuthorities([]byte("nothing"))
	require.Error(t, err)
}
