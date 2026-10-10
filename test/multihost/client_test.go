//go:build multihost

package multihost_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func requireHosts(t *testing.T) {
	t.Helper()
	if !enabled {
		t.Skip("the multi-host Docker tests run with MH_TEST=1 (make test-multihost)")
	}
}

// claim sets up the fresh install's administrator, with a setup token from
// `pando admin setup-token` in the first replica (R-046, issue #130).
func claim() {
	token, err := inner(cControl, "exec", cPando, "pando", "admin", "setup-token")
	if err != nil {
		return
	}
	resp, err := http.Post(base+"/api/v1/setup", "application/json",
		strings.NewReader(fmt.Sprintf(`{"setup_token":%q,"username":"admin","password":%q}`, token, adminPassword)))
	if err == nil {
		_ = resp.Body.Close()
	}
}

// awaitRuntime waits for rt_hosts to be healthy and for both app hosts'
// agents to run: Pando starts them on its first rejoin pass.
func awaitRuntime(within time.Duration) error {
	last := "no answer"
	err := retry(within, func() error {
		c, err := session()
		if err != nil {
			last = err.Error()
			return err
		}
		out, status, err := c.raw(http.MethodGet, "/adapters", "")
		if err != nil || status != http.StatusOK {
			last = fmt.Sprintf("%d %v %s", status, err, out)
			return fmt.Errorf("%s", last)
		}
		var list struct {
			Adapters []struct {
				ID      string `json:"id"`
				Healthy bool   `json:"healthy"`
				Error   any    `json:"health_error"`
			} `json:"adapters"`
		}
		_ = json.Unmarshal([]byte(out), &list)
		healthy := false
		for _, a := range list.Adapters {
			if a.ID == "rt_hosts" {
				healthy = a.Healthy
				last = fmt.Sprintf("rt_hosts healthy=%v %v", a.Healthy, a.Error)
			}
		}
		if !healthy {
			return fmt.Errorf("%s", last)
		}
		for _, h := range []string{cHostA, cHostB} {
			running, err := inner(h, "inspect", "-f", "{{.State.Running}}", "pando-agent")
			if err != nil || running != "true" {
				last = fmt.Sprintf("the agent on %s is not running yet: %v", h, err)
				return fmt.Errorf("%s", last)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("the docker-hosts runtime did not become ready within %s: %s", within, last)
	}
	return nil
}

type client struct {
	http   *http.Client
	cookie string
}

func session() (*client, error) {
	c := &client{http: &http.Client{Timeout: 2 * time.Minute}}
	resp, err := c.http.Post(base+"/api/v1/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, adminPassword)))
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("sign in: %d %s", resp.StatusCode, body)
	}
	for _, ck := range resp.Cookies() {
		if ck.Name == "pando_session" {
			c.cookie = ck.Value
		}
	}
	return c, nil
}

func login(t *testing.T) *client {
	t.Helper()
	requireHosts(t)
	c, err := session()
	require.NoError(t, err)
	require.NotEmpty(t, c.cookie)
	return c
}

func (c *client) raw(method, path, body string) (string, int, error) {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, base+"/api/v1"+path, reader)
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	resp, err := c.http.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, _ := io.ReadAll(resp.Body)
	return string(out), resp.StatusCode, nil
}

func (c *client) do(t *testing.T, method, path, body string) (string, int) {
	t.Helper()
	out, status, err := c.raw(method, path, body)
	require.NoError(t, err, "%s %s", method, path)
	return out, status
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

// nextPort hands each app its own loopback port.
var nextPort atomic.Int32

func init() { nextPort.Store(9100) }

// echoSpec is the echo image on the docker-hosts runtime, limited to
// memory bytes (0: the install default).
func echoSpec(port int, memory int64, env string) string {
	resources := ""
	if memory > 0 {
		resources = fmt.Sprintf(`,"resources": {"cpu_millis": 100, "memory_bytes": %d}`, memory)
	}
	envs := `{"key": "HTTP_PORT", "value": "8080"}`
	if env != "" {
		envs += `, {"key": "MH_TEST_REVISION", "value": "` + env + `"}`
	}
	return fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": %q},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"env": [%s],
			"ports": [{"number": 8080, "protocol": "http", "source": "user"}]%s}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_hosts", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, echoRemote, envs, resources, port)
}

// createApp makes an app, deleted when the test ends, and waits for its
// containers to be gone so the next test reads the hosts' room afresh.
func (c *client) createApp(t *testing.T, name string) string {
	t.Helper()
	app := c.must(t, http.MethodPost, "/apps", fmt.Sprintf(`{"name":%q}`, name), http.StatusAccepted)
	id := app["id"].(string)
	t.Cleanup(func() { c.deleteApp(t, id) })
	return id
}

func (c *client) deleteApp(t *testing.T, id string) {
	c.do(t, http.MethodDelete, "/apps/"+id+"?force=true", "")
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if where, _ := hostOf(id); where == "" {
			return
		}
		time.Sleep(2 * time.Second)
	}
}

// putSpec writes a revision, pins it and starts a deploy of it. The status
// and body are returned, so a refusal can be asserted.
func (c *client) putSpec(t *testing.T, app, spec string) int {
	t.Helper()
	rev := c.must(t, http.MethodPost, "/apps/"+app+"/specs", spec, http.StatusCreated)
	n := int(rev["revision"].(float64))
	c.must(t, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", app, n), "", http.StatusOK)
	return n
}

func (c *client) startDeploy(t *testing.T, app string) (map[string]any, int, string) {
	t.Helper()
	out, status := c.do(t, http.MethodPost, "/apps/"+app+"/deployments", "{}")
	var v map[string]any
	_ = json.Unmarshal([]byte(out), &v)
	return v, status, out
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
			t.Fatalf("deployment %s did not finish within %s (last status %v)\n%s", dep, within, d["status"], c.deploymentLogs(app, dep))
		}
		time.Sleep(2 * time.Second)
	}
}

