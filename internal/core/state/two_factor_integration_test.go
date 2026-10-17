//go:build integration

package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

func twoFactorStore(t *testing.T, db *state.DB) *state.TwoFactor {
	t.Helper()
	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(context.Background(), cfg))
	return state.NewTwoFactor(db, sa, "sek_local")
}

// TestR405_TheAuthenticatorKeyIsSealedAndRecoveryCodesAreDigests asserts
// R-405's storage: the key exists only as ciphertext (R-190), a pending key
// is not the one in use until enabled, and recovery codes are spent once.
func TestR405_TheAuthenticatorKeyIsSealedAndRecoveryCodesAreDigests(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	tf := twoFactorStore(t, db)
	dana := seedUser(t, db, "dana")

	st, err := tf.Status(ctx, dana.ID)
	require.NoError(t, err)
	require.Nil(t, st.EnabledAt)
	require.Nil(t, st.OfferedAt)

	const key = "JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP"
	require.NoError(t, tf.PutPending(ctx, dana.ID, secret.New(key)))
	current, pending, err := tf.Secrets(ctx, dana.ID)
	require.NoError(t, err)
	require.True(t, current.IsZero(), "nothing in use yet")
	require.Equal(t, key, pending.Reveal())

	var ciphertext []byte
	require.NoError(t, db.QueryRow(ctx,
		`SELECT ciphertext FROM user_two_factor_credentials WHERE user_id = $1`, dana.ID).Scan(&ciphertext))
	require.False(t, bytes.Contains(ciphertext, []byte(key)), "only ciphertext is kept")

	ok, err := tf.AcceptStep(ctx, dana.ID, 100)
	require.NoError(t, err)
	require.False(t, ok, "no step is accepted before it is on")

	require.NoError(t, tf.Enable(ctx, dana.ID, pending, 100, []string{"d1", "d2"}))
	current, pending, err = tf.Secrets(ctx, dana.ID)
	require.NoError(t, err)
	require.Equal(t, key, current.Reveal())
	require.True(t, pending.IsZero(), "the pending key is gone")
	st, err = tf.Status(ctx, dana.ID)
	require.NoError(t, err)
	require.NotNil(t, st.EnabledAt)
	require.NotNil(t, st.OfferedAt, "setting it up counts as having been asked")
	require.EqualValues(t, 100, st.LastStep)
	require.Equal(t, 2, st.RecoveryCodesLeft)

	users := state.NewUsers(db)
	u, _, err := users.ByID(ctx, dana.ID)
	require.NoError(t, err)
	require.True(t, u.TwoFactorEnabled)

	for step, want := range map[int64]bool{100: false, 99: false} {
		ok, err := tf.AcceptStep(ctx, dana.ID, step)
		require.NoError(t, err)
		require.Equal(t, want, ok, "step %d", step)
	}
	ok, err = tf.AcceptStep(ctx, dana.ID, 101)
	require.NoError(t, err)
	require.True(t, ok)

	ok, err = tf.UseRecoveryCode(ctx, dana.ID, "d1")
	require.NoError(t, err)
	require.True(t, ok)
	ok, err = tf.UseRecoveryCode(ctx, dana.ID, "d1")
	require.NoError(t, err)
	require.False(t, ok, "a recovery code works once")

	require.NoError(t, tf.ReplaceRecoveryCodes(ctx, dana.ID, []string{"n1", "n2", "n3"}))
	ok, err = tf.UseRecoveryCode(ctx, dana.ID, "d2")
	require.NoError(t, err)
	require.False(t, ok, "the old set is gone")
	st, _ = tf.Status(ctx, dana.ID)
	require.Equal(t, 3, st.RecoveryCodesLeft)

	had, err := tf.Disable(ctx, dana.ID, false)
	require.NoError(t, err)
	require.True(t, had)
	st, _ = tf.Status(ctx, dana.ID)
	require.Nil(t, st.EnabledAt)
	require.NotNil(t, st.OfferedAt, "turning it off yourself is not being asked again")
	require.Zero(t, st.RecoveryCodesLeft)
	current, _, err = tf.Secrets(ctx, dana.ID)
	require.NoError(t, err)
	require.True(t, current.IsZero())

	had, err = tf.Disable(ctx, dana.ID, true)
	require.NoError(t, err)
	require.False(t, had, "nothing left to remove")
	st, _ = tf.Status(ctx, dana.ID)
	require.Nil(t, st.OfferedAt, "an administrator's reset asks again")

	require.NoError(t, tf.MarkOffered(ctx, dana.ID))
	st, _ = tf.Status(ctx, dana.ID)
	require.NotNil(t, st.OfferedAt)
}

func TestTwoFactorOnAnAccountThatIsNotThere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	tf := twoFactorStore(t, db)

	st, err := tf.Status(ctx, "usr_nobody")
	require.NoError(t, err)
	require.Nil(t, st.EnabledAt)

	_, err = tf.Disable(ctx, "usr_nobody", true)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// A provider's account gets its second factor from the provider (R-044):
// enabling one here is refused.
func TestR405_AProviderAccountCannotTurnOnTwoFactorHere(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	tf := twoFactorStore(t, db)
	_, err := db.Exec(ctx, `INSERT INTO identity_adapters (id, kind, name, config) VALUES ('idp_okta', 'oidc', 'Okta', '{}')`)
	require.NoError(t, err)
	u, err := state.NewUsers(db).Create(ctx, "idp_okta", "okta-sub", "", "Okta Person", "", false)
	require.NoError(t, err)

	err = tf.Enable(ctx, u.ID, secret.New("JBSWY3DPEHPK3PXP"), 1, []string{"d"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "identity provider")
}
