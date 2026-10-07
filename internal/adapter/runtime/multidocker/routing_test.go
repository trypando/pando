package multidocker

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// recHost is a fakeHost that also answers what the adapter routes to an
// app's host, and records each call.
type recHost struct {
	*fakeHost

	platform string
	inUse    api.InUse

	rejoinN, reclaimN     int
	rejoinErr, reclaimErr error

	callMu sync.Mutex
	calls  []string
}

func (r *recHost) record(call string) {
	r.callMu.Lock()
	r.calls = append(r.calls, call)
	r.callMu.Unlock()
}

func (r *recHost) got() []string {
	r.callMu.Lock()
	defer r.callMu.Unlock()
	return append([]string(nil), r.calls...)
}

func (r *recHost) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	if r.down {
		return api.RuntimeCapabilities{}, errDown
	}
	return api.RuntimeCapabilities{Platform: r.platform}, nil
}

func (r *recHost) InUse(context.Context) (api.InUse, error) {
	if r.down {
		return api.InUse{}, errDown
	}
	return r.inUse, nil
}

func (r *recHost) Usage(_ context.Context, ref api.BundleRef) (api.BundleUsage, error) {
	r.record("usage " + ref.BundleID)
	return api.BundleUsage{}, nil
}

func (r *recHost) Stop(_ context.Context, ref api.BundleRef) error {
	r.record("stop " + ref.BundleID)
	return nil
}

func (r *recHost) Destroy(_ context.Context, ref api.BundleRef, opts api.DestroyOptions) error {
	r.record("destroy " + ref.BundleID)
	if r.down {
		return errDown
	}
	return nil
}

func (r *recHost) CreateVolume(_ context.Context, req api.VolumeRequest) (api.VolumeHandle, error) {
	r.record("create volume " + req.VolumeID)
	if r.down {
		return api.VolumeHandle{}, errDown
	}
	r.mu.Lock()
	if r.bundles == nil {
		r.bundles = map[string]bool{}
	}
	r.bundles[req.BundleID] = true
	r.mu.Unlock()
	return api.VolumeHandle{VolumeID: req.VolumeID, Handle: "pando-" + req.BundleID + "-" + req.VolumeID}, nil
}

func (r *recHost) SnapshotVolume(_ context.Context, h api.VolumeHandle, dst io.Writer) error {
	r.record("snapshot " + h.Handle + " " + h.VolumeID)
	_, err := io.WriteString(dst, "tarball")
	return err
}

func (r *recHost) RestoreVolume(_ context.Context, h api.VolumeHandle, src io.Reader) error {
	b, _ := io.ReadAll(src)
	r.record("restore " + h.Handle + " " + h.VolumeID + " " + string(b))
	return nil
}

func (r *recHost) Logs(_ context.Context, ref api.WorkloadRef, _ api.LogOptions) (io.ReadCloser, error) {
	r.record("logs " + ref.BundleID + "/" + ref.Workload)
	return io.NopCloser(strings.NewReader("a log line")), nil
}

func (r *recHost) Exec(_ context.Context, ref api.WorkloadRef, _ api.ExecRequest) (api.ExecSession, error) {
	r.record("exec " + ref.BundleID + "/" + ref.Workload)
	return nil, nil
}

func (r *recHost) RejoinNetworks(context.Context, func(string) bool) (int, error) {
	r.record("rejoin")
	return r.rejoinN, r.rejoinErr
}

func (r *recHost) ReclaimNetworks(context.Context, func(string) bool) (int, error) {
	r.record("reclaim")
	return r.reclaimN, r.reclaimErr
}

// fakeLocal is the control host's adapter joined as Pando's own container.
type fakeLocal struct {
	api.RuntimeAdapter
	api.SelfUpgrader

	caps     api.RuntimeCapabilities
	capsErr  error
	self     api.SelfWorkload
	selfErr  error
	edgeVols []api.VolumeHandle

	calls []string
}

