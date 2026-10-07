//go:build integration

// Package replicas_test runs Pando as several replicas behind a load balancer,
// against one Postgres, and changes the topology under it (issue #72).
//
// It drives the stack test/replicas/docker-compose.replicas.yml lays over the
// shipped Compose file. `make test-replicas` brings that up, runs this, runs
// the design 07 sequences through the same balancer, and takes it down:
//
//	make test-replicas
//
// Run by hand against a stack already up:
//
//	PANDO_REPLICAS_PROJECT=pando-replicas PANDO_TEST_URL=http://localhost:18080/api/v1 \
//	PANDO_TEST_PASSWORD=… go test -tags=integration ./test/replicas/
package replicas_test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func baseURL() string {
	if v := os.Getenv("PANDO_TEST_URL"); v != "" {
		return strings.TrimSuffix(v, "/")
	}
	return "http://localhost:18080/api/v1"
}

func rootURL() string { return strings.TrimSuffix(baseURL(), "/api/v1") }

// project is the Compose project the stack runs as. Unset means there is no
// stack this test may change, and it skips: it restarts and kills containers,
// and must never do that to a stack somebody is using.
func project(t *testing.T) string {
	t.Helper()
	p := os.Getenv("PANDO_REPLICAS_PROJECT")
	if p == "" {
		t.Skip("PANDO_REPLICAS_PROJECT is not set; run `make test-replicas`, which brings up a stack this test may restart")
	}
	return p
}

// compose runs docker compose against the replicas stack, from the repository
// root, where the Compose files' relative paths resolve.
func compose(t *testing.T, args ...string) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	require.NoError(t, err)
	full := append([]string{"compose", "-p", project(t),
		"-f", "docker-compose.yml", "-f", "test/replicas/docker-compose.replicas.yml"}, args...)
	cmd := exec.Command("docker", full...)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "docker %s\n%s", strings.Join(full, " "), out)
	return strings.TrimSpace(string(out))
}

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	require.NoError(t, err, "docker %s\n%s", strings.Join(args, " "), out)
	return strings.TrimSpace(string(out))
}

// replicaContainers lists the running pando containers.
func replicaContainers(t *testing.T) []string {
	t.Helper()
	out := compose(t, "ps", "-q", "--status", "running", "pando")
	if out == "" {
		return nil
	}
	ids := strings.Fields(out)
	sort.Strings(ids)
	return ids
}

// psql answers a query against the install's database, as its owner.
func psql(t *testing.T, query string) string {
	t.Helper()
	return compose(t, "exec", "-T", "postgres", "psql", "-U", "pando", "-d", "pando", "-tAc", query)
}

// liveReplicas counts the replicas the install itself considers alive.
func liveReplicas(t *testing.T) int {
	t.Helper()
	var n int
	_, err := fmt.Sscan(psql(t, `SELECT count(*) FROM pando_replicas
		WHERE stopped_at IS NULL AND heartbeat_at > now() - interval '45 seconds'`), &n)
	require.NoError(t, err)
	return n
}

// awaitReplicas waits for n running containers, n live replicas, and a
// balancer that answers.
func awaitReplicas(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool {
		if len(replicaContainers(t)) != n || liveReplicas(t) != n {
			return false
		}
		for range 2 * n {
			resp, err := fresh().Get(rootURL() + "/healthz")
			if err != nil {
				return false
			}
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return false
			}
		}
		return true
	}, 3*time.Minute, time.Second, "the stack did not settle at %d replicas", n)
}

// fresh is a client that opens a new connection for every request, so the
// balancer chooses a replica for each one.
func fresh() *http.Client {
	return &http.Client{
		Timeout:   30 * time.Second,
		Transport: &http.Transport{DisableKeepAlives: true},
	}
}

type session struct{ cookie string }

