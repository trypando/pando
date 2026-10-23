//go:build integration

package main

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
)

// TestR405_ResettingTwoFactorFromTheHost asserts what `pando admin
// reset-two-factor` promises an installation whose only administrator lost
// their phone and their recovery codes: the authenticator and codes are gone,
// they are asked to set it up again, every session ends, the wrong-code count
// is lifted, and it is audited (R-227).
func TestR405_ResettingTwoFactorFromTheHost(t *testing.T) {
	ctx := context.Background()
	db, users := withAdmin(t)

	admin, found, err := users.ByExternalID(ctx, state.LocalAdapterID, bootstrap.AdminUsername)
	require.NoError(t, err)
	require.True(t, found)
	_, err = db.Exec(ctx, `
		UPDATE users SET totp_enabled_at = now(), totp_last_step = 1, two_factor_offered_at = now() WHERE id = $1`, admin.ID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO user_recovery_codes (user_id, digest) VALUES ($1, 'd1')`, admin.ID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO sessions (id, secret_hash, user_id, adapter_id, expires_at)
		VALUES ('ses_live', 'hmac-sha256:' || repeat('0', 64), $1, $2, now() + interval '1 day')`, admin.ID, admin.AdapterID)
	require.NoError(t, err)
	require.NoError(t, state.NewFailedAttempts(db).Record(ctx, state.SignInCodeKey(admin.ID), time.Hour))

	out, err := runAdmin(t, "reset-two-factor")
	require.NoError(t, err)
	require.Contains(t, out, "Two-factor authentication removed for admin.")

	st, err := state.NewTwoFactor(db, nil, "").Status(ctx, admin.ID)
	require.NoError(t, err)
	require.Nil(t, st.EnabledAt)
	require.Nil(t, st.OfferedAt, "asked again")
	require.Zero(t, st.RecoveryCodesLeft)

	var live, failures, audited int
	require.NoError(t, db.QueryRow(ctx,
		`SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, admin.ID).Scan(&live))
	require.Zero(t, live)
	require.NoError(t, db.QueryRow(ctx,
		`SELECT count(*) FROM failed_attempts WHERE key = $1`, state.SignInCodeKey(admin.ID)).Scan(&failures))
	require.Zero(t, failures)
	require.NoError(t, db.QueryRow(ctx, `
		SELECT count(*) FROM audit_events
		WHERE action = 'user.two_factor.reset' AND target_id = $1 AND principal_kind = 'system'
		  AND detail->>'had_two_factor' = 'true'`, admin.ID).Scan(&audited))
	require.Equal(t, 1, audited)
}

func TestResettingTwoFactorFromTheHostNeedsALocalAccount(t *testing.T) {
	withAdmin(t)
	_, err := runAdmin(t, "reset-two-factor", "nobody")
	require.ErrorContains(t, err, "no local account called \"nobody\"")
}
