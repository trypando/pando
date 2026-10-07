//go:build kubernetes

package kubernetes_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
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

// sourceArchive is a gzipped tar of files, as `pando deploy ./` sends one.
func sourceArchive(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body))}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

// TestR262_AnUploadedSourceIsBuiltByEitherReplica: an app's files sent to
// one replica are stored on /var/lib/pando, which both replicas share on a
// ReadWriteMany volume (O-39), so the deploy builds them whichever replica
// runs it. Several deploys, so both replicas take one.
func TestR262_AnUploadedSourceIsBuiltByEitherReplica(t *testing.T) {
	c := login(t)
	app := c.createApp(t, "k8s-upload-"+stamp())

	archive := sourceArchive(t, map[string]string{
		"Dockerfile": "FROM busybox:1.37\nCOPY index.html /www/index.html\nEXPOSE 8000\nCMD [\"httpd\", \"-f\", \"-p\", \"8000\", \"-h\", \"/www\"]\n",
		"index.html": "uploaded-and-built\n",
	})
	req, err := http.NewRequest(http.MethodPost, fwd.base()+"/api/v1/apps/"+app+"/source", bytes.NewReader(archive))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/gzip")
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	resp, err := c.http.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)

	dep := c.startDeploy(t, app, `{
		"schema_version": 1,
		"source": {"type": "upload", "upload_id": "`+app+`"},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 8000, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_traefik", "mode": "path"},
		"runtime": {"adapter_ref": "rt_kubernetes", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`)
	replicas := map[string]bool{}
	for i := 0; ; i++ {
		final := c.awaitDeployment(t, app, dep, 15*time.Minute)
		require.Equal(t, "succeeded", final["status"], "deploy failed: %v\n%s", final["error_detail"], c.deploymentLogs(t, app, dep))
		replicas[psql(t, `SELECT r.hostname FROM deployments d JOIN pando_replicas r ON r.id = d.replica_id WHERE d.id = '`+dep+`'`)] = true
		if len(replicas) == 2 || i == 5 {
			break
		}
		dep = c.must(t, http.MethodPost, "/apps/"+app+"/deployments", "{}", http.StatusAccepted)["id"].(string)
	}
	require.Len(t, replicas, 2, "both replicas built the upload one replica stored")

	status, page := c.throughProxy(t, fwd.base(), "", c.slug(t, app), "/", nil)
	require.Equal(t, http.StatusOK, status, page)
	require.Contains(t, page, "uploaded-and-built")
}
