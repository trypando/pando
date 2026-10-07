//go:build kubernetes

package kubernetes_test

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR120_ABuildIsPushedToTheRegistryAndPulledByDigest: a Dockerfile app
// built by BuildKit in the cluster is pushed to the install registry, and its
// pod runs that image by digest, pulled by the node from the registry
// (notes-image-registry-issue-72.md; Kubernetes offers only registry
// delivery).
//
// The source is a git repository, cloned over the network. An uploaded source
// would land on one replica's /var/lib/pando, which on kind is not shared
// (manifests/kustomization.yaml), and the deploy may run on the other.
func TestR120_ABuildIsPushedToTheRegistryAndPulledByDigest(t *testing.T) {
	c := login(t)
	repo := os.Getenv("PANDO_K8S_BUILD_REPO")
	if repo == "" {
		// busybox httpd on port 8000, a two-line Dockerfile.
		repo = "https://github.com/crccheck/docker-hello-world"
	}
	app := c.createApp(t, "k8s-build-"+stamp())
	dep := c.startDeploy(t, app, `{
		"schema_version": 1,
		"source": {"type": "git", "url": "`+repo+`", "ref": "master"},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 8000, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_traefik", "mode": "path"},
		"runtime": {"adapter_ref": "rt_kubernetes", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	final := c.awaitDeployment(t, app, dep, 15*time.Minute)
	logs := c.deploymentLogs(t, app, dep)
	require.Equal(t, "succeeded", final["status"], "build failed: %v\n%s", final["error_detail"], logs)

	// The pod runs the pushed image by digest, from the registry.
	image := mustKubectl(t, "-n", namespaceOf(app), "get", "pods", "-l", "pando.dev/workload=web",
		"-o", "jsonpath={.items[0].spec.containers[0].image}")
	require.True(t, strings.HasPrefix(image, "registry.pando.svc.cluster.local:5000/"), image)
	require.Contains(t, image, "@sha256:", "pinned by the digest the registry reported (R-120)")

	// And the registry holds it.
	catalog := mustKubectl(t, "-n", "pando", "exec", "deploy/pando", "--", "wget", "-T", "5", "-qO-",
		"http://registry.pando.svc.cluster.local:5000/v2/_catalog")
	require.Contains(t, catalog, strings.ToLower(app))

	status, body := c.throughProxy(t, fwd.base(), "", c.slug(t, app), "/", nil)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "Hello World")
}
