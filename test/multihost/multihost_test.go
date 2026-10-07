//go:build multihost

package multihost_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/hostagent"
)

// TestR243_CapacityIsSummedOverTheHostsWithTheLargestFit: the runtime reports
// both app hosts as reachable, sums the hosts open to new apps, and keeps each
// host's free room apart — an app that fits in the sum but in no one host is
// refused when its deploy is planned, rather than placed (R-243, R-242).
func TestR243_CapacityIsSummedOverTheHostsWithTheLargestFit(t *testing.T) {
	c := login(t)
	cap := c.capacity(t)

	a, b, ctl := cap.Hosts["host-a"], cap.Hosts["host-b"], cap.Hosts["control"]
	require.True(t, a.Reachable, "%+v", a)
	require.True(t, b.Reachable, "%+v", b)
	require.True(t, ctl.Reachable, "%+v", ctl)
	require.False(t, ctl.Open, "the control host is closed to new apps in this topology")
	require.Equal(t, int64(hostAMemory), a.TotalMem)
	require.Equal(t, int64(hostBMemory), b.TotalMem)
	require.Equal(t, int64(hostAMemory+hostBMemory), cap.TotalMem, "the hosts open to new apps, summed")
	require.Equal(t, int64(2*hostCPU), cap.TotalCPU)
	require.Positive(t, a.FreeMem)
	require.Positive(t, b.FreeMem)

	// More than either host has free, less than both together: refused at
	// plan time, by the largest fit, and placed nowhere.
	largest := max(a.FreeMem, b.FreeMem)
	ask := largest + 256<<20
	require.Less(t, ask, a.FreeMem+b.FreeMem)
	app := c.createApp(t, "mh-too-big-"+stamp())
	c.putSpec(t, app, echoSpec(int(nextPort.Add(1)), ask, ""))
	dep, status, out := c.startDeploy(t, app)
	if status == http.StatusAccepted {
		final := c.awaitDeployment(t, app, dep["id"].(string), 3*time.Minute)
		require.Equal(t, "failed", final["status"], "an app no one host has room for must not deploy")
		out = fmt.Sprint(final["error_detail"]) + c.deploymentLogs(app, dep["id"].(string))
	} else {
		require.GreaterOrEqual(t, status, 400, out)
	}
	t.Logf("refusal: %s", out)
	require.Contains(t, strings.ToLower(out), "room", "the refusal says no single place has room: %s", out)
	where, err := hostOf(app)
	require.NoError(t, err)
	require.Empty(t, where, "nothing of the refused app is on any host")
}

// TestR256_ANewAppGoesToTheHostWithTheMostFreeMemory: a new app goes to the
// host with the most free memory, and once it has taken enough that the
// other host is roomier, the next new app goes there.
func TestR256_ANewAppGoesToTheHostWithTheMostFreeMemory(t *testing.T) {
	c := login(t)
	cap := c.capacity(t)
	want, other := cap.roomiest()

	// Enough that afterwards the other host has more free memory.
	fill := cap.Hosts[want].FreeMem - cap.Hosts[other].FreeMem + 128<<20
	first := c.deployEcho(t, "mh-place-1", fill)
	requireOn(t, first, want)

	after := c.capacity(t)
	require.Less(t, after.Hosts[want].FreeMem, after.Hosts[other].FreeMem, "%+v", after.Hosts)
	second := c.deployEcho(t, "mh-place-2", 128<<20)
	requireOn(t, second, other)
}

