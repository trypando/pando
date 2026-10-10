//go:build integration

package acceptance_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The acceptance tests drive a running Compose stack over HTTP, as a client
// would. Nothing is stubbed: the assertions are about the shipped topology.
//
// Bring the stack up first:
//
//	docker compose up -d
//	go test -tags=integration ./test/acceptance/
//
// 8080 is what Compose gives you. An install somewhere else — a host where 8080
// was taken, a second stack beside a first — sets PANDO_TEST_URL.
func baseURL() string {
	if v := os.Getenv("PANDO_TEST_URL"); v != "" {
		return v
	}
	return "http://localhost:8080/api/v1"
}

// requireStack skips when the stack was never up, and FAILS when it was up and
// has gone away.
//
// The distinction is the whole point. A developer running `go test ./...`
// without Compose should see skips, not noise — that was the original intent
// and it is right. But the same skip applied to a stack that disappeared
// mid-run turns a broken server into a green suite: a run that skipped 35 of 54
// tests reported `ok`, and the only way to notice was to count the tests that
// ran against the tests that exist.
//
// That is the worst failure a test suite can have. It is not that it missed a
// bug; it is that it said everything was fine while testing nothing.
//
// So the first check records whether the stack was ever reachable. After that,
// unreachable means something took the server down — which is a finding, not a
// reason to stay quiet.
func requireStack(t *testing.T) {
	t.Helper()

	resp, err := http.Get(strings.TrimSuffix(baseURL(), "/api/v1") + "/healthz")
	if err == nil {
		_ = resp.Body.Close()
		stackWasUp.Store(true)
		return
	}

	if stackWasUp.Load() {
		t.Fatalf("the Pando stack at %s was reachable earlier in this run and is not now: %v\n"+
			"Something in the suite took the server down. Do not read the rest of this run as a pass.",
			baseURL(), err)
	}
	t.Skipf("pando stack not running at %s: %v", baseURL(), err)
}

// stackWasUp records that the stack answered at least once, so a later failure
// to reach it can be told apart from never having had one.
var stackWasUp atomic.Bool

type client struct {
	http   *http.Client
	cookie string
}

// login signs in as the first-run administrator.
//
// The password is PANDO_TEST_PASSWORD when set. Otherwise the suite sets up a
// fresh stack itself, through POST /setup, with suitePassword — and on a stack
// it set up before, signs in with the same one (R-046).
func login(t *testing.T) *client {
	t.Helper()
	requireStack(t)

	password := adminPassword
	require.NotEmpty(t, password,
		"could not sign in as admin.\n"+
			"Either bring the stack up fresh — `docker compose down -v && docker compose up -d` —\n"+
			"or set PANDO_TEST_PASSWORD to the admin account's password.")

	// Generous, because POST /deployments is not the quick call its 202 status
	// suggests. It resolves the app's ref to a commit before returning, and
	// resolving a ref means cloning — design 01 §2.1 is explicit that a deploy
	// never resolves a ref implicitly at runtime, so the work happens here. A
	// cold clone of a real repository takes longer than a conversational
	// timeout allows.
	c := &client{http: &http.Client{Timeout: 2 * time.Minute}}

	resp, err := c.http.Post(baseURL()+"/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode,
		"the admin password did not work — set PANDO_TEST_PASSWORD if it has been changed")

	for _, ck := range resp.Cookies() {
		if ck.Name == "pando_session" {
			c.cookie = ck.Value
		}
	}
	require.NotEmpty(t, c.cookie)
	return c
}

func (c *client) do(t *testing.T, method, path, body string) (string, int) {
	t.Helper()

	var reader io.Reader
	if body != "" {
		reader = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, baseURL()+path, reader)
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})

	resp, err := c.http.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	out, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	return string(out), resp.StatusCode
}

func (c *client) postRaw(t *testing.T, path, body string) (string, int) {
	return c.do(t, http.MethodPost, path, body)
}

