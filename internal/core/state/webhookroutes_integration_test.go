//go:build integration

package state_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

// TestR433_ASignatureIsClaimedOnceAcrossReplicas asserts the record the replay
// check rests on: a live signature cannot be claimed twice, a released one can,
// an accepted one reads as accepted until it expires, and an expired claim counts
// as absent.
func TestR433_ASignatureIsClaimedOnceAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := webhookApp(ctx, t, db)
	d := state.NewWebhookDeliveries(db)
	digest := []byte("digest")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	soon := now.Add(5 * time.Minute)

	claim, first, err := d.Claim(ctx, appID, "noodle", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookClaimed, claim)
	require.NotEmpty(t, first)

	claim, _, err = d.Claim(ctx, appID, "noodle", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookInFlight, claim)

	// Another route's identical signature is another delivery.
	claim, _, err = d.Claim(ctx, appID, "other", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookClaimed, claim)

	require.NoError(t, d.Release(ctx, appID, "noodle", digest, first))
	claim, second, err := d.Claim(ctx, appID, "noodle", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookClaimed, claim, "the retry after a failure")

	week := now.Add(7 * 24 * time.Hour)
	require.NoError(t, d.Accept(ctx, appID, "noodle", digest, week))
	claim, _, err = d.Claim(ctx, appID, "noodle", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookAccepted, claim)

	require.NoError(t, d.Release(ctx, appID, "noodle", digest, second))
	claim, _, err = d.Claim(ctx, appID, "noodle", digest, now, soon)
	require.NoError(t, err)
	require.Equal(t, state.WebhookAccepted, claim, "an accepted signature is not released")

	after := week.Add(time.Minute)
	claim, _, err = d.Claim(ctx, appID, "noodle", digest, after, after.Add(5*time.Minute))
	require.NoError(t, err)
	require.Equal(t, state.WebhookClaimed, claim, "past the window a replay passes")

	removed, err := state.NewRetention(db).WebhookDeliveries(ctx, after.Add(time.Hour), 100)
	require.NoError(t, err)
	require.EqualValues(t, 2, removed)
}

// TestR433_AnExpiredClaimCannotUndoANewerOne asserts the claim token: a slow
// delivery whose claim expired cannot delete the copy that took the signature
// over, and if the app accepted it, that stands.
func TestR433_AnExpiredClaimCannotUndoANewerOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := webhookApp(ctx, t, db)
	d := state.NewWebhookDeliveries(db)
	digest := []byte("digest")
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)

	_, slow, err := d.Claim(ctx, appID, "noodle", digest, now, now.Add(time.Minute))
	require.NoError(t, err)
	later := now.Add(2 * time.Minute)
	claim, newer, err := d.Claim(ctx, appID, "noodle", digest, later, later.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, state.WebhookClaimed, claim)
	require.NotEqual(t, slow, newer)

	require.NoError(t, d.Release(ctx, appID, "noodle", digest, slow))
	claim, _, err = d.Claim(ctx, appID, "noodle", digest, later, later.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, state.WebhookInFlight, claim, "the newer claim stands")

	require.NoError(t, d.Accept(ctx, appID, "noodle", digest, later.Add(time.Hour)))
	require.NoError(t, d.Release(ctx, appID, "noodle", digest, newer))
	claim, _, err = d.Claim(ctx, appID, "noodle", digest, later, later.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, state.WebhookAccepted, claim, "accepted by the slow one, not undone by the newer")

	// And with nothing left to update, an accepted delivery is still recorded.
	require.NoError(t, d.Accept(ctx, appID, "noodle", []byte("gone"), later.Add(time.Hour)))
	claim, _, err = d.Claim(ctx, appID, "noodle", []byte("gone"), later, later.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, state.WebhookAccepted, claim)
}

// TestR427_ANewRouteDoesNotInheritAStaleSecret asserts a name declared again by
// a spec save starts with no secret, while a route still in the previous
// revision or still running keeps its own.
func TestR427_ANewRouteDoesNotInheritAStaleSecret(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := webhookApp(ctx, t, db)
	apps := state.NewApps(db)
	k := state.NewWebhookRouteSecrets(db, sealing{}, "sec_local")
	route := spec.WebhookRoute{Name: "noodle", Method: "POST", Path: "/hook",
		Verification: spec.WebhookVerification{Kind: spec.WebhookHeader, Header: "X-Token"}}
	save := func(routes ...spec.WebhookRoute) {
		t.Helper()
		s := minimalSpec()
		s.WebhookRoutes = routes
		_, err := apps.CreateRevision(ctx, appID, s, spec.OriginEdited, "usr_test")
		require.NoError(t, err)
	}
	has := func() bool {
		t.Helper()
		names, err := k.Names(ctx, appID)
		require.NoError(t, err)
		return names["noodle"]
	}

	save(route)
	require.NoError(t, k.Put(ctx, appID, "noodle", secret.New("old")))
	save(route)
	require.True(t, has(), "an unchanged route keeps it")
	save()
	require.True(t, has(), "the previous revision still has the route")
	save(route)
	require.False(t, has(), "declared again, it starts with none")
}

