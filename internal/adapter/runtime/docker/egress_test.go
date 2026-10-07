package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// world is a fake daemon that remembers: networks and containers are created,
// connected, inspected and removed, so a test can look at what Apply left
// behind rather than at the calls it made.
type world struct {
	t      *testing.T
	f      *fakeDaemon
	mu     sync.Mutex
	seq    int
	nets   map[string]*fakeNet // by ID
	ctrs   map[string]*fakeCtr // by ID
	images map[string]bool
}

type fakeNet struct {
	ID       string
	Name     string
	Internal bool
	Labels   map[string]string
	Members  map[string]bool // container IDs
}

type fakeCtr struct {
	ID       string
	Name     string
	Req      container.CreateRequest
	Running  bool
	Networks map[string][]string // network name -> aliases
	Path     string
	Args     []string
	Image    string
}

func newWorld(t *testing.T, config map[string]any) (*world, *Adapter) {
	t.Helper()
	f, a := newFakeDaemon(t, config)
	w := &world{t: t, f: f, nets: map[string]*fakeNet{}, ctrs: map[string]*fakeCtr{}, images: map[string]bool{}}

	f.on("GET /networks", w.listNetworks)
	f.on("POST /networks/create", w.createNetwork)
	f.on("GET /networks/*", w.inspectNetwork)
	f.on("DELETE /networks/*", w.removeNetwork)
	f.on("POST /networks/*", w.connectNetwork)
	f.on("GET /containers/json", w.listContainers)
	f.on("POST /containers/create", w.createContainer)
	f.on("GET /containers/*", w.inspectContainer)
	f.on("POST /containers/*", w.startContainer)
	f.on("DELETE /containers/*", w.removeContainer)
	f.on("GET /images/*", func(rw http.ResponseWriter, _ *http.Request) {
		writeJSON(rw, http.StatusOK, map[string]any{"Id": "sha256:img"})
	})
	f.on("POST /images/*", func(rw http.ResponseWriter, _ *http.Request) { rw.WriteHeader(http.StatusCreated) })
	f.on("GET /volumes", respond(http.StatusOK, map[string]any{"Volumes": []any{}}))
	return w, a
}

func (w *world) id(prefix string) string {
	w.seq++
	return fmt.Sprintf("%s%d", prefix, w.seq)
}

// lastSegment returns the path element after the resource, e.g. the ID in
// /networks/{id}/connect.
func pathID(r *http.Request, resource string) string {
	p := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
	rest := strings.TrimPrefix(p, "/"+resource+"/")
	id, _, _ := strings.Cut(rest, "/")
	return id
}

type filterSet map[string]map[string]bool

func filtersOf(r *http.Request) filterSet {
	out := filterSet{}
	if raw := r.URL.Query().Get("filters"); raw != "" {
		_ = json.Unmarshal([]byte(raw), &out)
	}
	return out
}

func (fs filterSet) labelsMatch(labels map[string]string) bool {
	for want := range fs["label"] {
		k, v, hasValue := strings.Cut(want, "=")
		got, ok := labels[k]
		if !ok || (hasValue && got != v) {
			return false
		}
	}
	return true
}

func (w *world) netByRef(ref string) *fakeNet {
	if n, ok := w.nets[ref]; ok {
		return n
	}
	for _, n := range w.nets {
		if n.Name == ref {
			return n
		}
	}
	return nil
}

func (w *world) ctrByRef(ref string) *fakeCtr {
	if c, ok := w.ctrs[ref]; ok {
		return c
	}
	for _, c := range w.ctrs {
		if c.Name == ref {
			return c
		}
	}
	return nil
}

func (w *world) netSummary(n *fakeNet) map[string]any {
	members := map[string]any{}
	for id := range n.Members {
		members[id] = map[string]any{"Name": w.ctrs[id].Name}
	}
	return map[string]any{"Id": n.ID, "Name": n.Name, "Internal": n.Internal, "Labels": n.Labels, "Containers": members,
		"IPAM": map[string]any{"Config": []any{}}}
}