// TestR023_AnAppOnAnotherHostIsReachedOnlyThroughTheProxy is Sequence C for
// an app on host B, which Pando's container shares no network with: the
// request passes the proxy's decision, crosses to host B's agent, and
// arrives with forged X-Pando-* headers replaced, Pando's cookies removed and
// a real assertion (R-053, R-173, R-054). A direct connection to the app's
// container, from outside host B, fails.
func TestR023_AnAppOnAnotherHostIsReachedOnlyThroughTheProxy(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-b")
	app := c.deployEcho(t, "mh-proxy", 128<<20)
	requireOn(t, app, "host-b")
	slug := c.slug(t, app)

	var status int
	var body string
	eventually(t, time.Minute, "the app answers through the proxy", func() bool {
		status, body = c.throughProxy(t, slug, "/", map[string]string{
			"X-Pando-User":          "admin@corp.com",
			"X-Pando-Email":         "admin@corp.com",
			"X-Pando-Assertion":     "forged.assertion.value",
			"X-Pando-Future-Header": "whatever",
		}, &http.Cookie{Name: "pando_csrf", Value: "secret-csrf"}, &http.Cookie{Name: "app_pref", Value: "kept"})
		return status == http.StatusOK
	})
	var echoed struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &echoed), body)
	headers := map[string]string{}
	for k, v := range echoed.Headers {
		headers[strings.ToLower(k)] = v
	}

	// R-053: forged values replaced, unknown ones removed.
	require.NotEqual(t, "admin@corp.com", headers["x-pando-user"])
	require.NotEqual(t, "admin@corp.com", headers["x-pando-email"])
	require.NotEqual(t, "forged.assertion.value", headers["x-pando-assertion"])
	require.Empty(t, headers["x-pando-future-header"])

	// R-054: a real assertion, for this app.
	token := headers["x-pando-assertion"]
	parts := strings.Split(token, ".")
	require.Len(t, parts, 3, "an assertion is a JWT: %q", token)

	// R-173: no pando_* cookie reaches the app; the app's own does.
	cookie := headers["cookie"]
	require.NotContains(t, cookie, "pando_session")
	require.NotContains(t, cookie, "pando_csrf")
	require.NotContains(t, cookie, "secret-csrf")
	require.Contains(t, cookie, "app_pref=kept")

	// Without the session, the same path is a redirect to sign in: the
	// decision happens before anything is sent to host B.
	anon := &http.Client{Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := anon.Get(base + "/" + slug + "/")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusFound, resp.StatusCode)

	// A direct connection from outside host B, even one routed into host B's
	// app range, does not reach the container.
	ip := containerIP(t, cHostB, "pando-"+app+"-web")
	out, err := probe(fmt.Sprintf(
		"ip route add 10.213.0.0/16 via %s && echo ROUTED && "+
			"(nc -z -w 4 %s 7443 && echo AGENT-ANSWERS) && "+
			"(wget -T 4 -qO- http://%s:8080/ >/dev/null 2>&1 && echo APP-REACHED || echo APP-BLOCKED) && "+
			"(wget -T 4 -qO- http://%s:8080/ >/dev/null 2>&1 && echo PORT-PUBLISHED || echo PORT-CLOSED)",
		ipHostB, ipHostB, ip, ipHostB))
	require.NoError(t, err, out)
	// The probe could change its routes and reach host B at all ...
	require.Contains(t, out, "ROUTED")
	require.Contains(t, out, "AGENT-ANSWERS")
	// ... and neither the container's address, routed into host B, nor the
	// app's port on host B's own address answers.
	require.Contains(t, out, "APP-BLOCKED", "a direct connection to the app's container reached it")
	require.Contains(t, out, "PORT-CLOSED", "the app's port is published on host B")
}

// probe runs a shell command in a throwaway container on mh-test-net, which
// may change its own routes and reaches nothing but that network.
func probe(script string) (string, error) {
	return docker("run", "--rm", "--name", "mh-test-probe-"+stamp(), "--label", "mh-test=1",
		"--network", netName, "--cap-add", "NET_ADMIN", "busybox:1.37", "sh", "-c", script)
}

func containerIP(t *testing.T, host, name string) string {
	t.Helper()
	out, err := inner(host, "inspect", "-f", "{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}", name)
	require.NoError(t, err)
	for _, ip := range strings.Fields(out) {
		if strings.HasPrefix(ip, "10.213.") {
			return ip
		}
	}
	t.Fatalf("%s on %s has no app network address: %q", name, host, out)
	return ""
}

