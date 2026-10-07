//go:build kubernetes

package kubernetes_test

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR174_TheEdgeRunsInTheClusterAndReachesAppsThroughTheProxy: with Traefik
// on kubernetes_api delivery, Pando runs the edge in pando-edge — at least two
// replicas, a PodDisruptionBudget, a NodePort Service — and writes
// IngressRoutes that all name pando-proxy, the alias for Pando's own Service
// (R-023). A request into the edge reaches an app through Pando's proxy.
func TestR174_TheEdgeRunsInTheClusterAndReachesAppsThroughTheProxy(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-edge", echoSpec())

	deployment := "pando-edge-rte-traefik"
	eventually(t, 3*time.Minute, "the edge has two ready replicas", func() bool {
		out, _ := kubectl("-n", "pando-edge", "get", "deployment", deployment, "-o", "jsonpath={.status.readyReplicas}")
		return out == "2"
	})
	require.Equal(t, "1", mustKubectl(t, "-n", "pando-edge", "get", "pdb", deployment, "-o", "jsonpath={.spec.minAvailable}"))
	require.Equal(t, "NodePort", mustKubectl(t, "-n", "pando-edge", "get", "service", deployment, "-o", "jsonpath={.spec.type}"))
	require.Equal(t, "ExternalName pando.pando.svc.cluster.local", mustKubectl(t, "-n", "pando-edge", "get", "service", "pando-proxy",
		"-o", "jsonpath={.spec.type} {.spec.externalName}"))

	// Every route's every backend is pando-proxy.
	var backends []string
	eventually(t, time.Minute, "the app's IngressRoute is written", func() bool {
		out := mustKubectl(t, "-n", "pando-edge", "get", "ingressroutes", "-o",
			`jsonpath={range .items[*]}{.metadata.name}{"\n"}{end}`)
		return strings.Contains(out, "pando-"+strings.ToLower(strings.ReplaceAll(app, "_", "-")))
	})
	backends = strings.Fields(mustKubectl(t, "-n", "pando-edge", "get", "ingressroutes", "-o",
		`jsonpath={range .items[*].spec.routes[*].services[*]}{.name}{" "}{end}`))
	require.NotEmpty(t, backends)
	for _, b := range backends {
		require.Equal(t, "pando-proxy", b, "an IngressRoute names a backend other than Pando's proxy (R-023)")
	}

	// Into the edge's Service, as a visitor would come in through its node
	// port: Traefik, then pando-proxy, then Pando's proxy, then the app.
	// With certificates on, port 80 redirects to HTTPS.
	httpPort, err := freePort()
	require.NoError(t, err)
	tlsPort, err := freePort()
	require.NoError(t, err)
	pf := exec.Command("kubectl", "--context", kubeContext(), "-n", "pando-edge", "port-forward",
		"service/"+deployment, fmt.Sprintf("%d:80", httpPort), fmt.Sprintf("%d:443", tlsPort))
	require.NoError(t, pf.Start())
	t.Cleanup(func() { _ = pf.Process.Kill(); _ = pf.Wait() })
	edge := fmt.Sprintf("https://127.0.0.1:%d", tlsPort)

	slug := c.slug(t, app)
	var status int
	var body string
	eventually(t, time.Minute, "port 80 redirects to HTTPS", func() bool {
		var err error
		status, _, err = c.proxyGet(fmt.Sprintf("http://127.0.0.1:%d", httpPort), "pando.pando.test", slug, "/", nil)
		return err == nil && (status == http.StatusMovedPermanently || status == http.StatusPermanentRedirect)
	})
	eventually(t, time.Minute, "a request through the edge reaches the app", func() bool {
		var err error
		status, body, err = c.proxyGet(edge, "pando.pando.test", slug, "/", map[string]string{"X-Pando-User": "forged"})
		return err == nil && status == http.StatusOK
	})
	require.Contains(t, body, `"x-pando-assertion"`, "the request passed through Pando's proxy, which adds the assertion")
	require.NotContains(t, body, `"forged"`, "and stripped the forged header (R-053)")

	// The edge's pods cannot reach the app's pod themselves: they carry the
	// edge's labels, not Pando's (R-023).
	edgePod := strings.Fields(mustKubectl(t, "-n", "pando-edge", "get", "pods", "-l", "app.kubernetes.io/component=edge",
		"-o", "jsonpath={.items[*].metadata.name}"))[0]
	out, err := kubectl("-n", "pando-edge", "exec", edgePod, "--", "wget", "-T", "5", "-qO-",
		fmt.Sprintf("http://web.%s.svc.cluster.local:8080/", namespaceOf(app)))
	require.Error(t, err, "the edge reached an app pod directly: %s", out)
	require.Contains(t, out, "timed out")
}
