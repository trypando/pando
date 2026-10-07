package multidocker

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// fakeHost stands in for one host's Docker adapter. Only what the multi-host
// adapter calls is implemented; anything else panics on the nil embedded
// interface, which a test would notice.
type fakeHost struct {
	api.RuntimeAdapter

	mu       sync.Mutex
	down     bool
	total    api.Fit
	free     api.Fit
	bundles  map[string]bool
	applied  []string
	observed []string
	running  int
	// own is what the one bundle on this host holds (RoomFor).
	own      api.Fit
	detached []string
}

var errDown = errors.New("connection refused")

func (f *fakeHost) Bundles(context.Context) (map[string]bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errDown
	}
	out := map[string]bool{}
	for b := range f.bundles {
		out[b] = true
	}
	return out, nil
}

func (f *fakeHost) Committed(context.Context) (api.Fit, error) { return api.Fit{}, nil }

func (f *fakeHost) Capacity(context.Context) (api.Capacity, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return api.Capacity{}, errDown
	}
	free := f.free
	return api.Capacity{TotalCPUMillis: f.total.CPUMillis, TotalMemoryBytes: f.total.MemoryBytes,
		RunningWorkloads: f.running, LargestFit: &free}, nil
}

func (f *fakeHost) HealthCheck(context.Context) error {
	if f.down {
		return errDown
	}
	return nil
}

func (f *fakeHost) Apply(_ context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return api.BundleHandle{}, errDown
	}
	if f.bundles == nil {
		f.bundles = map[string]bool{}
	}
	f.bundles[p.BundleID] = true
	f.applied = append(f.applied, p.BundleID)
	return api.BundleHandle{BundleID: p.BundleID}, nil
}

func (f *fakeHost) Observe(_ context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return api.ObservedBundle{}, errDown
	}
	f.observed = append(f.observed, ref.BundleID)
	return api.ObservedBundle{Exists: true,
		Volumes: []api.ObservedVolume{{VolumeID: "vol_1", Present: true, Handle: "pando-" + ref.BundleID + "-vol_1"}}}, nil
}

func (f *fakeHost) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	return api.Upstream{URL: "http://pando-" + ref.BundleID + "-" + ref.Workload + ":" + itoa(port)}, nil
}

func (f *fakeHost) DestroyVolume(_ context.Context, h api.VolumeHandle) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = append(f.observed, "destroy "+h.Handle)
	return nil
}

func (f *fakeHost) RoomFor(context.Context, string) (*api.Fit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		return nil, errDown
	}
	room := f.free
	room.CPUMillis += f.own.CPUMillis
	room.MemoryBytes += f.own.MemoryBytes
	return &room, nil
}

func (f *fakeHost) DetachProxy(_ context.Context, bundle string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.detached = append(f.detached, bundle)
	return nil
}

func (f *fakeHost) Destroy(context.Context, api.BundleRef, api.DestroyOptions) error { return nil }

func (f *fakeHost) RejoinNetworks(context.Context, func(string) bool) (int, error)  { return 0, nil }
func (f *fakeHost) ReclaimNetworks(context.Context, func(string) bool) (int, error) { return 0, nil }

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

const gib = int64(1 << 30)

// twoHosts is an adapter over a control host and two app hosts.
func newTestAdapter(t *testing.T, hosts map[string]*fakeHost, order ...string) *Adapter {
	t.Helper()
	caPEM, err := hostagent.NewAuthority()
	require.NoError(t, err)
	as, err := hostagent.ParseAuthorities(caPEM)
	require.NoError(t, err)
	cert, err := as.IssueClient()
	require.NoError(t, err)
	a := &Adapter{authorities: as, clientCert: clientCert{cert: cert}, placement: placements{byBundle: map[string]string{}}}
	for i, name := range order {
		h := &host{cfg: HostConfig{Name: name, Control: i == 0}, rt: hosts[name], agentAddr: name + ":7443"}
		if i == 0 {
			a.control = h
		}
		a.hosts = append(a.hosts, h)
	}
	return a
}

func plan(bundle string, memory int64) api.BundlePlan {
	return api.BundlePlan{BundleID: bundle, Network: api.NetworkPlan{Private: true},
		Workloads: []api.WorkloadPlan{{Name: "web", Resources: api.ResourcePlan{CPUMillis: 500, MemoryBytes: memory}}}}
}

