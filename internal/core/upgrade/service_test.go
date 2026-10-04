package upgrade_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// fakeRuntime is a runtime adapter that only answers what the upgrade asks.
type fakeRuntime struct {
	api.RuntimeAdapter
	self    api.SelfWorkload
	selfErr error
	noSelf  bool
	pulled  []string
	helpers []api.HelperSpec
	removed int
}

func (f *fakeRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsSelfUpgrade: !f.noSelf}, nil
}
func (f *fakeRuntime) Self(context.Context) (api.SelfWorkload, error) { return f.self, f.selfErr }
func (f *fakeRuntime) PullImage(_ context.Context, ref string) error {
	f.pulled = append(f.pulled, ref)
	return nil
}
func (f *fakeRuntime) StartHelper(_ context.Context, spec api.HelperSpec) (string, error) {
	f.helpers = append(f.helpers, spec)
	return "helper", nil
}
func (f *fakeRuntime) RemoveHelpers(context.Context) error { f.removed++; return nil }

type fakeUpdates struct{ st update.Status }

func (f *fakeUpdates) Status(context.Context) (update.Status, error) { return f.st, nil }

type harness struct {
	svc      *upgrade.Service
	rt       *fakeRuntime
	doc      policy.Document
	events   []audit.Event
	backups  int
	verified []string
	verify   error
	canCopy  error
	notes    []api.Notification
	dropped  int
	clock    *clock.Fake
	updates  *fakeUpdates
}

func (h *harness) actions() []string {
	var out []string
	for _, e := range h.events {
		out = append(out, e.Action)
	}
	return out
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		rt:    &fakeRuntime{self: api.SelfWorkload{ID: "c0ffee", Image: "trypando/pando:latest"}},
		doc:   policy.Document{UpgradeInPlace: true},
		clock: clock.NewFake(time.Date(2026, 10, 4, 2, 30, 0, 0, time.UTC)), // a Sunday
		updates: &fakeUpdates{st: update.Status{
			Current: "0.3.1", Enabled: true, Channel: "stable", Latest: "0.4.0", Available: true,
			Releases: []update.Release{
				{Version: "0.4.0", Breaking: true, Notes: "### Upgrade notes\n\n- A variable is renamed.\n"},
				{Version: "0.3.2", Security: true},
			},
		}},
	}
	h.svc = &upgrade.Service{
		Version: "0.3.1",
		Updates: h.updates,
		Verify: func(_ context.Context, v string) (string, error) {
			h.verified = append(h.verified, v)
			return "sha256:abc", h.verify
		},
		Runtime: func(context.Context) (api.RuntimeAdapter, error) { return h.rt, nil },
		Policy:  func(context.Context) (policy.Document, error) { return h.doc, nil },
		CanCopy: func(context.Context) error { return h.canCopy },
		Backup: func(context.Context, authz.Principal, secret.Value) (string, error) {
			h.backups++
			return "bkp_1", nil
		},
		Recipients:   func(context.Context) ([]string, error) { return []string{"usr_admin"}, nil },
		DropSnapshot: func(context.Context) error { h.dropped++; return nil },
		Notify:       func(_ context.Context, n api.Notification) error { h.notes = append(h.notes, n); return nil },
		Audit:        func(_ context.Context, e audit.Event) { h.events = append(h.events, e) },
		DatabaseURL:  secret.New("postgres://pando:pw@postgres/pando"),
		WorkDir:      t.TempDir(),
		Clock:        h.clock,
		Logger:       zap.NewNop(),
	}
	return h
}

var admin = authz.Principal{Kind: authz.KindUser, ID: "usr_admin", UserID: "usr_admin"}

// TestR355_EveryReasonAnUpgradeIsNotPossibleIsSaid asserts R-355's refusals,
// each with what to change.
func TestR355_EveryReasonAnUpgradeIsNotPossibleIsSaid(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t)
	p, err := h.svc.PlanFor(ctx, "0.3.2")
	require.NoError(t, err)
	require.True(t, p.Possible, "%v", p.Reasons)
	require.Equal(t, "trypando/pando:latest", p.Tag)
	require.Contains(t, p.Note, "Every app is unreachable while Pando restarts")

	h.doc.UpgradeInPlace = false
	p, _ = h.svc.PlanFor(ctx, "0.3.2")
	require.False(t, p.Possible)
	require.Contains(t, p.Reasons[0], "PANDO_POLICY_UPGRADE_IN_PLACE=true")

	h = newHarness(t)
	h.rt.self.Image = "trypando/pando:0.3.1"
	p, _ = h.svc.PlanFor(ctx, "0.3.2")
	require.False(t, p.Possible)
	require.Contains(t, p.Reasons[0], "an exact version")

	h = newHarness(t)
	h.canCopy = errs.New(errs.ValidInvalid, "It needs CREATEDB.").WithRemedy("Grant it.")
	p, _ = h.svc.PlanFor(ctx, "0.3.2")
	require.Contains(t, p.Reasons[0], "CREATEDB")

	h = newHarness(t)
	h.rt.noSelf = true
	p, _ = h.svc.PlanFor(ctx, "0.3.2")
	require.Contains(t, p.Reasons[0], "not running in a container")

	h = newHarness(t)
	p, _ = h.svc.PlanFor(ctx, "0.9.0")
	require.Contains(t, p.Reasons[0], "is not a release newer than")

	h = newHarness(t)
	h.svc.Version = "dev"
	h.updates.st.Development = true
	p, _ = h.svc.PlanFor(ctx, "0.3.2")
	require.Contains(t, p.Reasons[0], "development build")

	_, err = newHarness(t).svc.Start(ctx, admin, upgrade.Request{Version: "0.9.0"})
	require.ErrorContains(t, err, "Pando cannot upgrade itself to 0.9.0")
}

