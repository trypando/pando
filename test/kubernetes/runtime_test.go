//go:build kubernetes

package kubernetes_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// TestR256_TwoReplicasShareOneDatabaseAndOneLeads: deploy/kubernetes runs two
// replicas against one Postgres, both ready, both registered, and exactly one
// holding the leader lock.
func TestR256_TwoReplicasShareOneDatabaseAndOneLeads(t *testing.T) {
	requireCluster(t)

	ready := mustKubectl(t, "-n", "pando", "get", "deployment", "pando", "-o", "jsonpath={.status.readyReplicas}")
	require.Equal(t, "2", ready, "both replicas are ready")

	// Each replica registers itself by its pod's hostname.
	pods := strings.Fields(mustKubectl(t, "-n", "pando", "get", "pods",
		"-l", "app.kubernetes.io/name=pando,app.kubernetes.io/component=server",
		"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}"))
	require.Len(t, pods, 2)
	eventually(t, time.Minute, "both pods registered as live replicas", func() bool {
		live := psql(t, `SELECT string_agg(hostname, ' ' ORDER BY hostname) FROM pando_replicas WHERE stopped_at IS NULL AND heartbeat_at > now() - interval '1 minute'`)
		for _, p := range pods {
			if !strings.Contains(live, p) {
				return false
			}
		}
		return true
	})

	// state.leaderLock, 0x70616e646f02, as Postgres reports a bigint key.
	// A replica that has just started may not have taken it yet.
	eventually(t, time.Minute, "exactly one replica leads", func() bool {
		return psql(t, `SELECT count(*) FROM pg_locks
			WHERE locktype = 'advisory' AND granted AND classid = 28769 AND objid = 1852075778 AND objsubid = 1`) == "1"
	})
}

// TestO43_TheRuntimePassesItsNetworkPolicyCanary: on kind's kindnet, which
// enforces NetworkPolicy, the adapter's health check runs the canary and the
// runtime reports a private network — without which the planner refuses it.
func TestO43_TheRuntimePassesItsNetworkPolicyCanary(t *testing.T) {
	c := login(t)
	var runtime map[string]any
	for _, a := range c.get(t, "/adapters")["adapters"].([]any) {
		if a.(map[string]any)["id"] == "rt_kubernetes" {
			runtime = a.(map[string]any)
		}
	}
	require.NotNil(t, runtime, "rt_kubernetes is configured")
	require.Equal(t, true, runtime["healthy"], "%v", runtime)
	caps := runtime["capabilities"].(map[string]any)
	require.Equal(t, true, caps["SupportsPrivateNetwork"],
		"the canary passed: kindnet refused the unlabeled client and admitted the labeled one")
	require.Equal(t, []any{"registry"}, caps["ImageDelivery"])

	// The canary cleans up after itself.
	eventually(t, 2*time.Minute, "the canaries' namespaces are removed", func() bool {
		out, err := kubectl("get", "namespaces", "-l", "pando.dev/role=canary", "-o", "name")
		return err == nil && strings.TrimSpace(out) == ""
	})
}

// TestR023_AnImageAppRunsAsABarePodInItsOwnNamespace asserts what an image
// app becomes: a namespace of its own under default deny, a headless Service
// per port, and one pod Kubernetes never restarts (R-151).
func TestR023_AnImageAppRunsAsABarePodInItsOwnNamespace(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-nginx", imageSpec("nginx:alpine", 80, ""))
	ns := namespaceOf(app)

	labels := mustKubectl(t, "get", "namespace", ns, "-o", "jsonpath={.metadata.labels}")
	require.Contains(t, labels, `"app.kubernetes.io/managed-by":"pando"`)
	require.Contains(t, labels, `"pod-security.kubernetes.io/enforce":"baseline"`)

	policies := mustKubectl(t, "-n", ns, "get", "networkpolicy", "-o", "json")
	require.Contains(t, policies, `"Ingress"`)
	require.Contains(t, policies, `"Egress"`)

	clusterIP := mustKubectl(t, "-n", ns, "get", "service", "web", "-o", "jsonpath={.spec.clusterIP} {.spec.type}")
	require.Equal(t, "None ClusterIP", clusterIP, "a headless Service: no virtual IP, no kube-proxy rules")

	pod := mustKubectl(t, "-n", ns, "get", "pods", "-l", "pando.dev/workload=web", "-o",
		"jsonpath={.items[0].spec.restartPolicy} {.items[0].status.phase} {.items[0].metadata.ownerReferences} {.items[0].spec.automountServiceAccountToken}")
	require.Equal(t, "Never Running  false", pod, "a bare pod, owned by nothing, never restarted, with no API token")

	require.Equal(t, "running", c.get(t, "/apps/"+app+"/status")["state"])

	// Through Pando's proxy, which is the only way in (R-023).
	status, body := c.throughProxy(t, fwd.base(), "", c.slug(t, app), "/", nil)
	require.Equal(t, http.StatusOK, status, body)
	require.Contains(t, body, "Welcome to nginx")
}

