package kubernetes

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/remotecommand"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Kind is the adapter's kind string.
const Kind = "kubernetes"

// Labels and annotations Pando puts on what it creates.
const (
	labelManagedBy = "app.kubernetes.io/managed-by"
	managedBy      = "pando"

	labelApp      = "pando.dev/app"
	labelBundle   = "pando.dev/bundle"
	labelWorkload = "pando.dev/workload"
	labelVolume   = "pando.dev/volume"
	labelEdge     = "pando.dev/edge"

	// labelRole marks a pod in an app's namespace that is not one of the
	// app's workloads — the egress gateway, a backup helper — so Observe does
	// not report it as one.
	labelRole        = "pando.dev/role"
	roleGateway      = "egress-gateway"
	roleVolumeHelper = "volume-helper"

	// annoDigest summarizes everything a workload's pod was created from, so a
	// changed plan is a pod that no longer matches and is recreated (R-144).
	annoDigest = "pando.dev/plan-digest"

	// annoCreated is when Pando made a pod, to the nanosecond (newest).
	annoCreated = "pando.dev/created"

	// Pod Security Admission, enforced per app namespace: baseline refuses
	// privileged pods, host namespaces and hostPath, which is what the Docker
	// adapter allows an app and no more.
	labelPSAEnforce = "pod-security.kubernetes.io/enforce"

	// The labels Pando's own server pods carry, and the only pods an app's
	// namespace admits from outside itself (R-023).
	labelName      = "app.kubernetes.io/name"
	labelComponent = "app.kubernetes.io/component"
	componentEdge  = "edge"
	componentSrv   = "server"

	// appContainer is the container a workload's pod runs it in.
	appContainer = "app"
)

// Adapter runs workloads as pods on a Kubernetes cluster.
type Adapter struct {
	config Config
	cs     kubernetes.Interface
	rest   *rest.Config

	// mc reads metrics.k8s.io (usage.go). Nil reports no use.
	mc             metricsclient.Interface
	metricsMu      sync.Mutex
	metricsChecked time.Time
	metricsOK      bool

	// stream runs a command in a pod's container (Exec, SnapshotVolume,
	// RestoreVolume). The API server's exec subresource; replaced in tests.
	stream streamFunc

	// probe runs the NetworkPolicy canary (canary.go); replaced in tests.
	probe func(ctx context.Context) (bool, error)

	canaryMu sync.Mutex
	canary   canaryResult

	// poll is how often a wait looks again; scheduleWait is how long Apply
	// waits to see whether a new pod can be placed at all; dependencyWait is
	// how long a workload waits for a dependency to report ready (R-096).
	poll           time.Duration
	scheduleWait   time.Duration
	dependencyWait time.Duration

	now func() time.Time
}

// streamFunc runs cmd in one container of a pod, with the streams given.
type streamFunc func(ctx context.Context, namespace, pod, container string, cmd []string, opts remotecommand.StreamOptions) error

