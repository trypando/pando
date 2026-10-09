package authz_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// --- test double -----------------------------------------------------------

type store struct {
	userStatus map[string]string
	owner      map[string]string // appID -> userID
	control    map[string][]authz.Grant
	data       map[string][]string // appID -> principal IDs with a data grant
	anonymous  map[string]bool
	passcode   map[string]string // app → the one live unlock token
	roles      map[string]authz.Role

	// install is a flat list, not keyed by app: an install grant has no app.
	// Held separately for the same reason the schema holds them in rows with a
	// null app_id — so an app lookup can never find one.
	install []authz.Grant
}

func newStore() *store {
	return &store{
		userStatus: map[string]string{},
		owner:      map[string]string{},
		control:    map[string][]authz.Grant{},
		data:       map[string][]string{},
		anonymous:  map[string]bool{},
		passcode:   map[string]string{},
		roles: map[string]authz.Role{
			authz.RoleViewer:        {ID: authz.RoleViewer, Name: "viewer", Builtin: true, Verbs: []authz.Verb{authz.AppView, authz.AppLogsRead}},
			authz.RoleOperator:      {ID: authz.RoleOperator, Name: "operator", Builtin: true, Verbs: []authz.Verb{authz.AppView, authz.AppLogsRead, authz.AppDeploy, authz.AppRestart, authz.AppSpecEdit, authz.AppSecretsWrite, authz.AppEgressTighten}},
			authz.RoleOwner:         {ID: authz.RoleOwner, Name: "owner", Builtin: true, Verbs: appVerbs()},
			authz.RoleAdministrator: {ID: authz.RoleAdministrator, Name: "administrator", Builtin: true, Verbs: installVerbs()},
			authz.RoleAppViewer:     {ID: authz.RoleAppViewer, Name: "app viewer", Builtin: true, Verbs: []authz.Verb{authz.InstallAppsView, authz.InstallAppsLogsRead}},
			authz.RoleAppManager:    {ID: authz.RoleAppManager, Name: "app manager", Builtin: true, Verbs: appManagerVerbs()},
			authz.RoleAuditor:       {ID: authz.RoleAuditor, Name: "auditor", Builtin: true, Verbs: []authz.Verb{authz.InstallAuditRead, authz.InstallAppsView, authz.InstallAppsLogsRead}},
		},
	}
}

// appVerbs is the owner's set: the catalog minus the install-scoped verbs,
// and minus app.deploy.approve, which no built-in role holds (R-155). Owner is
// an app role, and an owner of one app administers nothing (R-031).
func appVerbs() []authz.Verb {
	var out []authz.Verb
	for _, v := range authz.Verbs {
		if !authz.InstallScoped(v) && v != authz.AppDeployApprove {
			out = append(out, v)
		}
	}
	return out
}

// appManagerVerbs is App manager's set: the install-wide counterpart of each of
// the Owner's verbs, which is every app verb's but app.deploy.approve's.
func appManagerVerbs() []authz.Verb {
	var out []authz.Verb
	for _, v := range appVerbs() {
		c, _ := authz.InstallCounterpart(v)
		out = append(out, c)
	}
	return out
}

func installVerbs() []authz.Verb {
	var out []authz.Verb
	for _, v := range authz.Verbs {
		if authz.InstallScoped(v) {
			out = append(out, v)
		}
	}
	return out
}

func (s *store) UserStatus(_ context.Context, userID string) (string, error) {
	if st, ok := s.userStatus[userID]; ok {
		return st, nil
	}
	return "deleted", nil
}

