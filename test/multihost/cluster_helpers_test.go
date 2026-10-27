//go:build multihost

package multihost_test

import (
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The cluster's timing, shortened so the run takes minutes (R-421, R-420):
// every host is started with T clT (probes every T/6), a standby pass every
// clStandbyEvery and a ship every clShipEvery (startPando).
const (
	clT            = 15 * time.Second
	clLostAfter    = 3 * clT
	clStandbyEvery = 10 * time.Second
	clShipEvery    = 30 * time.Second
)

func requireCluster(t *testing.T) *client {
	t.Helper()
	if !clusterEnabled {
		t.Skip("the cluster tests run with MH_TEST=1 MH_CLUSTER=1 (make test-cluster)")
	}
	var c *client
	require.NoError(t, retry(3*time.Minute, func() error {
		var err error
		c, err = session()
		return err
	}), "signing in on h1")
	return c
}

// psql runs a query on the install's database and returns its rows, one per
// line, columns separated by |.
func psql(query string) (string, error) {
	return docker("exec", clPostgres, "psql", "-U", "pando", "-d", "pando", "-tA", "-c", query)
}

func mustPSQL(t *testing.T, query string) string {
	t.Helper()
	out, err := psql(query)
	require.NoError(t, err, query)
	return out
}

// try is an API call that rides out a restart: a refused connection or a 502
// or 503 is asked again, for up to two minutes.
func (c *client) try(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	var out string
	var status int
	err := retry(2*time.Minute, func() error {
		var err error
		out, status, err = c.raw(method, path, body)
		if err != nil {
			return err
		}
		if status == http.StatusUnauthorized {
			if fresh, serr := session(); serr == nil {
				*c = *fresh
			}
			return fmt.Errorf("signed out by a restart")
		}
		if status == http.StatusBadGateway || status == http.StatusServiceUnavailable {
			return fmt.Errorf("%d", status)
		}
		return nil
	})
	require.NoError(t, err, "%s %s", method, path)
	require.Equal(t, want, status, "%s %s: %s", method, path, out)
	var v map[string]any
	if strings.TrimSpace(out) != "" {
		require.NoError(t, json.Unmarshal([]byte(out), &v), out)
	}
	return v
}

type clusterHost struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Roles      []string `json:"roles"`
	Address    string   `json:"address"`
	JoinedWith string   `json:"joined_with"`
	Version    string   `json:"version"`
}

func (c *client) clusterHosts(t *testing.T) map[string]clusterHost {
	t.Helper()
	v := c.try(t, http.MethodGet, "/cluster/hosts", "", http.StatusOK)
	raw, err := json.Marshal(v["hosts"])
	require.NoError(t, err)
	var list []clusterHost
	require.NoError(t, json.Unmarshal(raw, &list))
	out := map[string]clusterHost{}
	for _, h := range list {
		out[h.Name] = h
	}
	return out
}

// mint makes a join token for roles and returns the token to join with.
func (c *client) mint(t *testing.T, roles ...string) string {
	t.Helper()
	body, err := json.Marshal(map[string]any{"roles": roles})
	require.NoError(t, err)
	v := c.try(t, http.MethodPost, "/cluster/join-tokens", string(body), http.StatusCreated)
	join, _ := v["join"].(string)
	require.NotEmpty(t, join, "a join token: %v", v)
	return join
}