func (w *world) listNetworks(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fs := filtersOf(r)
	out := []any{}
	for _, n := range w.nets {
		match := fs.labelsMatch(n.Labels)
		for name := range fs["name"] {
			match = match && strings.Contains(n.Name, name)
		}
		if match {
			s := w.netSummary(n)
			delete(s, "Containers") // NetworkList does not populate them
			out = append(out, s)
		}
	}
	writeJSON(rw, http.StatusOK, out)
}

func (w *world) createNetwork(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var req network.CreateRequest
	require.NoError(w.t, json.NewDecoder(r.Body).Decode(&req))
	if w.netByRef(req.Name) != nil {
		writeJSON(rw, http.StatusConflict, map[string]string{"message": "network with name " + req.Name + " already exists"})
		return
	}
	n := &fakeNet{ID: w.id("net"), Name: req.Name, Internal: req.Internal, Labels: req.Labels, Members: map[string]bool{}}
	w.nets[n.ID] = n
	writeJSON(rw, http.StatusCreated, map[string]string{"Id": n.ID})
}

func (w *world) inspectNetwork(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.netByRef(pathID(r, "networks"))
	if n == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "network not found"})
		return
	}
	writeJSON(rw, http.StatusOK, w.netSummary(n))
}

func (w *world) removeNetwork(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.netByRef(pathID(r, "networks"))
	if n == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "network not found"})
		return
	}
	if len(n.Members) > 0 {
		writeJSON(rw, http.StatusForbidden, map[string]string{"message": "error while removing network: network " + n.Name + " has active endpoints"})
		return
	}
	delete(w.nets, n.ID)
	rw.WriteHeader(http.StatusNoContent)
}

func (w *world) connectNetwork(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := w.netByRef(pathID(r, "networks"))
	var req network.ConnectRequest
	require.NoError(w.t, json.NewDecoder(r.Body).Decode(&req))
	c := w.ctrByRef(req.Container)
	if n == nil || c == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "No such container: " + req.Container})
		return
	}
	if n.Members[c.ID] {
		writeJSON(rw, http.StatusForbidden, map[string]string{"message": "endpoint with name " + c.Name + " already exists in network " + n.Name})
		return
	}
	var aliases []string
	if req.EndpointConfig != nil {
		aliases = req.EndpointConfig.Aliases
	}
	n.Members[c.ID] = true
	c.Networks[n.Name] = aliases
	rw.WriteHeader(http.StatusOK)
}

func (w *world) ctrSummary(c *fakeCtr) map[string]any {
	state := "exited"
	if c.Running {
		state = "running"
	}
	nets := map[string]any{}
	for name := range c.Networks {
		nets[name] = map[string]any{}
	}
	return map[string]any{"Id": c.ID, "Names": []string{"/" + c.Name}, "State": state, "Labels": c.Req.Labels,
		"NetworkSettings": map[string]any{"Networks": nets}}
}

func (w *world) listContainers(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fs := filtersOf(r)
	out := []any{}
	for _, c := range w.ctrs {
		labels := map[string]string{}
		if c.Req.Config != nil {
			labels = c.Req.Labels
		}
		if fs.labelsMatch(labels) {
			out = append(out, w.ctrSummary(c))
		}
	}
	writeJSON(rw, http.StatusOK, out)
}

func (w *world) createContainer(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	var req container.CreateRequest
	require.NoError(w.t, json.NewDecoder(r.Body).Decode(&req))
	name := r.URL.Query().Get("name")
	if w.ctrByRef(name) != nil {
		writeJSON(rw, http.StatusConflict, map[string]string{"message": "Conflict. The container name " + name + " is already in use"})
		return
	}
	c := &fakeCtr{ID: w.id("ctr"), Name: name, Req: req, Networks: map[string][]string{}, Image: req.Image}
	if req.NetworkingConfig != nil {
		for netName, ep := range req.NetworkingConfig.EndpointsConfig {
			n := w.netByRef(netName)
			require.NotNil(w.t, n, "created on a network that does not exist: %s", netName)
			n.Members[c.ID] = true
			c.Networks[n.Name] = ep.Aliases
		}
	}
	w.ctrs[c.ID] = c
	writeJSON(rw, http.StatusCreated, map[string]string{"Id": c.ID})
}