// TestR026_AnAppHostPublishesOnlyTheAgentsPort: with an app on each host,
// the only container with a published port on either app host is the agent,
// and its only published port is the agent port.
func TestR026_AnAppHostPublishesOnlyTheAgentsPort(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-a")
	onA := c.deployEcho(t, "mh-ports-a", 128<<20)
	requireOn(t, onA, "host-a")
	c.steerTo(t, "host-b")
	onB := c.deployEcho(t, "mh-ports-b", 128<<20)
	requireOn(t, onB, "host-b")

	for _, h := range []string{cHostA, cHostB} {
		ids, err := inner(h, "ps", "-q")
		require.NoError(t, err)
		require.NotEmpty(t, ids)
		published := map[string]string{}
		for _, id := range strings.Fields(ids) {
			out, err := inner(h, "inspect", "-f", "{{.Name}} {{json .NetworkSettings.Ports}}", id)
			require.NoError(t, err)
			name, ports, _ := strings.Cut(out, " ")
			var bindings map[string][]struct{ HostIP, HostPort string }
			require.NoError(t, json.Unmarshal([]byte(ports), &bindings), out)
			for port, b := range bindings {
				if len(b) > 0 {
					published[strings.TrimPrefix(name, "/")] += port + " "
				}
			}
		}
		require.Equal(t, map[string]string{"pando-agent": "7443/tcp "}, published, "published ports on %s", h)
	}
}

// TestR023_AHostAgentRefusesAClientWithoutPandosCertificate: real TLS
// handshakes against host B's agent. Without a certificate, with another
// authority's, or in plain text, nothing is forwarded; with Pando's, a target
// outside the agent's app networks is refused, and an app's container is
// reached — which is why that certificate is held only by Pando's replicas.
func TestR023_AHostAgentRefusesAClientWithoutPandosCertificate(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-b")
	app := c.deployEcho(t, "mh-agent", 128<<20)
	requireOn(t, app, "host-b")
	target := "pando-" + app + "-web"
	addr := "127.0.0.1:" + agentPorts["host-b"]

	ours, err := hostagent.ParseAuthorities(authorityPEM)
	require.NoError(t, err)
	pandoCert, err := ours.IssueClient()
	require.NoError(t, err)
	otherPEM, err := hostagent.NewAuthority()
	require.NoError(t, err)
	others, err := hostagent.ParseAuthorities(otherPEM)
	require.NoError(t, err)
	otherCert, err := others.IssueClient()
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// No client certificate.
	noCert := hostagent.ClientTLS(ours, tls.Certificate{}, "host-b")
	noCert.Certificates = nil
	_, err = hostagent.Dial(ctx, addr, noCert, target, 8080)
	require.Error(t, err, "an agent forwarded for a client with no certificate")

	// Another authority's client certificate.
	_, err = hostagent.Dial(ctx, addr, hostagent.ClientTLS(ours, otherCert, "host-b"), target, 8080)
	require.Error(t, err, "an agent forwarded for another authority's certificate")

	// Plain text.
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	require.NoError(t, err)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn, "PANDO-AGENT/1 %s 8080\nGET / HTTP/1.0\r\n\r\n", target)
	reply, _ := io.ReadAll(conn)
	_ = conn.Close()
	require.NotContains(t, string(reply), "OK", "an agent forwarded a plain-text connection")
	require.NotContains(t, string(reply), "HTTP/", "an agent forwarded a plain-text connection")

	// Pando's certificate, a target outside the app networks: the agent
	// itself, which resolves to its own network's address.
	_, err = hostagent.Dial(ctx, addr, hostagent.ClientTLS(ours, pandoCert, "host-b"), "pando-agent", 7443)
	require.ErrorContains(t, err, "refused")

	// A container on host B that is on no app network.
	_, err = inner(cHostB, "run", "-d", "--name", "pando-mh-test-outsider", "--label", "mh-test=1",
		"-e", "HTTP_PORT=8080", echoRemote)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = inner(cHostB, "rm", "-f", "pando-mh-test-outsider") })
	_, err = hostagent.Dial(ctx, addr, hostagent.ClientTLS(ours, pandoCert, "host-b"), "pando-mh-test-outsider", 8080)
	require.ErrorContains(t, err, "refused")

	// And Pando's certificate to the app: carried.
	conn, err = hostagent.Dial(ctx, addr, hostagent.ClientTLS(ours, pandoCert, "host-b"), target, 8080)
	require.NoError(t, err)
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn, "GET /direct HTTP/1.0\r\nHost: x\r\n\r\n")
	reply, _ = io.ReadAll(conn)
	_ = conn.Close()
	require.Contains(t, string(reply), "200 OK")
}

