//go:build multihost

package multihost_test

// The cluster end to end (#347): joining, conversion in place, a warm
// standby with its volume shipped, the proxy from every control host, a host
// cut off and failed over, its return, no failover for a dead Pando on a live
// machine, and a removed host's certificates. The tests run in this order and
// share the cluster cluster_main_test.go builds; one that needs what an
// earlier one made skips when that one did not make it.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/hostagent"
)

// What the tests hand each other.
var (
	clJoined   bool   // every host joined and the runtime was converted
	clApp0     string // deployed before any host joined
	clFiller   string // on the second app host, no standby
	clApp      string // the counter app, with a volume and a standby
	clPlain    string // an app without volumes, with a standby, beside it
	clHome     *clHost
	clStandby  *clHost
	clShipped  bool // the counter app's volume reached its standby
	clCount    int  // the counter as last read before the cut
	clFailedTo *clHost

	clPlainStandby  *clHost
	clPlainFailedTo *clHost
)

// TestR415_HostsJoinWithATokenAndTheRuntimeBecomesDockerHostsInPlace joins a
// control-and-app host and an app-only host at setup with PANDO_JOIN_TOKEN
// (R-415). Each is listed, confirmed, with the token's roles; the install's
// own machine is the first host (R-414); and the first app-role join turned
// the `docker` runtime into `docker-hosts` in place, the app already running
// on the first host left as it was (R-418).
func TestR415_HostsJoinWithATokenAndTheRuntimeBecomesDockerHostsInPlace(t *testing.T) {
	c := requireCluster(t)

	// An app on the single host, before anything joins.
	clApp0 = c.deployApp(t, "mh-cl-app0-"+stamp(), counterApp(9101, "off", 64<<20, true))
	before, err := bundleOn(h1, clApp0, false)
	require.NoError(t, err)
	require.NotEmpty(t, before, "app0 runs on h1")
	require.Equal(t, "docker", mustPSQL(t, `SELECT kind FROM adapter_configs WHERE id = 'rt_docker'`))

	// h2: control and app (MH_CLUSTER_H2_ROLES may make it app only). Its
	// first start uses a database of its own, then the join hands it the
	// install's (R-415).
	require.NoError(t, startPando(h2, c.mint(t, h2Roles()...)))
	eventually(t, 4*time.Minute, "h2 is listed", func() bool { _, ok := c.clusterHosts(t)["h2"]; return ok })
	if h2.control {
		// Its own Pando is still there once it has restarted into the
		// cluster: a joined host's container is named pando-agent, the name
		// the runtime gives the agents it runs itself.
		time.Sleep(10 * time.Second)
		_, err := inner(h2.container, "inspect", "-f", "{{.State.Running}}", h2.pando)
		require.NoError(t, err, "h2's own Pando container was removed after it joined with the control role")
		require.NoError(t, awaitReady(base2, 3*time.Minute), "h2 serves the API once it has joined")
	}

	// h3: app only. It never receives the database URL.
	require.NoError(t, startPando(h3, c.mint(t, "app")))
	eventually(t, 4*time.Minute, "h3 is listed", func() bool { _, ok := c.clusterHosts(t)["h3"]; return ok })

	list := c.clusterHosts(t)
	require.Len(t, list, 3, "%+v", list)
	require.Empty(t, list["h1"].JoinedWith, "the install's own machine is the first host")
	require.ElementsMatch(t, []string{"control", "app"}, list["h1"].Roles)
	require.NotEmpty(t, list["h2"].JoinedWith)
	require.ElementsMatch(t, h2Roles(), list["h2"].Roles)
	require.NotEmpty(t, list["h3"].JoinedWith)
	require.Equal(t, []string{"app"}, list["h3"].Roles)
	require.Equal(t, "0", mustPSQL(t, `SELECT count(*) FROM cluster_hosts WHERE confirmed_at IS NULL`),
		"no host is left pending")
	out, err := inner(h3.container, "exec", h3.pando, "ls", "/var/lib/pando/cluster")
	require.NoError(t, err)
	require.NotContains(t, out, "database-url", "an app host is never handed the database URL")

	// R-418: the same adapter row, now docker-hosts, listing all three.
	eventually(t, 2*time.Minute, "the runtime lists every app host", func() bool {
		row := mustPSQL(t, `SELECT kind || ' ' || config::text FROM adapter_configs WHERE id = 'rt_docker'`)
		return strings.HasPrefix(row, "docker-hosts ") && strings.Contains(row, `"agent://`+h2.ip) &&
			strings.Contains(row, `"agent://`+h3.ip)
	})
	require.Equal(t, "1", mustPSQL(t, `SELECT count(*) FROM audit_events WHERE action = 'cluster.runtime.convert'`))
	eventually(t, 3*time.Minute, "the converted runtime reaches every host", func() bool {
		v := c.try(t, http.MethodGet, "/capacity", "", http.StatusOK)
		raw, _ := json.Marshal(v)
		var body struct {
			Runtimes []struct {
				Ref     string `json:"adapter_ref"`
				Details struct {
					Hosts []hostRow `json:"hosts"`
				} `json:"details"`
			} `json:"runtimes"`
		}
		_ = json.Unmarshal(raw, &body)
		reachable := 0
		for _, r := range body.Runtimes {
			for _, h := range r.Details.Hosts {
				if r.Ref == "rt_docker" && h.Reachable {
					reachable++
				}
			}
		}
		return reachable == 3
	})
	after, err := bundleOn(h1, clApp0, false)
	require.NoError(t, err)
	require.Equal(t, before, after, "app0's containers were not re-created by the conversion")
	at := c.appVia(t, clApp0, h1)
	eventually(t, 2*time.Minute, "app0 still answers", func() bool {
		status, _, err := c.fetch(at+"/cgi-bin/count", nil)
		return err == nil && status == http.StatusOK
	})
	clJoined = true
}

