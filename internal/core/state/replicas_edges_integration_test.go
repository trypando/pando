//go:build integration

package state_test

import (
	"context"
	"net/url"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// ownerConn is a connection to ownerURL as the owning role, closed when the
// test ends.
func ownerConn(t *testing.T, ownerURL string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// TestR256_AReplicaStartingWhileAnotherBootstrapsWaitsItsTurn asserts that a
// replica starting while another holds the startup lock waits for it rather
// than racing it, and that the wait ends with the caller's context, with a
// message that says what it was waiting for (R-256, issue #72).
func TestR256_AReplicaStartingWhileAnotherBootstrapsWaitsItsTurn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)

	// Another replica is bootstrapping.
	other := ownerConn(t, ownerURL)
	_, err := other.Exec(ctx, `SELECT pg_advisory_lock($1)`, state.BootstrapLock)
	require.NoError(t, err)

	waited := make(chan error, 1)
	go func() {
		db, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
		if err == nil {
			db.Close()
		}
		waited <- err
	}()

	short, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err = state.Connect(short, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
	require.Error(t, err, "a start whose context ends while it waits gives up")
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "startup lock")

	select {
	case err := <-waited:
		t.Fatalf("a replica started while another was bootstrapping: %v", err)
	default:
	}

	_, err = other.Exec(ctx, `SELECT pg_advisory_unlock($1)`, state.BootstrapLock)
	require.NoError(t, err)
	select {
	case err := <-waited:
		require.NoError(t, err, "once the other replica is done, the waiting one starts")
	case <-time.After(30 * time.Second):
		t.Fatal("the waiting replica never started")
	}
}

// TestR256_AnOwnerThatCannotReadTheStoredPasswordsDoesNotReplaceThem asserts
// that a start whose database account cannot prepare or read the stored role
// passwords stops with what it needs, rather than giving the roles fresh
// passwords — which would lock out every replica already running (R-256,
// issue #72).
func TestR256_AnOwnerThatCannotReadTheStoredPasswordsDoesNotReplaceThem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, _ := statetest.Database(t)
	owner := ownerConn(t, ownerURL)

	readStored := func() string {
		var pw string
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT password FROM pando_private.role_passwords WHERE role = $1`, state.AppRole).Scan(&pw))
		return pw
	}
	before := readStored()

	// An account that can log in and read the schema version, and no more.
	role := "limited_" + id.New(id.Replica)[len("rep_"):]
	var dbName string
	require.NoError(t, owner.QueryRow(ctx, `SELECT current_database()`).Scan(&dbName))
	for _, stmt := range []string{
		`CREATE ROLE "` + role + `" LOGIN PASSWORD 'limited'`,
		`GRANT SELECT ON schema_migrations TO "` + role + `"`,
	} {
		_, err := owner.Exec(ctx, stmt)
		require.NoError(t, err)
	}
	u, err := url.Parse(ownerURL)
	require.NoError(t, err)
	u.User = url.UserPassword(role, "limited")
	limitedURL := u.String()

	_, err = state.Connect(ctx, state.ConnectOptions{OwnerURL: limitedURL, SkipMigrate: true})
	require.Error(t, err)
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.Internal, e.Code)
	require.Contains(t, e.Message, "schema that holds its database role passwords")
	require.Contains(t, e.Remedy, "CREATE", "the remedy names the privilege it lacks")

	// It may create in the database and the private schema, but not read the
	// passwords recorded there.
	for _, stmt := range []string{
		`GRANT CREATE ON DATABASE "` + dbName + `" TO "` + role + `"`,
		`GRANT USAGE, CREATE ON SCHEMA pando_private TO "` + role + `"`,
	} {
		_, err := owner.Exec(ctx, stmt)
		require.NoError(t, err)
	}
	_, err = state.Connect(ctx, state.ConnectOptions{OwnerURL: limitedURL, SkipMigrate: true})
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "could not read a database role's password")

	require.Equal(t, before, readStored(), "the password every replica uses is left alone")
}

// TestR256_APasswordThatCannotBeRecordedStopsTheStart asserts that a start
// which had to give a role a new password, and could not record it, fails:
// a password only this process knows is one no other replica could use
// (R-256, issue #72).
func TestR256_APasswordThatCannotBeRecordedStopsTheStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL := startPostgres(t) // the roles are the cluster's; this one is its own

	first, err := state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL})
	require.NoError(t, err)
	first.Close()

	owner := ownerConn(t, ownerURL)
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_password() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'the disk is full'; END $$`,
		`CREATE TRIGGER refuse_password BEFORE INSERT OR UPDATE ON pando_private.role_passwords
			FOR EACH ROW EXECUTE FUNCTION refuse_password()`,
		// Changed outside Pando, so the next start must set a new one.
		`ALTER ROLE ` + state.AppRole + ` PASSWORD 'set-by-hand'`,
	} {
		_, err := owner.Exec(ctx, stmt)
		require.NoError(t, err)
	}

	_, err = state.Connect(ctx, state.ConnectOptions{OwnerURL: ownerURL, SkipMigrate: true})
	require.Error(t, err)
	e := errs.As(err)
	require.NotNil(t, e)
	require.Equal(t, errs.Internal, e.Code)
	require.Contains(t, e.Message, "could not record a database role's password")
	require.Equal(t, state.AppRole, e.Details["role"])
}