// TestR010_AnAppStaysOnItsHostAndAStoppedHostsAppsAreUnobservable: a
// redeploy stays where the app runs. When host B stops answering, its apps
// are shown as unreachable and never as failed (R-151); a first deploy of a
// new app still lands on host A; neither a redeploy nor the reconciler puts a
// copy of host B's app on host A (O-46). When host B returns, its app is
// observed there again and started (R-148).
func TestR010_AnAppStaysOnItsHostAndAStoppedHostsAppsAreUnobservable(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-b")
	app := c.createApp(t, "mh-sticky-"+stamp())
	port := int(nextPort.Add(1))
	c.deploy(t, app, echoSpec(port, 128<<20, ""))
	requireOn(t, app, "host-b")

	// Make host A the roomier, then redeploy: still host B.
	c.steerTo(t, "host-a")
	c.deploy(t, app, echoSpec(port, 128<<20, "2"))
	requireOn(t, app, "host-b")
	env, err := inner(cHostB, "inspect", "-f", "{{json .Config.Env}}", "pando-"+app+"-web")
	require.NoError(t, err)
	require.Contains(t, env, "MH_TEST_REVISION=2", "the redeploy replaced the workload on host B")

	// A second app on host B that nobody touches while it is away.
	c.steerTo(t, "host-b")
	idle := c.deployEcho(t, "mh-idle", 128<<20)
	requireOn(t, idle, "host-b")

	// Host B goes away.
	_, err = docker("stop", "-t", "5", cHostB)
	require.NoError(t, err)
	t.Cleanup(func() { startHostB(t) })

	for _, id := range []string{app, idle} {
		eventually(t, 2*time.Minute, "an app on host B is reported unreachable", func() bool {
			return c.get(t, "/apps/"+id+"/status")["observability"] == "unreachable"
		})
	}
	// And they stay unfailed while the reconciler keeps looking (its failure
	// window is two minutes in this topology).
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		for _, id := range []string{app, idle} {
			s := c.get(t, "/apps/"+id+"/status")
			require.NotEqual(t, "failed", s["state"], "an app on a host that does not answer is not failed (R-151): %v", s)
		}
		time.Sleep(5 * time.Second)
	}

	// A new app's first deploy goes to the host that answers.
	fresh := c.deployEcho(t, "mh-while-b-down", 128<<20)
	requireOn(t, fresh, "host-a")

	// A redeploy of the app on host B is refused, not placed on host A.
	c.putSpec(t, app, echoSpec(port, 128<<20, "3"))
	dep, status, out := c.startDeploy(t, app)
	appliedAndFailed := status == http.StatusAccepted
	if appliedAndFailed {
		final := c.awaitDeployment(t, app, dep["id"].(string), 3*time.Minute)
		require.NotEqual(t, "succeeded", final["status"], "a redeploy succeeded while the app's host was down")
		out = fmt.Sprint(final["error_detail"])
	} else {
		require.GreaterOrEqual(t, status, 400, out)
	}
	t.Logf("redeploy while host B is down: %s", out)
	for _, id := range []string{app, idle} {
		onA, err := inner(cHostA, "ps", "-aq", "--filter", "label=io.pando.bundle="+id)
		require.NoError(t, err)
		require.Empty(t, onA, "an app of host B's was re-created on host A")
	}
	// O-50, decided: a deploy that fails at the apply step leaves its app
	// failed (design 05 §1.1), held there until a person acts (R-151), even
	// when the runtime refused before changing anything. One refused at the
	// request never started, and the app is as it was.
	if appliedAndFailed {
		require.Equal(t, "failed", c.get(t, "/apps/"+app+"/status")["state"],
			"a deploy that failed at apply leaves its app failed")
	} else {
		require.NotEqual(t, "failed", c.get(t, "/apps/"+app+"/status")["state"])
	}
	require.NotEqual(t, "failed", c.get(t, "/apps/"+idle+"/status")["state"],
		"an app nobody deployed is not failed by another's deploy")

	// Host B returns: the app nobody touched is observed there again, and
	// the reconciler starts its stopped container (R-148).
	startHostB(t)
	last := ""
	eventually(t, 3*time.Minute, "the app on host B is observed and running again", func() bool {
		s := c.get(t, "/apps/"+idle+"/status")
		if now := fmt.Sprint(s); now != last {
			t.Logf("status: %s", now)
			last = now
		}
		running, _ := inner(cHostB, "inspect", "-f", "{{.State.Running}}", "pando-"+idle+"-web")
		return s["observability"] != "unreachable" && s["state"] == "running" && running == "true"
	})
	requireOn(t, idle, "host-b")
	requireOn(t, app, "host-b")
	// The app whose redeploy failed at apply stays failed now its host is
	// back: nothing but a person acting moves it (R-151, O-50).
	if appliedAndFailed {
		require.Equal(t, "failed", c.get(t, "/apps/"+app+"/status")["state"],
			"a failed app stays failed when its host returns")
	}
}