func (l *fakeLocal) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return l.caps, l.capsErr
}
func (l *fakeLocal) Self(context.Context) (api.SelfWorkload, error) { return l.self, l.selfErr }
func (l *fakeLocal) Trial(_ context.Context, req api.TrialRequest) (api.TrialResult, error) {
	l.calls = append(l.calls, "trial")
	return api.TrialResult{}, nil
}
func (l *fakeLocal) ApplyEdge(_ context.Context, p api.EdgePlan) error {
	l.calls = append(l.calls, "apply edge")
	return nil
}
func (l *fakeLocal) ObserveEdge(_ context.Context, name string) (api.EdgeState, error) {
	l.calls = append(l.calls, "observe edge "+name)
	return api.EdgeState{}, nil
}
func (l *fakeLocal) RemoveEdge(_ context.Context, name string) error {
	l.calls = append(l.calls, "remove edge "+name)
	return nil
}
func (l *fakeLocal) Edges(context.Context) ([]string, error) {
	l.calls = append(l.calls, "edges")
	return []string{"pando-edge"}, nil
}
func (l *fakeLocal) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) {
	return append([]api.VolumeHandle(nil), l.edgeVols...), nil
}
func (l *fakeLocal) PullImage(_ context.Context, ref string) error {
	l.calls = append(l.calls, "pull "+ref)
	return nil
}
func (l *fakeLocal) StartHelper(context.Context, api.HelperSpec) (string, error) {
	l.calls = append(l.calls, "helper")
	return "helper-1", nil
}
func (l *fakeLocal) RemoveHelpers(context.Context) error {
	l.calls = append(l.calls, "remove helpers")
	return nil
}

// recAdapter is an adapter over recording hosts, the first the control host.
func recAdapter(t *testing.T, local *fakeLocal, hosts map[string]*recHost, order ...string) *Adapter {
	t.Helper()
	fakes := map[string]*fakeHost{}
	for _, name := range order {
		fakes[name] = hosts[name].fakeHost
	}
	a := newTestAdapter(t, fakes, order...)
	for _, h := range a.hosts {
		h.rt = hosts[h.cfg.Name]
	}
	if local != nil {
		a.local = local
	}
	return a
}

func roomy() *fakeHost {
	return &fakeHost{total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}}
}

// TestR151_TheRuntimeIsHealthyWhileTheControlHostAndAnOpenHostAnswer asserts
// HealthCheck's rule: a silent app host makes its own apps unobservable, not
// the whole runtime unavailable (R-151: an unreachable app is never failed),
// while a silent control host, no open host, or a mix of platforms is.
func TestR151_TheRuntimeIsHealthyWhileTheControlHostAndAnOpenHostAnswer(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy(), platform: "linux/amd64"},
		"app-1":   {fakeHost: roomy(), platform: "linux/amd64"},
		"app-2":   {fakeHost: roomy(), platform: "linux/amd64"},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1", "app-2")
	require.NoError(t, a.HealthCheck(ctx))

	hosts["app-1"].down = true
	require.NoError(t, a.HealthCheck(ctx), "one silent app host does not make the runtime unavailable")

	a.hosts[0].cfg.NoPlacement = true
	a.hosts[2].cfg.NoPlacement = true
	err := a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app-1", "the silent host is named")

	a.hosts[2].cfg.NoPlacement = false
	hosts["app-2"].platform = "linux/arm64"
	err = a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "linux/amd64, linux/arm64")

	hosts["control"].down = true
	err = a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "control host, control")

	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(New().HealthCheck(ctx)), "not set up")
}

// TestR254_CapabilitiesAreTheControlHostsWithWhatSeveralHostsChange asserts
// the capabilities the planner reads (R-254): images by registry only, egress
// restriction only with an image every host can pull, and a platform only
// when every host runs the same one.
func TestR254_CapabilitiesAreTheControlHostsWithWhatSeveralHostsChange(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{caps: api.RuntimeCapabilities{
		ImageDelivery: []api.ImageDelivery{api.ImageDeliveryRegistry, api.ImageDeliveryImport},
		Platform:      "linux/amd64", SupportsEgressRestriction: true,
	}, self: api.SelfWorkload{Image: "sha256:0123"}}
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy(), platform: "linux/amd64"},
		"app-1":   {fakeHost: roomy(), platform: "linux/amd64"},
	}
	a := recAdapter(t, local, hosts, "control", "app-1")

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, []api.ImageDelivery{api.ImageDeliveryRegistry}, caps.ImageDelivery)
	require.Equal(t, "linux/amd64", caps.Platform)
	require.False(t, caps.SupportsEgressRestriction,
		"Pando's container runs from an image ID no other host can pull, and no agent_image is set")

	local.self.Image = "registry.example/pando:1.2.3"
	caps, err = a.Capabilities(ctx)
	require.NoError(t, err)
	require.True(t, caps.SupportsEgressRestriction)

	hosts["app-1"].platform = "linux/arm64"
	caps, err = a.Capabilities(ctx)
	require.NoError(t, err)
	require.Empty(t, caps.Platform, "no one platform is stated for a mix")

	local.capsErr = errors.New("daemon gone")
	_, err = a.Capabilities(ctx)
	require.Error(t, err)
}