// TestR256_ANewAppGoesToTheHostWithTheMostFreeMemoryThatFits asserts the
// placement rule in notes-multi-host-docker-issue-72.md, inside the adapter
// (R-256).
func TestR256_ANewAppGoesToTheHostWithTheMostFreeMemoryThatFits(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 1 * gib}},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 16 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 6 * gib}},
		"app-2":   {total: api.Fit{CPUMillis: 8000, MemoryBytes: 32 * gib}, free: api.Fit{CPUMillis: 200, MemoryBytes: 20 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1", "app-2")

	// app-2 has the most memory free, but not the half core this app asks
	// for; app-1 is the roomiest host where it fits.
	handle, err := a.Apply(context.Background(), plan("app_a", 2*gib))
	require.NoError(t, err)
	require.Equal(t, "app-1", handle.Handle)
	require.Equal(t, []string{"app_a"}, hosts["app-1"].applied)

	// A host closed to new apps is never chosen, however much room it has.
	a.hosts[1].cfg.NoPlacement = true
	hosts["app-2"].free.CPUMillis = 8000
	handle, err = a.Apply(context.Background(), plan("app_b", 2*gib))
	require.NoError(t, err)
	require.Equal(t, "app-2", handle.Handle)

	// Ties go to the host with fewer apps.
	a.hosts[1].cfg.NoPlacement = false
	hosts["app-1"].free = api.Fit{CPUMillis: 4000, MemoryBytes: 20 * gib}
	hosts["app-2"].bundles["app_x"] = true
	handle, err = a.Apply(context.Background(), plan("app_c", gib))
	require.NoError(t, err)
	require.Equal(t, "app-1", handle.Handle)
}

// TestR256_NoHostThatFitsIsARefusalNamingTheLargestFreeSpace asserts that a
// bundle is never split over hosts and never squeezed onto one without room.
func TestR256_NoHostThatFitsIsARefusalNamingTheLargestFreeSpace(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 2 * gib}},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 3 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	_, err := a.Apply(context.Background(), plan("app_big", 4*gib))
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "3.0 GB of memory")
	require.Contains(t, errs.As(err).Message, "app-1")
	require.Empty(t, hosts["app-1"].applied)
	require.Empty(t, hosts["control"].applied)
}

// TestR010_AnAppStaysOnItsHostAcrossRedeploys asserts that placement is
// sticky: an app on a host is applied there again even when another host now
// has more room, because its volumes are there (R-204, R-206) and Pando does
// not move apps (R-010, O-46).
func TestR010_AnAppStaysOnItsHostAcrossRedeploys(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 0, MemoryBytes: 0},
			bundles: map[string]bool{"app_kept": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	for range 3 {
		handle, err := a.Apply(context.Background(), plan("app_kept", gib))
		require.NoError(t, err)
		require.Equal(t, "app-1", handle.Handle)
	}
	require.Empty(t, hosts["control"].applied)

	// And a new adapter — a restarted replica — finds it there too, from the
	// host's own record.
	b := newTestAdapter(t, hosts, "control", "app-1")
	handle, err := b.Apply(context.Background(), plan("app_kept", gib))
	require.NoError(t, err)
	require.Equal(t, "app-1", handle.Handle)
}