// webhookApp is an app for the rows' foreign keys.
func webhookApp(ctx context.Context, t *testing.T, db *state.DB) string {
	t.Helper()
	owner := seedUser(t, db, "hook-owner")
	app, err := state.NewApps(db).Create(ctx, "hooks", "hooks", owner.ID, owner.ID,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)
	return app.ID
}

// sealing stands in for a secrets adapter: what it stores is not the value.
type sealing struct{ api.SecretsAdapter }

func (sealing) Put(_ context.Context, _ api.SecretRef, v secret.Value) (api.StoredRef, error) {
	return api.StoredRef{Ciphertext: []byte("sealed:" + v.Reveal())}, nil
}

func (sealing) Get(_ context.Context, ref api.StoredRef) (secret.Value, error) {
	return secret.New(strings.TrimPrefix(string(ref.Ciphertext), "sealed:")), nil
}

// TestR427_ARouteSecretIsKeptSealedPerRoute asserts a secret is what the adapter
// sealed, one per route, replaced in place, and gone with its route.
func TestR427_ARouteSecretIsKeptSealedPerRoute(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := webhookApp(ctx, t, db)
	k := state.NewWebhookRouteSecrets(db, sealing{}, "sec_local")

	_, found, err := k.Get(ctx, appID, "noodle")
	require.NoError(t, err)
	require.False(t, found)

	require.NoError(t, k.Put(ctx, appID, "noodle", secret.New("first")))
	require.NoError(t, k.Put(ctx, appID, "noodle", secret.New("second")))
	require.NoError(t, k.Put(ctx, appID, "other", secret.New("third")))

	v, found, err := k.Get(ctx, appID, "noodle")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "second", v.Reveal())

	names, err := k.Names(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, map[string]bool{"noodle": true, "other": true}, names)

	require.NoError(t, k.Delete(ctx, appID, "noodle"))
	_, found, err = k.Get(ctx, appID, "noodle")
	require.NoError(t, err)
	require.False(t, found)
}

type brokenSecrets struct{ api.SecretsAdapter }

func (brokenSecrets) Put(context.Context, api.SecretRef, secret.Value) (api.StoredRef, error) {
	return api.StoredRef{}, errors.New("vault gone")
}

func (brokenSecrets) Get(context.Context, api.StoredRef) (secret.Value, error) {
	return secret.Value{}, errors.New("vault gone")
}

// TestR427_AFailedStoreIsReported asserts every read and write says it failed,
// so the proxy refuses a delivery rather than treating an error as no record.
func TestR427_AFailedStoreIsReported(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	appID := webhookApp(ctx, t, db)

	var none *state.WebhookRouteSecrets
	require.Error(t, none.Put(ctx, appID, "noodle", secret.New("x")), "no adapter to seal it with")

	broken := state.NewWebhookRouteSecrets(db, brokenSecrets{}, "sec_local")
	require.Error(t, broken.Put(ctx, appID, "noodle", secret.New("x")))
	require.NoError(t, state.NewWebhookRouteSecrets(db, sealing{}, "sec_local").Put(ctx, appID, "noodle", secret.New("x")))
	_, _, err := broken.Get(ctx, appID, "noodle")
	require.Error(t, err)
	_, _, err = state.NewWebhookRouteSecrets(db, nil, "").Get(ctx, appID, "noodle")
	require.Error(t, err, "a stored secret and nothing to open it")

	gone, cancel := context.WithCancel(ctx)
	cancel()
	k := state.NewWebhookRouteSecrets(db, sealing{}, "sec_local")
	require.Error(t, k.Put(gone, appID, "noodle", secret.New("x")))
	_, _, err = k.Get(gone, appID, "noodle")
	require.Error(t, err)
	_, err = k.Names(gone, appID)
	require.Error(t, err)
	require.Error(t, k.Delete(gone, appID, "noodle"))

	d := state.NewWebhookDeliveries(db)
	now := time.Now()
	_, _, err = d.Claim(gone, appID, "noodle", []byte("d"), now, now)
	require.Error(t, err)
	require.Error(t, d.Accept(gone, appID, "noodle", []byte("d"), now))
	require.Error(t, d.Release(gone, appID, "noodle", []byte("d"), "t"))
}
