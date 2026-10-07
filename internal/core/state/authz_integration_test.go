//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// connected is a fresh, migrated database of the test's own, copied from one
// prepared for the whole package (issue #31). A test about the cluster itself
// — roles, ownership, a second Connect — uses startPostgres instead.
func connected(t *testing.T) *state.DB {
	t.Helper()
	db, _ := statetest.Connect(t)
	return db
}

func seedUser(t *testing.T, db *state.DB, username string) state.User {
	t.Helper()
	ctx := context.Background()
	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))

	digest, err := hash.New(secret.New("correct-password"))
	require.NoError(t, err)

	u, err := users.Create(ctx, state.LocalAdapterID, username, username+"@corp.com", username, digest, false)
	require.NoError(t, err)
	return u
}

func seedApp(t *testing.T, db *state.DB, ownerID string) string {
	t.Helper()
	appID := id.New(id.App)
	_, err := db.Exec(context.Background(),
		`INSERT INTO apps (id, name, slug, owner_user_id, state) VALUES ($1, $2, $3, $4, 'draft')`,
		appID, "notes", appID, ownerID)
	require.NoError(t, err)
	return appID
}

// TestR081_BuiltInRolesAreImmutable asserts R-081 at the database, which is
// where the requirement says the enforcement belongs.
//
// Built-in role contents change only by migration. The trigger refuses every
// runtime path, so a bug in an API handler cannot widen a role.
func TestR081_BuiltInRolesAreImmutable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	for _, roleID := range []string{authz.RoleViewer, authz.RoleOperator, authz.RoleOwner} {
		_, err := db.Exec(ctx,
			`UPDATE roles SET verbs = ARRAY['app.delete'] WHERE id = $1`, roleID)
		require.Error(t, err, "UPDATE on built-in role %s must be refused", roleID)

		_, err = db.Exec(ctx, `DELETE FROM roles WHERE id = $1`, roleID)
		require.Error(t, err, "DELETE of built-in role %s must be refused", roleID)
	}

	// Custom roles remain editable — the trigger guards built-ins only (R-082).
	_, err := db.Exec(ctx,
		`INSERT INTO roles (id, name, builtin, verbs) VALUES ('role_custom', 'deployer', false, ARRAY['app.deploy'])`)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `UPDATE roles SET verbs = ARRAY['app.deploy','app.view'] WHERE id = 'role_custom'`)
	require.NoError(t, err, "custom roles must stay editable")

	_, err = db.Exec(ctx, `DELETE FROM roles WHERE id = 'role_custom'`)
	require.NoError(t, err)
}

// TestR081_SeededVerbSetsMatchTheRequirement asserts the migration seeded what
// R-080 and R-184 specify, including that the three *.override verbs are
// Owner-only.
func TestR081_SeededVerbSetsMatchTheRequirement(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAuthzStore(db)

	viewer, err := store.Role(ctx, authz.RoleViewer)
	require.NoError(t, err)
	require.ElementsMatch(t, []authz.Verb{authz.AppView, authz.AppLogsRead}, viewer.Verbs)

	operator, err := store.Role(ctx, authz.RoleOperator)
	require.NoError(t, err)
	require.False(t, operator.Has(authz.AppSecretsRead), "R-083")
	require.False(t, operator.Has(authz.AppExec), "R-084")
	for _, v := range []authz.Verb{authz.AppRoutingOverride, authz.AppResourceOverride, authz.AppEgressLoosen} {
		require.False(t, operator.Has(v), "%s is Owner-only", v)
	}
	require.True(t, operator.Has(authz.AppEgressTighten), "R-184: tightening is Operator's too")

	// Owner holds every *app* verb but app.deploy.approve, and no install
	// verb. An owner of one app administers nothing: R-031 gives every app an
	// owner of record, and that is not the same office as administering the
	// installation (O-17). Approving deploys is no built-in role's (R-155).
	owner, err := store.Role(ctx, authz.RoleOwner)
	require.NoError(t, err)
	for _, v := range authz.Verbs {
		if authz.InstallScoped(v) || v == authz.AppDeployApprove {
			require.False(t, owner.Has(v), "owner must not hold %s", v)
			continue
		}
		require.True(t, owner.Has(v), "owner is missing %s", v)
	}

	// And the administrator is the mirror image: every install verb, no app
	// verb. Managing a particular app still requires a grant on it.
	admin, err := store.Role(ctx, authz.RoleAdministrator)
	require.NoError(t, err)
	require.Equal(t, "administrator", admin.Name)
	require.True(t, admin.Builtin)
	for _, v := range authz.Verbs {
		require.Equal(t, authz.InstallScoped(v), admin.Has(v),
			"administrator should hold %s only if it is install-scoped", v)
	}
	require.Equal(t, len(authz.Verbs)-1, len(owner.Verbs)+len(admin.Verbs),
		"the two built-in top roles partition the catalog but for app.deploy.approve")
}