func (s *store) ControlGrantsFor(_ context.Context, appID string, p authz.Principal) ([]authz.Grant, error) {
	var out []authz.Grant
	for _, g := range s.control[appID] {
		if matches(g.PrincipalKind, g.PrincipalID, p) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *store) InstallGrantsFor(_ context.Context, p authz.Principal) ([]authz.Grant, error) {
	var out []authz.Grant
	for _, g := range s.install {
		if matches(g.PrincipalKind, g.PrincipalID, p) {
			out = append(out, g)
		}
	}
	return out, nil
}

func (s *store) IsOwner(_ context.Context, appID, userID string) (bool, error) {
	return userID != "" && s.owner[appID] == userID, nil
}

func (s *store) HasDataGrant(_ context.Context, appID string, p authz.Principal) (bool, error) {
	for _, id := range s.data[appID] {
		if id == p.UserID || id == p.ID {
			return true, nil
		}
		for _, g := range p.Groups {
			if id == g {
				return true, nil
			}
		}
	}
	return false, nil
}

func (s *store) AnonymousAccess(_ context.Context, appID string) (bool, bool, error) {
	return s.anonymous[appID], s.passcode[appID] != "", nil
}
func (s *store) PasscodeUnlocked(_ context.Context, appID, token string) (bool, error) {
	return token != "" && s.passcode[appID] == token, nil
}

func (s *store) Role(_ context.Context, roleID string) (authz.Role, error) {
	return s.roles[roleID], nil
}

func matches(kind, id string, p authz.Principal) bool {
	switch kind {
	case "user":
		return id == p.UserID
	case "token":
		return id == p.ID && p.UserID == ""
	case "group":
		for _, g := range p.Groups {
			if g == id {
				return true
			}
		}
	}
	return false
}

type denyingPolicy struct {
	verb authz.Verb

	// agentsOnly narrows the rule to token principals, which is how O-12
	// expresses "agents may not exec here" — as policy rather than as a list
	// the MCP server keeps, because an agent holding a token can call the REST
	// API directly.
	agentsOnly bool
}

func (d denyingPolicy) Allows(_ context.Context, p authz.Principal, v authz.Verb, _ string) error {
	if v != d.verb {
		return nil
	}
	if d.agentsOnly && p.Kind != authz.KindToken {
		return nil
	}
	return errs.New(errs.PolicyExecDisabled, "Running commands in apps is turned off for this installation.")
}

type recorder struct {
	denials int
	through []throughInstall
}

type throughInstall struct {
	appID, grantID string
	verb, through  authz.Verb
}

func (r *recorder) Denied(context.Context, authz.Principal, string, authz.Verb, errs.Code) {
	r.denials++
}

func (r *recorder) ThroughInstall(_ context.Context, _ authz.Principal, appID string, verb, through authz.Verb, g authz.Grant) {
	r.through = append(r.through, throughInstall{appID: appID, grantID: g.ID, verb: verb, through: through})
}

// --- fixtures --------------------------------------------------------------

const (
	app   = "app_01HQ8"
	alice = "usr_alice"
	bob   = "usr_bob"
)

func activeUser(id string) authz.Principal {
	return authz.Principal{Kind: authz.KindUser, ID: id, UserID: id, Status: "active"}
}

// --- the evaluation order ---------------------------------------------------

// TestR029_ControlPlaneRoleDoesNotGrantDataPlaneUse asserts R-029 and R-070.
//
// This is the test guarding the mistake that was made once already during
// design: an operator can deploy someone else's app but may not *use* it.
// CheckData contains exactly one cross-plane implication — ownership — and if a
// change adds another, this fails.
func TestR029_ControlPlaneRoleDoesNotGrantDataPlaneUse(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.owner[app] = alice
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleOperator}}

	a := authz.New(s, nil, nil)
	operator := activeUser(bob)

	require.NoError(t, a.CheckControl(ctx, operator, app, authz.AppDeploy),
		"an operator should be able to deploy")

	err := a.CheckData(ctx, operator, app)
	require.Error(t, err, "an operator on someone else's app must be denied USE of it")
	require.Equal(t, errs.PermDenied, errs.CodeOf(err))
}

// TestR029_OwnerRoleAloneDoesNotGrantUse asserts that even the owner *role* is
// not the owner *of record*. Only R-031 ownership crosses the planes.
func TestR029_OwnerRoleAloneDoesNotGrantUse(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.owner[app] = alice
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleOwner}}

	a := authz.New(s, nil, nil)
	require.Error(t, a.CheckData(ctx, activeUser(bob), app),
		"holding the owner ROLE is not the same as being the app's owner of record")
}

// TestR072_OwnershipGrantsUse asserts the one permitted implication.
func TestR072_OwnershipGrantsUse(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[alice] = "active"
	s.owner[app] = alice

	require.NoError(t, authz.New(s, nil, nil).CheckData(ctx, activeUser(alice), app))
}

// TestR059_DelegatedTokenIsOrphanedByItsOwnersDeletion asserts R-059, and is
// named in phase 1's Done when.
//
// The check is a live lookup on every request rather than a cascade run at
// revocation time. A cascade means a missed cascade is a permanent security
// hole; a live lookup cannot be missed.
func TestR059_DelegatedTokenIsOrphanedByItsOwnersDeletion(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[alice] = "active"
	s.owner[app] = alice

	token := authz.Principal{Kind: authz.KindToken, ID: "tok_01HQ8", UserID: alice, TokenID: "tok_01HQ8"}
	a := authz.New(s, nil, nil)

	require.NoError(t, a.CheckData(ctx, token, app), "the token acts as its owner while the owner is active")

	// No cascade runs, no grant is rewritten — only the owner's status changes.
	s.userStatus[alice] = "deleted"

	err := a.CheckData(ctx, token, app)
	require.Error(t, err)
	require.Equal(t, errs.AuthTokenOrphaned, errs.CodeOf(err))
}