func login(t *testing.T) session {
	t.Helper()
	password := os.Getenv("PANDO_TEST_PASSWORD")
	require.NotEmpty(t, password, "set PANDO_TEST_PASSWORD to the stack's admin password")

	// A fresh stack waits for its first administrator (R-046); claim it.
	if resp, err := fresh().Get(baseURL() + "/setup"); err == nil {
		var setup struct {
			Needed bool `json:"needed"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&setup)
		_ = resp.Body.Close()
		if setup.Needed {
			resp, err := fresh().Post(baseURL()+"/setup", "application/json",
				strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
			require.NoError(t, err)
			_ = resp.Body.Close()
		}
	}

	resp, err := fresh().Post(baseURL()+"/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	for _, c := range resp.Cookies() {
		if c.Name == "pando_session" {
			return session{cookie: c.Value}
		}
	}
	t.Fatal("signing in set no session cookie")
	return session{}
}

func (s session) do(method, path, body string) (string, int, error) {
	req, err := http.NewRequest(method, baseURL()+path, strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: s.cookie})
	resp, err := fresh().Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode, err
}

func (s session) must(t *testing.T, method, path, body string, want int) map[string]any {
	t.Helper()
	got, status, err := s.do(method, path, body)
	require.NoError(t, err)
	require.Equal(t, want, status, got)
	out := map[string]any{}
	if strings.TrimSpace(got) != "" {
		require.NoError(t, json.Unmarshal([]byte(got), &out), got)
	}
	return out
}

// jwksKids reads the published key IDs once, through the balancer.
func jwksKids(t *testing.T) []string {
	t.Helper()
	resp, err := fresh().Get(rootURL() + "/.well-known/jwks.json")
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var doc struct {
		Keys []struct {
			Kid string `json:"kid"`
		} `json:"keys"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&doc))
	kids := make([]string, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		kids = append(kids, k.Kid)
	}
	sort.Strings(kids)
	return kids
}

// liveKids are the key IDs of the replicas alive now.
func liveKids(t *testing.T) []string {
	t.Helper()
	out := psql(t, `SELECT assertion_kid FROM pando_replicas
		WHERE stopped_at IS NULL AND heartbeat_at > now() - interval '45 seconds' ORDER BY 1`)
	kids := strings.Fields(out)
	sort.Strings(kids)
	return kids
}

// poller makes authenticated requests through the balancer until stopped, and
// counts the ones that did not succeed: what a person using the install would
// have seen while its replicas changed.
type poller struct {
	stop     chan struct{}
	wg       sync.WaitGroup
	ok, bad  atomic.Int64
	mu       sync.Mutex
	failures []string
}

func poll(s session) *poller {
	p := &poller{stop: make(chan struct{})}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.stop:
				return
			case <-time.After(100 * time.Millisecond):
			}
			body, status, err := s.do(http.MethodGet, "/apps", "")
			if err == nil && status == http.StatusOK {
				p.ok.Add(1)
				continue
			}
			p.bad.Add(1)
			p.mu.Lock()
			p.failures = append(p.failures, fmt.Sprintf("%d %v %.120s", status, err, body))
			p.mu.Unlock()
		}
	}()
	return p
}

func (p *poller) done() { close(p.stop); p.wg.Wait() }