// TestR419_AnAppWithAVolumeGetsAStoppedStandbyOnAnotherHostAndItsVolumeShips
// deploys the counter app with its standby on, and an app without volumes
// beside it: each runs on one host, its standby is created and stopped on
// another (R-419), and the counter app's volume is shipped there (R-420).
func TestR419_AnAppWithAVolumeGetsAStoppedStandbyOnAnotherHostAndItsVolumeShips(t *testing.T) {
	c := requireCluster(t)
	if !clJoined {
		t.Skip("the hosts did not join")
	}
	// A filler with no standby takes the next host, so the apps land on the
	// one left and not on the install's own machine: cutting that one off
	// would take the API the test reads with it.
	clFiller = c.deployApp(t, "mh-cl-filler-"+stamp(), counterApp(9102, "off", 64<<20, true))
	filler := runningHosts(clFiller)
	require.Len(t, filler, 1)
	clApp = c.deployApp(t, "mh-cl-app-"+stamp(), counterApp(9103, "on", 32<<20, true))
	on := runningHosts(clApp)
	require.Len(t, on, 1, "the app runs on exactly one host")
	clHome = hostByName(on[0])
	clPlain = c.deployApp(t, "mh-cl-plain-"+stamp(), counterApp(9104, "on", 16<<20, false))
	require.Equal(t, []string{clHome.name}, runningHosts(clPlain), "both apps on the roomiest host")
	t.Logf("filler on %v, apps on %s", filler, clHome.name)
	require.NotEqual(t, "h1", clHome.name, "the apps should land on a joined host (placement: most free memory)")

	// Their standbys: made by the leader's standby pass, within a minute or
	// two.
	clStandby = awaitStandby(t, clApp, clHome)
	clPlainStandby = awaitStandby(t, clPlain, clHome)
	t.Logf("standbys on %s (counter) and %s (plain)", clStandby.name, clPlainStandby.name)

	// Count into the volume, then wait for a ship that started after the
	// last count, so the copy holds it.
	at := c.appVia(t, clApp, h1)
	for range 5 {
		var status int
		var body string
		eventually(t, time.Minute, "the app counts", func() bool {
			var err error
			status, body, err = c.fetch(at+"/cgi-bin/count", nil)
			return err == nil && status == http.StatusOK
		})
		n, boots, err := counter(body)
		require.NoError(t, err)
		require.Equal(t, 1, boots, body)
		clCount = n
	}
	require.Equal(t, 5, clCount)
	counted := mustPSQL(t, `SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US')`)
	// The first ship is due as soon as the standby exists, the next after
	// clShipEvery: up to one interval and a pass, and the copy's time.
	deadline := time.Now().Add(clShipEvery + clStandbyEvery + 2*time.Minute)
	for time.Now().Before(deadline) {
		out := mustPSQL(t, fmt.Sprintf(`SELECT copies FROM app_standbys WHERE app_id = '%s' AND shipped_at > '%s'`, clApp, counted))
		if out != "" && out != "0" {
			clShipped = true
			break
		}
		time.Sleep(5 * time.Second)
	}
	if !clShipped {
		// Not fatal: the failover tests go on with the app without volumes,
		// and check the counter app is not started from no copy (R-420).
		t.Errorf("no ship of the counter app's volume reached its standby in time; the standby's last error: %q",
			mustPSQL(t, fmt.Sprintf(`SELECT last_error FROM app_standbys WHERE app_id = '%s'`, clApp)))
		return
	}
	vols, err := inner(clStandby.container, "volume", "ls", "-q", "--filter", "label=io.pando.standby="+clApp)
	require.NoError(t, err)
	require.NotEmpty(t, strings.TrimSpace(vols), "the copy is a staging volume on the standby's host")
}