// TestR081_AdministratorIsImmutableToo asserts the fourth built-in role is
// protected by the same trigger as the other three (R-081).
func TestR081_AdministratorIsImmutableToo(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)

	_, err := db.Exec(ctx,
		`UPDATE roles SET verbs = verbs || 'app.exec' WHERE id = $1`, authz.RoleAdministrator)
	require.Error(t, err, "widening the administrator role at runtime must be refused")

	_, err = db.Exec(ctx, `DELETE FROM roles WHERE id = $1`, authz.RoleAdministrator)
	require.Error(t, err)
}

// TestR081_CreatorHoldsOnlyAppCreate asserts the Creator built-in role: install
// scope, one verb, and the same protection as the others. What a creator can
// manage beyond that comes from owning what they made (R-073), not from here.
func TestR081_CreatorHoldsOnlyAppCreate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAuthzStore(db)

	creator, err := store.Role(ctx, authz.RoleCreator)
	require.NoError(t, err)
	require.Equal(t, "creator", creator.Name)
	require.True(t, creator.Builtin)
	require.Equal(t, []authz.Verb{authz.AppCreate}, creator.Verbs)

	_, err = db.Exec(ctx,
		`UPDATE roles SET verbs = verbs || 'install.users.manage' WHERE id = $1`, authz.RoleCreator)
	require.Error(t, err, "widening the creator role at runtime must be refused")
	_, err = db.Exec(ctx, `DELETE FROM roles WHERE id = $1`, authz.RoleCreator)
	require.Error(t, err)
}

// TestR081_InstallRolesStandForAppRolesOnEveryApp asserts the built-in roles
// issue #81 seeds against the Go catalog, so the migration and authz.everyApp
// cannot drift apart: Administrator holds every install verb and no app verb;
// App viewer and App manager hold the install-wide counterparts of exactly the
// Viewer's and the Owner's verbs; Auditor holds the audit log and App viewer's
// two; and each is as immutable as the others.
func TestR081_InstallRolesStandForAppRolesOnEveryApp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	store := state.NewAuthzStore(db)

	role := func(id string) authz.Role {
		r, err := store.Role(ctx, id)
		require.NoError(t, err)
		require.True(t, r.Builtin, id)
		for _, v := range r.Verbs {
			require.True(t, authz.IsVerb(v), "%s holds %s, which is not in the catalog", id, v)
		}
		return r
	}
	counterparts := func(appRole string) []authz.Verb {
		var out []authz.Verb
		for _, v := range role(appRole).Verbs {
			c, ok := authz.InstallCounterpart(v)
			require.True(t, ok, v)
			out = append(out, c)
		}
		return out
	}

	var install []authz.Verb
	for _, v := range authz.Verbs {
		if authz.InstallScoped(v) {
			install = append(install, v)
		}
	}
	require.ElementsMatch(t, install, role(authz.RoleAdministrator).Verbs)
	require.ElementsMatch(t, counterparts(authz.RoleViewer), role(authz.RoleAppViewer).Verbs)
	require.ElementsMatch(t, counterparts(authz.RoleOwner), role(authz.RoleAppManager).Verbs)
	require.NotContains(t, role(authz.RoleAppManager).Verbs, authz.InstallDeploysApprove, "R-155")
	require.ElementsMatch(t,
		append([]authz.Verb{authz.InstallAuditRead}, counterparts(authz.RoleViewer)...),
		role(authz.RoleAuditor).Verbs)

	for _, id := range []string{authz.RoleAppViewer, authz.RoleAppManager, authz.RoleAuditor} {
		_, err := db.Exec(ctx, `UPDATE roles SET verbs = verbs || 'install.users.manage' WHERE id = $1`, id)
		require.Error(t, err, "%s must not be widened at runtime (R-081)", id)
		_, err = db.Exec(ctx, `DELETE FROM roles WHERE id = $1`, id)
		require.Error(t, err, id)
	}
}

