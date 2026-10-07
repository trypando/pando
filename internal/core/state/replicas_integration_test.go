//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
)

// Several Pando processes against one database (issue #72). Each "replica"
// here is its own connection to one database, which is all a replica is to
// the state store.

// TestR256_ASecondReplicaStartingDoesNotLockTheFirstOut asserts that replicas
// share one database without disturbing each other (R-256, as amended for
// issue #72).
//
// Every start used to give the application role a fresh password. The first
// replica's pool kept its open connections and could open no new ones, so a
// rolling restart or a scale-up took every other replica down.
func TestR256_ASecondReplicaStartingDoesNotLockTheFirstOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	first, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
	require.NoError(t, err)
	t.Cleanup(first.Close)

	// Several more start at once, as a scale-up does. Bootstrap is
	// serialized, so none trips over another's grants.
	var wg sync.WaitGroup
	errs := make([]error, 3)
	dbs := make([]*state.DB, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			dbs[i], errs[i] = state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err)
		t.Cleanup(dbs[i].Close)
		require.NotEqual(t, first.Replica(), dbs[i].Replica(), "every process is a replica of its own")
	}

	// Drop every connection the first replica held: what it opens now uses
	// the password it was given, which must still be the role's.
	first.Reset()
	require.NoError(t, first.Ping(ctx), "the first replica can still open new connections")
}

// TestR348_TheApplicationRoleCannotReadTheStoredRolePasswords asserts R-348
// against the passwords kept so that replicas agree on them: the role serving
// traffic must not be able to log in as the role that removes audit history.
func TestR348_TheApplicationRoleCannotReadTheStoredRolePasswords(t *testing.T) {
	t.Parallel()
	db := connected(t)
	_, err := db.Exec(context.Background(), `SELECT password FROM pando_private.role_passwords`)
	require.Error(t, err)
	require.Contains(t, err.Error(), "permission denied")
}

// TestR256_OnlyAStoppedReplicasWorkIsRecovered asserts that a starting or
// sweeping replica leaves another live replica's deploys and detections alone,
// and puts a stopped one's back in the queue (O-32).
func TestR256_OnlyAStoppedReplicasWorkIsRecovered(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	live, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t) // the same password every copy uses
	gone, err := state.ConnectCopy(ctx, ownerURL, appPassword)
	require.NoError(t, err)
	t.Cleanup(gone.Close)

	register := func(db *state.DB) {
		require.NoError(t, state.NewReplicas(db).Register(ctx, state.Replica{
			ID: db.Replica(), Hostname: "test", AssertionKID: "kid", AssertionKey: make([]byte, 32),
		}))
	}
	register(live)
	register(gone)

	owner := seedUser(t, live, "replica-owner")
	apps := state.NewApps(live)
	deploy := func(db *state.DB, name string) (string, string) {
		app, err := apps.Create(ctx, name, id.New(id.App), owner.ID, owner.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name})
		require.NoError(t, err)
		rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
			SchemaVersion: spec.SchemaVersion, AppID: app.ID,
			Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + name},
		}, spec.OriginManual, owner.ID)
		require.NoError(t, err)
		dep, err := state.NewDeployments(db).Create(ctx, app.ID, rev.ID, "manual", owner.ID)
		require.NoError(t, err)
		claimed, err := state.NewDeployments(db).Claim(ctx, 1)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, state.NewDeployments(db).SetStatus(ctx, dep.ID, state.DeployBuilding))
		require.NoError(t, state.NewDetections(db).Start(ctx, app.ID))
		apps, err := state.NewDetections(db).Claim(ctx, 1)
		require.NoError(t, err)
		require.Equal(t, []string{app.ID}, apps)
		return app.ID, dep.ID
	}
	liveApp, liveDep := deploy(live, "on-live")
	goneApp, goneDep := deploy(gone, "on-gone")

	deployments := state.NewDeployments(live)
	detections := state.NewDetections(live)
	runner, err := deployments.Runner(ctx, goneDep)
	require.NoError(t, err)
	require.Equal(t, gone.Replica(), runner, "a deploy records the replica running it")

	// Both alive: nothing is anybody's to recover.
	n, err := deployments.RecoverInFlight(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "a live replica's deploys are left alone")
	n, err = detections.RecoverRunning(ctx)
	require.NoError(t, err)
	require.Zero(t, n, "a live replica's detections are left alone")

	// One stops.
	require.NoError(t, state.NewReplicas(gone).Stop(ctx, gone.Replica()))
	n, err = deployments.RecoverInFlight(ctx)
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = detections.RecoverRunning(ctx)
	require.NoError(t, err)

	dep, _, err := deployments.ByID(ctx, goneDep)
	require.NoError(t, err)
	require.Equal(t, state.DeployPending, dep.Status, "back in the queue")
	waiting, err := deployments.Waiting(ctx, goneDep)
	require.NoError(t, err)
	require.True(t, waiting, "for any replica to take")
	dep, _, err = deployments.ByID(ctx, liveDep)
	require.NoError(t, err)
	require.Equal(t, state.DeployBuilding, dep.Status, "the live replica's deploy carries on")

	got, err := detections.Get(ctx, goneApp)
	require.NoError(t, err)
	require.Equal(t, state.DetectionRunning, got.Status)
	requeued, err := detections.Claim(ctx, 5)
	require.NoError(t, err)
	require.Equal(t, []string{goneApp}, requeued, "the stopped replica's detection is queued again")
	got, err = detections.Get(ctx, liveApp)
	require.NoError(t, err)
	require.Equal(t, state.DetectionRunning, got.Status)

	// A stopped replica keeps its heartbeat refused: it must come back as a
	// new replica, not carry on as the one whose work was abandoned.
	alive, err := state.NewReplicas(gone).Heartbeat(ctx, gone.Replica())
	require.NoError(t, err)
	require.False(t, alive)
}

