//go:build integration

package reconciler_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// R-152: history is trimmed, and never a revision anyone could roll back to.
//
// The exemption is the whole test. Rollback is repointing at a revision that
// provably existed, and spec_pins is that proof — pruning a revision someone
// had pinned would make a rollback target vanish from under a person looking
// at it. Getting this wrong costs history that cannot be recovered, which is
// why the saving is never worth a doubt.
func TestR152_PruningNeverRemovesARevisionThatWasEverPinned(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "gc-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	// Twenty-five revisions against a retention of ten. Two of them get pinned
	// early, so they are well outside the window the trim keeps.
	var pinnedEarly []string
	for i := range 25 {
		s := &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			AppID:         app.ID,
			Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
			Build:         spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads:     []spec.Workload{{Name: "web", Image: fmt.Sprintf("example/app:%d", i), Primary: true, Exposed: true}},
			Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
			Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
			Deploy:        spec.Deploy{Strategy: spec.DeployRecreate},
			Retention:     spec.Retention{SpecRevisions: 10},
		}
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginEdited, owner)
		require.NoError(t, err)

		if i == 1 || i == 3 {
			require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))
			pinnedEarly = append(pinnedEarly, rev.ID)
		}
	}

	// And the current pin, which is also the retention setting in force.
	all, err := apps.ListRevisions(ctx, app.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, all[0].ID, state.StateRunning, owner))

	gc := &reconciler.GC{Apps: apps, Logger: zap.NewNop()}
	gc.Collect(ctx)

	for _, specID := range pinnedEarly {
		_, found, err := apps.RevisionByID(ctx, specID)
		require.NoError(t, err)
		require.True(t, found,
			"revision %s was pinned once and must stay: it is a rollback target", specID)
	}

	remaining, err := apps.ListRevisions(ctx, app.ID)
	require.NoError(t, err)
	require.Less(t, len(remaining), 25, "something was pruned")
	require.GreaterOrEqual(t, len(remaining), 10, "the retention window is kept")
}

// R-319 against R-152: a scan is an append-only fact about one revision, and
// pruning history must neither fail on it nor rewrite it.
//
// The scan's foreign key said ON DELETE SET NULL, which is an UPDATE of
// app_scans, which the append-only trigger refuses — so once a revision that
// had been scanned fell outside the retention window, every prune failed and
// no history was trimmed on that install again.
func TestR319_PruningKeepsARevisionAScanDescribes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	scans := state.NewScans(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "gc-scan-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	var scannedRev, unscannedRev string
	for i := range 15 {
		s := &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			AppID:         app.ID,
			Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
			Build:         spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads:     []spec.Workload{{Name: "web", Image: fmt.Sprintf("example/app:%d", i), Primary: true, Exposed: true}},
			Routing:       spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
			Runtime:       spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
			Deploy:        spec.Deploy{Strategy: spec.DeployRecreate},
			Retention:     spec.Retention{SpecRevisions: 10},
		}
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginEdited, owner)
		require.NoError(t, err)
		switch i {
		case 0:
			unscannedRev = rev.ID
		case 1:
			// A draft somebody scanned before deciding not to deploy it.
			scannedRev = rev.ID
		}
	}
	seventy := 70
	scan, err := scans.Record(ctx, state.Scan{AppID: app.ID, SpecID: scannedRev, ScannerRef: "scn_fake", Score: &seventy})
	require.NoError(t, err)

	all, err := apps.ListRevisions(ctx, app.ID)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, all[0].ID, state.StateRunning, owner))

	pruned, err := apps.PruneSpecRevisions(ctx)
	require.NoError(t, err, "a scanned revision outside the window must not make pruning fail")
	require.Positive(t, pruned, "the unscanned history outside the window is still trimmed")

	_, found, err := apps.RevisionByID(ctx, unscannedRev)
	require.NoError(t, err)
	require.False(t, found, "an unscanned, never-pinned revision outside the window is pruned")

	_, found, err = apps.RevisionByID(ctx, scannedRev)
	require.NoError(t, err)
	require.True(t, found, "the revision a scan describes stays, so the scan still says what it scanned")

	history, err := scans.History(ctx, app.ID, 10)
	require.NoError(t, err)
	require.Len(t, history, 1)
	require.Equal(t, scan.ID, history[0].ID)
	require.Equal(t, scannedRev, history[0].SpecID, "the scan was not rewritten")

	// The app's own deletion still takes its revisions and scans together.
	_, err = db.Exec(ctx, `DELETE FROM apps WHERE id = $1`, app.ID)
	require.NoError(t, err, "deleting an app with scanned revisions cascades cleanly")
}

// Pruning is idempotent and safe to run on an install with nothing to prune.
func TestPruningAnAppWithLittleHistoryDoesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "gc-small-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	s := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID,
		Source:    spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:     spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true}},
		Routing:   spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime:   spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:    spec.Deploy{Strategy: spec.DeployRecreate},
	}
	rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginDetected, owner)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))

	gc := &reconciler.GC{Apps: apps, Logger: zap.NewNop()}
	gc.Collect(ctx)
	gc.Collect(ctx)

	remaining, err := apps.ListRevisions(ctx, app.ID)
	require.NoError(t, err)
	require.Len(t, remaining, 1)
}