// awaitStandby waits for app's standby row to name a host other than home,
// and requires its containers there to be created and never started.
func awaitStandby(t *testing.T, app string, home *clHost) *clHost {
	t.Helper()
	var row string
	eventually(t, 4*time.Minute, "the app has a standby on another host", func() bool {
		row = mustPSQL(t, fmt.Sprintf(`SELECT host || '|' || home_host || '|' || last_error FROM app_standbys WHERE app_id = '%s'`, app))
		parts := strings.SplitN(row, "|", 3)
		return len(parts) == 3 && parts[0] != "" && parts[0] != home.name
	})
	parts := strings.SplitN(row, "|", 3)
	sb := hostByName(parts[0])
	require.NotNil(t, sb, row)
	require.Equal(t, home.name, parts[1], "the home recorded is where it runs")
	ids := standbyOn(sb, app)
	require.NotEmpty(t, ids, "the standby's containers exist on %s", sb.name)
	for _, id := range ids {
		state, err := inner(sb.container, "inspect", "-f", "{{.State.Status}}", id)
		require.NoError(t, err)
		require.Equal(t, "created", state, "a standby is created and never started")
	}
	return sb
}

// TestR023_EveryControlHostReachesTheAppOnlyThroughItsProxy reaches the app
// through the first host's proxy and through the joined control host's:
// forged X-Pando-* headers do not arrive, Pando's cookies are not forwarded,
// an unsigned request is sent to sign in (R-023, R-053, R-173).
func TestR023_EveryControlHostReachesTheAppOnlyThroughItsProxy(t *testing.T) {
	c := requireCluster(t)
	if clApp == "" {
		t.Skip("the app was not deployed")
	}
	for _, via := range clHosts {
		if !via.control {
			continue
		}
		t.Run(via.name, func(t *testing.T) {
			at := c.appVia(t, clApp, via)
			var status int
			var body string
			eventually(t, 2*time.Minute, "the app answers through "+via.name, func() bool {
				var err error
				status, body, err = c.fetch(at+"/cgi-bin/env", map[string]string{
					"X-Pando-User":          "admin@corp.com",
					"X-Pando-Email":         "admin@corp.com",
					"X-Pando-Assertion":     "forged.assertion.value",
					"X-Pando-Future-Header": "whatever",
				}, &http.Cookie{Name: "pando_csrf", Value: "secret-csrf"}, &http.Cookie{Name: "app_pref", Value: "kept"})
				return err == nil && status == http.StatusOK
			})
			env := map[string]string{}
			for _, line := range strings.Split(body, "\n") {
				if k, v, ok := strings.Cut(line, "="); ok {
					env[k] = v
				}
			}
			require.NotEqual(t, "admin@corp.com", env["HTTP_X_PANDO_USER"], body)
			require.NotEqual(t, "admin@corp.com", env["HTTP_X_PANDO_EMAIL"], body)
			require.NotEqual(t, "forged.assertion.value", env["HTTP_X_PANDO_ASSERTION"], body)
			require.Empty(t, env["HTTP_X_PANDO_FUTURE_HEADER"], body)
			require.Len(t, strings.Split(env["HTTP_X_PANDO_ASSERTION"], "."), 3, "a real assertion: %s", body)
			cookie := env["HTTP_COOKIE"]
			require.NotContains(t, cookie, "pando_session")
			require.NotContains(t, cookie, "pando_csrf")
			require.Contains(t, cookie, "app_pref=kept")

			var anon *client
			status, _, err := anon.fetch(at+"/cgi-bin/env", nil)
			require.NoError(t, err)
			require.Equal(t, http.StatusFound, status, "unsigned, the proxy sends to sign in before reaching the host")
		})
	}
}

