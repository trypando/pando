//go:build kubernetes

// Package kubernetes_test runs Pando on a real Kubernetes cluster and drives it
// as a client would (notes-kubernetes-runtime-issue-72.md, "Tests this PR is
// done with"). `make test-kubernetes` creates the kind cluster, deploys
// deploy/kubernetes into it with Postgres and a registry beside it, runs this
// package, and deletes the cluster. See README.md.
//
// The API is reached through `kubectl port-forward` to Service pando, which
// lands on one replica; the forward is restarted whenever that replica goes
// away. What the cluster itself must show — namespaces, policies, pods — is
// read with kubectl.
package kubernetes_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// kubeContext is the cluster this package runs against.
func kubeContext() string {
	if v := os.Getenv("PANDO_K8S_CONTEXT"); v != "" {
		return v
	}
	return "kind-pando-k8s"
}

// The administrator setup.sh's fresh install is claimed with.
const adminPassword = "pando-acceptance-suite-admin"

var (
	enabled bool
	fwd     *forwarder
)

func TestMain(m *testing.M) {
	// Skipped unless pointed at the cluster on purpose: PANDO_K8S_CONTEXT,
	// or kubectl's current context being the kind cluster.
	current, _ := exec.Command("kubectl", "config", "current-context").Output()
	enabled = os.Getenv("PANDO_K8S_CONTEXT") != "" || strings.TrimSpace(string(current)) == kubeContext()
	if enabled {
		fwd = &forwarder{}
		if err := fwd.ensure(); err != nil {
			fmt.Fprintln(os.Stderr, "could not reach Pando through a port-forward:", err)
			os.Exit(1)
		}
		claim()
	}
	code := m.Run()
	if fwd != nil {
		fwd.stop()
	}
	os.Exit(code)
}

func requireCluster(t *testing.T) {
	t.Helper()
	if !enabled {
		t.Skipf("no Kubernetes cluster: set PANDO_K8S_CONTEXT, or make %s kubectl's current context (make test-kubernetes)", kubeContext())
	}
}