// TestR049_SuspendedIsNotDeletedButBothDeny asserts R-049: suspension denies
// access without being deletion, because destruction rules fire on one and not
// the other.
func TestR049_SuspendedIsNotDeletedButBothDeny(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.owner[app] = alice
	a := authz.New(s, nil, nil)

	for _, status := range []string{"suspended", "deleted"} {
		s.userStatus[alice] = status
		p := authz.Principal{Kind: authz.KindUser, ID: alice, UserID: alice, Status: status}
		require.Error(t, a.CheckData(ctx, p, app), "a %s user must be denied", status)
	}

	s.userStatus[alice] = "active"
	require.NoError(t, a.CheckData(ctx, activeUser(alice), app))
}

// TestR272_PolicyIsAFloorAndDeniesTheOwnerToo asserts R-272: policy is
// evaluated before grants and cannot be overridden by one.
func TestR272_PolicyIsAFloorAndDeniesTheOwnerToo(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[alice] = "active"
	s.owner[app] = alice
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleOwner}}

	a := authz.New(s, denyingPolicy{verb: authz.AppExec}, nil)

	err := a.CheckControl(ctx, activeUser(alice), app, authz.AppExec)
	require.Error(t, err, "policy disabling exec install-wide must deny the owner too")
	require.Equal(t, errs.PolicyExecDisabled, errs.CodeOf(err))

	require.NoError(t, a.CheckControl(ctx, activeUser(alice), app, authz.AppDeploy),
		"other verbs are unaffected")
}

// TestR075_AnonymousGrantAllowsUnauthenticatedUse asserts R-075: the anonymous
// grant is a grant, evaluated on the same path as any other.
func TestR075_AnonymousGrantAllowsUnauthenticatedUse(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	a := authz.New(s, nil, nil)

	require.Error(t, a.CheckData(ctx, authz.Anonymous(), app))

	s.anonymous[app] = true
	require.NoError(t, a.CheckData(ctx, authz.Anonymous(), app))
}

// TestR079_GroupMembershipIsResolvedFromThePrincipal asserts that a group grant
// works and that membership arrives on the principal, resolved live per request
// rather than denormalized into the grant.
func TestR079_GroupMembershipIsResolvedFromThePrincipal(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.owner[app] = alice
	s.data[app] = []string{"grp_engineering"}
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "group", PrincipalID: "grp_engineering", RoleID: authz.RoleViewer}}

	a := authz.New(s, nil, nil)

	member := activeUser(bob)
	member.Groups = []string{"grp_engineering"}
	require.NoError(t, a.CheckData(ctx, member, app))
	require.NoError(t, a.CheckControl(ctx, member, app, authz.AppView))

	// Removing the group from the principal is all it takes — no grant changes.
	require.Error(t, a.CheckData(ctx, activeUser(bob), app))
	require.Error(t, a.CheckControl(ctx, activeUser(bob), app, authz.AppView))
}

// TestR060_AccountTokenIsItsOwnPrincipal asserts R-060.
func TestR060_AccountTokenIsItsOwnPrincipal(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.owner[app] = alice
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "token", PrincipalID: "tok_ci", RoleID: authz.RoleOperator}}
	s.data[app] = []string{"tok_ci"}

	account := authz.Principal{Kind: authz.KindToken, ID: "tok_ci", TokenID: "tok_ci"} // no UserID
	a := authz.New(s, nil, nil)

	require.NoError(t, a.CheckControl(ctx, account, app, authz.AppDeploy))
	require.NoError(t, a.CheckData(ctx, account, app))
	require.Error(t, a.CheckControl(ctx, account, app, authz.AppExec), "operator does not hold exec")
}