func TestTheAgentImageIsTheConfiguredOneOrPandosOwn(t *testing.T) {
	ctx := context.Background()
	a := &Adapter{}
	require.Empty(t, a.agentImage(ctx), "no control host adapter yet")

	local := &fakeLocal{self: api.SelfWorkload{Image: "trypando/pando:0.9.0"}}
	a.local = local
	require.Equal(t, "trypando/pando:0.9.0", a.agentImage(ctx))

	local.selfErr = errors.New("not in a container")
	require.Empty(t, a.agentImage(ctx))

	a.config.AgentImage = "registry.example/pando:1.0"
	require.Equal(t, "registry.example/pando:1.0", a.agentImage(ctx))
}

func TestInUseSumsTheHostsThatAnswer(t *testing.T) {
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy(), inUse: api.InUse{CPUMillis: 100, MemoryBytes: gib}},
		"app-1":   {fakeHost: roomy(), inUse: api.InUse{CPUMillis: 250, MemoryBytes: 2 * gib}},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1")
	got, err := a.InUse(context.Background())
	require.NoError(t, err)
	require.Equal(t, 350, got.CPUMillis)
	require.Equal(t, 3*gib, got.MemoryBytes)

	hosts["app-1"].down = true
	got, err = a.InUse(context.Background())
	require.NoError(t, err)
	require.Equal(t, 100, got.CPUMillis)

	hosts["control"].down = true
	_, err = a.InUse(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	_, err = a.Capacity(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "no host answered")
}

// TestR256_EveryOperationOnAnAppRunsOnItsHost asserts that what the
// reconciler and the console do to an app reaches the host it was placed on
// and no other.
func TestR256_EveryOperationOnAnAppRunsOnItsHost(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy()},
		"app-1":   {fakeHost: &fakeHost{bundles: map[string]bool{"app_a": true}}},
	}
	a := recAdapter(t, &fakeLocal{}, hosts, "control", "app-1")
	ref := api.BundleRef{BundleID: "app_a"}
	wref := api.WorkloadRef{BundleID: "app_a", Workload: "web"}

	_, err := a.Usage(ctx, ref)
	require.NoError(t, err)
	require.NoError(t, a.Stop(ctx, ref))
	rc, err := a.Logs(ctx, wref, api.LogOptions{})
	require.NoError(t, err)
	b, _ := io.ReadAll(rc)
	require.Equal(t, "a log line", string(b))
	_, err = a.Exec(ctx, wref, api.ExecRequest{})
	require.NoError(t, err)

	require.Equal(t, []string{"usage app_a", "stop app_a", "logs app_a/web", "exec app_a/web"}, hosts["app-1"].got())
	require.Empty(t, hosts["control"].got())

	// An app on no host: there is nothing to read or stop.
	missing := api.BundleRef{BundleID: "app_none"}
	_, err = a.Usage(ctx, missing)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	_, err = a.Logs(ctx, api.WorkloadRef{BundleID: "app_none"}, api.LogOptions{})
	require.Error(t, err)
	_, err = a.Exec(ctx, api.WorkloadRef{BundleID: "app_none"}, api.ExecRequest{})
	require.Error(t, err)
	require.NoError(t, a.Stop(ctx, missing), "stopping what is nowhere is done")
	require.NoError(t, a.Destroy(ctx, missing, api.DestroyOptions{}))

	// A host that is down: an error, not "nowhere".
	hosts["app-1"].down = true
	b2 := newTestAdapter(t, map[string]*fakeHost{"control": hosts["control"].fakeHost, "app-1": hosts["app-1"].fakeHost}, "control", "app-1")
	b2.hosts[1].rt = hosts["app-1"]
	_, err = b2.Usage(ctx, ref)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Error(t, b2.Stop(ctx, ref))
}

