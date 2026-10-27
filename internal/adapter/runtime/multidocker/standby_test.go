package multidocker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
)

// fakeFile is one entry in a fake volume.
type fakeFile struct {
	Entry docker.VolumeEntry
	Data  string
}

type fakeCopy struct {
	seq     int
	files   map[string]fakeFile
	listing docker.VolumeListing
}

// standbyFake is a host whose Docker adapter also does a standby's work, in
// memory: volumes are maps of paths, copies are snapshots of them.
type standbyFake struct {
	*fakeHost

	sm        sync.Mutex
	clock     int64
	volumes   map[string]map[string]fakeFile // bundle/volume -> path -> file
	copies    map[string][]fakeCopy
	incoming  map[string]map[string]fakeFile
	stopped   []string // ApplyStopped bundles
	destroyed []string
	reads     [][]string // the paths each ReadVolumeFiles sent
	restored  map[string]int
	homes     map[string]time.Time
	failStage error

	// healthyFrom says whether the bundle comes up healthy from the copy
	// restored into it, by seq. Nil is always.
	healthyFrom func(seq int) bool
}

func newStandbyFake(free int64) *standbyFake {
	return &standbyFake{
		fakeHost: &fakeHost{total: api.Fit{CPUMillis: 8000, MemoryBytes: 32 * gib}, free: api.Fit{CPUMillis: 8000, MemoryBytes: free}},
		volumes:  map[string]map[string]fakeFile{}, copies: map[string][]fakeCopy{},
		incoming: map[string]map[string]fakeFile{}, restored: map[string]int{}, homes: map[string]time.Time{},
		clock: 1000,
	}
}

func key(bundle, volume string) string { return bundle + "/" + volume }

// write puts a file in a volume at the fake's clock, which then moves on two
// seconds, so the next listing is past it.
func (f *standbyFake) write(bundle, volume, path, data string) {
	f.sm.Lock()
	defer f.sm.Unlock()
	k := key(bundle, volume)
	if f.volumes[k] == nil {
		f.volumes[k] = map[string]fakeFile{}
	}
	f.volumes[k][path] = fakeFile{Entry: docker.VolumeEntry{Path: path, Mode: 0o100644, Size: int64(len(data)), MTime: f.clock}, Data: data}
	f.clock += 2
}

func (f *standbyFake) remove(bundle, volume, path string) {
	f.sm.Lock()
	defer f.sm.Unlock()
	delete(f.volumes[key(bundle, volume)], path)
	f.clock += 2
}

func (f *standbyFake) ApplyStopped(_ context.Context, p api.BundlePlan) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	if f.down {
		return errDown
	}
	f.stopped = append(f.stopped, p.BundleID)
	return nil
}

func (f *standbyFake) Destroy(_ context.Context, ref api.BundleRef, _ api.DestroyOptions) error {
	f.sm.Lock()
	f.destroyed = append(f.destroyed, ref.BundleID)
	f.sm.Unlock()
	f.mu.Lock()
	delete(f.bundles, ref.BundleID)
	f.mu.Unlock()
	return nil
}

func (f *standbyFake) Stop(context.Context, api.BundleRef) error { return nil }

func (f *standbyFake) Observe(_ context.Context, ref api.BundleRef) (api.ObservedBundle, error) {
	f.sm.Lock()
	defer f.sm.Unlock()
	if f.down {
		return api.ObservedBundle{}, errDown
	}
	seq := 0
	for k, s := range f.restored {
		if len(k) > len(ref.BundleID) && k[:len(ref.BundleID)] == ref.BundleID {
			seq = s
		}
	}
	healthy := f.healthyFrom == nil || f.healthyFrom(seq)
	return api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{{Name: "web", Present: true, Running: true, Healthy: &healthy}}}, nil
}

func (f *standbyFake) ListVolume(_ context.Context, bundle, volume string) (docker.VolumeListing, error) {
	f.sm.Lock()
	defer f.sm.Unlock()
	out := docker.VolumeListing{TakenAt: f.clock}
	for _, file := range f.volumes[key(bundle, volume)] {
		out.Entries = append(out.Entries, file.Entry)
	}
	sort.Slice(out.Entries, func(i, j int) bool { return out.Entries[i].Path < out.Entries[j].Path })
	f.clock++
	return out, nil
}