func startHostB(t *testing.T) {
	running, _ := docker("inspect", "-f", "{{.State.Running}}", cHostB)
	if running == "true" {
		return
	}
	_, err := docker("start", cHostB)
	require.NoError(t, err)
	require.NoError(t, awaitDaemon(cHostB, 2*time.Minute))
	require.NoError(t, retry(2*time.Minute, func() error {
		out, err := inner(cHostB, "inspect", "-f", "{{.State.Running}}", "pando-agent")
		if err != nil || out != "true" {
			return fmt.Errorf("agent not running: %v %s", err, out)
		}
		return nil
	}))
}

// TestR224_DeletingAnAppTearsItDownAndTheAgentLeavesItsNetworks: deleting
// an app removes its containers from its host, and the agent leaves the
// app's networks so they are removed rather than kept by the agent.
func TestR224_DeletingAnAppTearsItDownAndTheAgentLeavesItsNetworks(t *testing.T) {
	c := login(t)
	app := c.deployEcho(t, "mh-delete", 128<<20)
	where, err := hostOf(app)
	require.NoError(t, err)
	h := hostNames[where]
	require.NotEqual(t, cControl, h)

	nets, err := inner(h, "network", "ls", "-q", "--filter", "label=io.pando.bundle="+app)
	require.NoError(t, err)
	require.NotEmpty(t, nets, "the app has its own network on %s", h)
	joined, err := inner(h, "inspect", "-f", "{{json .NetworkSettings.Networks}}", "pando-agent")
	require.NoError(t, err)
	require.Contains(t, joined, "pando-"+app, "the agent is joined to the app's network")

	c.must(t, http.MethodDelete, "/apps/"+app+"?force=true", "", http.StatusNoContent)
	eventually(t, 2*time.Minute, "the app's containers are removed", func() bool {
		out, err := inner(h, "ps", "-aq", "--filter", "label=io.pando.bundle="+app)
		return err == nil && out == ""
	})
	eventually(t, 2*time.Minute, "the app's networks are removed and the agent left them", func() bool {
		out, err := inner(h, "network", "ls", "-q", "--filter", "label=io.pando.bundle="+app)
		if err != nil || out != "" {
			return false
		}
		joined, err := inner(h, "inspect", "-f", "{{json .NetworkSettings.Networks}}", "pando-agent")
		return err == nil && !strings.Contains(joined, app)
	})
}

// TestR120_ABuildIsDeliveredThroughTheRegistryAndPulledByDigest: an
// uploaded source built by BuildKit is pushed to the install registry, and
// the app's host runs it by digest, pulled from the registry
// (notes-image-registry-issue-72.md; this runtime offers only registry
// delivery).
func TestR120_ABuildIsDeliveredThroughTheRegistryAndPulledByDigest(t *testing.T) {
	c := login(t)
	app := c.createApp(t, "mh-build-"+stamp())
	archive := sourceArchive(t, map[string]string{
		"Dockerfile": "FROM " + busyboxRemote + "\nCOPY index.html /www/index.html\nEXPOSE 8000\nCMD [\"httpd\", \"-f\", \"-p\", \"8000\", \"-h\", \"/www\"]\n",
		"index.html": "built-and-pulled\n",
	})
	req, err := http.NewRequest(http.MethodPost, base+"/api/v1/apps/"+app+"/source", bytes.NewReader(archive))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/gzip")
	req.AddCookie(&http.Cookie{Name: "pando_session", Value: c.cookie})
	resp, err := c.http.Do(req)
	require.NoError(t, err)
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "%s", body)

	c.deploy(t, app, fmt.Sprintf(`{
		"schema_version": 1,
		"source": {"type": "upload", "upload_id": %q},
		"build": {"strategy": "dockerfile", "adapter_ref": "bld_buildkit"},
		"workloads": [{"name": "web", "primary": true, "exposed": true,
			"ports": [{"number": 8000, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_loopback", "mode": "port", "port": %d},
		"runtime": {"adapter_ref": "rt_hosts", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`, app, nextPort.Add(1)))

	where, err := hostOf(app)
	require.NoError(t, err)
	require.NotEqual(t, "control", where)
	image, err := inner(hostNames[where], "inspect", "-f", "{{.Config.Image}}", "pando-"+app+"-web")
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(image, registry+"/"), "the host runs the image from the registry: %s", image)
	require.Contains(t, image, "@sha256:", "by digest (R-120)")

	catalog, err := docker("exec", cHostA, "wget", "-T", "5", "-qO-", "http://"+registry+"/v2/_catalog")
	require.NoError(t, err)
	require.Contains(t, catalog, strings.ToLower(app))

	eventually(t, time.Minute, "the built app answers through the proxy", func() bool {
		status, page := c.throughProxy(t, c.slug(t, app), "/", nil)
		return status == http.StatusOK && strings.Contains(page, "built-and-pulled")
	})
}

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