func (c *client) get(t *testing.T, path string) map[string]any {
	t.Helper()
	body, status := c.do(t, http.MethodGet, path, "")
	require.Equal(t, http.StatusOK, status, body)

	var out map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &out))
	return out
}

func (c *client) put(t *testing.T, path, body string) {
	t.Helper()
	out, status := c.do(t, http.MethodPut, path, body)
	require.Less(t, status, 300, out)
}

func (c *client) createApp(t *testing.T, name string) string {
	t.Helper()
	body, status := c.do(t, http.MethodPost, "/apps", fmt.Sprintf(`{"name":%q}`, name))
	require.Equal(t, http.StatusAccepted, status, body)

	var app map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &app))

	appID := app["id"].(string)
	cleanupBundle(t, appID)
	return appID
}

// cleanupBundle removes the containers a deployed test app leaves running.
//
// Nothing in Pando does this, and that is a real gap rather than a test
// convenience: deleting an app archives the row and never tells the runtime to
// destroy the bundle. The adapter implements Destroy and no caller invokes it.
// Convergence — including tearing down what should no longer exist — is the
// reconciler's job, and the reconciler is phase 7.
//
// Containers only. The bundle's private network is deliberately left alone:
// Pando's own container is joined to every one of them, because that is how the
// proxy reaches an app at all (R-023), and force-disconnecting a running Pando
// from a bridge network disturbs its routing badly enough that its next
// outbound clone hangs for minutes. That was a real failure here, and it looked
// exactly like a slow network rather than like the test suite sabotaging the
// server. Networks are reclaimed at suite start instead, by pruneStaleBundles,
// when the Pando attached to them is already gone.
func cleanupBundle(t *testing.T, appID string) {
	t.Helper()
	t.Cleanup(func() {
		// Delete the app through the API, the way a person would.
		//
		// Removing only the containers left the app row behind, still counted
		// as running — and every app reserves CPU and memory, so a suite that
		// creates twenty apps and deletes none eventually hits R-242's capacity
		// check and fails with CAPACITY_WOULD_OVERSUBSCRIBE on a deploy that
		// has nothing wrong with it.
		//
		// That only started biting when hand-written specs began getting the
		// install's resource defaults. Before, every test app reserved nothing,
		// so the capacity check had nothing to count and the accumulation was
		// invisible.
		//
		// force=true because these are test apps: their data is not worth the
		// backup R-204 would otherwise take.
		deleteQuietly(appID)

		out, _ := exec.Command("docker", "ps", "-aq",
			"--filter", "label=io.pando.app="+appID).Output()
		for _, id := range strings.Fields(string(out)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	})
}

// cleanupClient is a signed-in client for teardown, built once per run.
//
// Deliberately free of testify and of *testing.T. Cleanup runs after the test
// has finished, and anything that calls t.FailNow() there — which every
// require.* does — panics and takes the whole test binary with it. That is not
// hypothetical: the first version of this called login(t) and the run stopped
// dead halfway through the suite, reporting twenty results out of forty-five
// and a failure in whichever test happened to be last.
//
// So this returns nil on any problem and says nothing. Cleanup must never be
// the thing that reports a failure: it runs after the assertion that matters
// has already had its say.
var (
	cleanupOnce   sync.Once
	cleanupCached *client
)

func cleanupClient() *client {
	cleanupOnce.Do(func() {
		password := adminPassword
		if password == "" {
			return
		}

		c := &client{http: &http.Client{Timeout: 30 * time.Second}}
		resp, err := c.http.Post(baseURL()+"/sessions", "application/json",
			strings.NewReader(fmt.Sprintf(`{"username":"admin","password":%q}`, password)))
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return
		}
		for _, ck := range resp.Cookies() {
			if ck.Name == "pando_session" {
				c.cookie = ck.Value
			}
		}
		if c.cookie != "" {
			cleanupCached = c
		}
	})
	return cleanupCached
}

// deleteQuietly removes an app without asserting anything.
func deleteQuietly(appID string) {
	c := cleanupClient()
	if c == nil {
		return
	}
	req, err := http.NewRequest(http.MethodDelete,
		baseURL()+"/apps/"+appID+"?force=true", nil)
	if err != nil {
		return
	}
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	resp, err := c.http.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}