func (f *standbyFake) ReadVolumeFiles(_ context.Context, bundle, volume string, paths []string, dst io.Writer) error {
	f.sm.Lock()
	sent := map[string]fakeFile{}
	for _, p := range paths {
		if file, ok := f.volumes[key(bundle, volume)][p]; ok {
			sent[p] = file
		}
	}
	f.reads = append(f.reads, paths)
	f.sm.Unlock()
	return json.NewEncoder(dst).Encode(sent)
}

func (f *standbyFake) StandbyCopies(_ context.Context, bundle, volume string) (docker.StandbyCopies, error) {
	f.sm.Lock()
	defer f.sm.Unlock()
	var out docker.StandbyCopies
	for _, c := range f.copies[key(bundle, volume)] {
		out.Seqs = append(out.Seqs, c.seq)
		out.Newest = c.listing
	}
	return out, nil
}

func (f *standbyFake) StageStandbyCopy(_ context.Context, bundle, volume string, remove []string, files io.Reader) error {
	if f.failStage != nil {
		if files != nil {
			_, _ = io.Copy(io.Discard, files)
		}
		return f.failStage
	}
	sent := map[string]fakeFile{}
	if files != nil {
		if err := json.NewDecoder(files).Decode(&sent); err != nil {
			return err
		}
	}
	f.sm.Lock()
	defer f.sm.Unlock()
	k := key(bundle, volume)
	next := map[string]fakeFile{}
	if cs := f.copies[k]; len(cs) > 0 {
		for p, file := range cs[len(cs)-1].files {
			next[p] = file
		}
	}
	for _, p := range remove {
		delete(next, p)
	}
	for p, file := range sent {
		next[p] = file
	}
	f.incoming[k] = next
	return nil
}

func (f *standbyFake) CommitStandbyCopy(_ context.Context, bundle, volume string, seq int, listing docker.VolumeListing, drop []int) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	k := key(bundle, volume)
	var kept []fakeCopy
	for _, c := range f.copies[k] {
		dropped := false
		for _, d := range drop {
			dropped = dropped || d == c.seq
		}
		if !dropped {
			kept = append(kept, c)
		}
	}
	f.copies[k] = append(kept, fakeCopy{seq: seq, files: f.incoming[k], listing: listing})
	return nil
}

func (f *standbyFake) RestoreStandbyCopy(_ context.Context, bundle, volume string, seq int) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	f.restored[key(bundle, volume)] = seq
	return nil
}

func (f *standbyFake) RemoveStandbyStorage(_ context.Context, bundle string) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	for k := range f.copies {
		if len(k) > len(bundle) && k[:len(bundle)+1] == bundle+"/" {
			delete(f.copies, k)
		}
	}
	return nil
}

func (f *standbyFake) MarkHome(_ context.Context, bundle string, at time.Time) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	f.homes[bundle] = at
	return nil
}

func (f *standbyFake) ClearHome(_ context.Context, bundle string) error {
	f.sm.Lock()
	defer f.sm.Unlock()
	delete(f.homes, bundle)
	return nil
}

func (f *standbyFake) HomeMarks(context.Context) (map[string]time.Time, error) {
	f.sm.Lock()
	defer f.sm.Unlock()
	out := map[string]time.Time{}
	for b, at := range f.homes {
		out[b] = at
	}
	return out, nil
}

// standbyAdapter is an adapter over hosts that can keep standbys, the first
// the control host.
func standbyAdapter(hosts map[string]*standbyFake, order ...string) *Adapter {
	a := &Adapter{placement: placements{byBundle: map[string]string{}}}
	for i, name := range order {
		h := &host{cfg: HostConfig{Name: name, Control: i == 0}, rt: hosts[name], agentAddr: name + ":7443"}
		if i == 0 {
			a.control = h
		}
		a.hosts = append(a.hosts, h)
	}
	return a
}

