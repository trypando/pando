//go:build kwokscale

// Package kwok_test measures where the Kubernetes runtime stops scaling
// (docs/design/notes-kubernetes-scale-issue-72.md). It drives the real
// adapters — the Kubernetes runtime's Apply and Observe, Traefik's
// IngressRoutes — against a kwok cluster, whose nodes are fake and whose pods
// report Running without running anything, so the control plane is all that
// is measured.
//
// `make test-kwok-scale APPS=2500,5000` creates the cluster, runs this, and
// deletes the cluster. Behind the kwokscale build tag: never in CI, never in
// coverage.
package kwok_test

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/routing/traefik"
	k8srt "github.com/trypando/pando/internal/adapter/runtime/kubernetes"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/secret"
)

// settings, from the environment the Makefile target sets.
type settings struct {
	kubeconfig   string
	steps        []int
	concurrency  int           // apps applied at once while filling the cluster
	observers    int           // apps observed at once: the reconciler's Concurrency
	stepTimeout  time.Duration // a step that takes longer stops the run
	poll         time.Duration // the adapter's wait interval
	out          string
	defaultQPSAt int // apps observed through a client with client-go's default limits
}

func load(t *testing.T) settings {
	s := settings{
		kubeconfig:   os.Getenv("PANDO_KWOK_KUBECONFIG"),
		concurrency:  envInt("PANDO_KWOK_CONCURRENCY", 64),
		observers:    envInt("PANDO_KWOK_OBSERVERS", 8),
		stepTimeout:  time.Duration(envInt("PANDO_KWOK_STEP_MINUTES", 60)) * time.Minute,
		poll:         time.Duration(envInt("PANDO_KWOK_POLL_MS", 2000)) * time.Millisecond,
		out:          os.Getenv("PANDO_KWOK_OUT"),
		defaultQPSAt: envInt("PANDO_KWOK_DEFAULT_QPS_SAMPLE", 200),
	}
	if s.kubeconfig == "" {
		t.Skip("no kwok cluster: run `make test-kwok-scale`, or set PANDO_KWOK_KUBECONFIG")
	}
	for _, f := range strings.Split(envOr("PANDO_KWOK_APPS", "2500,5000,10000,20000"), ",") {
		n, err := strconv.Atoi(strings.TrimSpace(f))
		if err != nil || n <= 0 {
			t.Fatalf("PANDO_KWOK_APPS: %q is not a number of apps", f)
		}
		s.steps = append(s.steps, n)
	}
	return s
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(k)); err == nil && n > 0 {
		return n
	}
	return def
}

// clients are the harness's connections, all through one recorder.
type clients struct {
	rest *rest.Config
	cs   kubernetes.Interface
	meta metadata.Interface
	rec  *recorder
}

// connect builds clients with client-go's limiter lifted, so what is timed is
// the API server. Pando's own client keeps the default of 5 requests a
// second with bursts of 10 (it sets neither), which the run measures
// separately (defaultQPSObserve).
func connect(t *testing.T, kubeconfig string, qps float32, burst int) clients {
	t.Helper()
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		&clientcmd.ClientConfigLoadingRules{ExplicitPath: kubeconfig}, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	rc.QPS, rc.Burst, rc.Timeout = qps, burst, 5*time.Minute
	rec := newRecorder()
	rc.Wrap(rec.wrap)
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	mc, err := metadata.NewForConfig(rc)
	if err != nil {
		t.Fatal(err)
	}
	return clients{rest: rc, cs: cs, meta: mc, rec: rec}
}

const (
	edgeNamespace = "pando-edge"
	proxyUpstream = "http://pando-proxy:8080"
)

func runtimeConfig() k8srt.Config {
	// kwokctl's ranges; the policies name them and nothing enforces them here.
	return k8srt.Config{
		PodCIDR: "10.0.0.0/16", ServiceCIDR: "10.96.0.0/12",
		EgressGatewayImage: "ghcr.io/trypando/pando:scale",
	}
}

