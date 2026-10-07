package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// counting is a store that counts what the authorizer asks it.
type counting struct {
	*store
	control, install, roles, status int
}

func (c *counting) ControlGrantsFor(ctx context.Context, appID string, p authz.Principal) ([]authz.Grant, error) {
	c.control++
	return c.store.ControlGrantsFor(ctx, appID, p)
}

func (c *counting) InstallGrantsFor(ctx context.Context, p authz.Principal) ([]authz.Grant, error) {
	c.install++
	return c.store.InstallGrantsFor(ctx, p)
}

func (c *counting) Role(ctx context.Context, roleID string) (authz.Role, error) {
	c.roles++
	return c.store.Role(ctx, roleID)
}

func (c *counting) UserStatus(ctx context.Context, userID string) (string, error) {
	c.status++
	return c.store.UserStatus(ctx, userID)
}

// countingPolicy denies the verbs it lists and counts how often its document
// is read: once per Allows, or once per Snapshot.
type countingPolicy struct {
	denied map[authz.Verb]bool
	reads  *int
	fail   bool
}

func (c countingPolicy) Allows(_ context.Context, _ authz.Principal, v authz.Verb, _ string) error {
	*c.reads++
	if c.fail {
		return errs.New(errs.Internal, "Could not read the host policy.")
	}
	return c.static(v)
}

func (c countingPolicy) static(v authz.Verb) error {
	if c.denied[v] {
		return errs.New(errs.PolicyExecDisabled, "This action is turned off for this installation.")
	}
	return nil
}

func (c countingPolicy) Snapshot(context.Context) (authz.Policy, error) {
	*c.reads++
	if c.fail {
		return nil, errs.New(errs.Internal, "Could not read the host policy.")
	}
	return snapshot(c), nil
}

type snapshot countingPolicy

func (s snapshot) Allows(_ context.Context, _ authz.Principal, v authz.Verb, _ string) error {
	return countingPolicy(s).static(v)
}

// TestR081_AppVerbsReadsEachThingOnceAndAnswersAsCheckControlDoes asserts
// that AppVerbs, which the API answers on every app screen, gives exactly the
// per-verb answer of CheckControl for every kind of principal and grant —
// app grants, groups, install-wide counterparts (R-081), account and
// delegated tokens, inactive accounts, policy denials (R-272) — while reading
// policy, grants and roles once for the whole call rather than once per verb.
func TestR081_AppVerbsReadsEachThingOnceAndAnswersAsCheckControlDoes(t *testing.T) {
	ctx := context.Background()

	const (
		carol   = "usr_carol"
		dave    = "usr_dave"
		erin    = "usr_erin"
		account = "tok_account"
		group   = "grp_team"
	)
	s := newStore()
	s.userStatus[alice] = "active"
	s.userStatus[bob] = "active"
	s.userStatus[carol] = "active"
	s.userStatus[dave] = "suspended"
	s.userStatus[erin] = "active"
	s.roles["role_custom"] = authz.Role{ID: "role_custom", Verbs: []authz.Verb{authz.AppDelete, authz.AppExec}}
	s.control[app] = []authz.Grant{
		{ID: "gr_1", Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleOperator},
		{ID: "gr_2", Plane: "control", PrincipalKind: "group", PrincipalID: group, RoleID: "role_custom"},
		{ID: "gr_3", Plane: "control", PrincipalKind: "user", PrincipalID: dave, RoleID: authz.RoleOwner},
		{ID: "gr_4", Plane: "control", PrincipalKind: "token", PrincipalID: account, RoleID: authz.RoleViewer},
		// A role the store reads with the grant, as the Postgres store does.
		{ID: "gr_5", Plane: "control", PrincipalKind: "user", PrincipalID: erin, RoleID: "role_joined",
			Role: &authz.Role{ID: "role_joined", Verbs: []authz.Verb{authz.AppRestart}}},
	}
	s.install = []authz.Grant{
		{ID: "gr_6", Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAppManager},
		{ID: "gr_7", Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleAppViewer},
	}

	principals := map[string]authz.Principal{
		"operator on the app, app viewer install-wide": activeUser(alice),
		"app manager install-wide":                     activeUser(bob),
		"through a group":                              {Kind: authz.KindUser, ID: carol, UserID: carol, Status: "active", Groups: []string{group}},
		"suspended owner":                              {Kind: authz.KindUser, ID: dave, UserID: dave, Status: "suspended"},
		"a role read with its grant":                   activeUser(erin),
		"account token":                                {Kind: authz.KindToken, ID: account, TokenID: account},
		"delegated token of an app manager":            {Kind: authz.KindToken, ID: "tok_bob", TokenID: "tok_bob", UserID: bob},
		"delegated token of a suspended owner":         {Kind: authz.KindToken, ID: "tok_dave", TokenID: "tok_dave", UserID: dave},
		"anonymous":                                    authz.Anonymous(),
	}
	policies := map[string]map[authz.Verb]bool{
		"no policy denials":    {},
		"exec and restart off": {authz.AppExec: true, authz.AppRestart: true},
	}

	for pname, denied := range policies {
		for name, p := range principals {
			reads := 0
			c := &counting{store: s}
			a := authz.New(c, countingPolicy{denied: denied, reads: &reads}, &recorder{})

			verbs, err := a.AppVerbs(ctx, p, app)
			require.NoError(t, err)
			require.LessOrEqual(t, reads, 1, "%s, %s: policy read once", pname, name)
			require.LessOrEqual(t, c.control, 1, "%s, %s: app grants read once", pname, name)
			require.LessOrEqual(t, c.install, 1, "%s, %s: install grants read once", pname, name)
			require.LessOrEqual(t, c.status, 1, "%s, %s: status read once", pname, name)
			require.LessOrEqual(t, c.roles, len(s.roles), "%s, %s: each role read at most once", pname, name)

			// The same question, one verb at a time, on a fresh authorizer.
			single := authz.New(s, countingPolicy{denied: denied, reads: new(int)}, nil)
			for _, v := range authz.AppVerbs() {
				allowed, err := single.Allows(ctx, p, app, v)
				require.NoError(t, err)
				require.Equal(t, allowed, contains(verbs, v), "%s, %s: %s", pname, name, v)
			}
		}
	}
}