// TestR023_AReplacedAgentIsJoinedToItsHostsAppNetworksAgain: an agent that
// disappears — removed by hand, or lost with its host's state — is made again
// by the adapter and joined to every app network on its host, so the apps
// there are reached through the proxy again without a redeploy.
func TestR023_AReplacedAgentIsJoinedToItsHostsAppNetworksAgain(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-a")
	app := c.deployEcho(t, "mh-rejoin", 128<<20)
	requireOn(t, app, "host-a")
	slug := c.slug(t, app)
	eventually(t, time.Minute, "the app answers through the proxy", func() bool {
		status, _ := c.throughProxy(t, slug, "/", nil)
		return status == http.StatusOK
	})

	before, err := inner(cHostA, "inspect", "-f", "{{.Id}}", "pando-agent")
	require.NoError(t, err)
	_, err = inner(cHostA, "rm", "-f", "pando-agent")
	require.NoError(t, err)

	eventually(t, 3*time.Minute, "the agent is made again and joined to the app's network", func() bool {
		id, err := inner(cHostA, "inspect", "-f", "{{.Id}}", "pando-agent")
		if err != nil || id == before {
			return false
		}
		joined, err := inner(cHostA, "inspect", "-f", "{{json .NetworkSettings.Networks}}", "pando-agent")
		return err == nil && strings.Contains(joined, "pando-"+app)
	})
	eventually(t, time.Minute, "the app answers through the proxy again", func() bool {
		status, _ := c.throughProxy(t, slug, "/", nil)
		return status == http.StatusOK
	})
}

// TestR023_EitherReplicaReachesAnAppOnAnotherHost: both of Pando's replicas
// on the control host reach an app on host B through its agent, each with its
// own client certificate from the one authority, and the two replicas'
// rejoin passes do not replace each other's agents.
func TestR023_EitherReplicaReachesAnAppOnAnotherHost(t *testing.T) {
	c := login(t)
	c.steerTo(t, "host-b")
	app := c.deployEcho(t, "mh-replicas", 128<<20)
	requireOn(t, app, "host-b")
	slug := c.slug(t, app)

	agents := map[string]string{}
	for _, h := range []string{cHostA, cHostB} {
		id, err := inner(h, "inspect", "-f", "{{.Id}}", "pando-agent")
		require.NoError(t, err)
		agents[h] = id
	}
	for _, via := range []string{base, base2} {
		eventually(t, time.Minute, "the app answers through "+via, func() bool {
			status, body := c.throughProxyVia(t, via, slug, "/")
			return status == http.StatusOK && strings.Contains(body, "x-pando-assertion")
		})
	}
	// Several rejoin passes of both replicas (every 15 s each).
	time.Sleep(40 * time.Second)
	for _, via := range []string{base, base2} {
		status, _ := c.throughProxyVia(t, via, slug, "/")
		require.Equal(t, http.StatusOK, status, via)
	}
	for h, id := range agents {
		now, err := inner(h, "inspect", "-f", "{{.Id}}", "pando-agent")
		require.NoError(t, err)
		require.Equal(t, id, now, "the agent on %s was replaced while nothing about it changed", h)
	}
}