// TestR081_BuiltInRoleVerbSets asserts the verb sets from R-080/R-081: the
// *.override verbs and app.egress.loosen are Owner-only, app.egress.tighten is
// Operator's too (R-184), and nobody built in holds app.deploy.approve (R-155).
func TestR081_BuiltInRoleVerbSets(t *testing.T) {
	s := newStore()

	viewer := s.roles[authz.RoleViewer]
	require.True(t, viewer.Has(authz.AppView))
	require.False(t, viewer.Has(authz.AppDeploy))

	operator := s.roles[authz.RoleOperator]
	require.True(t, operator.Has(authz.AppDeploy))
	require.True(t, operator.Has(authz.AppSecretsWrite))
	require.False(t, operator.Has(authz.AppSecretsRead), "R-083: write is separable from read")
	require.False(t, operator.Has(authz.AppExec), "R-084: exec is its own verb")

	for _, v := range []authz.Verb{authz.AppRoutingOverride, authz.AppResourceOverride, authz.AppEgressLoosen} {
		require.False(t, operator.Has(v), "%s deviates from a host default and is Owner-only", v)
		require.True(t, s.roles[authz.RoleOwner].Has(v))
	}
	require.True(t, operator.Has(authz.AppEgressTighten), "tightening is always within policy (R-272)")
	require.True(t, s.roles[authz.RoleOwner].Has(authz.AppEgressTighten))
	require.False(t, s.roles[authz.RoleOwner].Has(authz.AppDeployApprove), "R-155: no built-in role approves one app's deploys")
	require.True(t, s.roles[authz.RoleAdministrator].Has(authz.InstallDeploysApprove))
}

// TestR082_NoVerbImplicationGraph asserts that holding one verb implies nothing.
// Implication graphs are where authorization bugs live.
func TestR082_NoVerbImplicationGraph(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.roles["role_custom"] = authz.Role{ID: "role_custom", Name: "deleter", Verbs: []authz.Verb{authz.AppDelete}}
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: "role_custom"}}

	a := authz.New(s, nil, nil)
	require.NoError(t, a.CheckControl(ctx, activeUser(bob), app, authz.AppDelete))
	require.Error(t, a.CheckControl(ctx, activeUser(bob), app, authz.AppView),
		"app.delete must not imply app.view")
}

// TestDenialsAreAudited asserts that every denial is recorded, not only
// successes. A denial pattern is the signal that matters for detecting misuse.
func TestDenialsAreAudited(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.owner[app] = alice

	rec := &recorder{}
	a := authz.New(s, nil, rec)

	require.Error(t, a.CheckData(ctx, activeUser(bob), app))
	require.Error(t, a.CheckControl(ctx, activeUser(bob), app, authz.AppDeploy))
	require.Equal(t, 2, rec.denials, "both denials should have been audited")

	s.owner[app] = bob
	require.NoError(t, a.CheckData(ctx, activeUser(bob), app))
	require.Equal(t, 2, rec.denials, "a success is not a denial")
}

// TestSystemPrincipalBypassesGrantsButIsStillAPrincipal asserts the reconciler's
// path: grant checks are bypassed, and the audit trail is not.
func TestSystemPrincipalBypassesGrantsButIsStillAPrincipal(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	rec := &recorder{}
	a := authz.New(s, nil, rec)

	require.NoError(t, a.CheckControl(ctx, authz.System(), app, authz.AppRestart))
	require.Equal(t, 0, rec.denials)
}

func TestVerbCatalogIsClosed(t *testing.T) {
	// 15 app verbs, and 26 install-scoped ones: the twelve that stand on their
	// own (O-17, R-217, install.tokens.manage, install.deploys.approve,
	// install.upgrade, install.events.manage, install.audit.export,
	// app.create) and the fourteen install.apps.* counterparts of app verbs
	// (issue #81). The count is here deliberately: R-080 says the catalog is
	// fixed, so adding a verb should require editing a test rather than only a
	// constant.
	require.Len(t, authz.Verbs, 41)
	require.True(t, authz.IsVerb(authz.InstallAuditExport), "R-385's verb must exist")
	require.True(t, authz.IsVerb(authz.InstallUpgrade), "R-356's verb must exist")
	require.True(t, authz.IsVerb(authz.AppEgressTighten), "R-184's verbs must exist")
	require.True(t, authz.IsVerb(authz.AppEgressLoosen), "R-184's verbs must exist")
	require.False(t, authz.IsVerb(authz.Verb("app.egress.override")), "renamed to app.egress.loosen by issue #79")
	require.True(t, authz.IsVerb(authz.AppDeployApprove), "R-155's verbs must exist")
	require.True(t, authz.IsVerb(authz.InstallDeploysApprove), "R-155's verbs must exist")
	require.False(t, authz.IsVerb(authz.Verb("install.apps.manage")), "a bundle, replaced by the App manager role in issue #81")
	require.False(t, authz.IsVerb(authz.Verb("app.do.anything")))

	var install, app int
	for _, v := range authz.Verbs {
		if authz.InstallScoped(v) {
			install++
		} else {
			app++
		}
	}
	require.Equal(t, 26, install)
	require.Equal(t, 15, app)
}