// TestR256_OneReplicaLeadsAtATime asserts that the install-wide jobs have one
// runner: the leader lock is held by one replica, and passes on when it is
// given up or its holder's connection goes.
func TestR256_OneReplicaLeadsAtATime(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t)
	b, err := state.ConnectCopy(ctx, ownerURL, appPassword)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	lead, ok, err := state.NewReplicas(a).TryLead(ctx, time.Second)
	require.NoError(t, err)
	require.True(t, ok)

	_, ok, err = state.NewReplicas(b).TryLead(ctx, time.Second)
	require.NoError(t, err)
	require.False(t, ok, "a second replica does not lead while the first does")

	lead.Resign(ctx)
	next, ok, err := state.NewReplicas(b).TryLead(ctx, time.Second)
	require.NoError(t, err)
	require.True(t, ok, "leadership passes on once given up")

	// A leader whose process dies releases the lock with its connection. Its
	// backend is ended from outside, which is what Postgres sees of a lost pod.
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	_, err = owner.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_locks
		WHERE locktype = 'advisory' AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		select {
		case <-next.Lost():
			return true
		default:
			return false
		}
	}, 10*time.Second, 100*time.Millisecond, "a leader that loses its connection knows it")
	next.Resign(ctx) // a no-op once lost
	again, ok, err := state.NewReplicas(a).TryLead(ctx, time.Second)
	require.NoError(t, err)
	require.True(t, ok, "and another replica can lead")
	again.Resign(ctx)
}

// TestR075a_WrongPasscodesAreCountedAcrossReplicas asserts R-075a's limit
// holds for the install rather than per process: N replicas behind a load
// balancer must not give an attacker N allowances.
func TestR075a_WrongPasscodesAreCountedAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t)
	b, err := state.ConnectCopy(ctx, ownerURL, appPassword)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	key := id.New(id.App) + "|203.0.113.7"
	for range 4 {
		require.NoError(t, state.NewPasscodeFailures(a).Record(ctx, key, time.Hour))
	}
	require.NoError(t, state.NewPasscodeFailures(b).Record(ctx, key, time.Hour))

	n, err := state.NewPasscodeFailures(b).Recent(ctx, key, time.Hour)
	require.NoError(t, err)
	require.Equal(t, 5, n, "a failure on one replica counts on every other")

	require.NoError(t, state.NewPasscodeFailures(a).Clear(ctx, key))
	n, err = state.NewPasscodeFailures(b).Recent(ctx, key, time.Hour)
	require.NoError(t, err)
	require.Zero(t, n)
}

// TestR190_AReplicaWithADifferentSecretsKeyRefusesToStart asserts R-190 holds
// across replicas: the key is not in the database, so a replica given a
// different one must stop before it writes a secret nobody else can read.
func TestR190_AReplicaWithADifferentSecretsKeyRefusesToStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	adapterWithKey := func(path string) *secretslocal.Adapter {
		sa := secretslocal.New()
		cfg, err := json.Marshal(map[string]string{"key_path": path})
		require.NoError(t, err)
		require.NoError(t, sa.Configure(ctx, cfg))
		return sa
	}
	shared := filepath.Join(t.TempDir(), "secrets.key")
	first := state.NewSecrets(db, adapterWithKey(shared), "sek_local")
	require.NoError(t, first.VerifyKey(ctx), "the first replica seeds the check")

	same := state.NewSecrets(db, adapterWithKey(shared), "sek_local")
	require.NoError(t, same.VerifyKey(ctx), "a replica with the same key passes")

	other := state.NewSecrets(db, adapterWithKey(filepath.Join(t.TempDir(), "secrets.key")), "sek_local")
	err := other.VerifyKey(ctx)
	require.Error(t, err, "a replica that generated its own key is refused")
	require.Contains(t, err.Error(), "different secrets encryption key")
}