// TestR421_ACutOffHostStopsItsAppsAndOnlyThenItsStandbyStarts cuts the apps'
// host off the network, its machine and Pando left running. Within T the cut
// host stops its own apps (it reached fewer than half the cluster); only
// after about 3T does the leader take it for lost and start the standbys; at
// no moment do two hosts run an app (R-153, R-421). The counter app then
// answers through the proxy with the data the last ship carried (R-420); one
// whose volume never shipped is not started from nothing.
func TestR421_ACutOffHostStopsItsAppsAndOnlyThenItsStandbyStarts(t *testing.T) {
	c := requireCluster(t)
	if clPlainStandby == nil {
		t.Skip("the apps have no standby")
	}
	counterWatch, plainWatch := watchCopies(clApp), watchCopies(clPlain)
	defer counterWatch.Stop()
	defer plainWatch.Stop()
	time.Sleep(3 * time.Second)
	require.Equal(t, []string{clHome.name}, runningHosts(clApp))
	require.Equal(t, []string{clHome.name}, runningHosts(clPlain))

	_, err := docker("network", "disconnect", clNet, clHome.container)
	require.NoError(t, err)
	cut := time.Now()
	t.Logf("cut %s off at %s", clHome.name, cut.Format(time.RFC3339))
	// Healed by the next test; healed here too if this one stops early.
	defer func() {
		if t.Failed() {
			_, _ = docker("network", "connect", "--ip", clHome.ip, clNet, clHome.container)
		}
	}()

	eventually(t, clT+30*time.Second, "the cut host stops its apps", func() bool {
		a, err1 := bundleOn(clHome, clApp, false)
		b, err2 := bundleOn(clHome, clPlain, false)
		return err1 == nil && err2 == nil && len(a)+len(b) == 0
	})
	fenced := time.Now()
	t.Logf("fenced after %s", fenced.Sub(cut).Round(time.Second))
	require.LessOrEqual(t, fenced.Sub(cut), clT+20*time.Second, "fenced within T")
	all, err := bundleOn(clHome, clPlain, true)
	require.NoError(t, err)
	require.NotEmpty(t, all, "stopped, not removed: its copy stays until the host returns")

	eventually(t, clLostAfter+3*time.Minute, "the plain app's standby starts", func() bool {
		ids, err := bundleOn(clPlainStandby, clPlain, false)
		return err == nil && len(ids) > 0
	})
	promoted := plainWatch.seenAfter(clPlainStandby.name, cut)
	if promoted.IsZero() {
		promoted = time.Now()
	}
	t.Logf("standby started on %s after %s", clPlainStandby.name, promoted.Sub(cut).Round(time.Second))
	require.GreaterOrEqual(t, promoted.Sub(cut), clLostAfter-10*time.Second, "not before 3T")
	require.True(t, promoted.After(plainWatch.lastSeen(clHome.name)), "the cut host had stopped before the standby started")
	require.Equal(t, "t", mustPSQL(t, fmt.Sprintf(`SELECT lost_at IS NOT NULL FROM cluster_hosts WHERE name = '%s'`, clHome.name)),
		"the leader marked the host lost")
	require.Equal(t, "1", mustPSQL(t, fmt.Sprintf(
		`SELECT count(*) FROM audit_events WHERE action = 'cluster.failover' AND (target_id = '%[1]s' OR app_id = '%[1]s')`, clPlain)),
		"the failover was audited before the standby started")
	clPlainFailedTo = clPlainStandby
	at := c.appVia(t, clPlain, h1)
	eventually(t, 4*time.Minute, "the plain app answers again through the proxy", func() bool {
		status, _, err := c.fetch(at+"/cgi-bin/count", nil)
		return err == nil && status == http.StatusOK
	})

	if !clShipped {
		// R-420: a standby with no copy of a volume is refused, not started
		// empty.
		time.Sleep(30 * time.Second)
		require.Empty(t, runningHosts(clApp), "the counter app was started without a copy of its volume")
		t.Logf("the counter app's standby, with no copy, was not started (its row: %s)",
			mustPSQL(t, fmt.Sprintf(`SELECT host || ' ' || home_host || ' ' || last_error FROM app_standbys WHERE app_id = '%s'`, clApp)))
	} else {
		eventually(t, 4*time.Minute, "the counter app's standby starts", func() bool {
			ids, err := bundleOn(clStandby, clApp, false)
			return err == nil && len(ids) > 0
		})
		clFailedTo = clStandby
		at := c.appVia(t, clApp, h1)
		var body string
		eventually(t, 4*time.Minute, "the counter app answers again through the proxy", func() bool {
			status, b, err := c.fetch(at+"/cgi-bin/count", nil)
			body = b
			return err == nil && status == http.StatusOK
		})
		n, boots, err := counter(body)
		require.NoError(t, err)
		require.Equal(t, clCount+1, n, "the count carried over in the shipped copy: %s", body)
		require.Equal(t, 2, boots, "started once before and once from the copy: %s", body)
		clCount = n
		require.Empty(t, counterWatch.overlaps(), "two hosts ran the counter app at once")
	}
	require.Empty(t, plainWatch.overlaps(), "two hosts ran the plain app at once")
}