// TestR080_EveryAppVerbHasOneInstallCounterpart asserts issue #81's table:
// every app verb has exactly one install-scoped verb standing for it on every
// app, and every install verb stands for at most one app verb. A verb added
// to the catalog fails here until its counterpart is written into everyApp
// on purpose.
func TestR080_EveryAppVerbHasOneInstallCounterpart(t *testing.T) {
	seen := map[authz.Verb]authz.Verb{}
	for _, v := range authz.AppVerbs() {
		c, ok := authz.InstallCounterpart(v)
		require.True(t, ok, "%s has no install-wide counterpart", v)
		require.True(t, authz.IsVerb(c), "%s's counterpart %s is not in the catalog", v, c)
		require.True(t, authz.InstallScoped(c), "%s's counterpart %s must be install-scoped (R-080)", v, c)
		back, ok := authz.EveryApp(c)
		require.True(t, ok)
		require.Equal(t, v, back)
		_, dup := seen[c]
		require.False(t, dup, "%s stands for two app verbs", c)
		seen[c] = v
	}

	// The install verbs that are not counterparts stand for nothing on an app.
	for _, v := range authz.Verbs {
		if !authz.InstallScoped(v) {
			_, ok := authz.EveryApp(v)
			require.False(t, ok, "an app verb is not an install counterpart: %s", v)
			continue
		}
		if _, ok := seen[v]; !ok {
			_, stands := authz.EveryApp(v)
			require.False(t, stands, "%s", v)
		}
	}
	require.Equal(t, authz.AppDeployApprove, seen[authz.InstallDeploysApprove],
		"install.deploys.approve is app.deploy.approve's counterpart (R-155)")
}

// TestR080_InstallVerbRequiresAnInstallGrant asserts install-level
// authorization: an ordinary account holds nothing install-wide, and the power
// arrives as a grant rather than as a property of the account.
func TestR080_InstallVerbRequiresAnInstallGrant(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	bobP := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}

	a := authz.New(s, nil, nil)

	// Signed in, and that is all. This is the state every account was in when
	// six endpoints were gated by "are you signed in" and nothing else.
	require.Error(t, a.CheckInstall(ctx, bobP, authz.InstallUsersManage))
	require.Error(t, a.CheckInstall(ctx, bobP, authz.AppCreate))

	// An owner grant on an app is not administration. Owning every app in the
	// install would still not be.
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleOwner}}
	require.NoError(t, a.CheckControl(ctx, bobP, app, authz.AppDelete))
	require.Error(t, a.CheckInstall(ctx, bobP, authz.InstallUsersManage),
		"an app role must never satisfy an install verb")

	// The grant with no app is what makes an administrator.
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}
	require.NoError(t, a.CheckInstall(ctx, bobP, authz.InstallUsersManage))
	require.NoError(t, a.CheckInstall(ctx, bobP, authz.AppCreate))

	// It reaches into every app through the install.apps.* verbs (R-081) —
	// and only through them: an install role without one reaches none.
	require.NoError(t, a.CheckControl(ctx, bobP, "app_other", authz.AppDelete),
		"an administrator manages every app")
	s.roles["role_audit_only"] = authz.Role{ID: "role_audit_only", Verbs: []authz.Verb{authz.InstallAuditRead, authz.InstallUsersManage}}
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: "role_audit_only"}}
	require.Error(t, a.CheckControl(ctx, bobP, "app_other", authz.AppView),
		"no install verb but install.apps.* bears on an app")
}

// TestR081_AdministratorsLookAfterEveryAppButDoNotUseIt asserts the
// Administrator's reach: every app verb on every app, through the install.apps.*
// counterparts and install.deploys.approve; host policy still comes first; and
// none of it opens an app.
func TestR081_AdministratorsLookAfterEveryAppButDoNotUseIt(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.owner[app] = "usr_someone_else"
	adminP := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}

	a := authz.New(s, nil, nil)
	for _, v := range authz.AppVerbs() {
		require.NoError(t, a.CheckControl(ctx, adminP, app, v), v)
	}
	// Managing is not using (R-087): the data plane still needs a grant.
	require.Error(t, a.CheckData(ctx, adminP, app))

	// Policy is a floor for administrators too (R-272).
	withPolicy := authz.New(s, denyingPolicy{verb: authz.AppExec}, nil)
	require.Error(t, withPolicy.CheckControl(ctx, adminP, app, authz.AppExec))
	verbs, err := withPolicy.AppVerbs(ctx, adminP, app)
	require.NoError(t, err)
	require.NotContains(t, verbs, authz.AppExec)
	require.Contains(t, verbs, authz.AppDelete)
}