// TestR080_GrantScopeIsEnforcedByTheDatabase asserts the structural half of
// install-level authorization: the two scopes cannot be mixed, whatever the
// application does.
//
// This is the property that makes "app_id IS NULL" a safe definition of install
// scope. Without it, a row with a null app and an app role would be a grant the
// install-wide query returns and whose verbs are app verbs.
func TestR080_GrantScopeIsEnforcedByTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	// An app role granted install-wide: refused by the composite foreign key.
	_, err := db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, NULL, 'control', 'install', 'user', $2, $3, 'system')`,
		id.New(id.Grant), alice.ID, authz.RoleOwner)
	require.Error(t, err, "an app role cannot be granted installation-wide")

	// An install role granted on one app: refused by the same key.
	_, err = db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, 'control', 'app', 'user', $3, $4, 'system')`,
		id.New(id.Grant), appID, alice.ID, authz.RoleAdministrator)
	require.Error(t, err, "an install role cannot be granted on a single app")

	// An install-scoped row that still names an app: refused by the CHECK.
	_, err = db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, role_scope, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, 'control', 'install', 'user', $3, $4, 'system')`,
		id.New(id.Grant), appID, alice.ID, authz.RoleAdministrator)
	require.Error(t, err)

	// Data-plane use is per-app and binary (R-070): there is no install-wide
	// "use" to grant.
	_, err = db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, created_by)
		VALUES ($1, NULL, 'data', 'user', $2, 'system')`, id.New(id.Grant), alice.ID)
	require.Error(t, err, "a data grant always names an app")
}

// TestR080_InstallGrantsAreNeverReturnedByAnAppLookup asserts the two scopes stay
// separate on the read path as well as the write path.
func TestR080_InstallGrantsAreNeverReturnedByAnAppLookup(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	grants := state.NewGrants(db)
	_, err := grants.GrantInstall(ctx, "user", alice.ID, authz.RoleAdministrator, "system")
	require.NoError(t, err)

	store := state.NewAuthzStore(db)
	p := authz.Principal{Kind: authz.KindUser, ID: alice.ID, UserID: alice.ID, Status: "active"}

	appScoped, err := store.ControlGrantsFor(ctx, appID, p)
	require.NoError(t, err)
	require.Empty(t, appScoped, "an install grant must not appear in an app's grants")

	installScoped, err := store.InstallGrantsFor(ctx, p)
	require.NoError(t, err)
	require.Len(t, installScoped, 1)

	verbs, err := store.InstallVerbsFor(ctx, p)
	require.NoError(t, err)
	require.Contains(t, verbs, string(authz.InstallUsersManage))
	require.NotContains(t, verbs, string(authz.AppExec))

	// Granting again replaces rather than duplicates: the schema allows one
	// control grant per principal install-wide, and GrantInstall is on the
	// bootstrap path, which runs on every start.
	_, err = grants.GrantInstall(ctx, "user", alice.ID, authz.RoleAdministrator, "system")
	require.NoError(t, err)
	installScoped, err = store.InstallGrantsFor(ctx, p)
	require.NoError(t, err)
	require.Len(t, installScoped, 1)

	// And it is revocable like any other grant. Administration is not a column
	// on the user.
	//
	// A second administrator first, because revoking the last account that can
	// manage accounts is refused — an installation nobody can administer is
	// unrecoverable through the API, and that guard is doing its job here
	// rather than being worked around. What this asserts is that the grant is
	// an ordinary row, which needs somebody else holding one.
	bob := seedUser(t, db, "bob")
	_, err = grants.GrantInstall(ctx, "user", bob.ID, authz.RoleAdministrator, "system")
	require.NoError(t, err)

	require.NoError(t, grants.RevokeInstall(ctx, "user", alice.ID))
	verbs, err = store.InstallVerbsFor(ctx, p)
	require.NoError(t, err)
	require.Empty(t, verbs)
}