func (w *world) inspectContainer(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.ctrByRef(pathID(r, "containers"))
	if c == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "No such container"})
		return
	}
	nets := map[string]any{}
	for name, aliases := range c.Networks {
		nets[name] = map[string]any{"Aliases": aliases}
	}
	cfg := map[string]any{}
	if c.Req.Config != nil {
		cfg = map[string]any{"Image": c.Req.Image, "Env": c.Req.Env, "Labels": c.Req.Labels}
	}
	runtime := ""
	if c.Req.HostConfig != nil {
		runtime = c.Req.HostConfig.Runtime
	}
	writeJSON(rw, http.StatusOK, map[string]any{
		"Id": c.ID, "Name": "/" + c.Name, "Path": c.Path, "Args": c.Args, "Image": c.Image,
		"State":           map[string]any{"Running": c.Running, "Status": map[bool]string{true: "running", false: "exited"}[c.Running]},
		"Config":          cfg,
		"HostConfig":      map[string]any{"Runtime": runtime},
		"NetworkSettings": map[string]any{"Networks": nets},
	})
}

func (w *world) startContainer(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.ctrByRef(pathID(r, "containers"))
	if c == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "No such container"})
		return
	}
	if strings.HasSuffix(r.URL.Path, "/start") {
		c.Running = true
	}
	if strings.HasSuffix(r.URL.Path, "/stop") {
		c.Running = false
	}
	rw.WriteHeader(http.StatusNoContent)
}

func (w *world) removeContainer(rw http.ResponseWriter, r *http.Request) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.ctrByRef(pathID(r, "containers"))
	if c == nil {
		writeJSON(rw, http.StatusNotFound, map[string]string{"message": "No such container"})
		return
	}
	for _, n := range w.nets {
		delete(n.Members, c.ID)
	}
	delete(w.ctrs, c.ID)
	rw.WriteHeader(http.StatusNoContent)
}

// addContainer puts a container in the world that Pando did not create — a
// stand-in for Pando's own.
func (w *world) addContainer(name, image, path string, args ...string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := &fakeCtr{ID: w.id("self"), Name: name, Running: true, Networks: map[string][]string{},
		Path: path, Args: args, Image: image}
	w.ctrs[c.ID] = c
}

// detach takes a container off a network without asking the adapter — what
// a replaced Pando container's dead endpoint amounts to after a restart.
func (w *world) detach(container string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	c := w.ctrByRef(container)
	for _, n := range w.nets {
		delete(n.Members, c.ID)
	}
	c.Networks = map[string][]string{}
}

func (w *world) network(name string) *fakeNet {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.netByRef(name)
}

func (w *world) networkNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, n := range w.nets {
		out = append(out, n.Name)
	}
	sort.Strings(out)
	return out
}

func (w *world) container(name string) *fakeCtr {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ctrByRef(name)
}

func (w *world) containerNames() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for _, c := range w.ctrs {
		if c.Req.Config != nil {
			out = append(out, c.Name)
		}
	}
	sort.Strings(out)
	return out
}

func envOf(c *fakeCtr) map[string]string {
	out := map[string]string{}
	for _, kv := range c.Req.Env {
		k, v, _ := strings.Cut(kv, "=")
		out[k] = v
	}
	return out
}

var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy", "NO_PROXY", "no_proxy"}

func egressPlan(bundleID string, rules egress.Rules) api.BundlePlan {
	return api.BundlePlan{
		BundleID: bundleID,
		Network:  api.NetworkPlan{Private: true, Egress: rules},
		Labels:   map[string]string{"pando.app": "app_" + bundleID},
		Workloads: []api.WorkloadPlan{
			{Name: "web", Image: "nginx:1", Env: map[string]secret.Value{"PORT": secret.New("8080")}, DependsOn: []string{"db"}},
			{Name: "db", Image: "postgres:17"},
		},
	}
}

var allowStripe = egress.Rules{Layers: []egress.Layer{{Mode: egress.Allowlist, From: "install", List: []string{"api.stripe.com:443"}}}}