// TestR256_PandoServesAsSeveralReplicasAgainstOneDatabase asserts the topology
// issue #72 supports: N Pando replicas, one Postgres, one Docker host, behind a
// load balancer — and that rolling restarts, scaling between one and two, and
// losing a replica outright do not break the install.
//
// The steps run in order, because each changes the stack the next one stands
// on. The design 07 sequences run afterwards, through the same balancer, by
// `make test-replicas`.
func TestR256_PandoServesAsSeveralReplicasAgainstOneDatabase(t *testing.T) {
	project(t)
	awaitReplicas(t, 2)
	s := login(t)

	t.Run("a session made on one replica is good on every replica", func(t *testing.T) {
		for range 20 {
			s.must(t, http.MethodGet, "/apps", "", http.StatusOK)
		}
	})

	t.Run("every replica publishes every live replica's signing key (R-051)", func(t *testing.T) {
		live := liveKids(t)
		require.Len(t, live, 2, "two replicas, two keys of their own")
		for range 10 {
			require.Subset(t, jwksKids(t), live,
				"whichever replica answered, its JWKS verifies what any replica signs")
		}
	})

	appID := ""
	t.Run("a deploy's log reads the same from every replica", func(t *testing.T) {
		app := s.must(t, http.MethodPost, "/apps", `{"name":"replicas-log-`+stamp()+`"}`, http.StatusAccepted)
		appID = app["id"].(string)
		s.must(t, http.MethodPost, "/apps/"+appID+"/specs", prebuiltSpec(19005, ""), http.StatusCreated)
		s.must(t, http.MethodPost, "/apps/"+appID+"/specs/1/pin", "", http.StatusOK)
		dep := s.must(t, http.MethodPost, "/apps/"+appID+"/deployments", "{}", http.StatusAccepted)
		depID := dep["id"].(string)
		require.Equal(t, "succeeded", awaitDeployment(t, s, appID, depID, 3*time.Minute)["status"])

		// The app answers through every replica's proxy, not only the one
		// that deployed it and joined its network (R-023). Up to one rejoin
		// interval for the other replica to join it.
		slug := s.must(t, http.MethodGet, "/apps/"+appID, "", http.StatusOK)["slug"].(string)
		require.Eventually(t, func() bool {
			for range 10 {
				req, err := http.NewRequest(http.MethodGet, rootURL()+"/"+slug+"/", nil)
				if err != nil {
					return false
				}
				req.AddCookie(&http.Cookie{Name: "pando_session", Value: s.cookie})
				resp, err := fresh().Do(req)
				if err != nil {
					return false
				}
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "nginx") {
					return false
				}
			}
			return true
		}, 45*time.Second, time.Second, "the app answers through every replica")

		first := deployLog(t, s, appID, depID)
		require.Contains(t, first, "=>", "the log has the deploy's steps")
		for range 6 {
			require.Equal(t, first, deployLog(t, s, appID, depID),
				"asked of either replica, the log is the one the deploying replica kept")
		}
	})

	t.Run("a rolling restart keeps serving", func(t *testing.T) {
		p := poll(s)
		for _, c := range replicaContainers(t) {
			docker(t, "restart", "--time", "20", c)
			awaitReplicas(t, 2)
		}
		p.done()
		require.Positive(t, p.ok.Load())
		require.Zero(t, p.bad.Load(), "requests failed during the roll: %v", p.failures)

		// And the session made before the roll still works: sessions are
		// rows, not any one process's memory.
		s.must(t, http.MethodGet, "/apps", "", http.StatusOK)
		require.Len(t, liveKids(t), 2)
	})

	t.Run("a replica lost mid-deploy has the deploy resumed by another", func(t *testing.T) {
		app := s.must(t, http.MethodPost, "/apps", `{"name":"replicas-lost-`+stamp()+`"}`, http.StatusAccepted)
		lostApp := app["id"].(string)
		// A health check that never passes holds the deploy open for its
		// two-minute wait: time enough to take its replica away.
		s.must(t, http.MethodPost, "/apps/"+lostApp+"/specs",
			prebuiltSpec(19006, `, "healthcheck": {"command": ["CMD", "false"], "interval_seconds": 1, "retries": 1}`),
			http.StatusCreated)
		s.must(t, http.MethodPost, "/apps/"+lostApp+"/specs/1/pin", "", http.StatusOK)
		dep := s.must(t, http.MethodPost, "/apps/"+lostApp+"/deployments", "{}", http.StatusAccepted)
		depID := dep["id"].(string)

		// The replica running it, by the hostname it registered, which is
		// its container's ID.
		var host string
		require.Eventually(t, func() bool {
			host = psql(t, `SELECT r.hostname FROM deployments d JOIN pando_replicas r ON r.id = d.replica_id
				WHERE d.id = '`+depID+`' AND d.status IN ('pending', 'building', 'applying')`)
			return host != ""
		}, 30*time.Second, 500*time.Millisecond, "the deploy is in flight on a replica")

		// SIGKILL: no shutdown, no goodbye, the way a node disappears. Docker
		// takes a kill for a deliberate stop and does not restart it, so the
		// test starts it again below, as an orchestrator would.
		docker(t, "kill", "--signal", "KILL", host)

		// The surviving leader notices the silence and puts the deploy back
		// in the queue, and the surviving replica takes it (O-32): every step
		// before a deploy commits is safe to repeat.
		require.Eventually(t, func() bool {
			other := psql(t, `SELECT r.hostname FROM deployments d JOIN pando_replicas r ON r.id = d.replica_id
				WHERE d.id = '`+depID+`'`)
			return other != "" && other != host
		}, 2*time.Minute, 2*time.Second, "the lost replica's deploy was never resumed elsewhere")

		// And it finishes there, so the app's next deploy is not refused on
		// its account.
		require.Eventually(t, func() bool {
			got := s.must(t, http.MethodGet, "/apps/"+lostApp+"/deployments/"+depID, "", http.StatusOK)
			return got["status"] == "succeeded" || got["status"] == "failed"
		}, 4*time.Minute, 2*time.Second, "the resumed deploy never finished")

		// The killed container comes back; the install returns to two
		// replicas, the killed one under a new identity.
		docker(t, "start", host)
		awaitReplicas(t, 2)
		next := s.must(t, http.MethodPost, "/apps/"+lostApp+"/deployments", "{}", http.StatusAccepted)
		require.NotEmpty(t, next["id"], "the next deploy is accepted")
	})

	t.Run("scaling to one and back to two", func(t *testing.T) {
		p := poll(s)
		compose(t, "up", "-d", "--no-recreate", "--scale", "pando=1", "pando")
		awaitReplicas(t, 1)
		s.must(t, http.MethodGet, "/apps", "", http.StatusOK)

		compose(t, "up", "-d", "--no-recreate", "--scale", "pando=2", "pando")
		awaitReplicas(t, 2)
		p.done()
		require.Zero(t, p.bad.Load(), "requests failed while scaling: %v", p.failures)
		require.Subset(t, jwksKids(t), liveKids(t))
	})

	t.Run("exactly one replica leads", func(t *testing.T) {
		var leaders int
		// state.leaderLock, 0x70616e646f02, as Postgres reports a bigint key:
		// the high half in classid, the low half in objid.
		_, err := fmt.Sscan(psql(t, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND classid = 28769 AND objid = 1852075778 AND objsubid = 1`), &leaders)
		require.NoError(t, err)
		require.Equal(t, 1, leaders, "one holder of the leader lock, whatever the topology has been through")
	})

	if appID != "" {
		_, _, _ = s.do(http.MethodDelete, "/apps/"+appID, "")
	}
}

func stamp() string { return fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000) }

// prebuiltSpec needs no build. port is in the range the replicas stack
// allocates (Makefile REPLICAS_ENV); extra is spliced into the workload.
func prebuiltSpec(port int, extra string) string {
	return `{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]` + extra + `}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": ` + fmt.Sprint(port) + `},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`
}

func awaitDeployment(t *testing.T, s session, appID, depID string, within time.Duration) map[string]any {
	t.Helper()
	var dep map[string]any
	require.Eventually(t, func() bool {
		dep = s.must(t, http.MethodGet, "/apps/"+appID+"/deployments/"+depID, "", http.StatusOK)
		switch dep["status"] {
		case "succeeded", "failed", "superseded":
			return true
		}
		return false
	}, within, 2*time.Second)
	return dep
}

// deployLog reads a deploy's log stream to its end.
func deployLog(t *testing.T, s session, appID, depID string) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, baseURL()+"/apps/"+appID+"/deployments/"+depID+"/logs", nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: s.cookie})
	resp, err := fresh().Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if after, ok := strings.CutPrefix(line, "data: "); ok {
			lines = append(lines, after)
		}
	}
	return strings.Join(lines, "\n")
}
