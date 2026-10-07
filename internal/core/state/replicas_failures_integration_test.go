//go:build integration

package state_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// unreachable is a replica's connection to a database that has gone away.
func unreachable(t *testing.T) *state.DB {
	t.Helper()
	ownerURL, appPassword := statetest.Database(t)
	db, err := state.ConnectCopy(context.Background(), ownerURL, appPassword)
	require.NoError(t, err)
	db.Close()
	return db
}

// requireInternal asserts err is the envelope's internal error, with a
// message a person can act on rather than the driver's.
func requireInternal(t *testing.T, err error, msg string) {
	t.Helper()
	require.Error(t, err, msg)
	e := errs.As(err)
	require.NotNil(t, e, "%s: %v is not an envelope error", msg, err)
	require.Equal(t, errs.Internal, e.Code, msg)
	require.NotEmpty(t, e.Message, msg)
}

// TestR256_AReplicaThatCannotReachTheDatabaseIsToldSo asserts that every
// question a replica asks of the replicas table fails loudly when the database
// is away, rather than answering as if the table were empty: a heartbeat that
// silently said "gone" would restart a healthy replica, and an empty key list
// would refuse every assertion (R-256, R-051).
func TestR256_AReplicaThatCannotReachTheDatabaseIsToldSo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := unreachable(t)
	replicas := state.NewReplicas(db)
	repID := id.New(id.Replica)

	requireInternal(t, replicas.Register(ctx, state.Replica{ID: repID}), "register")
	alive, err := replicas.Heartbeat(ctx, repID)
	requireInternal(t, err, "heartbeat")
	require.False(t, alive)
	requireInternal(t, replicas.Stop(ctx, repID), "stop")
	_, err = replicas.Live(ctx)
	requireInternal(t, err, "live")
	_, _, err = replicas.ByID(ctx, repID)
	requireInternal(t, err, "by id")
	_, err = replicas.VerificationKeys(ctx, time.Minute)
	requireInternal(t, err, "verification keys")
	_, err = replicas.Prune(ctx, time.Hour)
	requireInternal(t, err, "prune")
	requireInternal(t, replicas.RequestRestart(ctx), "request restart")
	_, err = replicas.RestartPending(ctx, repID)
	requireInternal(t, err, "restart pending")
	_, ok, err := replicas.TryLead(ctx, time.Second)
	requireInternal(t, err, "try lead")
	require.False(t, ok, "nobody leads on a database it cannot reach")
	requireInternal(t, db.Exclusive(ctx, 1, func() error { return nil }), "exclusive")

	_, err = state.NewDeployments(db).Runner(ctx, id.New(id.Deployment))
	requireInternal(t, err, "an unreadable runner is an error, not 'nobody runs it'")

	failures := state.NewPasscodeFailures(db)
	_, err = failures.Recent(ctx, "k", time.Hour)
	requireInternal(t, err, "recent passcode failures")
	requireInternal(t, failures.Record(ctx, "k", time.Hour), "record a passcode failure")
	requireInternal(t, failures.Clear(ctx, "k"), "clear passcode failures")
	requireInternal(t, failures.Prune(ctx, time.Hour), "prune passcode failures")

	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(ctx, cfg))
	requireInternal(t, state.NewSecrets(db, sa, "sek_local").VerifyKey(ctx), "verify the secrets key")
}

// TestR256_ADeployNobodyClaimedHasNoRunner asserts that a deploy still in
// the queue, and one that does not exist, name no replica.
func TestR256_ADeployNobodyClaimedHasNoRunner(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	runner, err := state.NewDeployments(db).Runner(ctx, id.New(id.Deployment))
	require.NoError(t, err)
	require.Empty(t, runner)
}

// externalSecrets is a secrets adapter that keeps values in a store of its
// own and hands back only a handle, as Vault or a cloud secret manager would.
type externalSecrets struct {
	*secretslocal.Adapter
	putErr  error
	mu      sync.Mutex
	deleted []adapterapi.StoredRef
}

func (e *externalSecrets) Put(_ context.Context, ref adapterapi.SecretRef, _ secret.Value) (adapterapi.StoredRef, error) {
	if e.putErr != nil {
		return adapterapi.StoredRef{}, e.putErr
	}
	return adapterapi.StoredRef{AppID: ref.AppID, Key: ref.Key, Handle: "external:" + ref.Key}, nil
}