// TestMain reclaims what previous runs left behind.
//
// Bundle networks are not Compose-managed, so `docker compose down -v` does not
// remove them and they accumulate one per deployed app across runs. Docker's
// default address pool holds about thirty; once it is full every deploy fails
// with a capacity error that has nothing to do with the test reporting it.
//
// Safe here in a way it is not mid-suite: the Pando that was attached to these
// networks belongs to a previous stack and is already gone.
func TestMain(m *testing.M) {
	pruneStaleBundles()
	captureAdminPassword()
	os.Exit(m.Run())
}

// adminPassword is the admin account's password, settled once before any test
// runs.
var adminPassword string

// suitePassword is what the suite sets a fresh stack's administrator up with.
// Fixed rather than random so a second run against the same stack can sign in
// without being told anything; the stack is a disposable local one.
const suitePassword = "pando-acceptance-suite-admin"

// captureAdminPassword settles the admin password at suite start.
//
// PANDO_TEST_PASSWORD wins, for a stack whose password somebody set. Otherwise
// a stack still waiting for its first administrator is set up here as "admin"
// with suitePassword, and a stack already set up is assumed to be one the suite
// set up on an earlier run. The setup token comes from `pando admin
// setup-token` in the stack's pando service (R-046, issue #130), not from the
// server log, which a recreated container no longer has.
func captureAdminPassword() {
	if adminPassword = os.Getenv("PANDO_TEST_PASSWORD"); adminPassword != "" {
		return
	}
	adminPassword = suitePassword

	c := &http.Client{Timeout: 30 * time.Second}
	resp, err := c.Get(baseURL() + "/setup")
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
	token, err := exec.Command("docker", "compose", "exec", "-T", "pando", "pando", "admin", "setup-token").Output()
	if err != nil {
		return
	}
	resp, err = c.Post(baseURL()+"/setup", "application/json",
		strings.NewReader(fmt.Sprintf(`{"setup_token":%q,"username":"admin","password":%q}`,
			strings.TrimSpace(string(token)), suitePassword)))
	if err == nil {
		_ = resp.Body.Close()
	}
}

// staleNetworkAge is how old an empty bundle network must be before the suite
// treats it as a previous run's leftover.
const staleNetworkAge = 10 * time.Minute

func pruneStaleBundles() {
	out, err := exec.Command("docker", "network", "ls", "-q",
		"--filter", "label=io.pando.managed").Output()
	if err != nil {
		return
	}

	for _, network := range strings.Fields(string(out)) {
		attached, _ := exec.Command("docker", "network", "inspect", network,
			"--format", "{{range .Containers}}{{.Name}} {{end}}").Output()

		// Only networks nothing is attached to.
		//
		// This used to force-disconnect whatever it found and then remove the
		// network — and what it found was the *running* Pando, which is joined
		// to every bundle network because that is how the proxy reaches an app.
		// The comment above cleanupBundle warns about precisely this, and this
		// function did it anyway, at the start of every run.
		//
		// The cost was not subtle once it bit: Pando kept serving inside its
		// container and stopped being reachable from the host, so the rest of
		// the suite failed as if the product were broken. It took a restart to
		// recover and a while to believe.
		//
		// Skipping an attached network loses nothing now. The GC tears down a
		// deleted app's bundle and reclaims its network (R-204), so the only
		// networks reaching here are orphans from a stack that has since been
		// recreated — and those have nothing attached, because the Pando that
		// was attached is gone.
		if len(strings.Fields(string(attached))) > 0 {
			continue
		}

		// And only ones from an earlier run. Integration packages run side by
		// side, and a bundle network is empty from the moment it is created
		// until its first container starts — which, when the image has to be
		// pulled first, is seconds. The runtime's own tests had their network
		// removed out from under them in that gap ("network … not found").
		created, err := exec.Command("docker", "network", "inspect", network,
			"--format", "{{.Created.Format \"2006-01-02T15:04:05Z07:00\"}}").Output()
		if err != nil {
			continue
		}
		at, err := time.Parse(time.RFC3339, strings.TrimSpace(string(created)))
		if err != nil || time.Since(at) < staleNetworkAge {
			continue
		}
		_ = exec.Command("docker", "network", "rm", network).Run()
	}
}