// Config is the adapter's configuration.
type Config struct {
	// Kubeconfig is a kubeconfig file. Empty is the in-cluster configuration,
	// which is how Pando runs: inside the cluster it deploys to, because its
	// proxy reaches a workload by a cluster DNS name.
	Kubeconfig string `json:"kubeconfig,omitempty"`
	Context    string `json:"context,omitempty"`

	// PandoNamespace is where Pando's server pods run, and PandoService the
	// Service in front of them. An app's namespace admits connections from
	// pods in PandoNamespace carrying Pando's server labels, and nothing else
	// from outside itself (R-023).
	PandoNamespace string `json:"pando_namespace,omitempty"`
	PandoService   string `json:"pando_service,omitempty"`
	ServiceAccount string `json:"service_account,omitempty"`

	// ProxyPort is the port Pando's proxy listens on in its pods, which the
	// edge is allowed to reach.
	ProxyPort int `json:"proxy_port,omitempty"`

	// AppRole is the ClusterRole bound to Pando's ServiceAccount inside each
	// app namespace it creates (deploy/kubernetes/rbac.yaml).
	AppRole string `json:"app_role,omitempty"`

	ClusterDomain string `json:"cluster_domain,omitempty"`

	// PodCIDR and ServiceCIDR are the cluster's address ranges. An app may
	// open connections out of the cluster, never into these: the API does not
	// report them reliably, so they are settings.
	PodCIDR     string `json:"pod_cidr"`
	ServiceCIDR string `json:"service_cidr"`

	// APIServerCIDR is the API server's address, which only the edge may
	// reach, when it falls inside an excluded range. Empty when it does not.
	APIServerCIDR string `json:"api_server_cidr,omitempty"`

	// StorageClass makes volumes; empty is the cluster's default class.
	StorageClass string `json:"storage_class,omitempty"`

	// VolumeSizeBytes is a volume's size when the request names none.
	VolumeSizeBytes int64 `json:"volume_size_bytes,omitempty"`

	// RuntimeClass runs app pods under a RuntimeClass, which decides the
	// isolation class reported (R-114, R-115): a handler of gVisor (runsc) or
	// Kata is `sandboxed`.
	RuntimeClass string `json:"runtime_class,omitempty"`

	// NodeSelector limits app pods, and the capacity reported, to nodes with
	// these labels, as "key=value,key=value".
	NodeSelector string `json:"node_selector,omitempty"`

	// EgressGatewayImage is an image with Pando's binary at
	// /usr/local/bin/pando, run as a restricted app's egress gateway (R-187).
	// Empty means egress cannot be restricted on this runtime (R-186).
	EgressGatewayImage string `json:"egress_gateway_image,omitempty"`

	// HelperImage runs the NetworkPolicy canary, the trial run's port
	// observer and volume backups: any image with a POSIX shell, tar, wget
	// and httpd, such as busybox.
	HelperImage string `json:"helper_image,omitempty"`

	// The edge (edge.go).
	EdgeNamespace     string `json:"edge_namespace,omitempty"`
	EdgeReplicas      int    `json:"edge_replicas,omitempty"`
	EdgeServiceType   string `json:"edge_service_type,omitempty"`
	EdgeHTTPNodePort  int    `json:"edge_http_node_port,omitempty"`
	EdgeHTTPSNodePort int    `json:"edge_https_node_port,omitempty"`
}

// Defaults.
const (
	defaultPandoNamespace = "pando"
	defaultPandoService   = "pando"
	defaultServiceAccount = "pando"
	defaultProxyPort      = 8080
	defaultAppRole        = "pando-app-manager"
	defaultClusterDomain  = "cluster.local"
	defaultVolumeSize     = int64(10 << 30)
	defaultHelperImage    = "busybox:1.37"
	defaultEdgeNamespace  = "pando-edge"
	defaultEdgeReplicas   = 2
	defaultHTTPNodePort   = 30080
	defaultHTTPSNodePort  = 30443
)

// New builds an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryRuntime }

// Configure reads the settings and connects to the cluster's API.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The Kubernetes runtime's configuration could not be read.", err)
		}
	}
	if err := cfg.validate(); err != nil {
		return err
	}

	var (
		rc  *rest.Config
		err error
	)
	if cfg.Kubeconfig != "" {
		rules := &clientcmd.ClientConfigLoadingRules{ExplicitPath: cfg.Kubeconfig}
		overrides := &clientcmd.ConfigOverrides{CurrentContext: cfg.Context}
		rc, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, overrides).ClientConfig()
		if err != nil {
			return errs.Wrap(errs.ValidInvalid,
				fmt.Sprintf("The Kubernetes runtime could not read the kubeconfig file at %s.", cfg.Kubeconfig), err).
				WithRemedy("Check the file exists and is readable by Pando, or clear the kubeconfig setting when Pando runs inside the cluster.")
		}
	} else {
		rc, err = rest.InClusterConfig()
		if err != nil {
			return errs.Wrap(errs.AdapterUnavailable,
				"The Kubernetes runtime has no kubeconfig file set, and Pando is not running inside a Kubernetes cluster.", err).
				WithRemedy("Run Pando inside the cluster it deploys to, as deploy/kubernetes does, or set the kubeconfig setting to a kubeconfig file.")
		}
	}
	cs, err := kubernetes.NewForConfig(rc)
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not set up the connection to the Kubernetes API.", err)
	}
	a.rest = rc
	if mc, err := metricsclient.NewForConfig(rc); err == nil {
		a.mc = mc
	}
	a.use(cs, cfg)
	return nil
}