func volumePlan(bundle string) api.BundlePlan {
	p := plan(bundle, gib)
	p.Volumes = []api.VolumePlan{{VolumeID: "vol_1", Name: "data"}}
	p.Workloads[0].Mounts = []api.MountPlan{{VolumeID: "vol_1", Path: "/data"}}
	return p
}

// threeHosts: the app on app-1, the roomiest; app-2 the next.
func threeHosts() (map[string]*standbyFake, *Adapter) {
	hosts := map[string]*standbyFake{
		"control": newStandbyFake(2 * gib), "app-1": newStandbyFake(20 * gib), "app-2": newStandbyFake(10 * gib),
	}
	hosts["control"].free.CPUMillis = 8000
	return hosts, standbyAdapter(hosts, "control", "app-1", "app-2")
}

// TestR419_AnAppGetsAStoppedStandbyOnAnotherHost asserts that a standby is
// the app's plan on a host other than the app's, created and never started,
// under a name that does not make that host the app's.
func TestR419_AnAppGetsAStoppedStandbyOnAnotherHost(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	handle, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	require.Equal(t, "app-1", handle.Handle)

	sb, err := a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a")})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host, "the roomiest host that is not the app's")
	require.Equal(t, []string{"app_a-standby"}, hosts["app-2"].stopped)
	require.Empty(t, hosts["app-2"].applied, "nothing of the standby is started (R-153)")

	// Kept where it is on the next pass, though another host is now roomier.
	hosts["control"].free.MemoryBytes = 30 * gib
	sb, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a"), Host: "app-2"})
	require.NoError(t, err)
	require.Equal(t, "app-2", sb.Host)

	// A standby host that closes to new apps loses it to another.
	a.hosts[2].cfg.NoPlacement = true
	sb, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a"), Host: "app-2"})
	require.NoError(t, err)
	require.Equal(t, "control", sb.Host)
	require.Contains(t, hosts["app-2"].destroyed, "app_a-standby", "the old standby is removed")

	// The app is still where it was.
	h, err := a.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-1", h.cfg.Name)
}

// TestR419_StandbyNeedsASecondHostOpenToNewApps asserts the capability is
// data (R-254), true only where a standby has somewhere to go.
func TestR419_StandbyNeedsASecondHostOpenToNewApps(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	require.True(t, a.standbyPossible())
	a.hosts[0].cfg.NoPlacement = true
	a.hosts[2].cfg.NoPlacement = true
	require.False(t, a.standbyPossible())

	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a")})
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "nowhere to keep a standby")
	require.Empty(t, hosts["app-2"].stopped)
}

// TestR420_ShippingCopiesOnlyChangedFiles asserts the copy is incremental: the
// first sends everything, later ones only what changed, and a removed file is
// removed from the copy.
func TestR420_ShippingCopiesOnlyChangedFiles(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	app, standby := hosts["app-1"], hosts["app-2"]
	app.write("app_a", "vol_1", "./a", "one")
	app.write("app_a", "vol_1", "./b", "two")
	app.write("app_a", "vol_1", "./c", "three")
	ship := func() api.ShipResult {
		t.Helper()
		got, err := a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
		require.NoError(t, err)
		return got
	}

	got := ship()
	require.Equal(t, api.ShipResult{Copies: 1, FilesSent: 3, BytesSent: 11}, got)

	app.write("app_a", "vol_1", "./b", "TWO!")
	app.remove("app_a", "vol_1", "./c")
	got = ship()
	require.Equal(t, 2, got.Copies)
	require.Equal(t, []string{"./b"}, app.reads[len(app.reads)-1], "only the changed file crosses")

	newest := standby.copies["app_a/vol_1"][1].files
	require.Equal(t, "one", newest["./a"].Data, "an unchanged file is carried from the copy before")
	require.Equal(t, "TWO!", newest["./b"].Data)
	require.NotContains(t, newest, "./c")
	require.Contains(t, standby.copies["app_a/vol_1"][0].files, "./c", "the copy before is untouched")

	// Nothing changed: no new copy.
	reads := len(app.reads)
	got = ship()
	require.True(t, got.Unchanged)
	require.Equal(t, 2, got.Copies)
	require.Len(t, app.reads, reads)
}