// TestR186_AnUnrestrictedAppRunsAsItAlwaysHas asserts R-186: with no
// restriction in effect nothing is put in the app's path — no gateway, no
// proxy variables, and the ordinary network with a route out, labeled as it
// always was.
func TestR186_AnUnrestrictedAppRunsAsItAlwaysHas(t *testing.T) {
	for name, rules := range map[string]egress.Rules{
		"no rules":                   {},
		"an install denylist, empty": {Layers: []egress.Layer{{Mode: egress.Denylist, From: "install"}}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			// A gateway image is known, so nothing below is for want of one.
			w, a := newWorld(t, map[string]any{"egress_gateway_image": "pando:test", "network_pool": "off"})

			_, err := a.Apply(ctx, egressPlan("b1", rules))
			require.NoError(t, err)

			require.Equal(t, []string{"pando-b1"}, w.networkNames())
			n := w.network("pando-b1")
			require.False(t, n.Internal, "the app's network has a route out")
			require.Equal(t, map[string]string{labelBundle: "b1", labelManaged: "true"}, n.Labels)

			require.Equal(t, []string{"pando-b1-db", "pando-b1-web"}, w.containerNames(), "no gateway")
			for _, c := range []string{"pando-b1-web", "pando-b1-db"} {
				env := envOf(w.container(c))
				for _, k := range proxyVars {
					require.NotContains(t, env, k, "%s has no %s", c, k)
				}
				require.Contains(t, w.container(c).Networks, "pando-b1")
			}
			require.Equal(t, "8080", envOf(w.container("pando-b1-web"))["PORT"])
		})
	}
}