// adapters is one Pando replica's runtime and routing, on the given clients.
func adapters(t *testing.T, ctx context.Context, s settings, c clients) (*k8srt.Adapter, *traefik.Adapter) {
	t.Helper()
	rt, err := k8srt.NewForScaleTest(ctx, c.cs, runtimeConfig(), s.poll)
	if err != nil {
		t.Fatal(err)
	}
	rtg := traefik.New()
	if err := rtg.Configure(ctx, []byte(fmt.Sprintf(
		`{"delivery":"kubernetes_api","namespace":%q,"kubeconfig":%q,"certificates":"http","acme_email":"ops@scale.test","base_domain":"apps.scale.test"}`,
		edgeNamespace, s.kubeconfig))); err != nil {
		t.Fatal(err)
	}
	dyn, err := dynamic.NewForConfig(c.rest)
	if err != nil {
		t.Fatal(err)
	}
	rtg.UseDynamicForScaleTest(dyn)
	if err := rtg.HealthCheck(ctx); err != nil {
		t.Fatalf("Traefik's IngressRoute CRD is not installed (test/kwok/ingressroute-crd.yaml): %v", err)
	}
	return rt, rtg
}

// appID is the i-th app's ID: app_ and 26 characters, as a ULID is.
func appID(i int) string { return fmt.Sprintf("app_01KWQK%020d", i) }

// plan is the i-th app's bundle, shaped like what the planner sends:
//
//   - every app has a web workload with a port, a health check, limits and
//     an environment of six variables;
//   - three in ten also have a database workload the web one depends on
//     (no volume: kwok has no storage provisioner, so a claim would never
//     bind and the pod would never be placed);
//   - eight in ten pull from Pando's registry with a credential, as a built
//     image does, and so have a pull Secret.
func plan(i int) api.BundlePlan {
	id := appID(i)
	var auth *api.RegistryAuth
	if i%5 != 0 {
		auth = &api.RegistryAuth{Registry: "registry.pando.svc:5000", Username: "pando-pull", Password: secret.New(fmt.Sprintf("pull-%d", i))}
	}
	env := map[string]secret.Value{
		"PORT": secret.New("3000"), "NODE_ENV": secret.New("production"), "LOG_LEVEL": secret.New("info"),
		"SESSION_SECRET": secret.New(fmt.Sprintf("s-%d", i)), "PUBLIC_URL": secret.New("https://" + host(i)),
		"FEATURE_FLAGS": secret.New("a,b,c"),
	}
	web := api.WorkloadPlan{
		Name: "web", Image: fmt.Sprintf("registry.pando.svc:5000/apps/%s@sha256:%064x", strings.ToLower(id), i),
		Env: env, Ports: []api.PortPlan{{Number: 3000, Protocol: "tcp"}},
		Health:    &api.HealthPlan{Path: "/healthz", Port: 3000, IntervalSeconds: 10, Retries: 3},
		Resources: api.ResourcePlan{CPUMillis: 250, MemoryBytes: 256 << 20},
		PullAuth:  auth, Exposed: true,
	}
	p := api.BundlePlan{
		BundleID: id, Network: api.NetworkPlan{Private: true},
		Labels: map[string]string{"pando.app": id}, FirstDeploy: true,
	}
	if i%10 < 3 {
		web.Env["DATABASE_URL"] = secret.New(fmt.Sprintf("postgres://app:pw-%d@db:5432/app", i))
		web.DependsOn = []string{"db"}
		p.Workloads = append(p.Workloads, api.WorkloadPlan{
			Name: "db", Image: "postgres:17", Env: map[string]secret.Value{"POSTGRES_PASSWORD": secret.New(fmt.Sprintf("pw-%d", i))},
			Ports:     []api.PortPlan{{Number: 5432, Protocol: "tcp"}},
			Resources: api.ResourcePlan{CPUMillis: 250, MemoryBytes: 256 << 20},
		})
	}
	p.Workloads = append(p.Workloads, web)
	return p
}

func host(i int) string { return fmt.Sprintf("app%d.apps.scale.test", i) }

