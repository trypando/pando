package multidocker

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/hostagent"
)

// joinedHost is a host that joined the cluster: its agent serves with the
// certificate it joined with and carries runtime calls to f, its runtime.
// It returns the agent's address and the certificate's fingerprint.
func joinedHost(t *testing.T, authorityPEM []byte, name string, f *fakeDaemon) (string, string) {
	t.Helper()
	as, err := hostagent.ParseAuthorities(authorityPEM)
	require.NoError(t, err)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := as.IssueHost(name, []string{"app"}, &key.PublicKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	serverTLS, err := hostagent.ServerTLS(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		as.PoolPEM())
	require.NoError(t, err)

	dir, err := os.MkdirTemp("", "md")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	sockLn, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(sockLn) }()
	t.Cleanup(func() { _ = srv.Close() })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	agent := &hostagent.Agent{TLS: serverTLS, Pool: netip.MustParsePrefix(defaultNetworkPool), RuntimeSocket: sock}
	go func() { done <- agent.Serve(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), hostagent.Fingerprint256(der)
}

func configureHosts(t *testing.T, authorityPEM []byte, hosts []map[string]any) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"hosts":       hosts,
		"credentials": map[string]string{"agent_authority": string(authorityPEM)},
	})
	require.NoError(t, err)
	a := New()
	return a, a.Configure(context.Background(), raw)
}

// TestR416_AJoinedHostsRuntimeIsReachedThroughItsAgent asserts R-416 from the
// runtime's side: an agent:// host's Docker API is called through the host's
// agent with the cluster's control certificate, never directly, and only an
// agent presenting the certificate the host joined with is taken for it. The
// runtime does not manage that agent: the host runs its own.
func TestR416_AJoinedHostsRuntimeIsReachedThroughItsAgent(t *testing.T) {
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	daemon := newFakeDaemon(t)
	daemon.on("GET /networks", respond(http.StatusOK, []any{}))
	daemon.on("GET /volumes", respond(http.StatusOK, map[string]any{"Volumes": []any{}}))
	addr, pin := joinedHost(t, authority, "app-1", daemon)
	control := map[string]any{"name": "control", "control": true, "agent_address": "10.0.0.5:7443", "endpoint": "unix:///var/run/docker.sock"}

	a, err := configureHosts(t, authority, []map[string]any{
		control, {"name": "app-1", "endpoint": "agent://" + addr, "cert_sha256": pin},
	})
	require.NoError(t, err)
	h := a.byName("app-1")
	require.Nil(t, h.agent, "a joined host's agent is its own, not the runtime's to replace")
	require.Equal(t, addr, h.agentAddr, "the proxy reaches the host's apps through the same agent")

	ctx := context.Background()
	_, err = h.rt.Bundles(ctx)
	require.NoError(t, err)
	require.Positive(t, daemon.called("GET /networks"), "the call reached the host's runtime through its agent")

	// Another certificate under the host's name is not the host.
	a, err = configureHosts(t, authority, []map[string]any{
		control, {"name": "app-1", "endpoint": "agent://" + addr, "cert_sha256": hostagent.Fingerprint256([]byte("x"))},
	})
	require.NoError(t, err)
	before := daemon.called("GET /networks")
	_, err = a.byName("app-1").rt.Bundles(ctx)
	require.Error(t, err)
	require.Equal(t, before, daemon.called("GET /networks"))
}

func TestAnAgentEndpointNamesAnAddress(t *testing.T) {
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	control := map[string]any{"name": "control", "control": true, "agent_address": "10.0.0.5:7443"}
	_, err = configureHosts(t, authority, []map[string]any{control, {"name": "app-1", "endpoint": "agent://"}})
	require.ErrorContains(t, err, "agent://host:port")

	// Without a port, the agent port.
	a, err := configureHosts(t, authority, []map[string]any{control, {"name": "app-1", "endpoint": "agent://10.0.0.6"}})
	require.NoError(t, err)
	require.Equal(t, "10.0.0.6:7443", a.byName("app-1").agentAddr)
}

// TestR418_AppsOfTheFormerSingleHostStayOnIt asserts R-418 from the
// runtime's side: after a single-host install becomes a multi-host one, its
// apps are found on the control host by their labeled networks and applied
// there again — never placed on the new, roomier host — and a volume
// recorded before, without a host, is the control host's.
func TestR418_AppsOfTheFormerSingleHostStayOnIt(t *testing.T) {
	hosts := map[string]*fakeHost{
		"pando-1": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 1 * gib},
			bundles: map[string]bool{"app_a": true, "app_b": true}},
		"app-1": {total: api.Fit{CPUMillis: 8000, MemoryBytes: 32 * gib}, free: api.Fit{CPUMillis: 8000, MemoryBytes: 30 * gib}},
	}
	a := newTestAdapter(t, hosts, "pando-1", "app-1")
	ctx := context.Background()

	for _, bundle := range []string{"app_a", "app_b"} {
		handle, err := a.Apply(ctx, plan(bundle, 512<<20))
		require.NoError(t, err)
		require.Equal(t, "pando-1", handle.Handle, bundle)
	}
	require.Empty(t, hosts["app-1"].applied, "nothing moved to the new host")

	require.NoError(t, a.DestroyVolume(ctx, api.VolumeHandle{Handle: "pando-app_a-vol_1"}))
	require.Contains(t, hosts["pando-1"].observed, "destroy pando-app_a-vol_1")
}