// TestR187_ARestrictedAppLeavesOnlyThroughTheGateway asserts R-187: a
// restricted app's network has no route out, its gateway is on that network
// under the name its workloads' proxy variables give and on a network of its
// own that does have one, and it carries the rules it enforces.
func TestR187_ARestrictedAppLeavesOnlyThroughTheGateway(t *testing.T) {
	ctx := context.Background()
	w, a := newWorld(t, map[string]any{"egress_gateway_image": "pando:test", "network_pool": "off"})

	plan := egressPlan("b1", allowStripe)
	_, err := a.Apply(ctx, plan)
	require.NoError(t, err)

	require.Equal(t, []string{"pando-b1-internal", "pando-b1-outbound"}, w.networkNames())
	internal := w.network("pando-b1-internal")
	require.True(t, internal.Internal, "no route out of the app's network")
	require.Equal(t, egressNetworkInternal, internal.Labels[labelEgressNetwork])
	require.Equal(t, "b1", internal.Labels[labelBundle])
	outbound := w.network("pando-b1-outbound")
	require.False(t, outbound.Internal, "the gateway's own network has one")

	gw := w.container("pando-egress-b1")
	require.NotNil(t, gw)
	require.True(t, gw.Running)
	require.Equal(t, "pando:test", gw.Req.Image)
	require.Equal(t, []string{"/usr/local/bin/pando"}, []string(gw.Req.Entrypoint))
	require.Equal(t, []string{"egress-gateway"}, []string(gw.Req.Cmd))
	require.Equal(t, gatewayUser, gw.Req.User, "never root")
	require.True(t, gw.Req.HostConfig.ReadonlyRootfs)
	require.Equal(t, []string{"ALL"}, []string(gw.Req.HostConfig.CapDrop))
	require.Equal(t, container.RestartPolicyUnlessStopped, gw.Req.HostConfig.RestartPolicy.Name)
	require.Positive(t, gw.Req.HostConfig.Memory)
	require.Equal(t, []string{gatewayAlias}, gw.Networks["pando-b1-internal"])
	require.Contains(t, gw.Networks, "pando-b1-outbound")
	require.Equal(t, roleEgressGateway, gw.Req.Labels[labelRole])
	require.Equal(t, "b1", gw.Req.Labels[labelBundle])
	require.Equal(t, "true", gw.Req.Labels[labelManaged])
	require.NotEmpty(t, gw.Req.Labels[labelEgressDigest])

	var carried egress.Rules
	require.NoError(t, json.Unmarshal([]byte(envOf(gw)["PANDO_EGRESS_RULES"]), &carried))
	require.Equal(t, allowStripe, carried, "the gateway enforces exactly the plan's rules")

	for _, name := range []string{"pando-b1-web", "pando-b1-db"} {
		c := w.container(name)
		require.Contains(t, c.Networks, "pando-b1-internal")
		require.NotContains(t, c.Networks, "pando-b1-outbound", "a workload never has the gateway's route")
		env := envOf(c)
		require.Equal(t, "http://pando-egress:3128", env["HTTP_PROXY"])
		require.Equal(t, "http://pando-egress:3128", env["HTTPS_PROXY"])
		require.Equal(t, "http://pando-egress:3128", env["http_proxy"])
		require.Equal(t, "http://pando-egress:3128", env["https_proxy"])
		require.Equal(t, "localhost,127.0.0.1,::1,db,web", env["NO_PROXY"], "the bundle's workloads reach each other directly")
		require.Equal(t, env["NO_PROXY"], env["no_proxy"])
	}
	require.Equal(t, "8080", envOf(w.container("pando-b1-web"))["PORT"])

	t.Run("the gateway is not one of the app's workloads", func(t *testing.T) {
		observed, err := a.Observe(ctx, api.BundleRef{BundleID: "b1"})
		require.NoError(t, err)
		var names []string
		for _, o := range observed.Workloads {
			names = append(names, o.Name)
		}
		sort.Strings(names)
		require.Equal(t, []string{"db", "web"}, names)
	})

	t.Run("applying again changes nothing", func(t *testing.T) {
		creates := w.f.called("POST /containers/create")
		_, err := a.Apply(ctx, plan)
		require.NoError(t, err)
		require.Equal(t, creates, w.f.called("POST /containers/create"))
	})

	t.Run("an app's own NO_PROXY is kept beside the bundle's", func(t *testing.T) {
		own := egressPlan("b2", allowStripe)
		own.Workloads[0].Env["NO_PROXY"] = secret.New("internal.example")
		own.Workloads[0].Env["HTTPS_PROXY"] = secret.New("http://corporate:8080")
		_, err := a.Apply(ctx, own)
		require.NoError(t, err)
		env := envOf(w.container("pando-b2-web"))
		require.Equal(t, "localhost,127.0.0.1,::1,db,web,internal.example", env["NO_PROXY"])
		require.Equal(t, "http://pando-egress:3128", env["HTTPS_PROXY"], "the gateway is the only way out")
	})

	t.Run("deleting the app takes the gateway and every network", func(t *testing.T) {
		require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "b1"}, api.DestroyOptions{KeepVolumes: true}))
		for _, n := range w.networkNames() {
			require.NotContains(t, n, "b1")
		}
		for _, c := range w.containerNames() {
			require.NotContains(t, c, "b1")
		}
	})
}

// TestR187_ChangedRulesRecreateOnlyTheGateway asserts that new rules reach
// the gateway, and that the app's workloads — whose configuration does not
// depend on the rules — are left running.
func TestR187_ChangedRulesRecreateOnlyTheGateway(t *testing.T) {
	ctx := context.Background()
	w, a := newWorld(t, map[string]any{"egress_gateway_image": "pando:test", "network_pool": "off"})

	_, err := a.Apply(ctx, egressPlan("b1", allowStripe))
	require.NoError(t, err)
	web, db, gw := w.container("pando-b1-web").ID, w.container("pando-b1-db").ID, w.container("pando-egress-b1")

	tighter := egress.Rules{BlockPrivate: true, Layers: allowStripe.Layers}
	_, err = a.Apply(ctx, egressPlan("b1", tighter))
	require.NoError(t, err)

	after := w.container("pando-egress-b1")
	require.NotEqual(t, gw.ID, after.ID, "the gateway was recreated")
	require.NotEqual(t, gw.Req.Labels[labelEgressDigest], after.Req.Labels[labelEgressDigest])
	require.Contains(t, envOf(after)["PANDO_EGRESS_RULES"], `"block_private":true`)
	require.Equal(t, web, w.container("pando-b1-web").ID, "the workloads were not")
	require.Equal(t, db, w.container("pando-b1-db").ID)

	t.Run("and a gateway someone stopped is started, not recreated", func(t *testing.T) {
		w.mu.Lock()
		w.ctrByRef("pando-egress-b1").Running = false
		w.mu.Unlock()
		_, err := a.Apply(ctx, egressPlan("b1", tighter))
		require.NoError(t, err)
		require.Equal(t, after.ID, w.container("pando-egress-b1").ID)
		require.True(t, w.container("pando-egress-b1").Running)
	})
}

