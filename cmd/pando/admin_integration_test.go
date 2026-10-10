//go:build integration

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// withAdmin is an install whose administrator was made from a password, and
// the environment `pando admin` reads its database from.
func withAdmin(t *testing.T) (*state.DB, *state.Users) {
	t.Helper()
	db, dsn := connectedURL(t)
	users := state.NewUsers(db)
	_, err := bootstrap.Run(context.Background(), users, state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("the-first-password-1234"))
	require.NoError(t, err)
	t.Setenv("PANDO_DATABASE_URL", dsn)
	return db, users
}

func runAdmin(t *testing.T, args ...string) (string, error) {
	t.Helper()
	configPath := ""
	cmd := adminCmd(&configPath)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(strings.NewReader("")) // not a terminal, so nothing prompts
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(context.Background())
	return out.String(), err
}

// TestR048_ResettingAPasswordFromTheHostEndsItsSessions asserts what
// `pando admin reset-password` promises: the new password works, the account
// must change it, every session it had is gone (R-048), and it is audited
// (R-227) — because it runs outside every check the API makes.
func TestR048_ResettingAPasswordFromTheHostEndsItsSessions(t *testing.T) {
	ctx := context.Background()
	db, users := withAdmin(t)

	var userID string
	require.NoError(t, db.QueryRow(ctx, `SELECT id FROM users WHERE external_id = $1`, bootstrap.AdminUsername).Scan(&userID))
	_, err := db.Exec(ctx, `INSERT INTO sessions (id, user_id, adapter_id, expires_at)
		SELECT 'ses_live', id, adapter_id, now() + interval '1 day' FROM users WHERE id = $1`, userID)
	require.NoError(t, err)

	out, err := runAdmin(t, "reset-password", "--password", "a-brand-new-password-9")
	require.NoError(t, err)
	require.Contains(t, out, "Password reset for admin.")
	require.NotContains(t, out, "New password:", "a supplied password is not echoed back")

	record, _, err := users.ByUsername(ctx, bootstrap.AdminUsername)
	require.NoError(t, err)
	ok, err := hash.Verify(secret.New("a-brand-new-password-9"), record.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)

	var live int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE user_id = $1 AND revoked_at IS NULL`, userID).Scan(&live))
	require.Zero(t, live, "a reset that left a session alive has not reset anything")

	var action string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT action FROM audit_events WHERE action = 'user.password.reset' AND target_id = $1`, userID).Scan(&action))
}

func TestWithoutAPasswordOneIsGeneratedAndShownOnce(t *testing.T) {
	withAdmin(t)
	out, err := runAdmin(t, "reset-password", "admin")
	require.NoError(t, err)
	require.Contains(t, out, "New password: ")
}

func TestAResetIsRefusedForAShortPasswordOrAnUnknownAccount(t *testing.T) {
	withAdmin(t)
	_, err := runAdmin(t, "reset-password", "--password", "short")
	require.ErrorContains(t, err, "at least")

	_, err = runAdmin(t, "reset-password", "nobody", "--password", "a-brand-new-password-9")
	require.Error(t, err)
}

// TestR043_PasswordSignInCanBeTurnedBackOnFromTheHost asserts the break-glass
// path for an install whose only way in is an identity provider that stopped
// working: `pando admin enable-password-sign-in` clears the policy, says so,
// and is audited — and does nothing, saying that too, when it is already on.
func TestR043_PasswordSignInCanBeTurnedBackOnFromTheHost(t *testing.T) {
	ctx := context.Background()
	db, _ := withAdmin(t)
	store := state.NewPolicy(db)

	out, err := runAdmin(t, "enable-password-sign-in")
	require.NoError(t, err)
	require.Contains(t, out, "already on")

	doc, err := store.Load(ctx)
	require.NoError(t, err)
	doc.DisablePasswordSignIn = true
	require.NoError(t, store.Save(ctx, doc, "test"))

	out, err = runAdmin(t, "enable-password-sign-in")
	require.NoError(t, err)
	require.Contains(t, out, "Password sign-in is on")
	doc, err = store.Load(ctx)
	require.NoError(t, err)
	require.False(t, doc.DisablePasswordSignIn)

	var via string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT detail->>'via' FROM audit_events WHERE action = 'policy.update' ORDER BY id DESC LIMIT 1`).Scan(&via))
	require.Equal(t, "pando admin enable-password-sign-in", via)

	// Fixed in the startup configuration, it is the configuration to change.
	t.Setenv("PANDO_POLICY_DISABLE_PASSWORD_SIGN_IN", "true")
	_, err = runAdmin(t, "enable-password-sign-in")
	require.ErrorContains(t, err, "startup configuration")
}

// TestR046_ASetupTokenFromTheHostClaimsAFreshInstall asserts `pando admin
// setup-token` (issue #130): the token alone on stdout, so a script can take
// it; the one printed at startup retired; audited; and refused once the
// installation is set up.
func TestR046_ASetupTokenFromTheHostClaimsAFreshInstall(t *testing.T) {
	ctx := context.Background()
	db, dsn := connectedURL(t)
	users := state.NewUsers(db)
	auditor := audit.New(db.Pool)
	first, err := bootstrap.Run(ctx, users, state.NewGrants(db), db, auditor, secret.Value{})
	require.NoError(t, err)
	t.Setenv("PANDO_DATABASE_URL", dsn)

	run := func() (string, string, error) {
		configPath := ""
		cmd := adminCmd(&configPath)
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"setup-token"})
		err := cmd.ExecuteContext(ctx)
		return stdout.String(), stderr.String(), err
	}

	stdout, stderr, err := run()
	require.NoError(t, err)
	token := strings.TrimSpace(stdout)
	require.NotEmpty(t, token)
	require.NotContains(t, token, "\n", "stdout is the token and nothing else")
	require.Contains(t, stderr, "set up the administrator")

	_, err = bootstrap.Claim(ctx, users, auditor, first.SetupToken, "mallory", "", secret.New("a-long-enough-password"))
	require.ErrorIs(t, err, state.ErrSetupTokenWrong, "the token printed at startup is retired")
	_, err = bootstrap.Claim(ctx, users, auditor, secret.New(token), "ada", "", secret.New("a-long-enough-password"))
	require.NoError(t, err)

	var n int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE action = 'setup.token.replace'`).Scan(&n))
	require.Equal(t, 1, n)

	_, _, err = run()
	require.ErrorIs(t, err, state.ErrAlreadySetUp)
}

// TestASetupTokenIsNotPrintedWhenTheDatabaseCannotBeReached asserts `pando
// admin setup-token` fails, and prints nothing a script could take for a
// token, when Postgres is not there.
func TestASetupTokenIsNotPrintedWhenTheDatabaseCannotBeReached(t *testing.T) {
	t.Setenv("PANDO_DATABASE_URL", "postgres://pando:x@127.0.0.1:1/pando?sslmode=disable")
	t.Setenv("PANDO_DATABASE_CONNECT_TIMEOUT", "1s")
	configPath := ""
	cmd := adminCmd(&configPath)
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"setup-token"})
	require.Error(t, cmd.ExecuteContext(context.Background()))
	require.Empty(t, stdout.String())
}