func route(i int) api.RouteRequest {
	return api.RouteRequest{
		AppID: appID(i), Mode: spec.RoutingSubdomain, Hostname: host(i), Port: 3000,
		ProxyUpstream: proxyUpstream, TLS: api.TLSRequest{Enabled: true},
	}
}

// saRetries counts Applies refused because the namespace's default
// ServiceAccount did not exist yet. The ServiceAccount admission plugin
// refuses a pod until the controller manager has made it, which under load
// is after Apply has made the namespace and reached the pod; the reconciler
// would try again on its next pass, and so does deploy, after two seconds.
var saRetries atomic.Int64

// deploy is what one deploy asks of the cluster: the bundle, then its route.
func deploy(ctx context.Context, rt *k8srt.Adapter, rtg *traefik.Adapter, i int) error {
	for attempt := 1; ; attempt++ {
		_, err := rt.Apply(ctx, plan(i))
		if err == nil {
			break
		}
		if attempt < 5 && strings.Contains(err.Error(), "error looking up service account") {
			saRetries.Add(1)
			time.Sleep(2 * time.Second)
			continue
		}
		return fmt.Errorf("apply %s: %w", appID(i), err)
	}
	if _, err := rtg.Ensure(ctx, route(i)); err != nil {
		return fmt.Errorf("route %s: %w", appID(i), err)
	}
	return nil
}

// observe is one reconcile's look at an app: its bundle and its route.
func observe(ctx context.Context, rt *k8srt.Adapter, rtg *traefik.Adapter, i int) error {
	got, err := rt.Observe(ctx, api.BundleRef{BundleID: appID(i)})
	if err != nil {
		return err
	}
	if !got.Exists {
		return fmt.Errorf("%s: nothing observed", appID(i))
	}
	_, err = rtg.Observe(ctx, api.RouteHandle{AppID: appID(i)})
	return err
}

