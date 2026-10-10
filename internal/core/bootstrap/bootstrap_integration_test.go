//go:build integration

package bootstrap_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

func newInstall(t *testing.T) (*state.DB, *state.Users, *state.Grants, *audit.Writer) {
	t.Helper()
	db, _ := statetest.Connect(t)
	return db, state.NewUsers(db), state.NewGrants(db), audit.New(db.Pool)
}

// TestR046_AFreshInstallWaitsToBeSetUp asserts R-046's default: with no
// password supplied, first run makes no account, only a setup token to print,
// and the installation waits for its first administrator.
func TestR046_AFreshInstallWaitsToBeSetUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.False(t, first.Created)
	require.True(t, first.Unclaimed)
	require.False(t, first.SetupToken.IsZero(), "an unclaimed installation has a setup token to print")

	needed, err := users.NeedsSetup(ctx)
	require.NoError(t, err)
	require.True(t, needed)

	// Claimed with the person's own choice, which need not be changed, and made
	// an administrator.
	chosen := secret.New("a-password-i-chose")
	user, err := bootstrap.Claim(ctx, users, auditor, first.SetupToken, "ada", "Ada", chosen)
	require.NoError(t, err)
	rec, found, err := users.ByUsername(ctx, "ada")
	require.NoError(t, err)
	require.True(t, found)
	ok, err := hash.Verify(chosen, rec.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok)
	var mustChange bool
	require.NoError(t, db.QueryRow(ctx,
		`SELECT must_change_password FROM users WHERE id = $1`, user.ID).Scan(&mustChange))
	require.False(t, mustChange, "a password its holder chose need not be changed")

	var role string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT role_id FROM grants WHERE app_id IS NULL AND principal_id = $1`, user.ID).Scan(&role))
	require.Equal(t, "role_administrator", role)

	// Once. The door exists only until it is used.
	_, err = bootstrap.Claim(ctx, users, auditor, first.SetupToken, "mallory", "", secret.New("another-long-password"))
	require.ErrorIs(t, err, state.ErrAlreadySetUp)
	again, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.False(t, again.Unclaimed)
	require.True(t, again.SetupToken.IsZero())
	require.Zero(t, setupTokens(t, db), "the claim used the token up")
}

func setupTokens(t *testing.T, db *state.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(context.Background(), `SELECT count(*) FROM setup_token`).Scan(&n))
	return n
}

// TestR046_TheSetupFormNeedsTheSetupToken asserts the setup form is not a door
// for whoever is first to the URL (issue #130): without the token Pando
// printed, or with a wrong one, the claim is refused and the installation
// still waits.
func TestR046_TheSetupFormNeedsTheSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)

	for _, token := range []secret.Value{{}, secret.New("not-the-token"), secret.New(first.SetupToken.Reveal() + "x")} {
		_, err := bootstrap.Claim(ctx, users, auditor, token, "mallory", "", secret.New("a-long-enough-password"))
		require.ErrorIs(t, err, state.ErrSetupTokenWrong)
	}
	needed, err := users.NeedsSetup(ctx)
	require.NoError(t, err)
	require.True(t, needed, "a refused claim leaves the installation waiting")
	require.Equal(t, 1, setupTokens(t, db), "a refused claim does not use the token up")

	_, err = bootstrap.Claim(ctx, users, auditor, first.SetupToken, "ada", "", secret.New("a-long-enough-password"))
	require.NoError(t, err)
}

// TestR046_TheSetupTokenIsMadeOnceAndKeptOnlyAsADigest asserts a restart does
// not make a second token, and that the database never holds the token itself.
func TestR046_TheSetupTokenIsMadeOnceAndKeptOnlyAsADigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	restart, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.True(t, restart.Unclaimed)
	require.True(t, restart.SetupToken.IsZero(), "the token is printed once, when it is made")
	require.True(t, first.SetupTokenMade.Equal(restart.SetupTokenMade), "a restart names the token already made")

	var stored string
	require.NoError(t, db.QueryRow(ctx, `SELECT token_hash FROM setup_token`).Scan(&stored))
	require.NotContains(t, stored, first.SetupToken.Reveal())

	_, err = bootstrap.Claim(ctx, users, auditor, first.SetupToken, "ada", "", secret.New("a-long-enough-password"))
	require.NoError(t, err, "the first token still works after a restart")
}

// TestR046_ANewSetupTokenRetiresTheOldOne asserts `pando admin setup-token`:
// for an operator who no longer has the log line, a new token replaces the
// old, and none can be made once the installation is set up.
func TestR046_ANewSetupTokenRetiresTheOldOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	replacement, err := users.ReplaceSetupToken(ctx)
	require.NoError(t, err)

	_, err = bootstrap.Claim(ctx, users, auditor, first.SetupToken, "mallory", "", secret.New("a-long-enough-password"))
	require.ErrorIs(t, err, state.ErrSetupTokenWrong, "the replaced token no longer works")
	_, err = bootstrap.Claim(ctx, users, auditor, secret.New(replacement), "ada", "", secret.New("a-long-enough-password"))
	require.NoError(t, err)

	_, err = users.ReplaceSetupToken(ctx)
	require.ErrorIs(t, err, state.ErrAlreadySetUp)
	require.Zero(t, setupTokens(t, db))
}

// TestR046_ASuppliedPasswordLeavesNoSetupToken asserts a token made on an
// earlier start does not outlive the account PANDO_ADMIN_PASSWORD makes.
func TestR046_ASuppliedPasswordLeavesNoSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	_, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.Equal(t, 1, setupTokens(t, db))

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.New("correct-horse-battery-staple"))
	require.NoError(t, err)
	require.True(t, first.Created)
	require.True(t, first.SetupToken.IsZero())
	require.Zero(t, setupTokens(t, db))
}

// Two people submitting the setup form at once: exactly one becomes the
// administrator.
func TestR046_OnlyOneClaimWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)
	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)

	const n = 8
	errsCh := make(chan error, n)
	for i := range n {
		go func() {
			_, err := bootstrap.Claim(ctx, users, auditor, first.SetupToken, fmt.Sprintf("claimant%d", i), "", secret.New("a-long-enough-password"))
			errsCh <- err
		}()
	}
	won := 0
	for range n {
		if err := <-errsCh; err == nil {
			won++
		} else {
			require.ErrorIs(t, err, state.ErrAlreadySetUp)
		}
	}
	require.Equal(t, 1, won)
}

// TestR046_AnOperatorCanSupplyTheFirstPassword asserts the [P] override: for
// an unattended install, PANDO_ADMIN_PASSWORD makes the account at startup.
func TestR046_AnOperatorCanSupplyTheFirstPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	chosen := secret.New("correct-horse-battery-staple")
	first, err := bootstrap.Run(ctx, users, grants, db, auditor, chosen)
	require.NoError(t, err)
	require.True(t, first.Created)
	require.False(t, first.Unclaimed)

	rec, found, err := users.ByUsername(ctx, bootstrap.AdminUsername)
	require.NoError(t, err)
	require.True(t, found)

	ok, err := hash.Verify(chosen, rec.PasswordHash)
	require.NoError(t, err)
	require.True(t, ok, "the supplied password is the one that signs in")
}

// R-046's forced change survives the override.
//
// An environment variable is not a safer place than a log line: it is in the
// Compose file, in `docker inspect`, and inherited by every child process.
// Supplying one buys a way in, not a credential.
func TestR046_ASuppliedPasswordStillMustBeChanged(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	_, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.New("correct-horse-battery-staple"))
	require.NoError(t, err)

	var mustChange bool
	require.NoError(t, db.QueryRow(ctx,
		`SELECT must_change_password FROM users WHERE external_id = $1`,
		bootstrap.AdminUsername).Scan(&mustChange))
	require.True(t, mustChange)
}

// A supplied password held to the same minimum as every other one.
//
// Refused at startup rather than accepted, because the alternative is an
// install whose only administrator has a four-character password and an
// operator who was never told.
func TestASuppliedPasswordTooShortIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	_, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.New("short"))
	require.Error(t, err)

	// And it created nothing on the way out.
	_, found, err := users.ByUsername(ctx, bootstrap.AdminUsername)
	require.NoError(t, err)
	require.False(t, found, "a refused bootstrap leaves no half-made administrator")
}

// TestR046_ReplicasStartingTogetherMakeOneAdministrator asserts R-046 holds
// when several Pando replicas start at once with PANDO_ADMIN_PASSWORD set
// (issue #72): one makes the account, and every other starts normally and
// finds it, rather than failing on the second copy of the username.
func TestR046_ReplicasStartingTogetherMakeOneAdministrator(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t)

	const replicas = 4
	results := make([]bootstrap.Result, replicas)
	errs := make([]error, replicas)
	done := make(chan int)
	for i := range replicas {
		go func(i int) {
			defer func() { done <- i }()
			conn, err := state.ConnectCopy(ctx, ownerURL, appPassword)
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			results[i], errs[i] = bootstrap.Run(ctx, state.NewUsers(conn), state.NewGrants(conn), conn,
				audit.New(conn.Pool), secret.New("a-long-enough-password"))
		}(i)
	}
	for range replicas {
		<-done
	}

	created := 0
	for i := range replicas {
		require.NoError(t, errs[i])
		if results[i].Created {
			created++
		}
	}
	require.Equal(t, 1, created, "exactly one replica makes the administrator")

	var n int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&n))
	require.Equal(t, 1, n)
}

// TestR046_ReplicasStartingTogetherMakeOneSetupToken asserts replicas starting
// at once on an unclaimed installation agree on one setup token: exactly one
// makes and prints it, and the rest find it.
func TestR046_ReplicasStartingTogetherMakeOneSetupToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	_, appPassword := statetest.Database(t)

	const replicas = 4
	results := make([]bootstrap.Result, replicas)
	errs := make([]error, replicas)
	done := make(chan int)
	for i := range replicas {
		go func(i int) {
			defer func() { done <- i }()
			conn, err := state.ConnectCopy(ctx, ownerURL, appPassword)
			if err != nil {
				errs[i] = err
				return
			}
			defer conn.Close()
			results[i], errs[i] = bootstrap.Run(ctx, state.NewUsers(conn), state.NewGrants(conn), conn,
				audit.New(conn.Pool), secret.Value{})
		}(i)
	}
	for range replicas {
		<-done
	}

	made := 0
	for i := range replicas {
		require.NoError(t, errs[i])
		require.True(t, results[i].Unclaimed)
		if !results[i].SetupToken.IsZero() {
			made++
		}
	}
	require.Equal(t, 1, made, "exactly one replica makes and prints the setup token")
	require.Equal(t, 1, setupTokens(t, db))
}
