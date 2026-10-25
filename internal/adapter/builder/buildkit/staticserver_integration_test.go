//go:build integration

package buildkit

import (
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR160_AStaticSitesDirectoryRedirectKeepsItsPort asserts R-160.
//
// The generated static server is built and run as it would be deployed, and
// asked for a directory without its trailing slash on an address with a port.
// nginx's default absolute redirect dropped the port and sent a port-mode app's
// visitors to port 80, where nothing listens (issue #67).
func TestR160_AStaticSitesDirectoryRedirectKeepsItsPort(t *testing.T) {
	root := pagesSite(t)
	dir, name, err := staticDockerfile(api.BuildRequest{StaticDir: "."}, root)
	require.NoError(t, err)

	tag := fmt.Sprintf("pando-test-static-%d", time.Now().UnixNano())
	docker(t, "build", "-q", "-t", tag, "-f", dir+"/"+name, root)
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	container := docker(t, "run", "-d", "-p", "127.0.0.1::80", tag)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	hostPort := docker(t, "port", container, "80/tcp")
	hostPort = strings.Split(hostPort, "\n")[0]

	client := &http.Client{
		Timeout:       5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	var resp *http.Response
	require.Eventually(t, func() bool {
		req, _ := http.NewRequest(http.MethodGet, "http://"+hostPort+"/solutions", nil)
		// The address a port-mode app is reached at: a name and a port.
		req.Host = "localhost:9001"
		resp, err = client.Do(req) //nolint:bodyclose // closed below
		return err == nil
	}, 30*time.Second, 200*time.Millisecond)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusMovedPermanently, resp.StatusCode)
	location := resp.Header.Get("Location")
	require.Equal(t, "/solutions/", location,
		fmt.Sprintf("the redirect must not name a host without its port; got %q", location))
}

// TestR202_AStaticSiteWritesNothingToItsConfiguration asserts that the generated
// static server gives the trial run nothing to report under R-202.
//
// Started through the base image's entrypoint, nginx's IPv6 script touched
// default.conf to see whether it could edit it. The file was unchanged, but the
// container's diff listed it, and every plain static site was told it wrote to
// /etc/nginx/conf.d outside its storage.
func TestR202_AStaticSiteWritesNothingToItsConfiguration(t *testing.T) {
	root := pagesSite(t)
	dir, name, err := staticDockerfile(api.BuildRequest{StaticDir: "."}, root)
	require.NoError(t, err)

	tag := fmt.Sprintf("pando-test-static-%d", time.Now().UnixNano())
	docker(t, "build", "-q", "-t", tag, "-f", dir+"/"+name, root)
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", tag).Run() })

	container := docker(t, "run", "-d", tag)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })
	require.Eventually(t, func() bool {
		return docker(t, "inspect", "-f", "{{.State.Running}}", container) == "true" &&
			strings.Contains(docker(t, "diff", container), "/run/nginx.pid")
	}, 30*time.Second, 200*time.Millisecond, "nginx did not start")

	require.NotContains(t, docker(t, "diff", container), "/etc/",
		"the static server must not change its own configuration at startup")
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %s: %s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}