// TestR088_TheLastAdministratorCannotBeRevoked asserts R-088.
//
// An installation with nobody who can manage accounts cannot be repaired
// through the API — the only way back is `pando admin` against the database,
// which requires shell access to the host. The refusal is at the store, below
// every surface, so the console, the CLI and the API all inherit it.
func TestR088_TheLastAdministratorCannotBeRevoked(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")

	grants := state.NewGrants(db)
	_, err := grants.GrantInstall(ctx, "user", alice.ID, authz.RoleAdministrator, "system")
	require.NoError(t, err)

	err = grants.RevokeInstall(ctx, "user", alice.ID)
	require.Error(t, err)
	require.Contains(t, err.Error(), "only account that can manage accounts")

	// Still administering, rather than half-revoked.
	verbs, err := state.NewAuthzStore(db).InstallVerbsFor(ctx, authz.Principal{
		Kind: authz.KindUser, ID: alice.ID, UserID: alice.ID, Status: "active",
	})
	require.NoError(t, err)
	require.Contains(t, verbs, string(authz.InstallUsersManage))
}

// TestR073_TwoPlanesAreTwoIndependentlyRevocableRows asserts R-073.
func TestR073_TwoPlanesAreTwoIndependentlyRevocableRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	for _, g := range []struct{ plane, role string }{
		{"control", authz.RoleOwner},
		{"data", ""},
	} {
		_, err := db.Exec(ctx, `
			INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
			VALUES ($1, $2, $3, 'user', $4, $5, $4)`,
			id.New(id.Grant), appID, g.plane, alice.ID, nullIfEmpty(g.role))
		require.NoError(t, err)
	}

	store := state.NewAuthzStore(db)
	p := authz.Principal{Kind: authz.KindUser, ID: alice.ID, UserID: alice.ID, Status: "active"}

	has, err := store.HasDataGrant(ctx, appID, p)
	require.NoError(t, err)
	require.True(t, has)

	// Revoking the data grant leaves the control grant standing.
	_, err = db.Exec(ctx, `DELETE FROM grants WHERE app_id = $1 AND plane = 'data'`, appID)
	require.NoError(t, err)

	has, err = store.HasDataGrant(ctx, appID, p)
	require.NoError(t, err)
	require.False(t, has)

	grants, err := store.ControlGrantsFor(ctx, appID, p)
	require.NoError(t, err)
	require.Len(t, grants, 1, "the control grant is independently revocable")
}

// TestR075_AnonymousGrantIsARowAndCannotBeDuplicated asserts R-074/R-075 and the
// NULLS NOT DISTINCT index — default NULL handling would let duplicates through.
func TestR075_AnonymousGrantIsARowAndCannotBeDuplicated(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	insert := func() error {
		_, err := db.Exec(ctx, `
			INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, created_by)
			VALUES ($1, $2, 'data', 'anonymous', NULL, $3)`, id.New(id.Grant), appID, alice.ID)
		return err
	}
	require.NoError(t, insert())
	require.Error(t, insert(), "the anonymous grant must not be insertable twice")

	has, _, err := state.NewAuthzStore(db).AnonymousAccess(ctx, appID)
	require.NoError(t, err)
	require.True(t, has)
}