// TestR187_SwitchingPostureMovesTheAppsNetwork asserts that an app moving
// between unrestricted and restricted moves to a network with the other
// posture — Docker cannot change Internal in place — and that Pando's own
// container is never detached to make that happen.
func TestR187_SwitchingPostureMovesTheAppsNetwork(t *testing.T) {
	ctx := context.Background()
	w, a := newWorld(t, map[string]any{
		"egress_gateway_image": "pando:test", "network_pool": "off", "proxy_container": "pando-self",
	})
	w.addContainer("pando-self", "sha256:pando", "/usr/local/bin/entrypoint.sh", "/usr/local/bin/pando", "serve")

	_, err := a.Apply(ctx, egressPlan("b1", egress.Rules{}))
	require.NoError(t, err)
	require.Contains(t, w.container("pando-self").Networks, "pando-b1", "the proxy reaches the app")

	// Restricted.
	_, err = a.Apply(ctx, egressPlan("b1", allowStripe))
	require.NoError(t, err)
	for _, name := range []string{"pando-b1-web", "pando-b1-db"} {
		c := w.container(name)
		require.Equal(t, []string{"pando-b1-internal"}, keys(c.Networks), "%s moved to the internal network", name)
	}
	self := w.container("pando-self")
	require.Contains(t, self.Networks, "pando-b1-internal", "the proxy reaches it there")
	require.NotContains(t, self.Networks, "pando-b1-outbound", "and is not on the gateway's way out")
	require.Contains(t, self.Networks, "pando-b1", "and was not detached from the old network")
	require.NotNil(t, w.network("pando-b1"), "which is therefore left, for the next restart to collect")

	// After a restart: the old Pando container's endpoints are dead, and
	// ReclaimNetworks runs before RejoinNetworks.
	w.detach("pando-self")
	n, err := a.ReclaimNetworks(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"pando-b1-internal", "pando-b1-outbound"}, w.networkNames(),
		"the network the app left is collected; the ones it is on are not")
	joined, err := a.RejoinNetworks(ctx, nil)
	require.NoError(t, err)
	require.Equal(t, 1, joined, "the proxy rejoins the app's network, not the gateway's way out")
	require.Equal(t, []string{"pando-b1-internal"}, keys(w.container("pando-self").Networks))

	// And back.
	_, err = a.Apply(ctx, egressPlan("b1", egress.Rules{}))
	require.NoError(t, err)
	require.Nil(t, w.container("pando-egress-b1"), "the gateway is gone")
	require.Nil(t, w.network("pando-b1-outbound"), "and its way out")
	require.False(t, w.network("pando-b1").Internal)
	for _, name := range []string{"pando-b1-web", "pando-b1-db"} {
		c := w.container(name)
		require.Equal(t, []string{"pando-b1"}, keys(c.Networks))
		for _, k := range proxyVars {
			require.NotContains(t, envOf(c), k)
		}
	}
}