// TestR420_TheStandbyKeepsThreeCopies asserts the last three copies are
// kept and older ones removed.
func TestR420_TheStandbyKeepsThreeCopies(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	for i := range 5 {
		hosts["app-1"].write("app_a", "vol_1", "./db", string(rune('a'+i)))
		got, err := a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
		require.NoError(t, err)
		require.Equal(t, min(i+1, 3), got.Copies)
	}
	var seqs []int
	for _, c := range hosts["app-2"].copies["app_a/vol_1"] {
		seqs = append(seqs, c.seq)
	}
	require.Equal(t, []int{3, 4, 5}, seqs)
}

// TestR420_PromoteFallsBackToThePreviousCopyWhenTheNewestIsUnhealthy asserts a
// failover starts from the newest copy and tries the one before when the app
// does not come up healthy from it.
func TestR420_PromoteFallsBackToThePreviousCopyWhenTheNewestIsUnhealthy(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	for _, v := range []string{"good", "torn"} {
		hosts["app-1"].write("app_a", "vol_1", "./db", v)
		_, err := a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
		require.NoError(t, err)
	}
	hosts["app-2"].healthyFrom = func(seq int) bool { return seq == 1 } // the newest, 2, is torn

	got, err := a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2",
		HealthTimeout: 30 * time.Millisecond, PollEvery: 5 * time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, api.PromoteResult{Host: "app-2", FormerHost: "app-1", Copy: 1, FellBack: true, Healthy: true}, got)
	require.Equal(t, 1, hosts["app-2"].restored["app_a/vol_1"])

	// No copy comes up: the oldest tried is left, reported unhealthy.
	hosts["app-2"].healthyFrom = func(int) bool { return false }
	got, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2",
		HealthTimeout: 10 * time.Millisecond, PollEvery: 2 * time.Millisecond})
	require.NoError(t, err)
	require.False(t, got.Healthy)
	require.Equal(t, 1, got.Copy)
}

// TestR419_OneCopyRunsAtATimeAfterPromote asserts that after a failover the
// app runs on the standby's host alone, is found there by a restarted
// process too, and that the lost host's copy, when it is back, is removed and
// never started.
func TestR419_OneCopyRunsAtATimeAfterPromote(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a")})
	require.NoError(t, err)
	hosts["app-1"].write("app_a", "vol_1", "./db", "data")
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.NoError(t, err)

	hosts["app-1"].down = true
	applied := len(hosts["app-1"].applied)
	got, err := a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2", PollEvery: time.Millisecond})
	require.NoError(t, err)
	require.True(t, got.Healthy)
	require.Contains(t, hosts["app-2"].destroyed, "app_a-standby", "the standby's own workloads are gone")
	require.Contains(t, hosts["app-2"].applied, "app_a")

	// The lost host comes back still holding its copy: the app stays put.
	hosts["app-1"].down = false
	a.refresh(ctx)
	h, err := a.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-2", h.cfg.Name)

	// So does a process started since, from the home mark on the hosts.
	b := standbyAdapter(hosts, "control", "app-1", "app-2")
	b.refresh(ctx)
	h, err = b.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-2", h.cfg.Name)

	// Its home is not stale; the lost host's copy is, and is removed.
	err = a.RemoveStaleCopy(ctx, api.BundleRef{BundleID: "app_a"}, "app-2")
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.NoError(t, a.RemoveStaleCopy(ctx, api.BundleRef{BundleID: "app_a"}, "app-1"))
	require.Contains(t, hosts["app-1"].destroyed, "app_a")
	require.Contains(t, hosts["app-1"].detached, "app_a")
	require.Len(t, hosts["app-1"].applied, applied, "the lost host's copy is never started again")
}

func TestPromoteWithoutACopyOfTheStorageIsRefused(t *testing.T) {
	ctx := context.Background()
	_, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	_, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2"})
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "no copy of its storage")

	_, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "gone"})
	require.Contains(t, errs.As(err).Message, "no longer in the Docker hosts runtime's host list")
}