// use installs a client and a validated configuration.
func (a *Adapter) use(cs kubernetes.Interface, cfg Config) {
	a.cs = cs
	a.config = cfg
	if a.stream == nil {
		a.stream = a.apiStream
	}
	if a.probe == nil {
		a.probe = a.runCanary
	}
	if a.poll == 0 {
		a.poll = 2 * time.Second
	}
	if a.scheduleWait == 0 {
		a.scheduleWait = 20 * time.Second
	}
	if a.dependencyWait == 0 {
		a.dependencyWait = 2 * time.Minute
	}
	if a.now == nil {
		a.now = func() time.Time { return time.Now().UTC() }
	}
}

// validate fills defaults and refuses a configuration that cannot work.
func (c *Config) validate() error {
	if c.PandoNamespace == "" {
		c.PandoNamespace = defaultPandoNamespace
	}
	if c.PandoService == "" {
		c.PandoService = defaultPandoService
	}
	if c.ServiceAccount == "" {
		c.ServiceAccount = defaultServiceAccount
	}
	if c.ProxyPort == 0 {
		c.ProxyPort = defaultProxyPort
	}
	if c.AppRole == "" {
		c.AppRole = defaultAppRole
	}
	if c.ClusterDomain == "" {
		c.ClusterDomain = defaultClusterDomain
	}
	if c.VolumeSizeBytes == 0 {
		c.VolumeSizeBytes = defaultVolumeSize
	}
	if c.HelperImage == "" {
		c.HelperImage = defaultHelperImage
	}
	if c.EdgeNamespace == "" {
		c.EdgeNamespace = defaultEdgeNamespace
	}
	if c.EdgeReplicas == 0 {
		c.EdgeReplicas = defaultEdgeReplicas
	}
	if c.EdgeServiceType == "" {
		c.EdgeServiceType = string(corev1.ServiceTypeLoadBalancer)
	}
	if c.EdgeHTTPNodePort == 0 {
		c.EdgeHTTPNodePort = defaultHTTPNodePort
	}
	if c.EdgeHTTPSNodePort == 0 {
		c.EdgeHTTPSNodePort = defaultHTTPSNodePort
	}

	for _, r := range []struct{ key, value string }{
		{"pod_cidr", c.PodCIDR}, {"service_cidr", c.ServiceCIDR},
	} {
		if r.value == "" {
			return errs.Newf(errs.ValidInvalid,
				"The Kubernetes runtime needs the cluster's %s, so that apps can be kept from opening connections into the cluster. It is not set.", r.key).
				WithRemedy("Set it to the range the cluster gives pods or Services, such as 10.244.0.0/16 for pods or 10.96.0.0/12 for Services. `kubectl cluster-info dump | grep -m1 -e cluster-cidr -e service-cluster-ip-range` shows them on most clusters.")
		}
		if _, _, err := net.ParseCIDR(r.value); err != nil {
			return errs.Newf(errs.ValidInvalid, "The Kubernetes runtime's %s is %q, which is not an address range such as 10.244.0.0/16.", r.key, r.value)
		}
	}
	if c.APIServerCIDR != "" {
		if _, _, err := net.ParseCIDR(c.APIServerCIDR); err != nil {
			return errs.Newf(errs.ValidInvalid, "The Kubernetes runtime's api_server_cidr is %q, which is not an address range such as 10.0.0.1/32.", c.APIServerCIDR)
		}
	}
	if c.EdgeReplicas < 2 {
		// The edge is in front of every app, and issue #72 is about the
		// install surviving the loss of any one process.
		return errs.Newf(errs.ValidInvalid,
			"The Kubernetes runtime's edge_replicas is %d. The edge runs at least 2 replicas, so that losing one does not take every app offline.", c.EdgeReplicas)
	}
	switch corev1.ServiceType(c.EdgeServiceType) {
	case corev1.ServiceTypeLoadBalancer, corev1.ServiceTypeNodePort:
	default:
		return errs.Newf(errs.ValidInvalid,
			"The Kubernetes runtime's edge_service_type is %q. Valid answers: LoadBalancer, or NodePort when the cluster has no load balancer and one outside it points at the nodes.", c.EdgeServiceType)
	}
	if _, err := parseSelector(c.NodeSelector); err != nil {
		return err
	}
	if c.ProxyPort < 1 || c.ProxyPort > 65535 {
		return errs.Newf(errs.ValidInvalid, "%d is not a port number. A port is between 1 and 65535.", c.ProxyPort)
	}
	return nil
}