// TestR360_ABreakingUpgradeNeedsTheVersionTypedToConfirm asserts R-360.
func TestR360_ABreakingUpgradeNeedsTheVersionTypedToConfirm(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	p, err := h.svc.PlanFor(ctx, "0.4.0")
	require.NoError(t, err)
	require.Len(t, p.Breaking, 1)
	require.Contains(t, p.Breaking[0].Notes, "Upgrade notes")

	_, err = h.svc.Start(ctx, admin, upgrade.Request{Version: "0.4.0", Passphrase: secret.New("a long passphrase")})
	require.ErrorContains(t, err, "may break something")
	require.Empty(t, h.verified, "refused before anything else happens")

	_, err = h.svc.Start(ctx, admin, upgrade.Request{Version: "0.4.0", ConfirmBreaking: "0.4.0", Passphrase: secret.New("a long passphrase")})
	require.NoError(t, err)
}

// TestR357_AnImageThatDoesNotVerifyStopsTheUpgradeBeforeTheBackup asserts
// R-357's ordering.
func TestR357_AnImageThatDoesNotVerifyStopsTheUpgradeBeforeTheBackup(t *testing.T) {
	h := newHarness(t)
	h.verify = errors.New("no matching signature")
	_, err := h.svc.Start(context.Background(), admin, upgrade.Request{Version: "0.3.2", Passphrase: secret.New("a long passphrase")})
	require.ErrorContains(t, err, "did not verify")
	require.Zero(t, h.backups)
	require.Empty(t, h.rt.pulled)
	require.Empty(t, h.rt.helpers)
	require.Equal(t, []string{"upgrade.refused"}, h.actions())
}

// TestR358_APersonsUpgradeTakesAFullBackupUnlessTheySkipItOutLoud asserts R-358.
func TestR358_APersonsUpgradeTakesAFullBackupUnlessTheySkipItOutLoud(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t)
	_, err := h.svc.Start(ctx, admin, upgrade.Request{Version: "0.3.2"})
	require.ErrorContains(t, err, "needs a passphrase")
	require.Empty(t, h.rt.helpers)

	o, err := h.svc.Start(ctx, admin, upgrade.Request{Version: "0.3.2", Passphrase: secret.New("a long passphrase")})
	require.NoError(t, err)
	require.Equal(t, 1, h.backups)
	require.Equal(t, "bkp_1", o.BackupID)

	h = newHarness(t)
	o, err = h.svc.Start(ctx, admin, upgrade.Request{Version: "0.3.2", SkipBackup: true})
	require.NoError(t, err)
	require.Zero(t, h.backups)
	require.True(t, o.SkipBackup)
	require.Equal(t, []string{"upgrade.backup_skipped", "upgrade.start"}, h.actions(), "skipping is on the record (R-356)")
}

// TestR359_StartingAnUpgradeRunsTheVerifiedDigestThroughTheHelper asserts
// the start of R-359: the digest verified is what is pulled and run, and the
// helper is told where to record how it went.
func TestR359_StartingAnUpgradeRunsTheVerifiedDigestThroughTheHelper(t *testing.T) {
	h := newHarness(t)
	o, err := h.svc.Start(context.Background(), admin, upgrade.Request{Version: "0.3.2", SkipBackup: true})
	require.NoError(t, err)
	require.Equal(t, "trypando/pando@sha256:abc", o.Image)
	require.Equal(t, []string{"trypando/pando@sha256:abc"}, h.rt.pulled)
	require.Len(t, h.rt.helpers, 1)
	spec := h.rt.helpers[0]
	require.Equal(t, "upgrade-helper", spec.Args[0])
	require.Contains(t, spec.Args, "c0ffee")
	require.Contains(t, spec.Args, upgrade.OutcomePath(h.svc.WorkDir))
	require.Equal(t, "postgres://pando:pw@postgres/pando", spec.Env[upgrade.DatabaseURLEnv].Reveal())
	for _, a := range spec.Args {
		require.NotContains(t, a, "pw@", "the database URL never reaches a process listing")
	}

	stored, err := h.svc.Last(context.Background())
	require.NoError(t, err)
	require.Equal(t, upgrade.StateRunning, stored.State)
	require.Equal(t, "trypando/pando:latest", stored.Tag)

	p, _ := h.svc.PlanFor(context.Background(), "0.3.2")
	require.Contains(t, p.Reasons[len(p.Reasons)-1], "already under way")
}