// each runs fn over [from, to) with n at once, and returns the per-item
// times and the errors. It stops early when ctx ends.
func each(ctx context.Context, from, to, n int, fn func(int) error) ([]time.Duration, []error) {
	var (
		mu    sync.Mutex
		times []time.Duration
		errs  []error
		next  = int64(from)
		wg    sync.WaitGroup
	)
	for w := 0; w < n; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				i := int(atomic.AddInt64(&next, 1) - 1)
				if i >= to {
					return
				}
				start := time.Now()
				err := fn(i)
				d := time.Since(start)
				mu.Lock()
				times = append(times, d)
				if err != nil {
					errs = append(errs, err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return times, errs
}

// row is one size's measurements.
type row struct {
	apps                     int
	applyWall                time.Duration
	applyErrors, saRetries   int
	perApp                   quants
	applyCalls, applyFailed  int
	applyAPI                 quants
	podsRunning, podsTotal   int
	runningWait              time.Duration
	observeWall              time.Duration
	observeCalls             int
	observeAPI               quants
	capacity                 time.Duration
	capacityErr              string
	oneMore                  time.Duration
	oneMoreCalls             int
	oneMoreAPI               time.Duration
	etcdBytes, apiserverRSS  float64
	counts                   map[string]int
	applyTable, observeTable string
	stopped                  string
	firstErrors              []string
}

func TestKwokScale(t *testing.T) {
	s := load(t)
	ctx := context.Background()
	c := connect(t, s.kubeconfig, 5000, 10000)
	rt, rtg := adapters(t, ctx, s, c)

	var (
		rows   []row
		have   int
		report = newReport(s)
	)
	report.defaultQPS = defaultQPSObserve(t, ctx, s)

	for _, target := range s.steps {
		if target <= have {
			continue
		}
		r := row{apps: target}
		stepCtx, cancel := context.WithTimeout(ctx, s.stepTimeout)

		// Fill the cluster to target.
		c.rec.take()
		saRetries.Store(0)
		start := time.Now()
		times, errs := each(stepCtx, have, target, s.concurrency, func(i int) error { return deploy(stepCtx, rt, rtg, i) })
		r.applyWall = time.Since(start)
		snap := c.rec.take()
		r.applyErrors, r.perApp, r.saRetries = len(errs), quantiles(times), int(saRetries.Load())
		r.applyCalls, r.applyFailed, r.applyAPI, r.applyTable = snap.total(), snap.failures(), quantiles(snap.all()), snap.table()
		for _, e := range errs[:min(len(errs), 5)] {
			r.firstErrors = append(r.firstErrors, e.Error())
		}
		have += len(times)
		r.apps = have
		t.Logf("%d apps: applied in %s, %d errors, %d calls", have, r.applyWall.Round(time.Second), len(errs), r.applyCalls)

		switch {
		case stepCtx.Err() != nil:
			r.stopped = fmt.Sprintf("filling to %d took longer than %s", target, s.stepTimeout)
		case len(errs) > max(10, len(times)/100):
			r.stopped = fmt.Sprintf("%d of %d deploys failed", len(errs), len(times))
		}
		cancel()

		r.podsRunning, r.podsTotal, r.runningWait = awaitRunning(ctx, c)
		c.rec.take()

		// One reconcile pass over every app, at the reconciler's concurrency.
		start = time.Now()
		_, oerrs := each(ctx, 0, have, s.observers, func(i int) error { return observe(ctx, rt, rtg, i) })
		r.observeWall = time.Since(start)
		snap = c.rec.take()
		r.observeCalls, r.observeAPI, r.observeTable = snap.total(), quantiles(snap.all()), snap.table()
		if len(oerrs) > 0 {
			r.firstErrors = append(r.firstErrors, fmt.Sprintf("observe: %d errors, first: %v", len(oerrs), oerrs[0]))
		}

		// What the planner reads before each deploy: the cluster's room,
		// which lists every pod in the cluster.
		start = time.Now()
		if _, err := rt.LargestFitFor(ctx, appID(have)); err != nil {
			r.capacityErr = err.Error()
		}
		r.capacity = time.Since(start)
		c.rec.take()

		// One more deploy at this size, alone.
		start = time.Now()
		if err := deploy(ctx, rt, rtg, have); err != nil {
			r.firstErrors = append(r.firstErrors, "one more: "+err.Error())
		}
		r.oneMore = time.Since(start)
		snap = c.rec.take()
		r.oneMoreCalls = snap.total()
		for _, d := range snap.all() {
			r.oneMoreAPI += d
		}
		have++

		r.etcdBytes, r.apiserverRSS = serverMetrics(ctx, c)
		r.counts = countObjects(ctx, c)
		c.rec.take()

		rows = append(rows, r)
		report.write(t, rows)
		if r.stopped != "" {
			t.Logf("stopped: %s", r.stopped)
			break
		}
		if !ready(ctx, c) {
			rows[len(rows)-1].stopped = "the API server stopped answering /readyz"
			report.write(t, rows)
			break
		}
	}
}

// defaultQPSObserve observes a sample of apps through a client with
// client-go's default limits, which is what Pando's adapter is given
// (Configure sets neither QPS nor Burst): the time per call there is the
// limiter's, not the server's.
func defaultQPSObserve(t *testing.T, ctx context.Context, s settings) string {
	t.Helper()
	n := s.defaultQPSAt
	full := connect(t, s.kubeconfig, 5000, 10000)
	rt, rtg := adapters(t, ctx, s, full)
	if _, errs := each(ctx, 1_000_000, 1_000_000+n, s.concurrency, func(i int) error { return deploy(ctx, rt, rtg, i) }); len(errs) > 0 {
		return fmt.Sprintf("could not set up the sample: %v", errs[0])
	}
	limited := connect(t, s.kubeconfig, 0, 0) // 0 is client-go's default: 5 a second, bursts of 10
	lrt, lrtg := adapters(t, ctx, s, limited)
	limited.rec.take()
	start := time.Now()
	_, errs := each(ctx, 1_000_000, 1_000_000+n, s.observers, func(i int) error { return observe(ctx, lrt, lrtg, i) })
	wall := time.Since(start)
	calls := limited.rec.take().total()
	// The sample's apps are removed, so the steps measure only their own.
	_, _ = each(ctx, 1_000_000, 1_000_000+n, s.concurrency, func(i int) error {
		_ = rtg.Remove(ctx, api.RouteHandle{AppID: appID(i)})
		return rt.Destroy(ctx, api.BundleRef{BundleID: appID(i)}, api.DestroyOptions{})
	})
	msg := fmt.Sprintf("Observing %d apps (%d calls) through a client with client-go's default limits took %s: %.1f calls a second",
		n, calls, wall.Round(100*time.Millisecond), float64(calls)/wall.Seconds())
	if len(errs) > 0 {
		msg += fmt.Sprintf(", %d errors (first: %v)", len(errs), errs[0])
	}
	return msg + "."
}

// awaitRunning waits up to ten minutes for kwok to report every app pod
// Running, and says how many are.
func awaitRunning(ctx context.Context, c clients) (running, total int, waited time.Duration) {
	start := time.Now()
	for {
		running, total = 0, 0
		cont := ""
		for {
			list, err := c.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/managed-by=pando", Limit: 5000, Continue: cont,
			})
			if err != nil {
				break
			}
			for _, p := range list.Items {
				total++
				if p.Status.Phase == "Running" {
					running++
				}
			}
			if cont = list.Continue; cont == "" {
				break
			}
		}
		if (total > 0 && running == total) || time.Since(start) > 10*time.Minute {
			return running, total, time.Since(start)
		}
		time.Sleep(10 * time.Second)
	}
}