func TestShippingIsRefusedToTheAppsOwnHostAndReportsAFailedCopy(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-1", VolumeIDs: []string{"vol_1"}})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	hosts["app-1"].write("app_a", "vol_1", "./db", "x")
	hosts["app-2"].failStage = errors.New("disk full")
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.ErrorContains(t, err, "disk full")
	require.Empty(t, hosts["app-2"].copies["app_a/vol_1"], "a copy that did not arrive is not committed")
}

func TestRemoveStandbyReachesEveryHost(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	require.NoError(t, a.RemoveStandby(ctx, api.BundleRef{BundleID: "app_a"}))
	for _, h := range hosts {
		require.Contains(t, h.destroyed, "app_a-standby")
	}
}

func TestAHostWithoutStandbySupportSaysSo(t *testing.T) {
	a := newTestAdapter(t, map[string]*fakeHost{"control": {}}, "control")
	_, err := standbyOf(a.hosts[0])
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "cannot keep a standby")
}

func TestDiffListingsComparesTypeModeSizeAndTime(t *testing.T) {
	file := func(p string, mode uint32, size, mtime int64) docker.VolumeEntry {
		return docker.VolumeEntry{Path: p, Mode: mode, Size: size, MTime: mtime}
	}
	prev := docker.VolumeListing{TakenAt: 100, Entries: []docker.VolumeEntry{
		file(".", 0o40755, 4096, 50), file("./same", 0o100644, 3, 50), file("./grown", 0o100644, 3, 50),
		file("./chmod", 0o100644, 3, 50), file("./racy", 0o100644, 3, 100), file("./was-file", 0o100644, 3, 50),
		file("./dir", 0o40755, 4096, 50), file("./gone", 0o100644, 3, 50),
	}}
	next := docker.VolumeListing{TakenAt: 200, Entries: []docker.VolumeEntry{
		file(".", 0o40755, 4096, 50), file("./same", 0o100644, 3, 50), file("./grown", 0o100644, 9, 150),
		file("./chmod", 0o100600, 3, 50), file("./racy", 0o100644, 3, 100), file("./was-file", 0o40755, 4096, 150),
		file("./dir", 0o40755, 4096, 150), file("./new", 0o100644, 1, 150),
	}}
	d := diffListings(prev, next, true)
	require.Equal(t, []string{"./chmod", "./dir", "./grown", "./new", "./racy", "./was-file"}, d.send)
	require.Equal(t, []string{"./chmod", "./gone", "./grown", "./racy", "./was-file"}, d.remove,
		"a directory that is still one is never removed")
	require.Equal(t, int64(3+9+1+3), d.bytes)

	first := diffListings(docker.VolumeListing{}, next, false)
	require.Len(t, first.send, len(next.Entries))
	require.Empty(t, first.remove)
}

func TestHealthyBundleNeedsEveryWorkloadUpAndHealthyWhereChecked(t *testing.T) {
	yes, no := true, false
	p := plan("app_a", gib)
	p.Workloads = append(p.Workloads, api.WorkloadPlan{Name: "db", Health: &api.HealthPlan{Port: 5432}})
	obs := func(web, db api.ObservedWorkload) api.ObservedBundle {
		web.Name, db.Name = "web", "db"
		return api.ObservedBundle{Workloads: []api.ObservedWorkload{web, db}}
	}
	require.True(t, healthyBundle(p, obs(api.ObservedWorkload{Running: true}, api.ObservedWorkload{Running: true, Healthy: &yes})))
	require.False(t, healthyBundle(p, obs(api.ObservedWorkload{Running: true}, api.ObservedWorkload{Running: true})),
		"a checked workload with no answer yet is not healthy")
	require.False(t, healthyBundle(p, obs(api.ObservedWorkload{Running: true, Healthy: &no}, api.ObservedWorkload{Running: true, Healthy: &yes})))
	require.False(t, healthyBundle(p, obs(api.ObservedWorkload{Running: true, Restarting: true}, api.ObservedWorkload{Running: true, Healthy: &yes})))
	require.False(t, healthyBundle(p, api.ObservedBundle{}))
}