// TestGrantShapesAreEnforcedByTheDatabase asserts the CHECK constraints: a
// data-plane grant has no role (R-070), and a non-anonymous grant has a
// principal (R-074).
func TestGrantShapesAreEnforcedByTheDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	_, err := db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
		VALUES ($1, $2, 'data', 'user', $3, $4, $3)`,
		id.New(id.Grant), appID, alice.ID, authz.RoleOwner)
	require.Error(t, err, "a data-plane grant must not carry a role (R-070)")

	_, err = db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, created_by)
		VALUES ($1, $2, 'data', 'user', NULL, $3)`, id.New(id.Grant), appID, alice.ID)
	require.Error(t, err, "only an anonymous grant may have a null principal (R-074)")
}

// TestR059_OrphanedDelegatedTokenEndToEnd asserts R-059 against the database,
// closing phase 1's Done when: the check is a live lookup, so suspending the
// owner is enough — no cascade runs and no grant is rewritten.
func TestR059_OrphanedDelegatedTokenEndToEnd(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	appID := seedApp(t, db, alice.ID)

	tokens := state.NewTokens(db, testTokenKey)
	issued, err := tokens.Create(ctx, state.TokenDelegated, "ci", alice.ID, alice.ID, nil)
	require.NoError(t, err)

	tok, err := tokens.Authenticate(ctx, issued.Secret)
	require.NoError(t, err)
	require.Equal(t, alice.ID, tok.OwnerUserID)

	store := state.NewAuthzStore(db)
	a := authz.New(store, nil, nil)
	principal := authz.Principal{Kind: authz.KindToken, ID: tok.ID, UserID: tok.OwnerUserID, TokenID: tok.ID}

	require.NoError(t, a.CheckData(ctx, principal, appID), "the token acts as its owner")

	require.NoError(t, state.NewUsers(db).SetStatus(ctx, alice.ID, "suspended"))

	err = a.CheckData(ctx, principal, appID)
	require.Error(t, err)
	require.Equal(t, errs.AuthTokenOrphaned, errs.CodeOf(err),
		"suspending the owner orphans the token with no cascade")
}

// TestR063_TokenSecretIsShownOnceAndStoredHashed asserts R-063.
func TestR063_TokenSecretIsShownOnceAndStoredHashed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")

	issued, err := state.NewTokens(db, testTokenKey).Create(ctx, state.TokenDelegated, "ci", alice.ID, alice.ID, nil)
	require.NoError(t, err)

	var stored string
	require.NoError(t, db.QueryRow(ctx, `SELECT hash FROM tokens WHERE id = $1`, issued.Token.ID).Scan(&stored))
	require.NotContains(t, stored, issued.Secret.Reveal(), "the secret must not be recoverable from the row")
	require.True(t, strings.HasPrefix(stored, "hmac-sha256:"), "an HMAC-SHA-256 digest under the token key (issue #93)")
}

// TestTokenAuthenticationFailuresAreIndistinguishable asserts that a caller
// cannot enumerate valid token IDs by comparing errors.
func TestTokenAuthenticationFailuresAreIndistinguishable(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db, testTokenKey)

	issued, err := tokens.Create(ctx, state.TokenDelegated, "ci", alice.ID, alice.ID, nil)
	require.NoError(t, err)

	expired := time.Now().UTC().Add(-time.Hour)
	stale, err := tokens.Create(ctx, state.TokenAccount, "old", "", alice.ID, &expired)
	require.NoError(t, err)

	revoked, err := tokens.Create(ctx, state.TokenAccount, "revoked", "", alice.ID, nil)
	require.NoError(t, err)
	require.NoError(t, tokens.Revoke(ctx, revoked.Token.ID))

	var messages []string
	for name, presented := range map[string]secret.Value{
		"garbage":       secret.New("nonsense"),
		"unknown id":    secret.New(id.New(id.Token) + ".whatever"),
		"wrong secret":  secret.New(issued.Token.ID + ".wrong"),
		"expired token": stale.Secret,
		"revoked token": revoked.Secret,
	} {
		_, err := tokens.Authenticate(ctx, presented)
		require.Error(t, err, name)
		require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err), name)
		messages = append(messages, errs.As(err).Message)
	}
	for _, m := range messages {
		require.Equal(t, messages[0], m, "every token failure must read identically")
	}
}