// kubectl runs kubectl against the test cluster.
func kubectl(args ...string) (string, error) {
	cmd := exec.Command("kubectl", append([]string{"--context", kubeContext()}, args...)...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err != nil {
		return out.String() + errb.String(), fmt.Errorf("kubectl %s: %w: %s", strings.Join(args, " "), err, errb.String())
	}
	return out.String(), nil
}

func mustKubectl(t *testing.T, args ...string) string {
	t.Helper()
	out, err := kubectl(args...)
	require.NoError(t, err, out)
	return out
}

// psql runs one query against Pando's database and returns the bare result.
func psql(t *testing.T, query string) string {
	t.Helper()
	return strings.TrimSpace(mustKubectl(t, "-n", "pando", "exec", "deploy/postgres", "--",
		"psql", "-U", "pando", "-d", "pando", "-tAc", query))
}

// forwarder keeps one `kubectl port-forward` to Service pando alive.
type forwarder struct {
	mu   sync.Mutex
	port int
	cmd  *exec.Cmd
}

func (f *forwarder) base() string { return fmt.Sprintf("http://127.0.0.1:%d", f.port) }

func (f *forwarder) healthy() bool {
	if f.cmd == nil {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(f.base() + "/readyz")
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ensure restarts the forward when the replica behind it has gone.
func (f *forwarder) ensure() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.healthy() {
		return nil
	}
	deadline := time.Now().Add(3 * time.Minute)
	for {
		f.stopLocked()
		port, err := freePort()
		if err != nil {
			return err
		}
		f.port = port
		f.cmd = exec.Command("kubectl", "--context", kubeContext(), "-n", "pando",
			"port-forward", "svc/pando", fmt.Sprintf("%d:8080", port))
		if err := f.cmd.Start(); err != nil {
			return err
		}
		for i := 0; i < 20; i++ {
			time.Sleep(500 * time.Millisecond)
			if f.healthy() {
				return nil
			}
		}
		if time.Now().After(deadline) {
			f.stopLocked()
			return fmt.Errorf("pando did not answer through a port-forward within 3m")
		}
	}
}

func (f *forwarder) stop() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopLocked()
}

func (f *forwarder) stopLocked() {
	if f.cmd != nil && f.cmd.Process != nil {
		_ = f.cmd.Process.Kill()
		_ = f.cmd.Wait()
	}
	f.cmd = nil
}

func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// claim sets up a fresh install's administrator, as the acceptance suite does.
func claim() {
	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(fwd.base() + "/api/v1/setup")
	if err != nil {
		return
	}
	var setup struct {
		Needed bool `json:"needed"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&setup)
	_ = resp.Body.Close()
	if !setup.Needed {
		return
	}
	resp, err = c.Post(fwd.base()+"/api/v1/setup", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, adminPassword)))
	if err == nil {
		_ = resp.Body.Close()
	}
}

type client struct {
	http   *http.Client
	cookie string
}

func login(t *testing.T) *client {
	t.Helper()
	requireCluster(t)
	require.NoError(t, fwd.ensure())
	c := &client{http: &http.Client{Timeout: 2 * time.Minute}}
	resp, err := c.http.Post(fwd.base()+"/api/v1/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, adminPassword)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	for _, ck := range resp.Cookies() {
		if ck.Name == "pando_session" {
			c.cookie = ck.Value
		}
	}
	require.NotEmpty(t, c.cookie)
	return c
}

// do makes one API call, retrying once through a fresh forward when the
// replica behind the old one went away.
func (c *client) do(t *testing.T, method, path, body string) (string, int) {
	t.Helper()
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if body != "" {
			reader = strings.NewReader(body)
		}
		req, err := http.NewRequest(method, fwd.base()+"/api/v1"+path, reader)
		require.NoError(t, err)
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
		resp, err := c.http.Do(req)
		if err != nil {
			require.Less(t, attempt, 3, "%s %s: %v", method, path, err)
			require.NoError(t, fwd.ensure())
			continue
		}
		out, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return string(out), resp.StatusCode
	}
}

func (c *client) must(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	out, status := c.do(t, method, path, body)
	require.Equal(t, want, status, "%s %s: %s", method, path, out)
	var v map[string]any
	if strings.TrimSpace(out) != "" {
		require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	}
	return v
}

func (c *client) get(t *testing.T, path string) map[string]any {
	t.Helper()
	return c.must(t, http.MethodGet, path, "", http.StatusOK)
}

func stamp() string { return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000) }

// imageSpec is an app running a published image on the Kubernetes runtime,
// routed by path. workload is spliced into the one workload's object.
func imageSpec(image string, port int, workload string) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": %q},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"image": %q,
			"ports": [{"number": %d, "protocol": "http", "source": "user"}]%s}],
		"routing": {"adapter_ref": "rte_traefik", "mode": "path"},
		"runtime": {"adapter_ref": "rt_kubernetes", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, image, image, port, workload)
}

// createApp makes an app, deleted when the test ends.
func (c *client) createApp(t *testing.T, name string) string {
	t.Helper()
	app := c.must(t, http.MethodPost, "/apps", fmt.Sprintf(`{"name":%q}`, name), http.StatusAccepted)
	id := app["id"].(string)
	t.Cleanup(func() { c.do(t, http.MethodDelete, "/apps/"+id+"?force=true", "") })
	return id
}

// startDeploy writes spec as revision 1, pins it and deploys it.
func (c *client) startDeploy(t *testing.T, app, spec string) string {
	t.Helper()
	c.must(t, http.MethodPost, "/apps/"+app+"/specs", spec, http.StatusCreated)
	c.must(t, http.MethodPost, "/apps/"+app+"/specs/1/pin", "", http.StatusOK)
	dep := c.must(t, http.MethodPost, "/apps/"+app+"/deployments", "{}", http.StatusAccepted)
	return dep["id"].(string)
}

func (c *client) awaitDeployment(t *testing.T, app, dep string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		d := c.get(t, fmt.Sprintf("/apps/%s/deployments/%s", app, dep))
		switch d["status"] {
		case "succeeded", "failed", "superseded":
			return d
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s did not finish within %s (last status %v)\n%s", dep, within, d["status"], c.deploymentLogs(t, app, dep))
		}
		time.Sleep(2 * time.Second)
	}
}

func (c *client) deploymentLogs(t *testing.T, app, dep string) string {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/apps/%s/deployments/%s/logs", fwd.base(), app, dep), nil)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.http.Do(req.WithContext(ctx))
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			lines = append(lines, after)
		}
	}
	return strings.Join(lines, "\n")
}

// deployImage deploys spec to a new app and requires it to succeed.
func (c *client) deployImage(t *testing.T, name, spec string) string {
	t.Helper()
	app := c.createApp(t, name+"-"+stamp())
	dep := c.startDeploy(t, app, spec)
	final := c.awaitDeployment(t, app, dep, 6*time.Minute)
	require.Equal(t, "succeeded", final["status"], "deploy failed: %v\n%s", final["error_detail"], c.deploymentLogs(t, app, dep))
	return app
}

// namespaceOf is the app's namespace (notes, "Namespace per app").
func namespaceOf(app string) string { return "pando-" + strings.ToLower(strings.ReplaceAll(app, "_", "-")) }

// throughProxy requests path under the app's slug from Pando's proxy, as the
// signed-in administrator, with extra headers and cookies.
func (c *client) throughProxy(t *testing.T, base, host, slug, path string, headers map[string]string, cookies ...*http.Cookie) (int, string) {
	t.Helper()
	status, body, err := c.proxyGet(base, host, slug, path, headers, cookies...)
	require.NoError(t, err)
	return status, body
}

// proxyGet is throughProxy returning a connection error rather than failing.
func (c *client) proxyGet(base, host, slug, path string, headers map[string]string, cookies ...*http.Cookie) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/"+slug+path, nil)
	if err != nil {
		return 0, "", err
	}
	if host != "" {
		req.Host = host
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	hc := &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

func (c *client) slug(t *testing.T, app string) string {
	t.Helper()
	return c.get(t, "/apps/"+app)["slug"].(string)
}

// eventually polls cond until it holds or within passes.
func eventually(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %s", what, within)
		}
		time.Sleep(2 * time.Second)
	}
}