// TestR272_AppVerbsHoldsNothingWhenPolicyCannotBeRead asserts that a policy
// document that cannot be read denies every verb in AppVerbs, as it does in
// each per-verb check: policy is a floor, and an unread floor is not absent.
func TestR272_AppVerbsHoldsNothingWhenPolicyCannotBeRead(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleOwner}}

	a := authz.New(s, countingPolicy{reads: new(int), fail: true}, nil)
	verbs, err := a.AppVerbs(ctx, activeUser(alice), app)
	require.NoError(t, err)
	require.Empty(t, verbs)
	allowed, err := a.Allows(ctx, activeUser(alice), app, authz.AppView)
	require.NoError(t, err)
	require.False(t, allowed)
}

// TestAppVerbsFailsOnAStoreFailure asserts AppVerbs still fails closed when a
// lookup it now shares across verbs fails.
func TestAppVerbsFailsOnAStoreFailure(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleViewer}}

	for name, f := range map[string]failing{
		"app grants":     {store: s, control: true},
		"a role":         {store: s, role: authz.RoleViewer},
		"install grants": {store: s, install: true},
	} {
		verbs, err := authz.New(f, nil, nil).AppVerbs(ctx, activeUser(bob), app)
		require.ErrorIs(t, err, errStore, name)
		require.Nil(t, verbs, name)
	}
}

// denialAudit is a policy that allows every verb and says whether anonymous
// denials are audited.
type denialAudit bool

func (denialAudit) Allows(context.Context, authz.Principal, authz.Verb, string) error { return nil }
func (d denialAudit) AuditsAnonymousDenials(context.Context) bool                     { return bool(d) }

// TestR227_HostPolicyMayStopAuditingAnonymousDenials asserts the
// disable_anonymous_denial_audit setting (design 06 §6): an anonymous visitor
// refused by a private app is audited by default and not when policy turns it
// off, and a signed-in person refused is audited either way.
func TestR227_HostPolicyMayStopAuditingAnonymousDenials(t *testing.T) {
	ctx := context.Background()
	s := newStore()

	for _, audits := range []bool{true, false} {
		rec := &recorder{}
		a := authz.New(s, denialAudit(audits), rec)

		err := a.CheckData(ctx, authz.Anonymous(), app)
		require.Equal(t, errs.PermDenied, errs.CodeOf(err), "still denied, whatever is recorded")
		if audits {
			require.Equal(t, 1, rec.denials, "audited by default")
		} else {
			require.Equal(t, 0, rec.denials, "not audited when policy says so")
		}

		rec.denials = 0
		s.userStatus[bob] = "active"
		require.Error(t, a.CheckData(ctx, activeUser(bob), app))
		require.Equal(t, 1, rec.denials, "a signed-in denial is always audited")
	}

	// A policy that does not say audits them.
	rec := &recorder{}
	require.Error(t, authz.New(s, nil, rec).CheckData(ctx, authz.Anonymous(), app))
	require.Equal(t, 1, rec.denials)
}