// parseSelector reads "key=value,key=value".
func parseSelector(s string) (map[string]string, error) {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			return nil, errs.Newf(errs.ValidInvalid,
				"The Kubernetes runtime's node_selector has %q, which is not of the form key=value. Separate several with commas.", part)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

// HealthCheck says whether the cluster can be used: the API answers, the
// configured RuntimeClass exists, and the network plugin enforces
// NetworkPolicy, which every isolation guarantee here rests on (O-43).
func (a *Adapter) HealthCheck(ctx context.Context) error {
	if a.cs == nil {
		return errs.New(errs.AdapterUnavailable, "The Kubernetes runtime has not been set up.")
	}
	if _, err := a.cs.Discovery().ServerVersion(); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "The Kubernetes API server is not responding.", err)
	}
	if a.config.RuntimeClass != "" {
		if _, err := a.runtimeClassHandler(ctx); err != nil {
			return err
		}
	}
	enforced, err := a.networkPolicyEnforced(ctx)
	if err != nil {
		return err
	}
	if !enforced {
		return errNotEnforced()
	}
	return nil
}

// runtimeClassHandler is the handler of the configured RuntimeClass.
func (a *Adapter) runtimeClassHandler(ctx context.Context) (string, error) {
	rc, err := a.cs.NodeV1().RuntimeClasses().Get(ctx, a.config.RuntimeClass, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return "", errs.Newf(errs.AdapterUnavailable,
			"The Kubernetes runtime is set to run apps under the RuntimeClass %q, and the cluster has no RuntimeClass by that name.",
			a.config.RuntimeClass).
			WithRemedy("Install the runtime on the nodes and create its RuntimeClass, or clear the runtime_class setting to use the cluster's default.")
	}
	if err != nil {
		return "", errs.Wrap(errs.AdapterUnavailable, "Could not ask the cluster about its RuntimeClasses.", err)
	}
	return rc.Handler, nil
}

// isolationOf is the class a RuntimeClass handler provides (R-115). Only a
// handler known to put a boundary between the app and the node's kernel
// raises it. The adapter checks the RuntimeClass exists; what its handler
// really runs is the node's, the same trust the Docker adapter places in a
// configured OCI runtime name (R-114).
func isolationOf(handler string) spec.IsolationClass {
	name := strings.ToLower(handler)
	switch {
	case name == "runsc" || strings.HasPrefix(name, "runsc-") || strings.Contains(name, "gvisor"):
		return spec.IsolationSandboxed
	case strings.HasPrefix(name, "kata"):
		return spec.IsolationSandboxed
	default:
		return spec.IsolationContainer
	}
}