// bundleOn lists the containers of the app's own bundle (not its standby's)
// on h: running ones only unless all.
func bundleOn(h *clHost, app string, all bool) ([]string, error) {
	args := []string{"ps", "-q", "--no-trunc", "--filter", "label=io.pando.bundle=" + app}
	if all {
		args = append(args, "-a")
	} else {
		args = append(args, "--filter", "status=running")
	}
	out, err := inner(h.container, args...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

// standbyOn lists the containers of the app's standby on h, stopped ones
// included.
func standbyOn(h *clHost, app string) []string {
	out, err := inner(h.container, "ps", "-aq", "--no-trunc", "--filter", "label=io.pando.bundle="+app+"-standby")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

func hostByName(name string) *clHost {
	for _, h := range clHosts {
		if h.name == name {
			return h
		}
	}
	return nil
}

// runningHosts names the hosts running the app's own containers.
func runningHosts(app string) []string {
	var on []string
	for _, h := range clHosts {
		ids, err := bundleOn(h, app, false)
		if err == nil && len(ids) > 0 {
			on = append(on, h.name)
		}
	}
	return on
}

// copies watches every host for the app's running containers until stopped,
// and records any moment two hosts ran it at once (R-153), and when each
// host was last and first seen running it.
type copies struct {
	app     string
	mu      sync.Mutex
	overlap []string
	seen    map[string][]time.Time // per host, the times it was seen running
	stop    chan struct{}
	done    chan struct{}
}

func watchCopies(app string) *copies {
	w := &copies{app: app, seen: map[string][]time.Time{}, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		for {
			select {
			case <-w.stop:
				return
			default:
			}
			now := time.Now()
			on := runningHosts(app)
			w.mu.Lock()
			if len(on) > 1 {
				w.overlap = append(w.overlap, fmt.Sprintf("%s: %v", now.Format(time.RFC3339), on))
			}
			for _, h := range on {
				w.seen[h] = append(w.seen[h], now)
			}
			w.mu.Unlock()
			time.Sleep(time.Second)
		}
	}()
	return w
}

func (w *copies) Stop() {
	select {
	case <-w.stop:
	default:
		close(w.stop)
	}
	<-w.done
}

func (w *copies) overlaps() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]string(nil), w.overlap...)
}

// lastSeen is the last time host was seen running the app, zero if never.
func (w *copies) lastSeen(host string) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.seen[host]
	if len(s) == 0 {
		return time.Time{}
	}
	return s[len(s)-1]
}

// seenAfter is the first time host was seen running the app after from.
func (w *copies) seenAfter(host string, from time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, at := range w.seen[host] {
		if at.After(from) {
			return at
		}
	}
	return time.Time{}
}

// appVia is the URL of the app's port on control host h's proxy.
func (c *client) appVia(t *testing.T, app string, h *clHost) string {
	t.Helper()
	v := c.try(t, http.MethodGet, "/apps/"+app, "", http.StatusOK)
	address, _ := v["address"].(string)
	_, p, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(address, "http://"), "https://"), "/"))
	require.NoError(t, err, "a port-mode address: %q", address)
	_, err = strconv.Atoi(p)
	require.NoError(t, err)
	published, err := publishedPort(h.container, p+"/tcp")
	require.NoError(t, err, "port %s is not published on %s", p, h.name)
	return "http://127.0.0.1:" + published
}

// fetch is a signed-in request to the app at url through Pando's proxy.
func (c *client) fetch(url string, headers map[string]string, cookies ...*http.Cookie) (int, string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if c != nil {
		req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	}
	for _, ck := range cookies {
		req.AddCookie(ck)
	}
	hc := &http.Client{Timeout: 20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body), nil
}

// counter reads count= and boots= from the app's /cgi-bin/count answer.
func counter(body string) (count, boots int, err error) {
	for _, f := range strings.Fields(body) {
		if v, ok := strings.CutPrefix(f, "count="); ok {
			count, err = strconv.Atoi(v)
		}
		if v, ok := strings.CutPrefix(f, "boots="); ok && err == nil {
			boots, err = strconv.Atoi(v)
		}
	}
	if err == nil && (count == 0 || boots == 0) {
		err = errors.New("not a counter answer: " + body)
	}
	return count, boots, err
}