// TestTokenShapesAreEnforced asserts R-058 and R-060: a delegated token needs an
// owner, an account token must not have one.
func TestTokenShapesAreEnforced(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	tokens := state.NewTokens(db, testTokenKey)

	_, err := tokens.Create(ctx, state.TokenDelegated, "no owner", "", alice.ID, nil)
	require.Error(t, err, "a delegated token needs an owner (R-058)")

	_, err = tokens.Create(ctx, state.TokenAccount, "has owner", alice.ID, alice.ID, nil)
	require.Error(t, err, "an account token is its own principal (R-060)")

	account, err := tokens.Create(ctx, state.TokenAccount, "ci", "", alice.ID, nil)
	require.NoError(t, err)

	tok, err := tokens.Authenticate(ctx, account.Secret)
	require.NoError(t, err)
	require.Empty(t, tok.OwnerUserID)
}

// TestR046_FirstRunCreatesOneAdminAndIsIdempotent asserts R-046.
func TestR046_FirstRunCreatesOneAdminAndIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	users := state.NewUsers(db)
	auditor := audit.New(db.Pool)

	grants := state.NewGrants(db)

	// Supplied, the unattended path: without it first run makes nothing and
	// the installation waits to be set up in the console.
	supplied := secret.New("correct-horse-battery-staple")
	first, err := bootstrap.Run(ctx, users, grants, db, auditor, supplied)
	require.NoError(t, err)
	require.True(t, first.Created)
	require.True(t, first.User.MustChangePassword, "the initial credential must be changed on first login")

	// Never stored in the clear.
	var storedHash string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE id = $1`, first.User.ID).Scan(&storedHash))
	require.NotContains(t, storedHash, supplied.Reveal())

	again, err := bootstrap.Run(ctx, users, grants, db, auditor, supplied)
	require.NoError(t, err)
	require.False(t, again.Created, "first run must not repeat")

	var count int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM users`).Scan(&count))
	require.Equal(t, 1, count)

	// The account is administrative because of a grant (O-17), and running
	// bootstrap twice leaves one. Without this row a fresh install has a user
	// who can sign in and do nothing.
	store := state.NewAuthzStore(db)
	verbs, err := store.InstallVerbsFor(ctx, authz.Principal{
		Kind: authz.KindUser, ID: first.User.ID, UserID: first.User.ID, Status: "active",
	})
	require.NoError(t, err)
	require.Contains(t, verbs, string(authz.InstallUsersManage))
	require.Contains(t, verbs, string(authz.AppCreate))

	require.NoError(t, db.QueryRow(ctx,
		`SELECT count(*) FROM grants WHERE app_id IS NULL`).Scan(&count))
	require.Equal(t, 1, count)
}

// TestR079_GroupMembershipIsResolvedLive asserts R-079 against the database: a
// grant made to a group is honored through membership, and removing membership
// revokes access without touching the grant.
func TestR079_GroupMembershipIsResolvedLive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	bob := seedUser(t, db, "bob")
	appID := seedApp(t, db, alice.ID)

	groupID := id.New(id.Group)
	_, err := db.Exec(ctx, `INSERT INTO groups (id, name) VALUES ($1, 'engineering')`, groupID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, bob.ID)
	require.NoError(t, err)

	_, err = db.Exec(ctx, `
		INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, created_by)
		VALUES ($1, $2, 'data', 'group', $3, $4)`, id.New(id.Grant), appID, groupID, alice.ID)
	require.NoError(t, err)

	store := state.NewAuthzStore(db)
	bobPrincipal := authz.Principal{Kind: authz.KindUser, ID: bob.ID, UserID: bob.ID, Status: "active"}

	has, err := store.HasDataGrant(ctx, appID, bobPrincipal)
	require.NoError(t, err)
	require.True(t, has)

	// Remove membership only. The grant is untouched.
	_, err = db.Exec(ctx, `DELETE FROM group_members WHERE group_id = $1 AND user_id = $2`, groupID, bob.ID)
	require.NoError(t, err)

	has, err = store.HasDataGrant(ctx, appID, bobPrincipal)
	require.NoError(t, err)
	require.False(t, has, "membership is resolved live, so removing it revokes access")
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
