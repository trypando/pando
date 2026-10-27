package hosts_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// csrFor is a certificate request for a new key, named name.
func csrFor(t *testing.T, name string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: name}}, key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
}

// TestR415_AJoinRefusesAHostNameTheRuntimeCannotUse asserts R-415 with
// R-418: a join is refused, spending no use of its token, under a name the
// Docker hosts runtime cannot take — longer than it allows, outside its
// characters, the reserved control, or one its host list already holds that
// Pando did not write — so that no joined host breaks every later sync of the
// runtime's host list. The refusal says what a usable name is (R-105).
func TestR415_AJoinRefusesAHostNameTheRuntimeCannotUse(t *testing.T) {
	ctx := context.Background()
	c := newCluster(t)
	hostList := []map[string]any{
		{"name": "pando-1", "control": true, "agent_address": "10.0.0.5:7443"},
		{"name": "gpu-box", "endpoint": "ssh://root@gpu.example.com"},
		{"name": "edge-9", "endpoint": "agent://10.0.0.9:7443", "cert_sha256": pin},
	}
	cfg, err := json.Marshal(map[string]any{"hosts": hostList, "network_pool": "10.208.0.0/12"})
	require.NoError(t, err)
	f := &runtimeFakes{adapters: []state.AdapterConfig{{
		ID: "rt_hosts", Category: "runtime", Kind: "docker-hosts", IsDefault: true, Enabled: true, Config: cfg,
	}}}
	c.svc.Runtime = newRuntime(t, f)

	join := func(name string) error {
		_, err := c.svc.Join(ctx, hosts.JoinRequest{
			Secret: secret.New("jt_01app.plain"), Name: name, CSR: csrFor(t, name),
			Platform: "linux/amd64", Version: "1.2.3", Address: "10.0.0.7:7443",
		})
		return err
	}
	c.token("app")

	for what, name := range map[string]string{
		"41 characters":          strings.Repeat("a", 41),
		"63 characters":          strings.Repeat("a", 63),
		"upper case":             "Edge-2",
		"a dot":                  "edge.2",
		"the reserved control":   "control",
		"the operator's host":    "gpu-box",
		"the runtime's own host": "pando-1",
	} {
		err := join(name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "%s: %v", what, err)
		require.NotEmpty(t, errs.As(err).Message, what)
		require.Zero(t, c.store.live["jt_01app.plain"].Uses, "%s: the token is not spent", what)
	}
	require.Contains(t, errs.As(join(strings.Repeat("a", 41))).Message, "up to 40")
	require.Contains(t, errs.As(join("control")).Message, "such as")

	// The longest name the runtime takes joins, and the runtime takes it.
	longest := strings.Repeat("a", 40)
	require.NoError(t, join(longest))
	app, err := hosts.RuntimeHosts(c.store.hosts)
	require.NoError(t, err)
	require.Equal(t, longest, app[0].Name)
}