// Capabilities reports what this adapter can do (R-254).
func (a *Adapter) Capabilities(ctx context.Context) (api.RuntimeCapabilities, error) {
	class := spec.IsolationContainer
	if a.config.RuntimeClass != "" && a.cs != nil {
		if handler, err := a.runtimeClassHandler(ctx); err == nil {
			class = isolationOf(handler)
		}
	}
	// Only the canary's answer, never a guess: until it has run and passed,
	// this runtime cannot keep apps apart, and the planner refuses it (R-026).
	enforced := a.canaryPassed()

	return api.RuntimeCapabilities{
		IsolationClass: class,

		SupportsPersistentVolumes: true,
		SupportsExec:              true,
		SupportsMultipleWorkloads: true, // a pod per workload
		SupportsCarriedFiles:      true, // a ConfigMap per workload
		SupportsPrivateNetwork:    enforced,
		SupportsResourceLimits:    true,
		SupportsStartThenSwap:     false,

		// From metrics.k8s.io when the cluster serves it (usage.go); without
		// it the console says the runtime does not report use rather than
		// showing zeros (R-245).
		ReportsUsage: a.metricsAvailable(ctx),

		// The kubelet rotates every container's log at one node-wide size;
		// there is no per-workload cap to set (R-222).
		LogRetention: api.LogRetentionCapability{SupportsSizeCap: false},

		// Every node pulls; there is no daemon to stream an image into.
		ImageDelivery: []api.ImageDelivery{api.ImageDeliveryRegistry},

		// Pando is upgraded by changing the image on its Deployment.
		SupportsSelfUpgrade: false,

		SupportsTrialRun: true,
		// A sidecar in the trial pod shares its network namespace and reads
		// the listening sockets. A sandbox has its own network stack, which
		// the sidecar cannot see into.
		SupportsPortObservation: class == spec.IsolationContainer,
		// Nothing like a container diff exists in the API.
		SupportsWriteObservation: false,

		SupportsEdge: true,
		EdgeConfig:   []api.EdgeConfig{api.EdgeConfigKubernetesAPI},

		SupportsEgressRestriction: enforced && a.config.EgressGatewayImage != "",

		Platform: a.platform(ctx),
	}, nil
}

// platform is the eligible nodes' os/arch when they agree, and empty when
// they do not or cannot be read.
func (a *Adapter) platform(ctx context.Context) string {
	if a.cs == nil {
		return ""
	}
	nodes, err := a.eligibleNodes(ctx)
	if err != nil || len(nodes) == 0 {
		return ""
	}
	seen := ""
	for _, n := range nodes {
		p := n.Status.NodeInfo.OperatingSystem + "/" + n.Status.NodeInfo.Architecture
		if seen != "" && p != seen {
			return ""
		}
		seen = p
	}
	if seen == "/" {
		return ""
	}
	return seen
}