// TestR256_AReplicaRowThatCannotBeReadIsAnError asserts that a replicas row
// the process cannot read fails the question rather than being left out: a
// key list missing a live replica's key would refuse every assertion it
// signed, and a live list missing it would take its work for abandoned
// (R-256, R-051).
func TestR256_AReplicaRowThatCannotBeReadIsAnError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	replicas := state.NewReplicas(db)
	repID := registerReplica(t, replicas, "unreadable")

	// A timestamp no Go time can hold.
	_, err := db.Exec(ctx, `UPDATE pando_replicas SET heartbeat_at = 'infinity' WHERE id = $1`, repID)
	require.NoError(t, err)

	_, err = replicas.Live(ctx)
	requireInternal(t, err, "live")
	_, _, err = replicas.ByID(ctx, repID)
	requireInternal(t, err, "by id")
	_, err = replicas.VerificationKeys(ctx, time.Minute)
	requireInternal(t, err, "verification keys")
}

// TestR256_ExclusiveWorkGivesUpWhenItsContextEnds asserts that work waiting
// for another replica's lock stops waiting when its context ends, and never
// runs.
func TestR256_ExclusiveWorkGivesUpWhenItsContextEnds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	const key int64 = 0x7465737402

	conn, err := db.Acquire(ctx)
	require.NoError(t, err)
	t.Cleanup(conn.Release)
	_, err = conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(ctx, `SELECT pg_advisory_unlock($1)`, key) })

	short, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	ran := false
	err = db.Exclusive(short, key, func() error { ran = true; return nil })
	requireInternal(t, err, "a lock held elsewhere past the deadline")
	require.False(t, ran, "work that never got the lock does not run")
}

// raceSeeder is a secrets adapter on a replica that reads no key check, and
// before it records its own, loses the race to another replica that records
// one first.
type raceSeeder struct {
	adapterapi.SecretsAdapter
	once   sync.Once
	winner func() error
	err    error
}

func (r *raceSeeder) Put(ctx context.Context, ref adapterapi.SecretRef, v secret.Value) (adapterapi.StoredRef, error) {
	r.once.Do(func() { r.err = r.winner() })
	return r.SecretsAdapter.Put(ctx, ref, v)
}

// TestR190_AReplicaThatLosesTheRaceToSeedTheKeyCheckIsCheckedAgainstTheWinner
// asserts that the replica whose key check was not the one recorded is held
// to the one that was: with the same key it starts, with another it is
// refused (R-190, issue #72).
func TestR190_AReplicaThatLosesTheRaceToSeedTheKeyCheckIsCheckedAgainstTheWinner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		sameKey  bool
		accepted bool
	}{
		{name: "same key", sameKey: true, accepted: true},
		{name: "different key", sameKey: false, accepted: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			db := connected(t)
			winnerKey := filepath.Join(t.TempDir(), "winner.key")
			loserKey := winnerKey
			if !tc.sameKey {
				loserKey = filepath.Join(t.TempDir(), "loser.key")
			}
			winner := state.NewSecrets(db, localSecrets(t, winnerKey), "sek_local")
			loser := &raceSeeder{
				SecretsAdapter: localSecrets(t, loserKey),
				winner:         func() error { return winner.VerifyKey(ctx) },
			}

			err := state.NewSecrets(db, loser, "sek_local").VerifyKey(ctx)
			require.NoError(t, loser.err, "the winner seeds the check")
			if tc.accepted {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				require.Contains(t, errs.As(err).Message, "different secrets encryption key")
			}
			n, _ := canaryRows(t, db)
			require.Equal(t, 1, n, "one check, the winner's")
		})
	}
}

// TestR190_AKeyCheckThatCannotBeRecordedStopsTheStart asserts that a replica
// that cannot record the key check does not start unchecked (R-190).
func TestR190_AKeyCheckThatCannotBeRecordedStopsTheStart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner := ownerConn(t, ownerURL)
	for _, stmt := range []string{
		`CREATE FUNCTION refuse_canary() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN RAISE EXCEPTION 'the disk is full'; END $$`,
		`CREATE TRIGGER refuse_canary BEFORE INSERT ON secrets_canary
			FOR EACH ROW EXECUTE FUNCTION refuse_canary()`,
	} {
		_, err := owner.Exec(ctx, stmt)
		require.NoError(t, err)
	}

	err := state.NewSecrets(db, localSecrets(t, filepath.Join(t.TempDir(), "k")), "sek_local").VerifyKey(ctx)
	requireInternal(t, err, "record the key check")
	require.Contains(t, errs.As(err).Message, "Could not record the check")
}

// TestR256_ADeployFromBeforeReplicasWereRecordedHasNoRunner asserts that a
// deploy started before replicas were recorded names no replica, so its live
// log is looked for nowhere else.
func TestR256_ADeployFromBeforeReplicasWereRecordedHasNoRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	owner := seedUser(t, db, "pre-replica-owner")
	apps := state.NewApps(db)
	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/old"}
	app, err := apps.Create(ctx, "old", id.New(id.App), owner.ID, owner.ID, src)
	require.NoError(t, err)
	rev, err := apps.CreateRevision(ctx, app.ID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: app.ID, Source: src,
	}, spec.OriginManual, owner.ID)
	require.NoError(t, err)
	deployments := state.NewDeployments(db)
	dep, err := deployments.Create(ctx, app.ID, rev.ID, "manual", owner.ID)
	require.NoError(t, err)

	runner, err := deployments.Runner(ctx, dep.ID)
	require.NoError(t, err)
	require.Equal(t, db.Replica(), runner)

	_, err = db.Exec(ctx, `UPDATE deployments SET replica_id = NULL WHERE id = $1`, dep.ID)
	require.NoError(t, err)
	runner, err = deployments.Runner(ctx, dep.ID)
	require.NoError(t, err)
	require.Empty(t, runner)
}
