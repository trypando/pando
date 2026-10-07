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
// password supplied, first run creates nothing and prints nothing, and the
// installation waits for its first administrator.
func TestR046_AFreshInstallWaitsToBeSetUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, users, grants, auditor := newInstall(t)

	first, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.False(t, first.Created)
	require.True(t, first.Unclaimed)

	needed, err := users.NeedsSetup(ctx)
	require.NoError(t, err)
	require.True(t, needed)

	// Claimed with the person's own choice, which need not be changed, and made
	// an administrator.
	chosen := secret.New("a-password-i-chose")
	user, err := bootstrap.Claim(ctx, users, auditor, "ada", "Ada", chosen)
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
	_, err = bootstrap.Claim(ctx, users, auditor, "mallory", "", secret.New("another-long-password"))
	require.ErrorIs(t, err, state.ErrAlreadySetUp)
	again, err := bootstrap.Run(ctx, users, grants, db, auditor, secret.Value{})
	require.NoError(t, err)
	require.False(t, again.Unclaimed)
}

// Two people submitting the setup form at once: exactly one becomes the
// administrator.
func TestR046_OnlyOneClaimWins(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	_, users, _, auditor := newInstall(t)

	const n = 8
	errsCh := make(chan error, n)
	for i := range n {
		go func() {
			_, err := bootstrap.Claim(ctx, users, auditor, fmt.Sprintf("claimant%d", i), "", secret.New("a-long-enough-password"))
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
