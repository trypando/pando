//go:build integration

package buildkit

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// TestR095_ARailpackPlanBuildsOnPandosRootlessBuildKit asserts R-095 and R-112
// together: a repository with no deployment instructions is planned by
// Railpack, and the plan is built by Railpack's frontend inside the rootless,
// unprivileged BuildKit Pando runs — no `railpack build`, no privileged
// BuildKit, no container runtime socket. The image carries the bundle label
// and serves HTTP.
//
// Needs railpack on PATH (it ships in the Pando image) and a Docker daemon.
// The BuildKit is the one Pando would start for the app, under names of the
// test's own, removed afterwards.
func TestR095_ARailpackPlanBuildsOnPandosRootlessBuildKit(t *testing.T) {
	if _, err := exec.LookPath(railpackBinary); err != nil {
		t.Skip("railpack is not on PATH; it ships in the Pando image")
	}
	ctx := context.Background()
	m := realManaged(t)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())

	t.Setenv("PANDO_BUILD_CACHE_DIR", t.TempDir())
	a := New()
	a.managed, a.daemons = m, newDaemons(m)

	root := writeFiles(t, map[string]string{
		"main.py": "import http.server, os\n" +
			"class H(http.server.BaseHTTPRequestHandler):\n" +
			"    def do_GET(self):\n" +
			"        self.send_response(200); self.end_headers(); self.wfile.write(b'railpack ok\\n')\n" +
			"http.server.HTTPServer(('0.0.0.0', int(os.environ.get('PORT', '8000'))), H).serve_forever()\n",
	})
	plan, err := a.Plan(ctx, view{root: root})
	require.NoError(t, err)
	require.True(t, isRailpackPlan(plan.Entry), "planned by Railpack: %s", plan.Entry)
	require.Equal(t, "python main.py", plan.Start)

	// Built from the stored plan, in a fresh checkout, as a deploy does (R-020).
	checkout := writeFiles(t, map[string]string{"main.py": readFile(root, "main.py")})
	var image bytes.Buffer
	var logs bytes.Buffer
	_, err = a.Build(ctx, api.BuildRequest{
		Strategy:       spec.BuildBuildpack,
		Source:         view{root: checkout},
		Dockerfile:     plan.Entry,
		GeneratedFiles: plan.Files,
		CacheNamespace: "app_01RAILPACKTEST/web",
		Tag:            stamp,
		ImageSink:      &image,
		LogSink:        &logs,
		Timeout:        15 * time.Minute,
	})
	require.NoError(t, err, "build log:\n%s", tail(logs.String()))

	name := m.daemonName("app_01RAILPACKTEST")
	out, err := exec.Command("docker", "inspect", "-f",
		"{{.HostConfig.Privileged}} {{json .HostConfig.Binds}} {{json .Mounts}}", name).CombinedOutput()
	require.NoError(t, err, string(out))
	require.True(t, strings.HasPrefix(string(out), "false "), "BuildKit is not privileged: %s", out)
	require.NotContains(t, string(out), ".sock", "and has no runtime socket")

	require.Equal(t, "app_01RAILPACKTEST", imageLabel(t, image.Bytes(), api.ImageLabelBundle),
		"the bundle label survives a frontend that takes no label options")

	load := exec.Command("docker", "load")
	load.Stdin = bytes.NewReader(image.Bytes())
	loaded, err := load.CombinedOutput()
	require.NoError(t, err, string(loaded))
	ref := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(loaded)), "Loaded image:"))
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", ref).Run() })

	container := docker(t, "run", "-d", "-e", "PORT=8000", "-p", "127.0.0.1::8000", ref)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	hostPort := strings.Split(docker(t, "port", container, "8000/tcp"), "\n")[0]

	var body string
	require.Eventually(t, func() bool {
		resp, err := http.Get("http://" + hostPort + "/") //nolint:noctx
		if err != nil {
			return false
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		body = string(b)
		return resp.StatusCode == http.StatusOK
	}, 60*time.Second, 500*time.Millisecond)
	require.Equal(t, "railpack ok\n", body)
}

// imageLabel reads a label out of a docker-format image tarball's config.
func imageLabel(t *testing.T, image []byte, key string) string {
	t.Helper()
	files := map[string][]byte{}
	tr := tar.NewReader(bytes.NewReader(image))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		if strings.HasSuffix(h.Name, ".json") || strings.HasPrefix(h.Name, "blobs/") {
			b, _ := io.ReadAll(tr)
			files[h.Name] = b
		}
	}
	var manifest []struct{ Config string }
	require.NoError(t, json.Unmarshal(files["manifest.json"], &manifest))
	require.NotEmpty(t, manifest)
	var config struct {
		Config struct{ Labels map[string]string }
	}
	require.NoError(t, json.Unmarshal(files[manifest[0].Config], &config))
	return config.Config.Labels[key]
}

func tail(s string) string {
	if len(s) > 4000 {
		return s[len(s)-4000:]
	}
	return s
}