// TestR148_AnUnreachableHostMarksItsAppsUnobservableNotFailed asserts design
// 05 §2 on several hosts: an app whose host does not answer is an
// observation error — which the reconciler records as unobservable and never
// counts toward failed (R-151) — and never "absent", which would have it
// created again on another host (O-46).
func TestR148_AnUnreachableHostMarksItsAppsUnobservableNotFailed(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib},
			bundles: map[string]bool{"app_known": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	ctx := context.Background()
	_, err := a.Observe(ctx, api.BundleRef{BundleID: "app_known"})
	require.NoError(t, err)

	hosts["app-1"].down = true

	// Known to be there: an error naming the host.
	_, err = a.Observe(ctx, api.BundleRef{BundleID: "app_known"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app-1")

	// Still known after the map is rebuilt while the host is down.
	a.refresh(ctx)
	_, err = a.Observe(ctx, api.BundleRef{BundleID: "app_known"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	// Not known — a restarted replica that never saw it: still an error,
	// because it might be on the host that did not answer.
	b := newTestAdapter(t, hosts, "control", "app-1")
	got, err := b.Observe(ctx, api.BundleRef{BundleID: "app_known"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.False(t, got.Exists)

	// And nothing is placed while a host is down: the app might be there.
	_, err = b.Apply(ctx, plan("app_known", gib))
	require.Error(t, err)
	require.Empty(t, hosts["control"].applied, "the app was not created again elsewhere")

	// With every host answering, an app no host has is simply absent.
	hosts["app-1"].down = false
	got, err = newTestAdapter(t, hosts, "control", "app-1").Observe(ctx, api.BundleRef{BundleID: "app_gone"})
	require.NoError(t, err)
	require.False(t, got.Exists)
}

// TestR243_CapacityReportsTheLargestPlaceAWorkloadFits asserts that totals
// sum the hosts open to new apps and LargestFit is the roomiest single host:
// 6 GB free over two hosts does not place a 4 GB workload.
func TestR243_CapacityReportsTheLargestPlaceAWorkloadFits(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 2000, MemoryBytes: 4 * gib}, free: api.Fit{CPUMillis: 1000, MemoryBytes: 3 * gib}, running: 2},
		"app-1":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 3000, MemoryBytes: 3 * gib}, running: 5},
		"app-2":   {total: api.Fit{CPUMillis: 8000, MemoryBytes: 16 * gib}, free: api.Fit{CPUMillis: 8000, MemoryBytes: 16 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1", "app-2")
	a.hosts[2].cfg.NoPlacement = true

	c, err := a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 6000, c.TotalCPUMillis)
	require.Equal(t, 12*gib, c.TotalMemoryBytes)
	require.Equal(t, 7, c.RunningWorkloads)
	require.NotNil(t, c.LargestFit)
	require.Equal(t, api.Fit{CPUMillis: 3000, MemoryBytes: 3 * gib}, *c.LargestFit,
		"the roomiest host open to new apps, not the sum, and not the closed one")
	require.Len(t, c.Details["hosts"], 3)

	// A host that does not answer is left out of the totals and shown as
	// unreachable.
	hosts["app-1"].down = true
	c, err = a.Capacity(context.Background())
	require.NoError(t, err)
	require.Equal(t, 4*gib, c.TotalMemoryBytes)
}

// TestR023_AnAppOnAnotherHostIsReachedThroughItsHostsAgent asserts the
// Upstream an app on this runtime gets: the container's name, which nothing
// on Pando's host resolves, and a Dial through the agent on the app's host
// (O-45). The proxy dials nothing else.
func TestR023_AnAppOnAnotherHostIsReachedThroughItsHostsAgent(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {},
		"app-1":   {bundles: map[string]bool{"app_web": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	up, err := a.Upstream(context.Background(), api.WorkloadRef{BundleID: "app_web", Workload: "web"}, 3000)
	require.NoError(t, err)
	require.Equal(t, "http://pando-app_web-web:3000", up.URL)
	require.NotNil(t, up.Dial)
	require.Equal(t, "app-1/pando-app_web-web:3000", up.PoolKey)

	// An app on no host has no upstream at all.
	_, err = a.Upstream(context.Background(), api.WorkloadRef{BundleID: "app_none", Workload: "web"}, 3000)
	require.Error(t, err)
}

func TestVolumeHandlesCarryTheirHost(t *testing.T) {
	hosts := map[string]*fakeHost{"control": {}, "app-1": {bundles: map[string]bool{"app_db": true}}}
	a := newTestAdapter(t, hosts, "control", "app-1")
	got, err := a.Observe(context.Background(), api.BundleRef{BundleID: "app_db"})
	require.NoError(t, err)
	require.Equal(t, "app-1/pando-app_db-vol_1", got.Volumes[0].Handle)

	require.NoError(t, a.DestroyVolume(context.Background(), api.VolumeHandle{Handle: got.Volumes[0].Handle}))
	require.Contains(t, hosts["app-1"].observed, "destroy pando-app_db-vol_1")

	require.Error(t, a.DestroyVolume(context.Background(), api.VolumeHandle{Handle: "gone/pando-x"}))
	require.Error(t, a.DestroyVolume(context.Background(), api.VolumeHandle{Handle: "no-host-part"}))
}

func TestConfigurationIsCheckedBeforeAnythingIsContacted(t *testing.T) {
	ca, err := hostagent.NewAuthority()
	require.NoError(t, err)
	good := func() map[string]any {
		return map[string]any{
			"hosts": []map[string]any{
				{"name": "control", "control": true, "agent_address": "10.0.0.5:7443", "endpoint": "unix:///var/run/docker.sock"},
				{"name": "app-1", "endpoint": "tcp://10.0.0.6:2376"},
			},
			"credentials": map[string]string{"agent_authority": string(ca), "docker_tls": "placeholder"},
		}
	}
	configure := func(cfg map[string]any) error {
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		return New().Configure(context.Background(), raw)
	}

	// The TLS bundle is not a real one, which Configure finds when it makes
	// the client: proof that validation passed first.
	err = configure(good())
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "app-1")

	for name, change := range map[string]func(map[string]any){
		"two control hosts": func(c map[string]any) {
			c["hosts"].([]map[string]any)[1]["control"] = true
		},
		"no control host": func(c map[string]any) {
			c["hosts"].([]map[string]any)[0]["control"] = false
		},
		"tcp without TLS": func(c map[string]any) {
			c["credentials"] = map[string]string{"agent_authority": string(ca)}
		},
		"ssh without a host key": func(c map[string]any) {
			c["hosts"].([]map[string]any)[1]["endpoint"] = "ssh://pando@10.0.0.6"
			c["credentials"] = map[string]string{"agent_authority": string(ca), "ssh_key": "k"}
		},
		"the app range turned off": func(c map[string]any) { c["network_pool"] = "off" },
		"no agent authority": func(c map[string]any) {
			c["credentials"] = map[string]string{"docker_tls": "placeholder"}
		},
		"a control host with no agent address": func(c map[string]any) {
			delete(c["hosts"].([]map[string]any)[0], "agent_address")
		},
		"a bad host name": func(c map[string]any) { c["hosts"].([]map[string]any)[1]["name"] = "App 1" },
	} {
		cfg := good()
		change(cfg)
		err := configure(cfg)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}

	// A form's text area sends the list as a string.
	cfg := good()
	list, _ := json.Marshal(cfg["hosts"])
	cfg["hosts"] = string(list)
	cfg["network_pool"] = "off"
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(configure(cfg)), "the string was read as the list")
}

func TestInfoIsAValidForm(t *testing.T) {
	require.NoError(t, Info().Validate())
}

// TestR256_AFirstDeployIsPlacedWhileAnotherHostIsUnreachable asserts that one
// silent host does not stop new apps going to the others: an app that has
// never deployed (BundlePlan.FirstDeploy) has nothing on the silent host.
func TestR256_AFirstDeployIsPlacedWhileAnotherHostIsUnreachable(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 2 * gib}},
		"app-1":   {down: true},
		"app-2":   {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 6 * gib}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1", "app-2")
	p := plan("app_new", gib)
	p.FirstDeploy = true
	handle, err := a.Apply(context.Background(), p)
	require.NoError(t, err)
	require.Equal(t, "app-2", handle.Handle)
}

// TestR010_ADeployedAppIsNeverPlacedElsewhereWhileAHostIsUnreachable asserts
// O-46's side of the same rule: an app that has deployed before may be on the
// silent host, so it is refused rather than created a second time elsewhere.
func TestR010_ADeployedAppIsNeverPlacedElsewhereWhileAHostIsUnreachable(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}},
		"app-1":   {down: true, bundles: map[string]bool{"app_old": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	_, err := a.Apply(context.Background(), plan("app_old", gib)) // FirstDeploy false
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app-1")
	require.Empty(t, hosts["control"].applied)
}

// TestR242_TheRoomForAnAppIsWhereItRunsWithWhatItHoldsThere asserts
// LargestFitFor: a placed app may go only to its host, and what it holds there
// counts as free; a new app may go to the roomiest host that answers.
func TestR242_TheRoomForAnAppIsWhereItRunsWithWhatItHoldsThere(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 6 * gib}},
		"app-1": {total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 500, MemoryBytes: gib / 2},
			own: api.Fit{CPUMillis: 1000, MemoryBytes: 2 * gib}, bundles: map[string]bool{"app_placed": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	got, err := a.LargestFitFor(context.Background(), "app_placed")
	require.NoError(t, err)
	require.Equal(t, api.Fit{CPUMillis: 1500, MemoryBytes: 2*gib + gib/2}, *got,
		"its own host only, though control has more free, with its own reservation counted as free")

	got, err = a.LargestFitFor(context.Background(), "app_new")
	require.NoError(t, err)
	require.Equal(t, api.Fit{CPUMillis: 4000, MemoryBytes: 6 * gib}, *got)

	// Its host silent: no answer, and Observe reports the host.
	hosts["app-1"].down = true
	got, err = a.LargestFitFor(context.Background(), "app_placed")
	require.NoError(t, err)
	require.Nil(t, got)
}

// TestR224_TheAgentLeavesADeletedAppsNetworksOnAnAppHost asserts that a
// deleted app's networks on an app host are not held by the agent until it is
// replaced: Destroy detaches it there, and only there — never on the control
// host, which may be Docker Desktop.
func TestR224_TheAgentLeavesADeletedAppsNetworksOnAnAppHost(t *testing.T) {
	hosts := map[string]*fakeHost{
		"control": {bundles: map[string]bool{"app_c": true}},
		"app-1":   {bundles: map[string]bool{"app_a": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1")
	ctx := context.Background()
	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_a"}, api.DestroyOptions{KeepVolumes: true}))
	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_c"}, api.DestroyOptions{KeepVolumes: true}))
	require.Equal(t, []string{"app_a"}, hosts["app-1"].detached)
	require.Empty(t, hosts["control"].detached)
}