var metricLine = regexp.MustCompile(`(?m)^(apiserver_storage_size_bytes|process_resident_memory_bytes)(\{[^}]*\})? ([0-9.e+]+)$`)

// serverMetrics reads etcd's database size, as the API server reports it,
// and the API server's resident memory.
func serverMetrics(ctx context.Context, c clients) (etcd, rss float64) {
	body, err := c.cs.CoreV1().RESTClient().Get().AbsPath("/metrics").DoRaw(ctx)
	if err != nil {
		return 0, 0
	}
	for _, m := range metricLine.FindAllStringSubmatch(string(body), -1) {
		v, _ := strconv.ParseFloat(m[3], 64)
		switch m[1] {
		case "apiserver_storage_size_bytes":
			etcd = max(etcd, v)
		case "process_resident_memory_bytes":
			rss = v
		}
	}
	return etcd, rss
}

func ready(ctx context.Context, c clients) bool {
	_, err := c.cs.CoreV1().RESTClient().Get().AbsPath("/readyz").DoRaw(ctx)
	return err == nil
}

// counted are the kinds whose totals the report shows, Pando's and the ones
// the cluster makes because of Pando's.
var counted = []struct {
	name string
	gvr  schema.GroupVersionResource
}{
	{"namespaces", schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}},
	{"pods", schema.GroupVersionResource{Version: "v1", Resource: "pods"}},
	{"services", schema.GroupVersionResource{Version: "v1", Resource: "services"}},
	{"endpoints", schema.GroupVersionResource{Version: "v1", Resource: "endpoints"}},
	{"endpointslices", schema.GroupVersionResource{Group: "discovery.k8s.io", Version: "v1", Resource: "endpointslices"}},
	{"secrets", schema.GroupVersionResource{Version: "v1", Resource: "secrets"}},
	{"configmaps", schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}},
	{"serviceaccounts", schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}},
	{"networkpolicies", schema.GroupVersionResource{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}},
	{"resourcequotas", schema.GroupVersionResource{Version: "v1", Resource: "resourcequotas"}},
	{"rolebindings", schema.GroupVersionResource{Group: "rbac.authorization.k8s.io", Version: "v1", Resource: "rolebindings"}},
	{"ingressroutes", schema.GroupVersionResource{Group: "traefik.io", Version: "v1alpha1", Resource: "ingressroutes"}},
	{"events", schema.GroupVersionResource{Version: "v1", Resource: "events"}},
}

func countObjects(ctx context.Context, c clients) map[string]int {
	out := map[string]int{}
	for _, k := range counted {
		n, cont := 0, ""
		for {
			list, err := c.meta.Resource(k.gvr).List(ctx, metav1.ListOptions{Limit: 5000, Continue: cont})
			if err != nil {
				n = -1
				break
			}
			n += len(list.Items)
			if cont = list.Continue; cont == "" {
				break
			}
		}
		out[k.name] = n
	}
	return out
}