// TestR204_DestroyWithoutKeepingVolumesFreesThePlacement asserts that an app
// destroyed with its volumes leaves no record of its host, so a new app of
// the same ID is placed afresh, while one destroyed keeping them stays.
func TestR204_DestroyWithoutKeepingVolumesFreesThePlacement(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy()},
		"app-1":   {fakeHost: roomy()},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1")
	a.placement.byBundle["app_a"] = "app-1"
	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_a"}, api.DestroyOptions{KeepVolumes: true}))
	require.NotNil(t, a.cached("app_a"), "its volumes keep it on app-1")

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_a"}, api.DestroyOptions{}))
	require.Nil(t, a.cached("app_a"))
	require.Equal(t, []string{"app_a", "app_a"}, hosts["app-1"].detached)

	// A failed teardown is reported and nothing is detached or forgotten.
	a.placement.byBundle["app_b"] = "app-1"
	hosts["app-1"].down = true
	require.Error(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_b"}, api.DestroyOptions{}))
	require.NotNil(t, a.cached("app_b"))
}

// TestR204_AVolumeIsMadeOnItsAppsHostAndCarriesIt asserts that storage is
// made where the app is, or where it is about to be placed, and that every
// operation on it goes back to that host by its handle.
func TestR204_AVolumeIsMadeOnItsAppsHostAndCarriesIt(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*recHost{
		"control": {fakeHost: &fakeHost{total: api.Fit{CPUMillis: 4000, MemoryBytes: 8 * gib}, free: api.Fit{CPUMillis: 4000, MemoryBytes: gib}}},
		"app-1":   {fakeHost: roomy()},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1")

	vh, err := a.CreateVolume(ctx, api.VolumeRequest{BundleID: "app_db", VolumeID: "vol_1"})
	require.NoError(t, err)
	require.Equal(t, "app-1/pando-app_db-vol_1", vh.Handle, "placed on the roomiest host, and the handle names it")

	// A second volume for the same app goes to the same host.
	vh2, err := a.CreateVolume(ctx, api.VolumeRequest{BundleID: "app_db", VolumeID: "vol_2"})
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(vh2.Handle, "app-1/"))

	var buf strings.Builder
	require.NoError(t, a.SnapshotVolume(ctx, vh, &buf))
	require.Equal(t, "tarball", buf.String())
	require.NoError(t, a.RestoreVolume(ctx, vh, strings.NewReader("tarball")))
	require.Contains(t, hosts["app-1"].got(), "snapshot pando-app_db-vol_1 vol_1")
	require.Contains(t, hosts["app-1"].got(), "restore pando-app_db-vol_1 vol_1 tarball")
	require.Empty(t, hosts["control"].got())

	require.Error(t, a.SnapshotVolume(ctx, api.VolumeHandle{Handle: "gone/pando-x"}, &buf))
	require.Error(t, a.RestoreVolume(ctx, api.VolumeHandle{Handle: "no-host"}, strings.NewReader("")))

	// The host refuses: no handle.
	hosts["app-1"].down = true
	_, err = a.CreateVolume(ctx, api.VolumeRequest{BundleID: "app_db", VolumeID: "vol_3"})
	require.Error(t, err)

	// A new app while a host is silent: it might be there, so no volume.
	_, err = a.CreateVolume(ctx, api.VolumeRequest{BundleID: "app_other", VolumeID: "vol_1"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	// No host with room: refused.
	hosts["app-1"].down = false
	hosts["app-1"].free = api.Fit{}
	hosts["control"].free = api.Fit{}
	a.hosts[0].cfg.NoPlacement = true
	a.hosts[1].cfg.NoPlacement = true
	_, err = a.CreateVolume(ctx, api.VolumeRequest{BundleID: "app_new", VolumeID: "vol_1"})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "No Docker host is open to new apps")
}

// TestO47_TrialsTheEdgeAndSelfUpgradeRunOnTheControlHost asserts that what
// runs where Pando runs (O-47) is the control host's adapter's.
func TestO47_TrialsTheEdgeAndSelfUpgradeRunOnTheControlHost(t *testing.T) {
	ctx := context.Background()
	local := &fakeLocal{self: api.SelfWorkload{ID: "self"}, edgeVols: []api.VolumeHandle{{Handle: "pando-edge-certs"}}}
	hosts := map[string]*recHost{"control": {fakeHost: roomy()}, "app-1": {fakeHost: roomy()}}
	a := recAdapter(t, local, hosts, "control", "app-1")

	_, err := a.Trial(ctx, api.TrialRequest{})
	require.NoError(t, err)
	require.NoError(t, a.ApplyEdge(ctx, api.EdgePlan{}))
	_, err = a.ObserveEdge(ctx, "pando-edge")
	require.NoError(t, err)
	require.NoError(t, a.RemoveEdge(ctx, "pando-edge"))
	edges, err := a.Edges(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"pando-edge"}, edges)
	vols, err := a.EdgeVolumes(ctx)
	require.NoError(t, err)
	require.Equal(t, "control/pando-edge-certs", vols[0].Handle, "edge volumes carry the control host")
	self, err := a.Self(ctx)
	require.NoError(t, err)
	require.Equal(t, "self", self.ID)
	require.NoError(t, a.PullImage(ctx, "trypando/pando@sha256:abc"))
	id, err := a.StartHelper(ctx, api.HelperSpec{})
	require.NoError(t, err)
	require.Equal(t, "helper-1", id)
	require.NoError(t, a.RemoveHelpers(ctx))

	require.Equal(t, []string{"trial", "apply edge", "observe edge pando-edge", "remove edge pando-edge", "edges",
		"pull trypando/pando@sha256:abc", "helper", "remove helpers"}, local.calls)
	require.Empty(t, hosts["app-1"].got())

	// Images come from a registry, never streamed to the control host.
	_, err = a.ImportImage(ctx, strings.NewReader(""))
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	require.Equal(t, Kind, a.Kind())
	require.Equal(t, api.CategoryRuntime, a.Category())
}