// eligibleNodes are the nodes an app pod may be placed on: schedulable, ready,
// not tainted against ordinary pods, and matching the node selector.
func (a *Adapter) eligibleNodes(ctx context.Context) ([]corev1.Node, error) {
	sel, _ := parseSelector(a.config.NodeSelector)
	list, err := a.cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	var out []corev1.Node
	for _, n := range list.Items {
		if n.Spec.Unschedulable || !nodeReady(n) || tainted(n) {
			continue
		}
		matches := true
		for k, v := range sel {
			if n.Labels[k] != v {
				matches = false
			}
		}
		if matches {
			out = append(out, n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func nodeReady(n corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// tainted reports a taint an app pod does not tolerate. App pods carry no
// tolerations.
func tainted(n corev1.Node) bool {
	for _, t := range n.Spec.Taints {
		if t.Effect == corev1.TaintEffectNoSchedule || t.Effect == corev1.TaintEffectNoExecute {
			return true
		}
	}
	return false
}

// Capacity is adapter-reported (R-243): what the eligible nodes can hold,
// less what pods outside Pando's namespaces already ask for on them, since a
// cluster also runs things that are not Pando's. The planner takes Pando's own
// allocations off the totals itself.
//
// LargestFit is the one node with the most room left once every pod's
// requests are counted, Pando's too, since the planner compares it as it is
// (R-242).
func (a *Adapter) Capacity(ctx context.Context) (api.Capacity, error) {
	return a.capacity(ctx, "")
}

// LargestFitFor is the roomiest node for one bundle: what each node has left,
// plus what the bundle's own pods ask for there, since a redeploy replaces
// them, so a redeploy that fits where the app runs is never refused (R-242).
// Nil when the cluster has no eligible node, and the totals check refuses.
func (a *Adapter) LargestFitFor(ctx context.Context, bundleID string) (*api.Fit, error) {
	c, err := a.capacity(ctx, namespaceFor(bundleID))
	if err != nil {
		return nil, err
	}
	return c.LargestFit, nil
}

// capacity reads the cluster's room. ownNS, when set, is a bundle's namespace,
// whose pods' requests count as free in LargestFit.
func (a *Adapter) capacity(ctx context.Context, ownNS string) (api.Capacity, error) {
	nodes, err := a.eligibleNodes(ctx)
	if err != nil {
		return api.Capacity{}, errs.Wrap(errs.AdapterUnavailable, "Could not read how much room the cluster has.", err)
	}
	ours, err := a.pandoNamespaces(ctx)
	if err != nil {
		return api.Capacity{}, errs.Wrap(errs.AdapterUnavailable, "Could not read how much room the cluster has.", err)
	}
	pods, err := a.cs.CoreV1().Pods(metav1.NamespaceAll).List(ctx, metav1.ListOptions{})
	if err != nil {
		return api.Capacity{}, errs.Wrap(errs.AdapterUnavailable, "Could not read how much room the cluster has.", err)
	}

	type room struct{ cpu, mem, otherCPU, otherMem, pandoCPU, pandoMem int64 }
	byNode := map[string]*room{}
	for _, n := range nodes {
		byNode[n.Name] = &room{
			cpu: n.Status.Allocatable.Cpu().MilliValue(),
			mem: n.Status.Allocatable.Memory().Value(),
		}
	}
	running := 0
	for _, p := range pods.Items {
		r, ok := byNode[p.Spec.NodeName]
		if !ok || p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			running++
		}
		cpu, mem := podRequests(p)
		switch {
		case ownNS != "" && p.Namespace == ownNS:
			// The bundle being planned: what it holds is free for it.
		case ours[p.Namespace]:
			r.pandoCPU += cpu
			r.pandoMem += mem
		default:
			r.otherCPU += cpu
			r.otherMem += mem
		}
	}

	capacity := api.Capacity{RunningWorkloads: running, Reported: a.now()}
	perNode := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		r := byNode[n.Name]
		cpu, mem := max(r.cpu-r.otherCPU, 0), max(r.mem-r.otherMem, 0)
		capacity.TotalCPUMillis += int(cpu)
		capacity.TotalMemoryBytes += mem
		free := api.Fit{CPUMillis: int(max(cpu-r.pandoCPU, 0)), MemoryBytes: max(mem-r.pandoMem, 0)}
		if f := capacity.LargestFit; f == nil || free.MemoryBytes > f.MemoryBytes ||
			(free.MemoryBytes == f.MemoryBytes && free.CPUMillis > f.CPUMillis) {
			capacity.LargestFit = &free
		}
		perNode = append(perNode, map[string]any{
			"node":                       n.Name,
			"allocatable_cpu_millis":     r.cpu,
			"allocatable_memory_bytes":   r.mem,
			"requested_by_others_millis": r.otherCPU,
			"requested_by_others_bytes":  r.otherMem,
			"kubelet_version":            n.Status.NodeInfo.KubeletVersion,
			"architecture":               n.Status.NodeInfo.Architecture,
		})
	}
	capacity.Details = map[string]any{
		"nodes":          perNode,
		"eligible_nodes": len(nodes),
		"note":           "Totals are the eligible nodes' allocatable CPU and memory, less what pods outside Pando's namespaces request on them. Volume storage comes from the cluster's storage class and is not counted.",
	}
	if a.config.NodeSelector != "" {
		capacity.Details["node_selector"] = a.config.NodeSelector
	}
	return capacity, nil
}

// podRequests sums a pod's containers' CPU and memory requests.
func podRequests(p corev1.Pod) (cpu, mem int64) {
	for _, c := range p.Spec.Containers {
		cpu += c.Resources.Requests.Cpu().MilliValue()
		mem += c.Resources.Requests.Memory().Value()
	}
	return cpu, mem
}

// pandoNamespaces are the namespaces holding Pando's own work: its own, the
// edge's, and every app's.
func (a *Adapter) pandoNamespaces(ctx context.Context) (map[string]bool, error) {
	list, err := a.cs.CoreV1().Namespaces().List(ctx, metav1.ListOptions{LabelSelector: labelManagedBy + "=" + managedBy})
	if err != nil {
		return nil, err
	}
	out := map[string]bool{a.config.PandoNamespace: true, a.config.EdgeNamespace: true}
	for _, n := range list.Items {
		out[n.Name] = true
	}
	return out, nil
}

func errNoUsage() error {
	return errs.New(errs.PlanCapabilityUnsupported, "This cluster does not report what apps are using: it serves no metrics API.").
		WithRemedy("Install metrics-server in the cluster.")
}

// ImportImage is not offered: every node pulls from a registry.
func (a *Adapter) ImportImage(context.Context, io.Reader) (string, error) {
	return "", errs.New(errs.PlanCapabilityUnsupported,
		"The Kubernetes runtime pulls every image from a registry and cannot take a built image directly.").
		WithRemedy("Set PANDO_REGISTRY_URL to the registry Pando should push built images to.")
}

// --- names -----------------------------------------------------------------

var dnsLabel = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)

// namespaceFor is an app's namespace: "pando-" and its ID, lowercased.
func namespaceFor(bundleID string) string { return "pando-" + toLabel(bundleID) }

// toLabel turns an identifier into a DNS label: lowercase, with anything but
// letters, digits and hyphens turned into a hyphen, at most 63 characters.
func toLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 63 {
		out = strings.TrimRight(out[:63], "-")
	}
	return out
}

// checkWorkloadName refuses a workload name that cannot name a Service.
func checkWorkloadName(name string) error {
	if len(name) <= 63 && dnsLabel.MatchString(name) {
		return nil
	}
	suggestion := toLabel(name)
	if suggestion == "" || !dnsLabel.MatchString(suggestion) {
		suggestion = "web"
	}
	return errs.Newf(errs.ValidInvalid,
		"On the Kubernetes runtime each part of an app is named by a DNS label, and %q is not one.", name).
		WithRemedy(fmt.Sprintf("Rename it to %q: lowercase letters, digits and hyphens, starting with a letter, at most 63 characters.", suggestion))
}

func pvcName(volumeID string) string       { return "pando-" + toLabel(volumeID) }
func envSecretName(workload string) string { return "pando-env-" + workload }
func filesName(workload string) string     { return "pando-files-" + workload }

const (
	pullSecretName   = "pando-pull"
	defaultPolicy    = "pando-default"
	gatewayPolicy    = "pando-egress-gateway"
	quotaName        = "pando-quota"
	roleBindingName  = "pando-app-manager"
	volumeHandleSep  = "/"
	volumeMountInPod = "/data"
)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryRuntime,
		Kind:        Kind,
		Name:        "Kubernetes",
		Description: "Runs apps as pods on the Kubernetes cluster Pando runs in, one namespace per app.",
		IDPrefix:    "rt_",
		Fields: []api.Field{
			{Key: "pod_cidr", Label: "Pod address range", Type: "string", Required: true, Placeholder: "10.244.0.0/16",
				Help: "The range the cluster gives pods. Apps may open connections out of the cluster, never into this range."},
			{Key: "service_cidr", Label: "Service address range", Type: "string", Required: true, Placeholder: "10.96.0.0/12",
				Help: "The range the cluster gives Services. Apps may not open connections into it."},
			{Key: "egress_gateway_image", Label: "Egress gateway image", Type: "string", Placeholder: "ghcr.io/trypando/pando:latest",
				Help: "An image with Pando's binary at /usr/local/bin/pando, run in front of an app whose egress rules restrict anything. Without one, such apps cannot be deployed."},
			{Key: "kubeconfig", Label: "Kubeconfig file", Type: "string", Advanced: true, Default: "The cluster Pando runs in",
				Help: "A kubeconfig file, for running Pando outside the cluster during development. Pando's proxy reaches apps by cluster DNS names, so in production Pando runs inside the cluster."},
			{Key: "context", Label: "Kubeconfig context", Type: "string", Advanced: true, Default: "The file's current context"},
			{Key: "pando_namespace", Label: "Pando's namespace", Type: "string", Advanced: true, Default: defaultPandoNamespace,
				Help: "Where Pando's server pods run. Apps admit connections only from Pando's server pods in this namespace."},
			{Key: "pando_service", Label: "Pando's Service", Type: "string", Advanced: true, Default: defaultPandoService},
			{Key: "service_account", Label: "Pando's ServiceAccount", Type: "string", Advanced: true, Default: defaultServiceAccount},
			{Key: "proxy_port", Label: "Proxy port", Type: "int", Advanced: true, Default: "8080", Help: "The port Pando's proxy listens on in its pods."},
			{Key: "app_role", Label: "App namespace role", Type: "string", Advanced: true, Default: defaultAppRole,
				Help: "The ClusterRole bound to Pando's ServiceAccount in each app namespace."},
			{Key: "cluster_domain", Label: "Cluster domain", Type: "string", Advanced: true, Default: defaultClusterDomain},
			{Key: "api_server_cidr", Label: "API server address", Type: "string", Advanced: true, Default: "Reached by the rule for addresses outside the cluster",
				Help: "Only needed when the API server's address is inside the pod or Service range: the edge reads its routes from it."},
			{Key: "storage_class", Label: "Storage class", Type: "string", Advanced: true, Default: "The cluster's default class"},
			{Key: "volume_size_bytes", Label: "Volume size", Type: "int", Advanced: true, Default: "10737418240 (10 GiB)",
				Help: "Bytes for a volume whose app does not say how large it is."},
			{Key: "runtime_class", Label: "RuntimeClass", Type: "string", Advanced: true, Default: "The cluster's default",
				Help: "A RuntimeClass to run apps under. One whose handler is gVisor (runsc) or Kata makes this a sandboxed runtime, which host policy can require."},
			{Key: "node_selector", Label: "Node selector", Type: "string", Advanced: true, Default: "Every schedulable node",
				Help: "Run apps only on nodes with these labels, as key=value pairs separated by commas."},
			{Key: "helper_image", Label: "Helper image", Type: "string", Advanced: true, Default: defaultHelperImage,
				Help: "Runs the network policy check, the port observer and volume backups. Any image with a shell, tar, wget and httpd."},
			{Key: "edge_namespace", Label: "Edge namespace", Type: "string", Advanced: true, Default: defaultEdgeNamespace},
			{Key: "edge_replicas", Label: "Edge replicas", Type: "int", Advanced: true, Default: "2",
				Help: "How many copies of the edge run, spread across nodes. At least 2."},
			{Key: "edge_service_type", Label: "Edge Service type", Type: "select", Advanced: true, Default: string(corev1.ServiceTypeLoadBalancer),
				Options: []api.Option{
					{Value: string(corev1.ServiceTypeLoadBalancer), Label: "LoadBalancer", Description: "The cluster's load balancer publishes ports 80 and 443."},
					{Value: string(corev1.ServiceTypeNodePort), Label: "NodePort", Description: "Every node listens on the node ports below, and a load balancer outside the cluster points at them."},
				}},
			{Key: "edge_http_node_port", Label: "Edge HTTP node port", Type: "int", Advanced: true, Default: "30080",
				ShownWhen: &api.Condition{Key: "edge_service_type", Values: []string{string(corev1.ServiceTypeNodePort)}}},
			{Key: "edge_https_node_port", Label: "Edge HTTPS node port", Type: "int", Advanced: true, Default: "30443",
				ShownWhen: &api.Condition{Key: "edge_service_type", Values: []string{string(corev1.ServiceTypeNodePort)}}},
		},
	}
}

var _ api.RuntimeAdapter = (*Adapter)(nil)