// report writes the results as Markdown, rewritten after every step so a run
// that falls over still leaves what it measured.
type report struct {
	s          settings
	started    time.Time
	defaultQPS string
}

func newReport(s settings) *report { return &report{s: s, started: time.Now()} }

func (r *report) write(t *testing.T, rows []row) {
	var b strings.Builder
	fmt.Fprintf(&b, "# Kubernetes runtime on kwok — %s\n\n", r.started.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "Steps %v; %d apps applied at once while filling; observe at %d at once; adapter poll %s.\n\n",
		r.s.steps, r.s.concurrency, r.s.observers, r.s.poll)
	if r.defaultQPS != "" {
		b.WriteString(r.defaultQPS + "\n\n")
	}
	b.WriteString("| apps | fill wall | fill errors (SA retries) | per deploy p50 / p95 / p99 | fill calls (failed) | call p50 / p95 / p99 | pods Running | observe pass | observe calls | observe call p50 / p95 / p99 | capacity read | one more deploy (calls, API time) | etcd db | apiserver RSS |\n")
	b.WriteString("|---:|---:|---:|---|---:|---|---|---:|---:|---|---:|---|---:|---:|\n")
	for _, x := range rows {
		capRead := ms(x.capacity)
		if x.capacityErr != "" {
			capRead += " (failed)"
		}
		fmt.Fprintf(&b, "| %d | %s | %d (%d) | %s / %s / %s | %d (%d) | %s / %s / %s | %d of %d | %s | %d | %s / %s / %s | %s | %s (%d, %s) | %.0f MiB | %.0f MiB |\n",
			x.apps, x.applyWall.Round(time.Second), x.applyErrors, x.saRetries, ms(x.perApp.p50), ms(x.perApp.p95), ms(x.perApp.p99),
			x.applyCalls, x.applyFailed, ms(x.applyAPI.p50), ms(x.applyAPI.p95), ms(x.applyAPI.p99), x.podsRunning, x.podsTotal,
			x.observeWall.Round(100*time.Millisecond), x.observeCalls, ms(x.observeAPI.p50), ms(x.observeAPI.p95), ms(x.observeAPI.p99),
			capRead, ms(x.oneMore), x.oneMoreCalls, ms(x.oneMoreAPI), x.etcdBytes/(1<<20), x.apiserverRSS/(1<<20))
	}
	b.WriteString("\n## Objects in the cluster\n\n| apps |")
	for _, k := range counted {
		b.WriteString(" " + k.name + " |")
	}
	b.WriteString("\n|---:|" + strings.Repeat("---:|", len(counted)) + "\n")
	for _, x := range rows {
		fmt.Fprintf(&b, "| %d |", x.apps)
		for _, k := range counted {
			fmt.Fprintf(&b, " %d |", x.counts[k.name])
		}
		b.WriteString("\n")
	}
	for _, x := range rows {
		fmt.Fprintf(&b, "\n## %d apps\n\n", x.apps)
		if x.stopped != "" {
			fmt.Fprintf(&b, "**Stopped:** %s.\n\n", x.stopped)
		}
		if x.capacityErr != "" {
			fmt.Fprintf(&b, "Capacity read failed: %s\n\n", x.capacityErr)
		}
		for _, e := range x.firstErrors {
			fmt.Fprintf(&b, "- %s\n", e)
		}
		fmt.Fprintf(&b, "\nPods Running %d of %d after waiting %s.\n\n### Calls while filling\n\n%s\n### Calls in one observe pass\n\n%s",
			x.podsRunning, x.podsTotal, x.runningWait.Round(time.Second), x.applyTable, x.observeTable)
	}
	t.Log("\n" + b.String())
	if r.s.out != "" {
		if err := os.WriteFile(r.s.out, []byte(b.String()), 0o600); err != nil {
			t.Errorf("writing %s: %v", r.s.out, err)
		}
	}
}