// putSpec writes a spec revision and returns its number.
//
// Use the number rather than counting: a deploy can write revisions of its
// own (an image source's digest is pinned as a new revision, issue #41), so
// "the second spec this test wrote" is not necessarily revision 2.
func (c *client) putSpec(t *testing.T, appID, spec string) int {
	t.Helper()
	body, status := c.do(t, http.MethodPost, fmt.Sprintf("/apps/%s/specs", appID), spec)
	require.Equal(t, http.StatusCreated, status, body)

	var rev struct {
		Revision int `json:"revision"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &rev))
	require.Positive(t, rev.Revision, body)
	return rev.Revision
}

func (c *client) pinSpec(t *testing.T, appID string, revision int) {
	t.Helper()
	body, status := c.do(t, http.MethodPost,
		fmt.Sprintf("/apps/%s/specs/%d/pin", appID, revision), "")
	require.Equal(t, http.StatusOK, status, body)
}

func (c *client) deploy(t *testing.T, appID string, revision int) map[string]any {
	t.Helper()
	payload := "{}"
	if revision > 0 {
		payload = fmt.Sprintf(`{"spec_revision":%d}`, revision)
	}
	body, status := c.do(t, http.MethodPost, fmt.Sprintf("/apps/%s/deployments", appID), payload)
	require.Equal(t, http.StatusAccepted, status, body)

	var dep map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &dep))
	return dep
}

// awaitDeployment polls until the deployment reaches a terminal status.
func (c *client) awaitDeployment(t *testing.T, appID, depID string, within time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)

	for {
		dep := c.get(t, fmt.Sprintf("/apps/%s/deployments/%s", appID, depID))
		switch dep["status"] {
		case "succeeded", "failed", "superseded":
			return dep
		}
		if time.Now().After(deadline) {
			t.Fatalf("deployment %s did not finish within %s (last status %v)\n%s",
				depID, within, dep["status"], c.deploymentLogs(t, appID, depID))
		}
		time.Sleep(2 * time.Second)
	}
}

// deploymentLogs reads the SSE stream to its end.
func (c *client) deploymentLogs(t *testing.T, appID, depID string) string {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet,
		fmt.Sprintf("%s/apps/%s/deployments/%s/logs", baseURL(), appID, depID), nil)
	require.NoError(t, err)
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})

	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
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

// prebuiltSpec is a spec that needs no build, so a test can assert on the
// pipeline rather than on a build.
func prebuiltSpec(port int) string {
	return fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "image", "image": "nginx:alpine"},
		"build": {"strategy": "prebuilt"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_docker", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, port)
}

// createUser adds a local user and returns its ID.
func (c *client) createUser(t *testing.T, username string) string {
	t.Helper()
	body, status := c.do(t, http.MethodPost, "/users",
		fmt.Sprintf(`{"username":%q,"password":"correct-horse-battery"}`, username))
	require.Equal(t, http.StatusCreated, status, body)

	var user map[string]any
	require.NoError(t, json.Unmarshal([]byte(body), &user))
	return user["id"].(string)
}

// asUser signs in as another account and returns a client for it.
func (c *client) asUser(t *testing.T, userID string) *client {
	t.Helper()

	username := c.get(t, "/users/"+userID)["external_id"].(string)
	other := &client{http: &http.Client{Timeout: 30 * time.Second}}

	resp, err := other.http.Post(baseURL()+"/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"correct-horse-battery"}`, username)))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	for _, ck := range resp.Cookies() {
		if ck.Name == "pando_session" {
			other.cookie = ck.Value
		}
	}
	require.NotEmpty(t, other.cookie)
	return other
}