// counterApp is the test's app: busybox httpd serving two CGI scripts, one
// counting requests into its volume, one printing what reached it. Every
// start counts itself into the volume too, so a copy that came back from a
// standby shows both counts carried over. Without volume, /data is the
// container's own and is lost with it.
func counterApp(port int, standby string, memory int64, volume bool) string {
	count := "#!/bin/sh\n" +
		"n=$(cat /data/count 2>/dev/null || echo 0)\n" +
		"n=$((n+1))\n" +
		"echo $n > /data/count\n" +
		"printf 'Content-Type: text/plain\\r\\n\\r\\n'\n" +
		"echo \"count=$n boots=$(cat /data/boots)\"\n"
	env := "#!/bin/sh\nprintf 'Content-Type: text/plain\\r\\n\\r\\n'\nenv\n"
	spec := map[string]any{
		"schema_version": 1,
		"source":         map[string]any{"type": "image", "image": clAppImage},
		"build":          map[string]any{"strategy": "prebuilt"},
		"volumes":        []any{map[string]any{"id": "vol_data", "name": "data", "declared": "user"}},
		"workloads": []any{map[string]any{
			"name": "web", "primary": true, "exposed": true, "image": clAppImage,
			"command": []string{"sh", "-c",
				"mkdir -p /data; n=$(cat /data/boots 2>/dev/null || echo 0); echo $((n+1)) > /data/boots; exec httpd -f -p 8080 -h /www"},
			"files": []any{
				map[string]any{"path": "/www/index.html", "content": "ok\n"},
				map[string]any{"path": "/www/cgi-bin/count", "content": count, "mode": 0o755},
				map[string]any{"path": "/www/cgi-bin/env", "content": env, "mode": 0o755},
			},
			"mounts":    []any{map[string]any{"volume_id": "vol_data", "path": "/data"}},
			"ports":     []any{map[string]any{"number": 8080, "protocol": "http", "source": "user"}},
			"resources": map[string]any{"cpu_millis": 100, "memory_bytes": memory},
		}},
		// Path mode first: a port is allocated, never typed (design 03
		// §4.2), so deployApp asks for one through the routing endpoint.
		"routing":      map[string]any{"adapter_ref": "rte_loopback", "mode": "path", "path_prefix": fmt.Sprintf("/mh-%d", port)},
		"runtime":      map[string]any{"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy":       map[string]any{"strategy": "recreate"},
		"availability": map[string]any{"standby": standby},
	}
	if !volume {
		delete(spec, "volumes")
		w := spec["workloads"].([]any)[0].(map[string]any)
		delete(w, "mounts")
	}
	raw, _ := json.Marshal(spec)
	return string(raw)
}

// deployApp makes an app and deploys spec to it, requiring success. Apps
// are not deleted as each test ends: the tests after it use them, and the
// machines go with the run.
func (c *client) deployApp(t *testing.T, name, spec string) string {
	t.Helper()
	app := c.try(t, http.MethodPost, "/apps", fmt.Sprintf(`{"name":%q}`, name), http.StatusAccepted)
	id := app["id"].(string)
	rev := c.try(t, http.MethodPost, "/apps/"+id+"/specs", spec, http.StatusCreated)
	n := int(rev["revision"].(float64))
	c.try(t, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", id, n), "", http.StatusOK)
	// The app's own port, from the range each control host publishes
	// (PANDO_SERVER_PORT_RANGE_START), which every control host's proxy
	// serves (R-166).
	moved := c.try(t, http.MethodPut, "/apps/"+id+"/routing",
		`{"adapter_ref":"rte_loopback","mode":"port","confirm":true}`, http.StatusCreated)
	n = int(moved["revision"].(float64))
	c.try(t, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", id, n), "", http.StatusOK)
	dep := c.try(t, http.MethodPost, "/apps/"+id+"/deployments", "{}", http.StatusAccepted)
	final := c.awaitDeployment(t, id, dep["id"].(string), 8*time.Minute)
	require.Equal(t, "succeeded", final["status"], "deploy of %s failed: %v\n%s", name, final["error_detail"],
		c.deploymentLogs(id, dep["id"].(string)))
	return id
}

// tlsFrom reads a certificate and key from a joined host's cluster
// directory.
func tlsFrom(t *testing.T, h *clHost, cert, key string) tls.Certificate {
	t.Helper()
	read := func(name string) []byte {
		out, err := inner(h.container, "exec", h.pando, "cat", "/var/lib/pando/cluster/"+name)
		require.NoError(t, err, "reading %s on %s", name, h.name)
		return []byte(out + "\n")
	}
	pair, err := tls.X509KeyPair(read(cert), read(key))
	require.NoError(t, err)
	return pair
}

// listenerOf is where the test reaches h's cluster listener.
func listenerOf(t *testing.T, h *clHost) string {
	t.Helper()
	port, err := publishedPort(h.container, "7444/tcp")
	require.NoError(t, err)
	return "127.0.0.1:" + port
}
