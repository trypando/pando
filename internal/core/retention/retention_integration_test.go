//go:build integration

package retention_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/retention"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

type fixture struct {
	ctx   context.Context
	db    *state.DB
	apps  *state.Apps
	owner string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	return fixture{ctx: ctx, db: db, apps: state.NewApps(db), owner: first.User.ID}
}

func (f fixture) app(t *testing.T) (string, state.Revision, state.Revision) {
	t.Helper()
	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"}
	app, err := f.apps.Create(f.ctx, "ret-"+id.New(id.App)[4:12], id.New(id.App), f.owner, f.owner, src)
	require.NoError(t, err)
	revs := make([]state.Revision, 2)
	for i := range revs {
		revs[i], err = f.apps.CreateRevision(f.ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
		}, spec.OriginManual, f.owner)
		require.NoError(t, err)
	}
	return app.ID, revs[0], revs[1]
}

func (f fixture) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, f.db.QueryRow(f.ctx, query, args...).Scan(&n))
	return n
}

// TestR224_RetentionRemovesOldDeploysButNeverARollbackTarget asserts the
// deployment retention rule (issue #72): an app keeps its newest N deploys,
// and beyond them every deploy goes except one in flight and the newest
// successful deploy of a revision the app ever pinned — the row that makes
// rolling back to it free of approval (R-157) and that the reconciler
// restores its image from. No spec revision is ever deleted (R-152).
func TestR224_RetentionRemovesOldDeploysButNeverARollbackTarget(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	appID, pinned, other := f.app(t)
	require.NoError(t, f.apps.Pin(f.ctx, appID, pinned.ID, state.StateRunning, f.owner))
	deployments := state.NewDeployments(f.db)

	deploy := func(rev state.Revision, status string) string {
		dep, err := deployments.Create(f.ctx, appID, rev.ID, state.TriggerManual, f.owner)
		require.NoError(t, err)
		if status != state.DeployPending {
			require.NoError(t, deployments.Finish(f.ctx, dep.ID, status, "", ""))
		}
		return dep.ID
	}
	rollbackTarget := deploy(pinned, state.DeploySucceeded)
	var failed []string
	for i := 0; i < 5; i++ {
		failed = append(failed, deploy(other, state.DeployFailed))
	}
	inFlight := deploy(other, state.DeployPending)
	revisions := f.count(t, `SELECT count(*) FROM spec_revisions WHERE app_id = $1`, appID)

	job := &retention.Job{
		Store:    state.NewRetention(f.db),
		Settings: retention.Settings{DeploymentsPerApp: 2, Batch: 2},
		Logger:   zap.NewNop(),
	}
	removed := job.Pass(f.ctx)
	require.EqualValues(t, 4, removed["deployments"], "batched, and drained in one pass")

	kept := map[string]bool{}
	list, err := deployments.ListForApp(f.ctx, appID)
	require.NoError(t, err)
	for _, d := range list {
		kept[d.ID] = true
	}
	require.True(t, kept[inFlight], "a deploy in flight is never removed")
	require.True(t, kept[failed[4]], "the newest N are kept")
	require.True(t, kept[rollbackTarget], "the newest successful deploy of a pinned revision is kept")
	require.Len(t, kept, 3)

	ran, err := deployments.RanSuccessfully(f.ctx, appID, pinned.ID)
	require.NoError(t, err)
	require.True(t, ran, "so rolling back to it still needs no approval (R-157)")
	require.Equal(t, revisions, f.count(t, `SELECT count(*) FROM spec_revisions WHERE app_id = $1`, appID),
		"no revision is removed (R-152)")
}

// TestR224_RetentionRemovesExpiredRowsFromTablesThatOnlyGrew asserts the rest
// of the job: sessions, idempotency keys, notifications, sign-in flows and
// scans past their windows go; what is inside them stays.
func TestR224_RetentionRemovesExpiredRowsFromTablesThatOnlyGrew(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	appID, rev, _ := f.app(t)
	now := time.Now().UTC()
	old := now.Add(-100 * 24 * time.Hour)

	var adapterID string
	require.NoError(t, f.db.QueryRow(f.ctx, `SELECT id FROM identity_adapters LIMIT 1`).Scan(&adapterID))
	exec := func(query string, args ...any) {
		t.Helper()
		_, err := f.db.Exec(f.ctx, query, args...)
		require.NoError(t, err)
	}
	// Sessions: expired long ago, revoked long ago, and live.
	exec(`INSERT INTO sessions (id, user_id, adapter_id, expires_at) VALUES ($1, $2, $3, $4)`,
		id.New(id.Session), f.owner, adapterID, old)
	exec(`INSERT INTO sessions (id, user_id, adapter_id, expires_at, revoked_at) VALUES ($1, $2, $3, $4, $5)`,
		id.New(id.Session), f.owner, adapterID, now.Add(time.Hour), old)
	live := id.New(id.Session)
	exec(`INSERT INTO sessions (id, user_id, adapter_id, expires_at) VALUES ($1, $2, $3, $4)`,
		live, f.owner, adapterID, now.Add(time.Hour))

	// Idempotency keys: one from last week, one from a minute ago.
	exec(`INSERT INTO idempotency_keys (key, principal_id, endpoint, status_code, body, created_at)
		VALUES ('old', $1, 'POST /apps', 202, '{}', $2)`, f.owner, now.Add(-7*24*time.Hour))
	exec(`INSERT INTO idempotency_keys (key, principal_id, endpoint, status_code, body)
		VALUES ('new', $1, 'POST /apps', 202, '{}')`, f.owner)

	// Notifications: one past its retain_until, one with none.
	exec(`INSERT INTO notifications (id, user_id, kind, subject, retain_until) VALUES ($1, $2, 'k', 's', $3)`,
		id.New(id.Notification), f.owner, now.Add(-time.Minute))
	exec(`INSERT INTO notifications (id, user_id, kind, subject) VALUES ($1, $2, 'k', 's')`,
		id.New(id.Notification), f.owner)

	// Scans: three old scans of one revision; only the newest stays.
	for i := 0; i < 3; i++ {
		exec(`INSERT INTO app_scans (id, app_id, spec_id, scanner_ref, ran_at) VALUES ($1, $2, $3, 'scn', $4)`,
			id.New(id.Scan), appID, rev.ID, old.Add(time.Duration(i)*time.Minute))
	}

	job := &retention.Job{
		Store:  state.NewRetention(f.db),
		Clock:  clock.NewFake(now),
		Logger: zap.NewNop(),
	}
	job.Pass(f.ctx)

	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM sessions WHERE user_id = $1`, f.owner))
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM sessions WHERE id = $1`, live), "a live session stays")
	require.Equal(t, 0, f.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'old'`))
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM idempotency_keys WHERE key = 'new'`))
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM notifications WHERE user_id = $1`, f.owner))
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM app_scans WHERE app_id = $1`, appID),
		"the newest scan of a revision is kept however old (R-319)")
	require.Equal(t, 1, f.count(t, `SELECT count(*) FROM app_scans WHERE app_id = $1 AND ran_at = $2`,
		appID, old.Add(2*time.Minute)))
}