// fakeBackups takes backups without a destination, or fails as told.
type fakeBackups struct {
	fail  error
	taken []backup.AppCreateRequest
}

func (f *fakeBackups) CreateForApp(_ context.Context, id string, req backup.AppCreateRequest) (backup.Created, error) {
	if f.fail != nil {
		return backup.Created{}, f.fail
	}
	f.taken = append(f.taken, req)
	return backup.Created{AdapterRef: "bk_fake", ObjectName: id}, nil
}

func (f *fakeBackups) Discard(context.Context, string, string) error { return nil }

// backupGC is a GC with one running app that keeps its data in a volume, and
// the log it writes.
func backupGC(t *testing.T, runner *fakeBackups, recordVolume bool) (*reconciler.RollingBackups, string, *observer.ObservedLogs) {
	t.Helper()
	ctx := context.Background()
	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "bk-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	s := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         app.ID,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true,
			Mounts: []spec.Mount{{VolumeID: "data", Path: "/data"}}}},
		Volumes:   []spec.Volume{{ID: "data", Name: "data"}},
		Routing:   spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime:   spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:    spec.Deploy{Strategy: spec.DeployRecreate},
		Retention: spec.Retention{BackupDailyCount: 7},
	}
	rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginEdited, owner)
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, state.StateRunning, owner))

	if recordVolume {
		require.NoError(t, state.NewVolumes(db).RecordFromRuntime(ctx, app.ID, "rt_fake",
			[]state.VolumeRecord{{VolumeID: "data", Name: "data", Handle: "pando-" + app.ID + "-data"}}))
	}

	core, logs := observer.New(zap.InfoLevel)
	return &reconciler.RollingBackups{
		Apps:         apps,
		Logger:       zap.New(core),
		Backups:      state.NewBackups(db),
		Backup:       runner,
		BundleSource: state.NewBundleSource(db),
		// Tried again on the very next pass, so the test need not wait the
		// hour a failed attempt waits in production.
		RetryAfter: time.Nanosecond,
	}, app.ID, logs
}

func lastAttempt(t *testing.T, gc *reconciler.RollingBackups, appID string) state.BackupAttempt {
	t.Helper()
	attempts, err := gc.Backups.Attempts(context.Background(), appID)
	require.NoError(t, err)
	require.Len(t, attempts, 1, "every scheduled attempt leaves a record")
	return attempts[0]
}

// TestR211_ARollingBackupIsTakenAndRecorded asserts the ordinary case: a
// running app with storage gets its daily backup, and the attempt says so.
func TestR211_ARollingBackupIsTakenAndRecorded(t *testing.T) {
	t.Parallel()
	runner := &fakeBackups{}
	gc, appID, logs := backupGC(t, runner, true)

	gc.Pass(context.Background())

	require.Len(t, runner.taken, 1)
	require.Len(t, runner.taken[0].Volumes, 1)
	attempt := lastAttempt(t, gc, appID)
	require.Equal(t, state.AttemptTaken, attempt.Outcome)
	require.NotEmpty(t, attempt.BackupID)
	require.Equal(t, 1, logs.FilterMessage("took a rolling backup").Len())

	// Taken once a day, not once a pass: the job asks for what is due.
	gc.Pass(context.Background())
	require.Len(t, runner.taken, 1)
}

// TestR211_ABackupWithNothingToCopyIsRecordedAsSkipped asserts issue #87's
// quietest failure: a spec that keeps data, and no record of the volume.
//
// This used to return nil, and the sweep logged "took a rolling backup" for a
// backup that was never written — every hour, for as long as it ran.
func TestR211_ABackupWithNothingToCopyIsRecordedAsSkipped(t *testing.T) {
	t.Parallel()
	runner := &fakeBackups{}
	gc, appID, logs := backupGC(t, runner, false)

	gc.Pass(context.Background())

	require.Empty(t, runner.taken)
	require.Zero(t, logs.FilterMessage("took a rolling backup").Len(), "nothing was taken, so nothing says it was")
	attempt := lastAttempt(t, gc, appID)
	require.Equal(t, state.AttemptSkipped, attempt.Outcome)
	require.Contains(t, attempt.Message, "nothing to copy")
	require.Contains(t, attempt.Remedy, "redeploy the app")
}

// TestR211_AFailedBackupIsRecordedWithWhatToDo asserts that a failure reaches
// somewhere a person looks, in the words of the error that caused it (R-105).
func TestR211_AFailedBackupIsRecordedWithWhatToDo(t *testing.T) {
	t.Parallel()
	runner := &fakeBackups{fail: errs.New(errs.Internal, "This installation has nowhere to put a backup.").
		WithRemedy("Configure a backup destination before taking a backup.")}
	gc, appID, _ := backupGC(t, runner, true)

	gc.Pass(context.Background())

	attempt := lastAttempt(t, gc, appID)
	require.Equal(t, state.AttemptFailed, attempt.Outcome)
	require.Equal(t, "This installation has nowhere to put a backup.", attempt.Message)
	require.Equal(t, "Configure a backup destination before taking a backup.", attempt.Remedy)

	// Tried again on the next pass rather than given up on, and the record
	// is replaced rather than accumulated.
	runner.fail = nil
	gc.Pass(context.Background())
	require.Len(t, runner.taken, 1)
	require.Equal(t, state.AttemptTaken, lastAttempt(t, gc, appID).Outcome)
}