// echoSpec is an app that answers with the request it received.
func echoSpec() string {
	return imageSpec("mendhak/http-https-echo:34", 8080, `, "env": [{"key": "HTTP_PORT", "value": "8080"}]`)
}

// TestR053_ThroughThePodProxyForgedHeadersAreReplaced asserts R-053, R-054
// and R-173 against an app on Kubernetes: what the app receives through the
// proxy, not what Pando believes it sent.
func TestR053_ThroughThePodProxyForgedHeadersAreReplaced(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-echo", echoSpec())

	status, body := c.throughProxy(t, fwd.base(), "", c.slug(t, app), "/", map[string]string{
		"X-Pando-User":          "admin@corp.com",
		"X-PANDO-GROUPS":        "admins,superusers",
		"X-Pando-Assertion":     "forged.assertion.value",
		"X-Pando-Future-Header": "whatever",
	}, &http.Cookie{Name: "pando_csrf", Value: "should-not-arrive"}, &http.Cookie{Name: "app_cookie", Value: "kept"})
	require.Equal(t, http.StatusOK, status, body)

	var echoed struct {
		Headers map[string]string `json:"headers"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &echoed), body)
	h := map[string]string{}
	for k, v := range echoed.Headers {
		h[strings.ToLower(k)] = v
	}

	require.NotEqual(t, "admin@corp.com", h["x-pando-user"], "R-053: a forged identity header reached the app")
	require.NotContains(t, h["x-pando-groups"], "superusers")
	require.Empty(t, h["x-pando-future-header"], "R-053: every X-Pando-* is stripped, known or not")

	// R-054: a real assertion, a three-part JWT, replaces the forged one.
	require.NotEqual(t, "forged.assertion.value", h["x-pando-assertion"])
	require.Len(t, strings.Split(h["x-pando-assertion"], "."), 3, "the app receives Pando's assertion")

	// R-173: Pando's own cookies never reach the app; the app's do.
	require.NotContains(t, h["cookie"], "pando_", "R-173: a pando_* cookie reached the app: %s", h["cookie"])
	require.Contains(t, h["cookie"], "app_cookie=kept")
}

// TestR025_OnlyPandosPodsReachAnAppPod: a pod in the default namespace, and
// one in another app's namespace, cannot open a connection to an app's pod;
// Pando's server pods can (R-023, R-025). The cluster enforces it, not Pando.
func TestR025_OnlyPandosPodsReachAnAppPod(t *testing.T) {
	c := login(t)
	target := c.deployImage(t, "k8s-target", imageSpec("nginx:alpine", 80, ""))
	other := c.deployImage(t, "k8s-other", imageSpec("nginx:alpine", 80, ""))
	url := fmt.Sprintf("http://web.%s.svc.cluster.local:80/", namespaceOf(target))

	// Pando's own pods reach it (the image's busybox wget).
	out, err := kubectl("-n", "pando", "exec", "deploy/pando", "--", "wget", "-T", "5", "-qO-", url)
	require.NoError(t, err, out)
	require.Contains(t, out, "Welcome to nginx")

	// Limits, because an app namespace's quota refuses a pod without them;
	// a refusal there is not the network refusing anything.
	probe := func(ns, name string) (string, error) {
		overrides := fmt.Sprintf(`{"apiVersion":"v1","spec":{"containers":[{"name":%q,"image":"busybox:1.37",
			"command":["wget","-T","5","-qO-",%q],
			"resources":{"requests":{"cpu":"20m","memory":"16Mi"},"limits":{"cpu":"20m","memory":"16Mi"}}}]}}`, name, url)
		return kubectl("-n", ns, "run", name, "--rm", "-i", "--restart=Never", "--quiet",
			"--image=busybox:1.37", "--labels=app.kubernetes.io/name=pando,app.kubernetes.io/component=server",
			"--overrides="+overrides)
	}

	// From the default namespace, even carrying Pando's labels: the policy
	// admits those labels only in namespace pando.
	out, err = probe("default", "k8s-probe-"+stamp())
	require.Error(t, err, "a pod in default reached an app pod: %s", out)
	require.Contains(t, out, "timed out", "the name resolved and the connection was dropped, not refused for another reason")

	// From another app's namespace.
	out, err = probe(namespaceOf(other), "k8s-probe-"+stamp())
	require.Error(t, err, "a pod in another app's namespace reached an app pod: %s", out)
	require.Contains(t, out, "timed out")
}

// TestR086_ExecAndLogsReachTheAppsPod: a terminal through Pando's API runs in
// the app's pod (pods/exec), and the app's own output comes back (pods/log).
func TestR086_ExecAndLogsReachTheAppsPod(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-exec", imageSpec("nginx:alpine", 80, ""))

	// Something for the log to hold.
	status, _ := c.throughProxy(t, fwd.base(), "", c.slug(t, app), "/pando-log-marker", nil)
	require.Equal(t, http.StatusNotFound, status)

	eventually(t, time.Minute, "the app's log holds the request", func() bool {
		out, code := c.do(t, http.MethodGet, "/apps/"+app+"/logs?tail=200", "")
		return code == http.StatusOK && strings.Contains(out, "pando-log-marker")
	})

	url := strings.Replace(fwd.base(), "http://", "ws://", 1) + "/api/v1/apps/" + app + "/exec"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	header := http.Header{}
	header.Set("Cookie", "pando_session="+c.cookie)
	conn, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte("echo pando-exec-$((6*7))\n")))
	var seen strings.Builder
	for !strings.Contains(seen.String(), "pando-exec-42") {
		readCtx, stop := context.WithTimeout(ctx, 20*time.Second)
		kind, data, err := conn.Read(readCtx)
		stop()
		require.NoError(t, err, "the shell never echoed the command's output; saw %q", seen.String())
		if kind == websocket.MessageBinary {
			seen.Write(data)
		}
	}
}

// volumeSpec is an app running busybox with one volume at /data. command
// is the workload's command, as JSON.
func volumeSpec(command string) string {
	return `{
		"schema_version": 1,
		"source": {"type": "image", "image": "busybox:1.37"},
		"build": {"strategy": "prebuilt"},
		"volumes": [{"id": "vol_data", "name": "data", "declared": "user"}],
		"workloads": [{"name": "web", "primary": true, "exposed": true, "image": "busybox:1.37",
			"command": ` + command + `,
			"mounts": [{"volume_id": "vol_data", "path": "/data"}],
			"ports": [{"number": 80, "protocol": "http", "source": "user"}]}],
		"routing": {"adapter_ref": "rte_traefik", "mode": "path"},
		"runtime": {"adapter_ref": "rt_kubernetes", "isolation_floor": 10},
		"deploy": {"strategy": "recreate"}
	}`
}

func webPod(t *testing.T, ns string) string {
	t.Helper()
	return mustKubectl(t, "-n", ns, "get", "pods", "-l", "pando.dev/workload=web",
		"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[0].metadata.name}")
}

// TestR151_AFailedAppsPodIsNotRestartedByKubernetes: an app that ran, lost its
// pod (as a node loss takes one) and now exits 1 on every start is recreated
// by the reconciler under its backoff (R-148, R-149), reaches failed (R-150),
// and stays there — and the cluster restarts nothing, because each pod is
// bare with restartPolicy Never (R-151).
//
// The app serves on its first start and exits on every start after it, by a
// marker on its volume, so the deploy itself succeeds and the crash loop is
// the reconciler's to handle.
func TestR151_AFailedAppsPodIsNotRestartedByKubernetes(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-crash", volumeSpec(
		`["sh", "-c", "if [ -f /data/booted ]; then echo crashing; exit 1; fi; touch /data/booted; exec httpd -f -p 80"]`))
	ns := namespaceOf(app)
	eventually(t, time.Minute, "the app is running", func() bool {
		return c.get(t, "/apps/"+app)["state"] == "running"
	})

	mustKubectl(t, "-n", ns, "delete", "pod", webPod(t, ns), "--wait=false")

	eventually(t, 8*time.Minute, "the app reached failed", func() bool {
		return c.get(t, "/apps/"+app)["state"] == "failed"
	})

	// Every pod is bare, never restarted by the kubelet, and not running.
	snapshot := func() string {
		return mustKubectl(t, "-n", ns, "get", "pods", "-l", "pando.dev/workload=web", "-o",
			`jsonpath={range .items[*]}{.metadata.name} {.spec.restartPolicy} {.status.containerStatuses[0].restartCount} {.status.phase}{"\n"}{end}`)
	}
	before := snapshot()
	require.NotEmpty(t, strings.TrimSpace(before), "the last crash's pod is kept, so its log can be read")
	for _, line := range strings.Split(strings.TrimSpace(before), "\n") {
		f := strings.Fields(line)
		require.Len(t, f, 4, line)
		require.Equal(t, "Never", f[1], line)
		require.Equal(t, "0", f[2], "the kubelet restarted a container: %s", line)
		require.NotEqual(t, "Running", f[3], line)
	}
	out, code := c.do(t, http.MethodGet, "/apps/"+app+"/logs?tail=50", "")
	require.Equal(t, http.StatusOK, code, out)
	require.Contains(t, out, "crashing", "the output of the crash that sent the app to failed is still readable")

	// And stays: past every backoff interval, nothing has made a new pod.
	time.Sleep(45 * time.Second)
	require.Equal(t, "failed", c.get(t, "/apps/"+app)["state"], "a failed app stays failed (R-151)")
	// An older finished pod may have been tidied away; nothing new was made
	// and nothing runs.
	after := snapshot()
	for _, line := range strings.Split(strings.TrimSpace(after), "\n") {
		if line == "" {
			continue
		}
		require.Contains(t, before, line, "a pod appeared, or changed, after the app failed")
	}
}

// TestR204_AVolumeIsRetainedAndTheDeleteKeepsAFinalBackup: an app's volume
// is a ReadWriteOnce claim whose bound volume is Retain, so its data outlives
// a pod being replaced; and deleting the app keeps a final backup of it,
// taken through the cluster (a helper pod streaming tar over pods/exec),
// before the teardown removes the claim and the namespace (R-204, R-205).
func TestR204_AVolumeIsRetainedAndTheDeleteKeepsAFinalBackup(t *testing.T) {
	c := login(t)
	app := c.deployImage(t, "k8s-volume", volumeSpec(`["httpd", "-f", "-p", "80"]`))
	ns := namespaceOf(app)

	claims := strings.Fields(mustKubectl(t, "-n", ns, "get", "pvc", "-o", "jsonpath={.items[*].metadata.name}"))
	require.Len(t, claims, 1)
	claim := claims[0]
	require.Equal(t, "ReadWriteOnce", mustKubectl(t, "-n", ns, "get", "pvc", claim, "-o", "jsonpath={.spec.accessModes[0]}"))
	var pv string
	eventually(t, time.Minute, "the claim is bound and its volume is Retain", func() bool {
		pv = mustKubectl(t, "-n", ns, "get", "pvc", claim, "-o", "jsonpath={.spec.volumeName}")
		return pv != "" && mustKubectl(t, "get", "pv", pv, "-o", "jsonpath={.spec.persistentVolumeReclaimPolicy}") == "Retain"
	})

	// Written, then the pod taken away: the reconciler's replacement reads it.
	mustKubectl(t, "-n", ns, "exec", webPod(t, ns), "-c", "app", "--", "sh", "-c", "echo kept > /data/marker")
	old := webPod(t, ns)
	mustKubectl(t, "-n", ns, "delete", "pod", old)
	eventually(t, 2*time.Minute, "the reconciler recreated the pod", func() bool {
		out, _ := kubectl("-n", ns, "get", "pods", "-l", "pando.dev/workload=web",
			"--field-selector", "status.phase=Running", "-o", "jsonpath={.items[*].metadata.name}")
		return out != "" && !strings.Contains(out, old)
	})
	require.Equal(t, "kept\n", mustKubectl(t, "-n", ns, "exec", webPod(t, ns), "-c", "app", "--", "cat", "/data/marker"))

	// Deleting an app with storage settles it: here, a final backup (R-204).
	c.must(t, http.MethodDelete, "/apps/"+app+"?backup=true", "", http.StatusNoContent)
	var kept bool
	for _, b := range c.get(t, "/backups")["backups"].([]any) {
		m := b.(map[string]any)
		if m["app_id"] == app && m["kind"] == "on_delete" {
			kept = true
		}
	}
	require.True(t, kept, "the delete kept a final backup of the app's storage")

	// The decision settled the storage, so the teardown removes it.
	eventually(t, 3*time.Minute, "the app's namespace is removed", func() bool {
		_, err := kubectl("get", "namespace", ns)
		return err != nil
	})
}
