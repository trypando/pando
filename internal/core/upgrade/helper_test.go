package upgrade_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/upgrade"
)

// fakeSwap records the helper's steps and fails the one it is told to.
type fakeSwap struct {
	steps []string
	fail  map[string]error
}

func (f *fakeSwap) step(name string) error {
	f.steps = append(f.steps, name)
	return f.fail[name]
}
func (f *fakeSwap) Stop(context.Context) error                 { return f.step("stop") }
func (f *fakeSwap) Recreate(context.Context, string) error     { return f.step("recreate") }
func (f *fakeSwap) Ready(context.Context, time.Duration) error { return f.step("ready") }
func (f *fakeSwap) Logs(context.Context) string {
	f.steps = append(f.steps, "logs")
	return "panic: migration 43"
}
func (f *fakeSwap) Discard(context.Context)                      { f.steps = append(f.steps, "discard") }
func (f *fakeSwap) Restore(context.Context) error                { return f.step("restore") }
func (f *fakeSwap) Finish(context.Context, string, string) error { return f.step("finish") }
func (f *fakeSwap) Snapshot(context.Context) error               { return f.step("db.snapshot") }
func (f *fakeSwap) RestoreDatabase(context.Context) error        { return f.step("db.restore") }

type fakeDB struct{ s *fakeSwap }

func (d fakeDB) Snapshot(ctx context.Context) error { return d.s.Snapshot(ctx) }
func (d fakeDB) Restore(ctx context.Context) error  { return d.s.RestoreDatabase(ctx) }

func runHelper(t *testing.T, fail map[string]error) (upgrade.Outcome, []string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "upgrade", "outcome.json")
	require.NoError(t, upgrade.WriteOutcome(path, upgrade.Outcome{
		ID: "upg_1", From: "0.3.1", To: "0.4.0", Image: "trypando/pando@sha256:new",
		Tag: "trypando/pando:latest", State: upgrade.StateRunning,
	}))
	s := &fakeSwap{fail: fail}
	o := upgrade.RunHelper(context.Background(), path, s, fakeDB{s}, time.Minute, clock.NewFake(time.Unix(0, 0)))

	stored, err := upgrade.ReadOutcome(path)
	require.NoError(t, err)
	require.Equal(t, o, *stored, "what is returned is what the next Pando reads")
	return o, s.steps
}

// TestR359_AFailedUpgradePutsThePreviousVersionBackOnItsOwn asserts R-359's
// sequence at each step that can fail.
func TestR359_AFailedUpgradePutsThePreviousVersionBackOnItsOwn(t *testing.T) {
	boom := errors.New("boom")

	t.Run("succeeds, and only then moves the tag", func(t *testing.T) {
		o, steps := runHelper(t, nil)
		require.Equal(t, upgrade.StateSucceeded, o.State)
		require.Equal(t, []string{"stop", "db.snapshot", "recreate", "ready", "finish"}, steps)
		require.False(t, o.SnapshotAt.IsZero(), "the copy is kept, to be dropped after the soak")
		require.False(t, o.SnapshotGone)
	})

	t.Run("the new version never reports ready", func(t *testing.T) {
		o, steps := runHelper(t, map[string]error{"ready": boom})
		require.Equal(t, upgrade.StateRolledBack, o.State)
		require.Equal(t, []string{"stop", "db.snapshot", "recreate", "ready", "logs", "discard", "db.restore", "restore"}, steps,
			"the database is restored before the previous version starts, which would refuse a migrated one (R-354)")
		require.Contains(t, o.Reason, "put 0.3.1 back")
		require.Equal(t, "panic: migration 43", o.Logs, "why it failed travels with the outcome")
		require.True(t, o.SnapshotGone)
		require.NotContains(t, steps, "finish", "the tag never moves to a version that failed")
	})

	t.Run("the new container cannot be created", func(t *testing.T) {
		o, steps := runHelper(t, map[string]error{"recreate": boom})
		require.Equal(t, upgrade.StateRolledBack, o.State)
		require.Equal(t, []string{"stop", "db.snapshot", "recreate", "logs", "discard", "db.restore", "restore"}, steps)
	})

	t.Run("the database cannot be copied", func(t *testing.T) {
		o, steps := runHelper(t, map[string]error{"db.snapshot": boom})
		require.Equal(t, upgrade.StateRolledBack, o.State)
		require.Equal(t, []string{"stop", "db.snapshot", "restore"}, steps, "the new version never starts without a copy to go back to")
		require.Contains(t, o.Reason, "did not go ahead")
	})

	t.Run("Pando cannot be stopped", func(t *testing.T) {
		o, steps := runHelper(t, map[string]error{"stop": boom})
		require.Equal(t, upgrade.StateFailed, o.State)
		require.Equal(t, []string{"stop", "restore"}, steps)
		require.Contains(t, o.Reason, "nothing was changed")
	})

	t.Run("the database cannot be restored", func(t *testing.T) {
		o, steps := runHelper(t, map[string]error{"ready": boom, "db.restore": boom})
		require.Equal(t, upgrade.StateFailed, o.State)
		require.NotContains(t, steps[len(steps)-1:], "restore",
			"the previous version is not started against a database it would refuse")
		require.Contains(t, o.Reason, "restore the full backup taken before the upgrade")
	})

	t.Run("healthy, but the tag could not be moved", func(t *testing.T) {
		o, _ := runHelper(t, map[string]error{"finish": boom})
		require.Equal(t, upgrade.StateSucceeded, o.State)
		require.Contains(t, o.Reason, "set the image where Pando is deployed")
	})
}

func TestNoUpgradeToRunIsAFailureNotAPanic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.json")
	s := &fakeSwap{}
	o := upgrade.RunHelper(context.Background(), path, s, fakeDB{s}, time.Minute, clock.NewFake(time.Unix(0, 0)))
	require.Equal(t, upgrade.StateFailed, o.State)
	require.Empty(t, s.steps)
}