func keys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestR186_ARuntimeThatCannotRestrictSaysSo asserts R-186 and R-254: the
// capability is reported only when there is an image to run the gateway from,
// and a restricted plan that reaches the adapter anyway is refused, never run
// with its rules ignored.
func TestR186_ARuntimeThatCannotRestrictSaysSo(t *testing.T) {
	t.Run("Pando on the host, nothing configured", func(t *testing.T) {
		w, a := newWorld(t, map[string]any{"proxy_container": "not-a-container", "network_pool": "off"})
		ctx := context.Background()
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err)
		require.False(t, caps.SupportsEgressRestriction)

		_, err = a.Apply(ctx, egressPlan("b1", allowStripe))
		require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
		require.NotEmpty(t, errs.As(err).Remedy)
		require.Empty(t, w.networkNames(), "nothing was created")
		require.Empty(t, w.containerNames())

		// An unrestricted app is unaffected.
		_, err = a.Apply(ctx, egressPlan("b1", egress.Rules{}))
		require.NoError(t, err)
	})

	t.Run("Pando in a container: its own image", func(t *testing.T) {
		w, a := newWorld(t, map[string]any{"proxy_container": "pando-self", "network_pool": "off"})
		ctx := context.Background()
		w.addContainer("pando-self", "sha256:pandoimage", "/usr/local/bin/entrypoint.sh", "/usr/local/bin/pando", "serve")
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err)
		require.True(t, caps.SupportsEgressRestriction)

		_, err = a.Apply(ctx, egressPlan("b1", allowStripe))
		require.NoError(t, err)
		gw := w.container("pando-egress-b1")
		require.Equal(t, "sha256:pandoimage", gw.Req.Image, "by ID, so a moved tag cannot change it")
		require.Equal(t, []string{"/usr/local/bin/pando"}, []string(gw.Req.Entrypoint))
	})

	t.Run("a container that is not Pando", func(t *testing.T) {
		w, a := newWorld(t, map[string]any{"proxy_container": "pando-self"})
		ctx := context.Background()
		w.addContainer("pando-self", "sha256:alpine", "sleep", "3600")
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err)
		require.False(t, caps.SupportsEgressRestriction, "its image is not known to have Pando in it")
	})

	t.Run("configured", func(t *testing.T) {
		_, a := newWorld(t, map[string]any{"egress_gateway_image": "ghcr.io/trypando/pando:1"})
		ctx := context.Background()
		caps, err := a.Capabilities(ctx)
		require.NoError(t, err)
		require.True(t, caps.SupportsEgressRestriction)
	})

	t.Run("a rule the gateway could not read", func(t *testing.T) {
		w, a := newWorld(t, map[string]any{"egress_gateway_image": "pando:test", "network_pool": "off"})
		ctx := context.Background()
		_, err := a.Apply(ctx, egressPlan("b1", egress.Rules{Layers: []egress.Layer{{Mode: egress.Denylist, List: []string{"not valid"}}}}))
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
		require.Empty(t, w.containerNames())
	})
}

// TestR023_RejoiningCostsTheSameWhateverTheNumberOfApps asserts that the pass
// every replica runs every fifteen seconds asks the daemon a fixed number of
// things, not a few per app (issue #72), and asks whether an app is this
// install's only for a network it is about to join.
func TestR023_RejoiningCostsTheSameWhateverTheNumberOfApps(t *testing.T) {
	ctx := context.Background()
	w, a := newWorld(t, map[string]any{"network_pool": "off", "proxy_container": "pando-self"})
	w.addContainer("pando-self", "sha256:pando", "/usr/local/bin/pando", "serve")

	const apps = 20
	for i := range apps {
		_, err := a.Apply(ctx, egressPlan(fmt.Sprintf("b%d", i), egress.Rules{}))
		require.NoError(t, err)
	}

	asked := 0
	owns := func(string) bool { asked++; return true }

	w.f.mu.Lock()
	w.f.calls = nil
	w.f.mu.Unlock()
	joined, err := a.RejoinNetworks(ctx, owns)
	require.NoError(t, err)
	require.Zero(t, joined, "already on every network")
	require.Zero(t, asked, "and so nothing to ask the database about")
	w.f.mu.Lock()
	calls := len(w.f.calls)
	w.f.mu.Unlock()
	require.LessOrEqual(t, calls, 3, "one list of networks, one of containers, one inspect of itself: %v", w.f.calls)

	// A replaced Pando container is on none of them, and rejoins them all.
	w.detach("pando-self")
	joined, err = a.RejoinNetworks(ctx, owns)
	require.NoError(t, err)
	require.Equal(t, apps, joined)
	require.Equal(t, apps, asked)
	require.Len(t, w.container("pando-self").Networks, apps)
}
