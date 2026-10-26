package hostagent

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// hostCert issues a joined host's certificate and key, as PEM.
func hostCert(t *testing.T, as Authorities, name string, roles ...string) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := as.IssueHost(name, roles, &key.PublicKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
}

func pair(t *testing.T, certPEM, keyPEM []byte) tls.Certificate {
	t.Helper()
	c, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return c
}

// fakeRuntime is a Docker API on a unix socket that answers /_ping.
func fakeRuntime(t *testing.T) string {
	t.Helper()
	// Short, for the 104 bytes macOS allows a socket path.
	dir, err := os.MkdirTemp("", "pa")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/_ping" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Api-Version", "1.47")
		_, _ = io.WriteString(w, "OK")
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

// joinedAgent is a joined app host's agent: serving with the certificate
// the host joined with, trusting the cluster authority, given the host's
// runtime socket.
func joinedAgent(t *testing.T, as Authorities, sock string) (string, tls.Certificate) {
	t.Helper()
	certPEM, keyPEM := hostCert(t, as, "app-1", "app")
	serverTLS, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	agent := &Agent{TLS: serverTLS, Pool: testPool, RuntimeSocket: sock}
	go func() { done <- agent.Serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		require.NoError(t, <-done)
	})
	return ln.Addr().String(), pair(t, certPEM, keyPEM)
}

// ping asks the runtime behind the agent at addr for /_ping.
func ping(addr string, tlsConfig *tls.Config) (string, error) {
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return DialRuntime(ctx, addr, tlsConfig)
		},
	}}
	defer hc.CloseIdleConnections()
	resp, err := hc.Get("http://docker/_ping")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.Header.Get("Api-Version") + " " + string(body), err
}

// TestR416_AJoinedHostsAgentCarriesRuntimeCallsForAControlHostOnly asserts
// R-416: a joined host's container runtime is reached only through its
// agent, and only with a certificate the cluster authority issued for a
// control host. An app host's certificate, the agent's own, a proxy
// certificate without the control role and one from another authority are
// all refused before the socket is touched.
func TestR416_AJoinedHostsAgentCarriesRuntimeCallsForAControlHostOnly(t *testing.T) {
	as := testAuthorities(t)
	addr, own := joinedAgent(t, as, fakeRuntime(t))

	t.Run("a control host's certificate reaches the runtime", func(t *testing.T) {
		got, err := ping(addr, ClientTLS(as, mustClient(t, as), "app-1"))
		require.NoError(t, err)
		require.Equal(t, "1.47 OK", got)
	})

	t.Run("an app host's certificate is refused", func(t *testing.T) {
		certPEM, keyPEM := hostCert(t, as, "app-2", "app")
		_, err := ping(addr, ClientTLS(as, pair(t, certPEM, keyPEM), "app-1"))
		require.Error(t, err)
	})

	t.Run("a host's certificate naming the control role is still not the proxy's", func(t *testing.T) {
		certPEM, keyPEM := hostCert(t, as, "control-2", "app", "control")
		_, err := ping(addr, ClientTLS(as, pair(t, certPEM, keyPEM), "app-1"))
		require.Error(t, err)
	})

	t.Run("the agent's own certificate is refused", func(t *testing.T) {
		_, err := ping(addr, ClientTLS(as, own, "app-1"))
		require.Error(t, err)
	})

	t.Run("an agent certificate the runtime adapter issued is refused", func(t *testing.T) {
		certPEM, keyPEM, err := as.IssueServer("app-1")
		require.NoError(t, err)
		_, err = ping(addr, ClientTLS(as, pair(t, certPEM, keyPEM), "app-1"))
		require.Error(t, err)
	})

	t.Run("a proxy certificate without the control role is refused", func(t *testing.T) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		require.NoError(t, err)
		der, err := as.sign(pkix.Name{CommonName: ClientCommonName}, nil, &key.PublicKey, x509.ExtKeyUsageClientAuth)
		require.NoError(t, err)
		cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
		_, err = DialRuntime(context.Background(), addr, ClientTLS(as, cert, "app-1"))
		require.ErrorContains(t, err, "only for a control host")
	})

	t.Run("a control certificate from another authority is refused", func(t *testing.T) {
		other := testAuthorities(t)
		_, err := ping(addr, ClientTLS(as, mustClient(t, other), "app-1"))
		require.Error(t, err)
	})
}

func TestR416_AnAgentWithoutARuntimeSocketCarriesNoRuntimeCalls(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	addr := agentOn(ctx, t, as, &Agent{})
	_, err := DialRuntime(ctx, addr, clientFor(t, as))
	require.ErrorContains(t, err, "does not carry container runtime calls")

	down := agentOn(ctx, t, as, &Agent{DialRuntime: func(context.Context) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}})
	_, err = DialRuntime(ctx, down, clientFor(t, as))
	require.ErrorContains(t, err, "not answering")
}

func TestARuntimeStreamCarriesBytesBothWays(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	addr := agentOn(ctx, t, as, &Agent{DialRuntime: func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			buf := make([]byte, 5)
			_, _ = io.ReadFull(b, buf)
			_, _ = b.Write(bytes.ToUpper(buf))
			_ = b.Close()
		}()
		return a, nil
	}})
	conn, err := DialRuntime(ctx, addr, clientFor(t, as))
	require.NoError(t, err)
	defer conn.Close()
	_, err = conn.Write([]byte("hello"))
	require.NoError(t, err)
	got, err := io.ReadAll(conn)
	require.NoError(t, err)
	require.Equal(t, "HELLO", string(got))
}

func TestIsControlAndPinnedClientTLS(t *testing.T) {
	as := testAuthorities(t)
	client := mustClient(t, as)
	require.True(t, IsControl(client.Leaf))
	require.False(t, IsControl(nil))
	certPEM, keyPEM := hostCert(t, as, "app-1", "app", "control")
	host := pair(t, certPEM, keyPEM)
	leaf, err := x509.ParseCertificate(host.Certificate[0])
	require.NoError(t, err)
	require.False(t, IsControl(leaf), "a host's certificate is never the proxy's")
	require.Equal(t, []string{ServerName("app-1")}, leaf.DNSNames)

	// An agent serving the certificate its host joined with is reached with
	// the pin, and one serving another under the same name is not.
	serverTLS, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	agent := &Agent{TLS: serverTLS, Pool: testPool, DialRuntime: func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() { _ = b.Close() }()
		return a, nil
	}}
	go func() { done <- agent.Serve(ctx, ln) }()
	defer func() { cancel(); <-done }()

	pin := Fingerprint256(host.Certificate[0])
	conn, err := DialRuntime(ctx, ln.Addr().String(), PinnedClientTLS(as, client, "app-1", pin))
	require.NoError(t, err)
	_ = conn.Close()

	_, err = DialRuntime(ctx, ln.Addr().String(), PinnedClientTLS(as, client, "app-1", Fingerprint256([]byte("another"))))
	require.ErrorContains(t, err, "not the one host app-1 joined the cluster with")
	require.Nil(t, PinnedClientTLS(as, client, "app-1", "").VerifyConnection)
}