func (c *client) deploymentLogs(app, dep string) string {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/v1/apps/%s/deployments/%s/logs", base, app, dep), nil)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
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

// deploy writes spec to app and requires the deploy to succeed.
func (c *client) deploy(t *testing.T, app, spec string) {
	t.Helper()
	c.putSpec(t, app, spec)
	dep, status, out := c.startDeploy(t, app)
	require.Equal(t, http.StatusAccepted, status, out)
	final := c.awaitDeployment(t, app, dep["id"].(string), 8*time.Minute)
	require.Equal(t, "succeeded", final["status"], "deploy failed: %v\n%s", final["error_detail"], c.deploymentLogs(app, dep["id"].(string)))
}

// deployEcho makes and deploys an echo app with a memory limit.
func (c *client) deployEcho(t *testing.T, name string, memory int64) string {
	t.Helper()
	app := c.createApp(t, name+"-"+stamp())
	c.deploy(t, app, echoSpec(int(nextPort.Add(1)), memory, ""))
	return app
}

// hostOf names the host whose daemon runs the app's containers, or "".
// Every host is asked; an app on two would be a placement bug.
func hostOf(app string) (string, error) {
	var on []string
	for name, c := range hostNames {
		out, err := inner(c, "ps", "-aq", "--filter", "label=io.pando.bundle="+app)
		if err != nil {
			continue
		}
		if strings.TrimSpace(out) != "" {
			on = append(on, name)
		}
	}
	switch len(on) {
	case 0:
		return "", nil
	case 1:
		return on[0], nil
	}
	return strings.Join(on, ","), fmt.Errorf("app %s has containers on %v", app, on)
}

func requireOn(t *testing.T, app, want string) {
	t.Helper()
	got, err := hostOf(app)
	require.NoError(t, err)
	require.Equal(t, want, got, "app %s placement", app)
}

// hostRow is one host in rt_hosts' capacity details.
type hostRow struct {
	Name      string `json:"name"`
	Control   bool   `json:"control"`
	Open      bool   `json:"open_to_new_apps"`
	Reachable bool   `json:"reachable"`
	TotalCPU  int64  `json:"total_cpu_millis"`
	TotalMem  int64  `json:"total_memory_bytes"`
	FreeCPU   int64  `json:"free_cpu_millis"`
	FreeMem   int64  `json:"free_memory_bytes"`
	Workloads int    `json:"running_workloads"`
}

type capacity struct {
	TotalCPU int64 `json:"total_cpu_millis"`
	TotalMem int64 `json:"total_memory_bytes"`
	Hosts    map[string]hostRow
}

func (c *client) capacity(t *testing.T) capacity {
	t.Helper()
	out, status := c.do(t, http.MethodGet, "/capacity", "")
	require.Equal(t, http.StatusOK, status, out)
	var body struct {
		Runtimes []struct {
			Ref      string `json:"adapter_ref"`
			Status   string `json:"status"`
			TotalCPU int64  `json:"total_cpu_millis"`
			TotalMem int64  `json:"total_memory_bytes"`
			Details  struct {
				Hosts []hostRow `json:"hosts"`
			} `json:"details"`
		} `json:"runtimes"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &body), out)
	for _, r := range body.Runtimes {
		if r.Ref != "rt_hosts" {
			continue
		}
		require.Equal(t, "ok", r.Status, out)
		cap := capacity{TotalCPU: r.TotalCPU, TotalMem: r.TotalMem, Hosts: map[string]hostRow{}}
		for _, h := range r.Details.Hosts {
			cap.Hosts[h.Name] = h
		}
		return cap
	}
	t.Fatalf("no rt_hosts in GET /capacity: %s", out)
	return capacity{}
}

// roomiest is the app host placement should choose for a new app: the most
// free memory, then fewer running workloads, then host-a (listed first).
func (cap capacity) roomiest() (string, string) {
	a, b := cap.Hosts["host-a"], cap.Hosts["host-b"]
	if b.FreeMem > a.FreeMem {
		return "host-b", "host-a"
	}
	return "host-a", "host-b"
}

// steerTo makes want the host with the most free memory, by placing a filler
// app on the other one when it is not already.
func (c *client) steerTo(t *testing.T, want string) {
	t.Helper()
	other := map[string]string{"host-a": "host-b", "host-b": "host-a"}[want]
	for range 3 {
		cap := c.capacity(t)
		if cap.Hosts[want].FreeMem > cap.Hosts[other].FreeMem {
			return
		}
		fill := cap.Hosts[other].FreeMem - cap.Hosts[want].FreeMem + 128<<20
		require.LessOrEqual(t, fill, cap.Hosts[other].FreeMem, "cannot steer to %s: %+v", want, cap.Hosts)
		app := c.deployEcho(t, "mh-filler", fill)
		if cap.Hosts[want].FreeMem < cap.Hosts[other].FreeMem {
			// Not a tie, so placement had one answer.
			requireOn(t, app, other)
		}
		// On a tie the filler went to whichever host has fewer apps, or
		// host-a; the next pass reads the hosts again.
	}
	t.Fatalf("could not make %s the roomier host", want)
}

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

func (c *client) slug(t *testing.T, app string) string {
	t.Helper()
	return c.get(t, "/apps/"+app)["slug"].(string)
}

// throughProxy requests path under the app's slug from Pando's proxy, signed
// in, with extra headers and cookies.
func (c *client) throughProxy(t *testing.T, slug, path string, headers map[string]string, cookies ...*http.Cookie) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/"+slug+path, nil)
	require.NoError(t, err)
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
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// throughProxyVia is a signed-in GET of the app's path through one replica.
func (c *client) throughProxyVia(t *testing.T, via, slug, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, via+"/"+slug+path, nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	hc := &http.Client{Timeout: 30 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, strings.ToLower(string(body))
}