// TestR081_BuiltInInstallRolesStandForAppRolesOnEveryApp asserts the roles
// that replaced the install.apps.view and install.apps.manage bundles (issue
// #81): App viewer is the Viewer on every app, App manager the Owner on every
// app — which does not approve deploys (R-155) — and Auditor sees every app and
// reads its logs and changes nothing.
func TestR081_BuiltInInstallRolesStandForAppRolesOnEveryApp(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	bobP := activeUser(bob)
	a := authz.New(s, nil, nil)

	verbsAs := func(role string) []authz.Verb {
		s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: role}}
		verbs, err := a.AppVerbs(ctx, bobP, app)
		require.NoError(t, err)
		return verbs
	}

	require.Equal(t, s.roles[authz.RoleViewer].Verbs, verbsAs(authz.RoleAppViewer))
	require.Equal(t, s.roles[authz.RoleOwner].Verbs, verbsAs(authz.RoleAppManager))
	require.Equal(t, []authz.Verb{authz.AppView, authz.AppLogsRead}, verbsAs(authz.RoleAuditor))

	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAuditor}}
	require.NoError(t, a.CheckInstall(ctx, bobP, authz.InstallAuditRead))
	require.Error(t, a.CheckInstall(ctx, bobP, authz.InstallView), "an auditor does not see accounts or adapters")
	require.Error(t, a.CheckControl(ctx, bobP, app, authz.AppDeploy))
	require.Error(t, a.CheckData(ctx, bobP, app), "reading every app's logs is not opening one (R-087)")
}

// TestR080_EachInstallCounterpartStandsForItsAppVerbAlone asserts issue #81:
// a custom install role holding one install.apps.* verb holds that app verb on
// every app, including one made after the grant, and no other — there is no
// implication graph (R-082), not even to app.view.
func TestR080_EachInstallCounterpartStandsForItsAppVerbAlone(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	bobP := activeUser(bob)
	a := authz.New(s, nil, nil)

	for _, v := range authz.AppVerbs() {
		c, _ := authz.InstallCounterpart(v)
		s.roles["role_one"] = authz.Role{ID: "role_one", Verbs: []authz.Verb{c}}
		s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: "role_one"}}
		for _, appID := range []string{app, "app_made_later"} {
			verbs, err := a.AppVerbs(ctx, bobP, appID)
			require.NoError(t, err)
			require.Equal(t, []authz.Verb{v}, verbs, "%s on %s", c, appID)
		}
		require.Error(t, a.CheckData(ctx, bobP, app), "%s is control plane only (R-087)", c)
	}
}

// TestR272_PolicyDeniesAnAppVerbHeldInstallWide asserts that host policy is
// asked about the app verb, so disabling it disables it for everyone holding
// its install-wide counterpart too — for agents alone, when it is agents'.
func TestR272_PolicyDeniesAnAppVerbHeldInstallWide(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.roles["role_exec_everywhere"] = authz.Role{ID: "role_exec_everywhere", Verbs: []authz.Verb{authz.InstallAppsExec}}
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: "role_exec_everywhere"}}

	require.NoError(t, authz.New(s, nil, nil).CheckControl(ctx, activeUser(bob), app, authz.AppExec))
	require.Error(t, authz.New(s, denyingPolicy{verb: authz.AppExec}, nil).CheckControl(ctx, activeUser(bob), app, authz.AppExec))

	s.userStatus[bob] = "active"
	agent := authz.Principal{Kind: authz.KindToken, ID: "tok_1", UserID: bob}
	agents := authz.New(s, denyingPolicy{verb: authz.AppExec, agentsOnly: true}, nil)
	require.Error(t, agents.CheckControl(ctx, agent, app, authz.AppExec))
	require.NoError(t, agents.CheckControl(ctx, activeUser(bob), app, authz.AppExec))
}