// TestR419_AReturningHostsStaleCopyIsRemovedAndNeverRestarted heals the
// network. The host that was cut off comes back: its stopped copy of a
// failed-over app is removed, never started (R-148 does not apply to a copy
// failed over), its lost mark is cleared, and the app gets a standby again.
func TestR419_AReturningHostsStaleCopyIsRemovedAndNeverRestarted(t *testing.T) {
	c := requireCluster(t)
	if clPlainFailedTo == nil {
		t.Skip("nothing failed over")
	}
	returning := clHome
	plainWatch := watchCopies(clPlain)
	defer plainWatch.Stop()
	_, err := docker("network", "connect", "--ip", returning.ip, clNet, returning.container)
	require.NoError(t, err)
	healed := time.Now()

	eventually(t, 4*time.Minute, "the returned host's stale copy is removed", func() bool {
		ids, err := bundleOn(returning, clPlain, true)
		return err == nil && len(ids) == 0
	})
	eventually(t, 2*time.Minute, "the lost mark is cleared", func() bool {
		return mustPSQL(t, fmt.Sprintf(`SELECT lost_at IS NULL FROM cluster_hosts WHERE name = '%s'`, returning.name)) == "t"
	})
	require.Equal(t, "1", mustPSQL(t,
		`SELECT count(*) FROM audit_events WHERE action = 'cluster.host.returned' AND occurred_at > now() - interval '10 minutes'`))
	var row string
	eventually(t, 4*time.Minute, "the plain app has a standby again", func() bool {
		row = mustPSQL(t, fmt.Sprintf(`SELECT host || '|' || home_host || '|' || stale_host FROM app_standbys WHERE app_id = '%s'`, clPlain))
		p := strings.Split(row, "|")
		return len(p) == 3 && p[0] != "" && p[0] != clPlainFailedTo.name && p[2] == "" &&
			len(standbyOn(hostByName(p[0]), clPlain)) > 0
	})
	p := strings.Split(row, "|")
	require.Equal(t, clPlainFailedTo.name, p[1], "the app's home is where it failed over to")
	if p[0] != returning.name {
		// notes-cluster.md, "After a failover": the returned host "becomes
		// the target for the app's next standby".
		t.Errorf("the new standby went to %s, not to the host that returned (%s)", p[0], returning.name)
	}
	t.Logf("standby re-placed on %s %s after the heal", p[0], time.Since(healed).Round(time.Second))
	require.True(t, plainWatch.lastSeen(returning.name).IsZero(), "the returned host ran its stale copy again")
	require.Empty(t, plainWatch.overlaps(), "two hosts ran the app at once")

	if clFailedTo == nil {
		// The counter app was never failed over (no copy): it is still homed
		// on the returning host, which the reconciler starts again (R-148).
		eventually(t, 4*time.Minute, "the counter app runs again on its own host", func() bool {
			return slices.Equal(runningHosts(clApp), []string{returning.name})
		})
	} else {
		require.True(t, w0(clApp, returning), "the counter app's stale copy is removed")
	}
	status, body, err := c.fetch(c.appVia(t, clPlain, h1)+"/cgi-bin/count", nil)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status, body)
}

// w0 reports whether the host holds no container of the app's own bundle.
func w0(app string, h *clHost) bool {
	ids, err := bundleOn(h, app, true)
	return err == nil && len(ids) == 0
}

