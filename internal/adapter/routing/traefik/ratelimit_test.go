package traefik

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// kubeconfigFile is a kubeconfig for a cluster nobody answers at: building a
// client from it connects to nothing.
func kubeconfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
	return path
}

// TestIngressRouteClientsAreLimitedAsConfigured: the routing adapter's client
// gets the configured rate, and a default far above client-go's 5 a second
// when none is set (notes-kubernetes-scale-issue-72.md).
func TestIngressRouteClientsAreLimitedAsConfigured(t *testing.T) {
	kc := kubeconfigFile(t)
	for _, c := range []struct {
		raw          string
		qps          float32
		burst        int
		configureErr bool
	}{
		{raw: `{"delivery":"kubernetes_api","managed":false,"kubeconfig":%q}`, qps: 100, burst: 200},
		{raw: `{"delivery":"kubernetes_api","managed":false,"kubeconfig":%q,"api_qps":40,"api_burst":80}`, qps: 40, burst: 80},
		{raw: `{"delivery":"kubernetes_api","managed":false,"kubeconfig":%q,"api_qps":300}`, qps: 300, burst: 300},
		{raw: `{"delivery":"kubernetes_api","managed":false,"kubeconfig":%q,"api_qps":50,"api_burst":10}`, configureErr: true},
		{raw: `{"delivery":"kubernetes_api","managed":false,"kubeconfig":%q,"api_qps":-1}`, configureErr: true},
	} {
		a := New()
		err := a.Configure(context.Background(), []byte(fmt.Sprintf(c.raw, kc)))
		if c.configureErr {
			require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), c.raw)
			continue
		}
		require.NoError(t, err, c.raw)
		rc, err := restConfig(a.config)
		require.NoError(t, err)
		require.Equal(t, c.qps, rc.QPS, c.raw)
		require.Equal(t, c.burst, rc.Burst, c.raw)
	}
}