// TestR080_AccessThroughAnInstallGrantIsAudited asserts issue #81's audit
// rule: a change allowed by an install-wide grant rather than one on the app
// records which install verb and which grant allowed it. A grant on the app
// records nothing extra, and neither do the two reads the console asks
// constantly, nor the checks that only decide what to show.
func TestR080_AccessThroughAnInstallGrantIsAudited(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	bobP := activeUser(bob)
	s.install = []authz.Grant{{ID: "gr_install", Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAppManager}}
	rec := &recorder{}
	a := authz.New(s, nil, rec)

	require.NoError(t, a.CheckControl(ctx, bobP, app, authz.AppDeploy))
	require.Equal(t, []throughInstall{{appID: app, grantID: "gr_install", verb: authz.AppDeploy, through: authz.InstallAppsDeploy}}, rec.through)

	require.NoError(t, a.CheckControl(ctx, bobP, app, authz.AppView))
	require.NoError(t, a.CheckControl(ctx, bobP, app, authz.AppLogsRead))
	_, err := a.AppVerbs(ctx, bobP, app)
	require.NoError(t, err)
	_, err = a.Allows(ctx, bobP, app, authz.AppDelete)
	require.NoError(t, err)
	require.NoError(t, a.PreviewControl(ctx, bobP, app, authz.AppDelete))
	require.Len(t, rec.through, 1, "reads and previews are not recorded")

	// The same verb through a grant on the app is an ordinary allow.
	s.control["app_owned"] = []authz.Grant{{ID: "gr_app", Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleOwner}}
	require.NoError(t, a.CheckControl(ctx, bobP, "app_owned", authz.AppDeploy))
	require.Len(t, rec.through, 1)
}

// AppVerbs is what the console shows as editable, so it must say exactly what
// CheckControl says, verb by verb, and audit nothing while it does.
func TestAppVerbsAgreesWithCheckControl(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	bobP := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}
	s.control[app] = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleOperator}}

	rec := &recorder{}
	a := authz.New(s, denyingPolicy{verb: authz.AppRestart}, rec)
	verbs, err := a.AppVerbs(ctx, bobP, app)
	require.NoError(t, err)
	require.Equal(t, 0, rec.denials, "looking is not trying")

	for _, v := range authz.AppVerbs() {
		allowed := a.CheckControl(ctx, bobP, app, v) == nil
		require.Equal(t, allowed, contains(verbs, v), v)
	}
}

func contains(vs []authz.Verb, v authz.Verb) bool {
	for _, got := range vs {
		if got == v {
			return true
		}
	}
	return false
}

// TestR080_ScopesCannotBeCheckedAgainstEachOther asserts the guard at the
// boundary between the two functions.
//
// The asymmetry is the point. An install verb evaluated against an app searches
// for a grant that cannot exist and denies — wrong but safe. An app verb
// evaluated install-wide searches for a grant that *can* exist and could allow.
// Both are refused so that neither call site can be written by accident.
func TestR080_ScopesCannotBeCheckedAgainstEachOther(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}
	bobP := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}

	a := authz.New(s, nil, nil)

	err := a.CheckControl(ctx, bobP, app, authz.InstallUsersManage)
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	err = a.CheckInstall(ctx, bobP, authz.AppExec)
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
}

// TestR080_AnonymousHoldsNothingInstallWide asserts that the install plane has no
// anonymous path at all — unlike the data plane, where R-075's anonymous grant
// is a real row.
func TestR080_AnonymousHoldsNothingInstallWide(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	a := authz.New(s, nil, nil)

	for _, v := range authz.Verbs {
		if !authz.InstallScoped(v) {
			continue
		}
		require.Error(t, a.CheckInstall(ctx, authz.Anonymous(), v))
	}
}

// TestR049_SuspendedAdministratorHoldsNothing asserts the evaluation order:
// principal status is checked before grants, so suspension takes effect without
// touching the grant.
func TestR049_SuspendedAdministratorHoldsNothing(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}

	a := authz.New(s, nil, nil)
	suspended := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "suspended"}

	err := a.CheckInstall(ctx, suspended, authz.InstallUsersManage)
	require.Error(t, err)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), "status is step 2, before grants")
}

// TestR272_HostPolicyIsAFloorForAdministratorsToo asserts R-272 on the install
// plane: policy is evaluated before grants and denies the holder.
func TestR272_HostPolicyIsAFloorForAdministratorsToo(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "user", PrincipalID: bob, RoleID: authz.RoleAdministrator}}
	bobP := authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}

	a := authz.New(s, denyingPolicy{verb: authz.InstallPolicyManage}, nil)

	require.NoError(t, a.CheckInstall(ctx, bobP, authz.InstallView))
	require.Error(t, a.CheckInstall(ctx, bobP, authz.InstallPolicyManage),
		"host policy is a floor, not an override — it denies the administrator too")
}