// TestR356_TheNextPandoRecordsHowTheUpgradeWentOnce asserts R-356's outcome
// events, and R-359's soak.
func TestR356_TheNextPandoRecordsHowTheUpgradeWentOnce(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.svc.Version = "0.3.2"
	h.updates.st = update.Status{Current: "0.3.2", Enabled: true}
	require.NoError(t, upgrade.WriteOutcome(upgrade.OutcomePath(h.svc.WorkDir), upgrade.Outcome{
		ID: "upg_1", From: "0.3.1", To: "0.3.2", State: upgrade.StateSucceeded,
		SnapshotAt: h.clock.Now(), FinishedAt: h.clock.Now(),
	}))

	h.svc.Tick(ctx)
	h.svc.Tick(ctx)
	require.Equal(t, []string{"upgrade.succeeded"}, h.actions(), "once, however many passes")
	require.Equal(t, 1, h.rt.removed)
	require.Zero(t, h.dropped, "the copy stays through the soak")

	h.clock.Advance(25 * time.Hour)
	h.svc.Tick(ctx)
	require.Equal(t, 1, h.dropped)
	o, _ := h.svc.Last(ctx)
	require.True(t, o.SnapshotGone)

	// A rollback is recorded and the holders of install.upgrade are told.
	h = newHarness(t)
	require.NoError(t, upgrade.WriteOutcome(upgrade.OutcomePath(h.svc.WorkDir), upgrade.Outcome{
		ID: "upg_2", From: "0.3.1", To: "0.4.0", State: upgrade.StateRolledBack, Reason: "0.4.0 did not start",
	}))
	h.updates.st.Available = false
	h.svc.Tick(ctx)
	require.Equal(t, []string{"upgrade.rolled_back"}, h.actions())
	require.Len(t, h.notes, 1)
	require.Equal(t, api.NotifyUpgradeFailed, h.notes[0].Kind)
	require.Equal(t, "usr_admin", h.notes[0].Recipients[0].UserID)

	// A helper that died without a word stops blocking, and says so.
	h = newHarness(t)
	h.updates.st.Available = false
	require.NoError(t, upgrade.WriteOutcome(upgrade.OutcomePath(h.svc.WorkDir), upgrade.Outcome{
		ID: "upg_3", To: "0.4.0", State: upgrade.StateRunning, StartedAt: h.clock.Now(),
	}))
	h.clock.Advance(time.Hour)
	h.svc.Tick(ctx)
	require.Equal(t, []string{"upgrade.failed"}, h.actions())
}

// TestR362_UpdateAvailableIsSentOncePerVersion asserts R-362.
func TestR362_UpdateAvailableIsSentOncePerVersion(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.svc.Tick(ctx)
	h.svc.Tick(ctx)
	require.Len(t, h.notes, 1)
	require.Equal(t, api.NotifyUpdateAvailable, h.notes[0].Kind)
	require.Contains(t, h.notes[0].Subject, "0.4.0")

	h.updates.st.Latest = "0.4.1"
	h.updates.st.Security = true
	h.svc.Tick(ctx)
	require.Len(t, h.notes, 2)
	require.Contains(t, h.notes[1].Body, "security advisory")
}

// TestR361_AnAutomaticUpgradeTakesTheNewestPatchInsideTheWindowOnly asserts R-361.
func TestR361_AnAutomaticUpgradeTakesTheNewestPatchInsideTheWindowOnly(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.updates.st.Releases = append(h.updates.st.Releases, update.Release{Version: "0.3.3"})
	h.doc.AutoUpgradePatches = true
	h.doc.MaintenanceWindow = "mon 02:00 2h"

	h.svc.Tick(ctx)
	require.Empty(t, h.rt.helpers, "Sunday is outside a Monday window")

	h.doc.MaintenanceWindow = "sun 02:00 2h"
	h.svc.Tick(ctx)
	require.Len(t, h.rt.helpers, 1)
	require.Equal(t, []string{"0.3.3"}, h.verified, "the newest patch of 0.3, never 0.4.0")
	require.Zero(t, h.backups, "no one is here to give a passphrase; the copy is the rollback")
	o, _ := h.svc.Last(ctx)
	require.True(t, o.Automatic)
	require.Equal(t, "system", o.StartedBy)

	// A version an upgrade already failed to reach is not tried again.
	o.State, o.Recorded = upgrade.StateRolledBack, true
	require.NoError(t, upgrade.WriteOutcome(upgrade.OutcomePath(h.svc.WorkDir), *o))
	h.svc.Tick(ctx)
	require.Len(t, h.rt.helpers, 1)
}