// TestR224_RejoinAndReclaimRunOnEveryHostAndNameTheFirstThatFails asserts
// the periodic network upkeep: every host is asked, the counts are summed,
// and an error names the host it came from.
func TestR224_RejoinAndReclaimRunOnEveryHostAndNameTheFirstThatFails(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*recHost{
		"control": {fakeHost: &fakeHost{bundles: map[string]bool{"app_c": true}}, rejoinN: 1, reclaimN: 2},
		"app-1":   {fakeHost: &fakeHost{bundles: map[string]bool{"app_a": true}}, rejoinN: 3, reclaimN: 4},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1")
	owns := func(string) bool { return true }

	n, err := a.RejoinNetworks(ctx, owns)
	require.NoError(t, err)
	require.Equal(t, 4, n)
	require.NotNil(t, a.ownsFunc(), "kept for an agent's re-creation")
	require.Equal(t, "app-1", a.cached("app_a").cfg.Name, "the placement map was refreshed")

	n, err = a.ReclaimNetworks(ctx, owns)
	require.NoError(t, err)
	require.Equal(t, 6, n)

	hosts["app-1"].rejoinErr = errors.New("network gone")
	hosts["app-1"].reclaimErr = errors.New("in use")
	_, err = a.RejoinNetworks(ctx, owns)
	require.ErrorContains(t, err, "app-1: network gone")
	_, err = a.ReclaimNetworks(ctx, owns)
	require.ErrorContains(t, err, "app-1: in use")
}

func TestAnApplyTheHostRefusesIsReported(t *testing.T) {
	hosts := map[string]*recHost{
		"control": {fakeHost: roomy()},
		"app-1":   {fakeHost: &fakeHost{down: true}},
	}
	a := recAdapter(t, nil, hosts, "control", "app-1")
	a.placement.byBundle["app_a"] = "app-1"
	_, err := a.Apply(context.Background(), plan("app_a", gib))
	require.ErrorIs(t, err, errDown)
}