// TestR421_ADeadPandoOnALiveMachineIsNeverTakenForLost stops Pando on both
// joined hosts, their machines left up, for longer than 3T: a refused
// connection is contact, so nothing is marked lost, nothing fails over, and
// the apps on those machines keep running (R-421).
func TestR421_ADeadPandoOnALiveMachineIsNeverTakenForLost(t *testing.T) {
	c := requireCluster(t)
	if clPlain == "" {
		t.Skip("the apps were not deployed")
	}
	apps := []string{clApp0, clFiller, clApp, clPlain}
	where := map[string][]string{}
	homes := map[string]string{}
	for _, app := range apps {
		where[app] = runningHosts(app)
		homes[app] = mustPSQL(t, fmt.Sprintf(`SELECT home_host FROM app_standbys WHERE app_id = '%s'`, app))
	}
	t.Logf("before: %v", where)
	require.Equal(t, "0", mustPSQL(t, `SELECT count(*) FROM cluster_hosts WHERE lost_at IS NOT NULL`))
	for _, h := range []*clHost{h2, h3} {
		_, err := inner(h.container, "stop", h.pando)
		require.NoError(t, err)
	}
	stopped := time.Now()
	defer func() {
		for _, h := range []*clHost{h2, h3} {
			_, _ = inner(h.container, "start", h.pando)
		}
	}()
	for time.Since(stopped) < clLostAfter+30*time.Second {
		require.Equal(t, "0", mustPSQL(t, `SELECT count(*) FROM cluster_hosts WHERE lost_at IS NOT NULL`),
			"a host was taken for lost while its machine answered")
		for _, app := range apps {
			require.Equal(t, where[app], runningHosts(app), "app %s kept running where it was", app)
		}
		time.Sleep(5 * time.Second)
	}
	for _, app := range apps {
		require.Equal(t, homes[app], mustPSQL(t, fmt.Sprintf(`SELECT home_host FROM app_standbys WHERE app_id = '%s'`, app)),
			"nothing failed over")
	}
	for _, h := range []*clHost{h2, h3} {
		_, err := inner(h.container, "start", h.pando)
		require.NoError(t, err)
	}
	if h2.control {
		require.NoError(t, awaitReady(base2, 3*time.Minute))
	}
	eventually(t, 2*time.Minute, "the app still answers", func() bool {
		status, _, err := c.fetch(c.appVia(t, clPlain, h1)+"/cgi-bin/count", nil)
		return err == nil && status == http.StatusOK
	})
}

// TestR415_ARemovedHostsCertificatesAreRefused removes the app-only host
// through the API. Before, its certificate reads the peer list and its probe
// certificate is answered; after, both are refused (R-415), and the runtime
// no longer lists it.
func TestR415_ARemovedHostsCertificatesAreRefused(t *testing.T) {
	c := requireCluster(t)
	host, ok := c.clusterHosts(t)["h3"]
	if !ok {
		t.Skip("h3 did not join")
	}
	own := tlsFrom(t, h3, "host.crt", "host.key")
	probe := tlsFrom(t, h3, "host-client.crt", "host-client.key")
	listener := listenerOf(t, h1)

	peers := func() (int, string) {
		hc := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{
			Certificates: []tls.Certificate{own}, InsecureSkipVerify: true, //nolint:gosec // the test reads the answer, not the server
		}}}
		resp, err := hc.Get("https://" + listener + "/cluster/peers")
		require.NoError(t, err)
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	ping := func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, err := hostagent.Ping(ctx, listener, &tls.Config{
			Certificates: []tls.Certificate{probe}, InsecureSkipVerify: true, //nolint:gosec // likewise
		}, host.ID)
		return err
	}

	status, body := peers()
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, host.ID)
	require.NoError(t, ping(), "h3's probe certificate is answered while it is a member")

	c.try(t, http.MethodDelete, "/cluster/hosts/"+host.ID, "", http.StatusOK)
	_, still := c.clusterHosts(t)["h3"]
	require.False(t, still)

	status, body = peers()
	require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, status, "a removed host reads no peer list: %s", body)
	eventually(t, time.Minute, "h1 refuses the removed host's pings", func() bool {
		err := ping()
		return err != nil && strings.Contains(err.Error(), "refused")
	})
	eventually(t, 2*time.Minute, "the runtime no longer lists h3", func() bool {
		row := mustPSQL(t, `SELECT config::text FROM adapter_configs WHERE id = 'rt_docker'`)
		return !strings.Contains(row, "agent://"+h3.ip) && !slices.Contains(strings.Fields(row), `"h3"`)
	})
}