func (e *externalSecrets) Delete(_ context.Context, ref adapterapi.StoredRef) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleted = append(e.deleted, ref)
	return nil
}

func localSecrets(t *testing.T, path string) *secretslocal.Adapter {
	t.Helper()
	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": path})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(context.Background(), cfg))
	return sa
}

func canaryRows(t *testing.T, db *state.DB) (int, string) {
	t.Helper()
	var n int
	var ref string
	require.NoError(t, db.QueryRow(context.Background(),
		`SELECT count(*), coalesce(max(adapter_ref), '') FROM secrets_canary`).Scan(&n, &ref))
	return n, ref
}

// TestR190_OnlyASecretsAdapterWithALocalKeyIsChecked asserts that the key
// check applies where there is a local key to differ, and nowhere else: no
// secrets adapter at all, or one that keeps values in an external store, has
// nothing to compare across replicas, and its probe value is not left behind.
func TestR190_OnlyASecretsAdapterWithALocalKeyIsChecked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	require.NoError(t, state.NewSecrets(db, nil, "").VerifyKey(ctx), "no secrets adapter, nothing to check")

	ext := &externalSecrets{Adapter: localSecrets(t, filepath.Join(t.TempDir(), "k"))}
	require.NoError(t, state.NewSecrets(db, ext, "sek_vault").VerifyKey(ctx))
	n, _ := canaryRows(t, db)
	require.Zero(t, n, "nothing is recorded for an adapter with no local key")
	require.Len(t, ext.deleted, 1, "the probe value is removed from the external store")
	require.Equal(t, "external:pando:secrets-key-canary", ext.deleted[0].Handle)

	failing := &externalSecrets{Adapter: ext.Adapter, putErr: errors.New("vault sealed")}
	require.ErrorContains(t, state.NewSecrets(db, failing, "sek_vault").VerifyKey(ctx), "vault sealed",
		"a secrets store that cannot be written to stops the start")
}

// TestR190_SwitchingSecretsAdaptersReplacesTheKeyCheck asserts that the check
// follows the secrets adapter the install uses: switching to another replaces
// it, and from then on replicas are compared against the new adapter's key.
func TestR190_SwitchingSecretsAdaptersReplacesTheKeyCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	oldKey := filepath.Join(t.TempDir(), "old.key")
	newKey := filepath.Join(t.TempDir(), "new.key")

	require.NoError(t, state.NewSecrets(db, localSecrets(t, oldKey), "sek_old").VerifyKey(ctx))
	_, ref := canaryRows(t, db)
	require.Equal(t, "sek_old", ref)

	require.NoError(t, state.NewSecrets(db, localSecrets(t, newKey), "sek_new").VerifyKey(ctx),
		"a check from an adapter no longer in use is replaced, not failed")
	n, ref := canaryRows(t, db)
	require.Equal(t, 1, n)
	require.Equal(t, "sek_new", ref)

	require.NoError(t, state.NewSecrets(db, localSecrets(t, newKey), "sek_new").VerifyKey(ctx))
	require.Error(t, state.NewSecrets(db, localSecrets(t, oldKey), "sek_new").VerifyKey(ctx),
		"a replica on the new adapter with the old key is refused")
}

// TestR190_ReplicasStartingAtOnceAgreeOnOneKeyCheck asserts that replicas
// seeding the check at the same moment end with one, and every one that holds
// the shared key passes it.
func TestR190_ReplicasStartingAtOnceAgreeOnOneKeyCheck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	shared := filepath.Join(t.TempDir(), "secrets.key")
	_ = localSecrets(t, shared) // the key exists before any replica starts

	replicas := make([]*state.Secrets, 8)
	for i := range replicas {
		replicas[i] = state.NewSecrets(db, localSecrets(t, shared), "sek_local")
	}
	var wg sync.WaitGroup
	results := make([]error, len(replicas))
	for i, s := range replicas {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = s.VerifyKey(ctx)
		}()
	}
	wg.Wait()
	for _, err := range results {
		require.NoError(t, err)
	}
	n, _ := canaryRows(t, db)
	require.Equal(t, 1, n)
}