// TestR080_DeniedInstallChecksAreAudited asserts that denial on the install plane
// is recorded like denial on any other. A denial pattern is the signal that
// matters for detecting misuse.
func TestR080_DeniedInstallChecksAreAudited(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[bob] = "active"
	rec := &recorder{}
	a := authz.New(s, nil, rec)

	require.Error(t, a.CheckInstall(ctx,
		authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"},
		authz.InstallUsersManage))
	require.Equal(t, 1, rec.denials)
}

// TestR060_AccountTokenHoldsItsOwnInstallGrant asserts R-060: an account token is
// its own principal and is looked up under its own ID, install-wide as per app.
func TestR060_AccountTokenHoldsItsOwnInstallGrant(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.install = []authz.Grant{{Plane: "control", PrincipalKind: "token", PrincipalID: "tok_1", RoleID: authz.RoleAdministrator}}

	a := authz.New(s, nil, nil)
	account := authz.Principal{Kind: authz.KindToken, ID: "tok_1", TokenID: "tok_1"}
	require.NoError(t, a.CheckInstall(ctx, account, authz.InstallView))

	other := authz.Principal{Kind: authz.KindToken, ID: "tok_2", TokenID: "tok_2"}
	require.Error(t, a.CheckInstall(ctx, other, authz.InstallView))
}

// TestO12_AgentExclusionsAreHostPolicyNotAnMCPList asserts the resolution of
// O-12: "agents may not exec here" is a policy scoped to token principals.
//
// The tempting alternative is a list of tools the MCP server refuses to expose,
// and it does not work — an agent holding a token can call the REST API
// directly, so an MCP-layer exclusion is a speed bump rather than a boundary.
// Expressed as policy, it is evaluated before grants on every surface.
func TestO12_AgentExclusionsAreHostPolicyNotAnMCPList(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.userStatus[alice] = "active"
	s.owner[app] = alice
	s.control[app] = []authz.Grant{
		{Plane: "control", PrincipalKind: "user", PrincipalID: alice, RoleID: authz.RoleOwner},
	}

	a := authz.New(s, denyingPolicy{verb: authz.AppExec, agentsOnly: true}, nil)

	// The person keeps the verb they hold.
	require.NoError(t, a.CheckControl(ctx, activeUser(alice), app, authz.AppExec))

	// Their delegated token does not. Authorization otherwise runs against the
	// owner exactly as if they made the request (R-058), so the *only* thing
	// separating these two calls is the principal's kind — which is the whole
	// mechanism.
	agent := authz.Principal{Kind: authz.KindToken, ID: "tok_agent", UserID: alice}
	err := a.CheckControl(ctx, agent, app, authz.AppExec)
	require.Error(t, err)
	require.Equal(t, errs.PolicyExecDisabled, errs.CodeOf(err))

	// And it does not leak into verbs the rule does not name.
	require.NoError(t, a.CheckControl(ctx, agent, app, authz.AppDeploy))
}

// TestR075a_APasscodeGateIsOnlyOnTheGrantToEveryone asserts CheckData's
// passcode rule: sharing with everyone behind a passcode lets in whoever shows
// a live unlock for that app, and nobody else — while the owner and anyone
// with their own grant never see the passcode at all.
func TestR075a_APasscodeGateIsOnlyOnTheGrantToEveryone(t *testing.T) {
	ctx := context.Background()
	s := newStore()
	s.anonymous[app] = true
	s.passcode[app] = "live-token"
	a := authz.New(s, nil, nil)

	stranger := authz.Anonymous()
	err := a.CheckData(ctx, stranger, app)
	require.Equal(t, errs.PermPasscodeRequired, errs.CodeOf(err))

	stranger.Passcodes = map[string]string{app: "old-token"}
	require.Equal(t, errs.PermPasscodeRequired, errs.CodeOf(a.CheckData(ctx, stranger, app)))

	// An unlock for another app is not one for this.
	stranger.Passcodes = map[string]string{"app_other": "live-token"}
	require.Equal(t, errs.PermPasscodeRequired, errs.CodeOf(a.CheckData(ctx, stranger, app)))

	stranger.Passcodes = map[string]string{app: "live-token"}
	require.NoError(t, a.CheckData(ctx, stranger, app))

	// The owner, and a person with a grant of their own, are let in as ever.
	s.userStatus[bob] = "active"
	s.owner[app] = bob
	require.NoError(t, a.CheckData(ctx, authz.Principal{Kind: authz.KindUser, ID: bob, UserID: bob, Status: "active"}, app))

	// Plain public stays plain public.
	s.passcode[app] = ""
	require.NoError(t, a.CheckData(ctx, authz.Anonymous(), app))
}
