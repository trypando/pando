package multidocker

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// diskPlan is volumePlan with a disk limit: its standby's copies are counted
// at three times it.
func diskPlan(bundle string, disk int64) api.BundlePlan {
	p := volumePlan(bundle)
	p.Workloads[0].Resources.DiskBytes = disk
	return p
}

// TestR242_StandbyCopiesAndRunningDiskAreBothCounted asserts R-242 and R-422
// on several hosts: a standby goes only where its copies fit beside what the
// host's running apps hold of the disk, not against the whole disk; a new
// app goes only where its disk limit fits beside the running apps and the
// standby copies a host keeps, and is refused with the numbers when none
// has it; and Capacity reports the copies as held, for the planner.
func TestR242_StandbyCopiesAndRunningDiskAreBothCounted(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	for _, h := range hosts {
		h.totalDisk = 100 * gib
	}
	hosts["app-2"].diskUsed = 80 * gib // 20 GB left: not the 30 three copies of 10 GB need
	_, err := a.Apply(ctx, diskPlan("app_a", 10*gib))
	require.NoError(t, err)
	sb, err := a.ApplyStandby(ctx, api.StandbyRequest{Bundle: diskPlan("app_a", 10*gib)})
	require.NoError(t, err)
	require.Equal(t, "control", sb.Host, "app-2 has the memory and not the disk")

	c, err := a.Capacity(ctx)
	require.NoError(t, err)
	require.Equal(t, 30*gib, c.HeldDiskBytes, "the copies are held, for the planner")

	// A new app: app-1 has the most memory and no disk left, the control
	// host's is taken by app_a's standby copies, app-2 has 20 GB.
	hosts["app-1"].diskUsed = 95 * gib
	hosts["control"].diskUsed = 65 * gib
	hosts["control"].free.MemoryBytes = 30 * gib
	h, err := a.place(ctx, diskPlan("app_b", 10*gib))
	require.NoError(t, err)
	require.Equal(t, "app-2", h.cfg.Name)

	hosts["app-2"].diskUsed = 95 * gib
	_, err = a.place(ctx, diskPlan("app_c", 10*gib))
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "No Docker host has the disk for this app: it may use 10.0 GB")
	require.Contains(t, errs.As(err).Message, "after its apps and the standby copies it keeps")

	// CPU or memory short everywhere is the refusal it was.
	_, err = a.place(ctx, diskPlan("app_d", 64*gib))
	require.Contains(t, errs.As(err).Message, "it may use 64.0 GB")
	big := plan("app_e", 64*gib)
	_, err = a.place(ctx, big)
	require.Contains(t, errs.As(err).Message, "No Docker host has room for this app")
}

// TestR422_StagingVolumesAreRemovedAfterPromotion asserts the standby's
// copies leave the promoted host once the app is healthy there, so they no
// longer hold its disk; they are kept while it is not.
func TestR422_StagingVolumesAreRemovedAfterPromotion(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	hosts["app-1"].write("app_a", "vol_1", "./db", "data")
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.NoError(t, err)
	require.NotEmpty(t, hosts["app-2"].copies["app_a/vol_1"])

	got, err := a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2", PollEvery: time.Millisecond})
	require.NoError(t, err)
	require.True(t, got.Healthy)
	require.Empty(t, hosts["app-2"].copies["app_a/vol_1"], "the staging volumes are gone")
}

// TestR153_APromotionNeverRestoresOverARunningVolume asserts a promotion to a
// host that already holds the app's home mark — a promotion cut short after
// the start, tried again by a restarted process — restores no copy over the
// volume the app runs on there, and reports the app there.
func TestR153_APromotionNeverRestoresOverARunningVolume(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	hosts["app-1"].write("app_a", "vol_1", "./db", "data")
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.NoError(t, err)
	hosts["app-2"].homes["app_a"] = time.Now() // reached the start, then stopped

	b := standbyAdapter(hosts, "control", "app-1", "app-2")
	got, err := b.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2", PollEvery: time.Millisecond})
	require.NoError(t, err)
	require.Equal(t, "app-2", got.Host)
	require.True(t, got.Healthy)
	require.Zero(t, hosts["app-2"].restores, "nothing restored over the running volume")
	h, err := b.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-2", h.cfg.Name)
}

// TestR153_APromotionRefusedBeforeTheStartLeavesNothingStarted asserts a
// failure before the app's plan is first applied names no host — core puts
// its record back — writes no home mark, and removes what it restored.
func TestR153_APromotionRefusedBeforeTheStartLeavesNothingStarted(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	hosts["app-1"].write("app_a", "vol_1", "./db", "data")
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.NoError(t, err)
	hosts["app-2"].failRestore = errors.New("disk full")

	got, err := a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2", PollEvery: time.Millisecond})
	require.Error(t, err)
	require.Empty(t, got.Host)
	require.Empty(t, hosts["app-2"].homes)
	require.Contains(t, hosts["app-2"].destroyed, "app_a", "what was restored is removed")
	require.NotContains(t, hosts["app-2"].applied, "app_a")
	require.NotEmpty(t, hosts["app-2"].copies["app_a/vol_1"], "the copies are kept to try again")
	h, err := a.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-1", h.cfg.Name, "still where it was")
}

// TestR153_AStandbyIsNotChangedWhileItIsPromoted asserts the runtime's own
// guard: while a promotion runs, the standby is not moved, shipped to or
// removed, and not promoted a second time.
func TestR153_AStandbyIsNotChangedWhileItIsPromoted(t *testing.T) {
	ctx := context.Background()
	hosts, a := threeHosts()
	_, err := a.Apply(ctx, volumePlan("app_a"))
	require.NoError(t, err)
	require.True(t, a.standby.begin("app_a"))
	_, err = a.ApplyStandby(ctx, api.StandbyRequest{Bundle: volumePlan("app_a"), Host: "app-2"})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	_, err = a.ShipVolumes(ctx, api.ShipRequest{BundleID: "app_a", Host: "app-2", VolumeIDs: []string{"vol_1"}})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(a.RemoveStandby(ctx, api.BundleRef{BundleID: "app_a"})))
	_, err = a.PromoteStandby(ctx, api.PromoteRequest{Bundle: volumePlan("app_a"), Host: "app-2"})
	require.Contains(t, errs.As(err).Message, "already being started")
	require.Empty(t, hosts["app-2"].destroyed)
	a.standby.end("app_a")
	require.NoError(t, a.RemoveStandby(ctx, api.BundleRef{BundleID: "app_a"}))
}

// TestR153_ABundleOnTwoHostsIsNeverResolvedByTheirOrder asserts placement does
// not guess: a bundle two hosts hold, with no promotion or home mark to say
// which is its home, is located on neither — the reconciler reports it
// unobservable and starts nothing — unless this process already had it on
// one of them.
func TestR153_ABundleOnTwoHostsIsNeverResolvedByTheirOrder(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]*fakeHost{
		"control": {bundles: map[string]bool{}},
		"app-1":   {bundles: map[string]bool{"app_a": true}},
		"app-2":   {bundles: map[string]bool{"app_a": true}},
	}
	a := newTestAdapter(t, hosts, "control", "app-1", "app-2")
	_, err := a.locate(ctx, "app_a")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "This app is on app-1 and app-2")
	_, err = a.Observe(ctx, api.BundleRef{BundleID: "app_a"})
	require.Error(t, err, "unobservable, never absent")

	b := newTestAdapter(t, hosts, "control", "app-1", "app-2")
	b.placement.byBundle["app_a"] = "app-2"
	b.refresh(ctx)
	h, err := b.locate(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, "app-2", h.cfg.Name, "kept where this process had it")
}
