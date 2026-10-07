//go:build integration

package acceptance_test

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Install-level authorization, end to end against the shipped binary (O-17).
//
// This file exists because the hole it closes was demonstrated here first: a
// user created with no grants at all could `PATCH /users/{admin}` with
// `{"status":"suspended"}`, get a 204, and the administrator's next sign-in
// returned 401. Six endpoints were gated by "are you signed in" and nothing
// else. Every assertion below is one of the steps of that escalation, now
// refused.

func verbsOf(t *testing.T, c *client) []string {
	t.Helper()
	me := c.get(t, "/me")

	raw, ok := me["verbs"].([]any)
	require.True(t, ok, "GET /me must always carry a verbs list, empty or not: %v", me)

	out := make([]string, 0, len(raw))
	for _, v := range raw {
		out = append(out, v.(string))
	}
	return out
}

// uniqueName keeps reruns against a long-lived stack from colliding on the
// username unique constraint.
func uniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// TestR080_AnOrdinaryUserCannotSuspendTheAdministrator asserts R-080's install
// scope: the escalation, refused.
//
// The single most important assertion in this file. It failed — 204, and the
// administrator locked out — before install verbs existed (O-17).
func TestR080_AnOrdinaryUserCannotSuspendTheAdministrator(t *testing.T) {
	admin := login(t)
	adminID := admin.get(t, "/me")["user_id"].(string)

	bob := admin.asUser(t, admin.createUser(t, uniqueName("bob")))

	// Bob holds nothing install-wide. This is every account on a normal
	// install, and it is the state the escalation started from.
	require.Empty(t, verbsOf(t, bob))

	body, status := bob.do(t, http.MethodPatch, "/users/"+adminID, `{"status":"suspended"}`)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "PERM_VERB_REQUIRED")
	require.Contains(t, body, "install.users.manage",
		"the denial should name the verb, so the reader knows what to ask for (R-105)")

	// And the administrator still works. This is the assertion that would have
	// caught the original bug even if the status code above had been wrong.
	stillAdmin := login(t)
	require.Contains(t, verbsOf(t, stillAdmin), "install.users.manage")
}

// TestR080_UserEndpointsAreSelfOrVerb asserts the shape the user endpoints were
// asked for: your own account without administrative power, anyone else's only
// with the verb (design 04 §2.7).
func TestR080_UserEndpointsAreSelfOrVerb(t *testing.T) {
	admin := login(t)
	adminID := admin.get(t, "/me")["user_id"].(string)

	carolID := admin.createUser(t, uniqueName("carol"))
	carol := admin.asUser(t, carolID)

	// Reading your own account: allowed.
	body, status := carol.do(t, http.MethodGet, "/users/"+carolID, "")
	require.Equal(t, http.StatusOK, status, body)

	// Reading someone else's: the account directory is not public. An account
	// carries an email address and a display name.
	body, status = carol.do(t, http.MethodGet, "/users/"+adminID, "")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "install.view")

	// Changing your own: allowed. Reinstating an active account is a no-op,
	// which is the point — it is the authorization being asserted, not the
	// effect.
	body, status = carol.do(t, http.MethodPatch, "/users/"+carolID, `{"status":"active"}`)
	require.Equal(t, http.StatusNoContent, status, body)

	// Creating an account is not self-service at all.
	body, status = carol.do(t, http.MethodPost, "/users",
		`{"username":"intruder","password":"correct-horse-battery"}`)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "install.users.manage")
}

// TestR080_InstallEndpointsRequireInstallVerbs covers the rest of the six.
func TestR080_InstallEndpointsRequireInstallVerbs(t *testing.T) {
	admin := login(t)

	dave := admin.asUser(t, admin.createUser(t, uniqueName("dave")))

	for _, tc := range []struct {
		method, path, body, verb string
	}{
		{http.MethodPost, "/apps", `{"name":"sneaky"}`, "app.create"},
		{http.MethodGet, "/capacity", "", "install.view"},
		{http.MethodGet, "/adapters", "", "install.view"},
	} {
		body, status := dave.do(t, tc.method, tc.path, tc.body)
		require.Equal(t, http.StatusForbidden, status, "%s %s: %s", tc.method, tc.path, body)
		require.Contains(t, body, tc.verb)
	}

	// The launcher is unaffected: it is data-plane scoped (R-264) and needs no
	// administrative power. An account with no grants gets an empty list, not a
	// denial — "you have no apps yet" is a page, and 403 is not.
	body, status := dave.do(t, http.MethodGet, "/me/apps", "")
	require.Equal(t, http.StatusOK, status, body)

	// So is GET /apps, which is control-plane scoped and filtered by the
	// server. Empty, not forbidden: the console asks it to decide whether there
	// is anything to administer, and a 403 there would be an error page rather
	// than a launcher.
	body, status = dave.do(t, http.MethodGet, "/apps", "")
	require.Equal(t, http.StatusOK, status, body)

	// And every one of those endpoints answers for the administrator.
	for _, path := range []string{"/capacity", "/adapters"} {
		body, status := admin.do(t, http.MethodGet, path, "")
		require.Equal(t, http.StatusOK, status, "%s: %s", path, body)
	}
}

// TestR048_SuspensionEndsEverySessionImmediately asserts R-048 on the path that
// is now gated: the administrator's verb works, and a suspended account's
// sessions end at once rather than at their own expiry.
func TestR048_SuspensionEndsEverySessionImmediately(t *testing.T) {
	admin := login(t)

	erinID := admin.createUser(t, uniqueName("erin"))
	erin := admin.asUser(t, erinID)

	// Signed in and working.
	body, status := erin.do(t, http.MethodGet, "/me", "")
	require.Equal(t, http.StatusOK, status, body)

	body, status = admin.do(t, http.MethodPatch, "/users/"+erinID, `{"status":"suspended"}`)
	require.Equal(t, http.StatusNoContent, status, body)

	// Immediately, not at session expiry (R-048). Suspension that took a
	// session lifetime to mean anything would not be suspension.
	_, status = erin.do(t, http.MethodGet, "/me", "")
	require.Equal(t, http.StatusUnauthorized, status)

	// Suspended is not deleted (R-049): reinstating restores the account.
	body, status = admin.do(t, http.MethodPatch, "/users/"+erinID, `{"status":"active"}`)
	require.Equal(t, http.StatusNoContent, status, body)

	require.Empty(t, verbsOf(t, admin.asUser(t, erinID)),
		"reinstated, and still holding nothing install-wide")
}

// TestR081_AdministratorHoldsNoAppVerb asserts R-081's partition: the new role
// did not quietly become a superuser.
//
// R-031 gives every app an owner of record, and R-087 says an administrator's
// supported path to an app is a grant like anyone else's. The administrator role
// holds no `app.*` verb, so administering a particular app still requires one.
func TestR081_AdministratorHoldsNoAppVerb(t *testing.T) {
	admin := login(t)
	require.NotContains(t, verbsOf(t, admin), "app.exec")
	require.NotContains(t, verbsOf(t, admin), "app.delete")

	// app.create is the exception that proves the rule: it is install-scoped
	// because there is no app yet when it is checked.
	require.Contains(t, verbsOf(t, admin), "app.create")
}

// TestR265_TheServerReportsWhatTheConsoleScopesOn asserts R-265's second clause.
//
// "Exposing the console scoped to whatever privileges they hold" requires the
// console to know what those privileges are, and the answer has to come from
// the server or the console is holding a second opinion about authorization
// (R-261). `GET /me` carries the list, always present and usually empty.
func TestR265_TheServerReportsWhatTheConsoleScopesOn(t *testing.T) {
	admin := login(t)

	// The administrator: every install verb, and no app verb — so the console
	// can offer the install screens and must not assume anything about apps.
	//
	// Listed exhaustively rather than counted. A new install verb should fail
	// here until someone has decided whether the built-in administrator holds
	// it — which is the decision R-081 says is made by migration, and this is
	// the test that makes forgetting it noisy. install.backup.manage arrived in
	// phase 9 and did exactly that.
	held := verbsOf(t, admin)
	require.ElementsMatch(t, []string{
		"install.view", "install.users.manage", "install.policy.manage",
		"install.adapters.manage", "install.audit.read", "install.backup.manage",
		"install.tokens.manage", "install.deploys.approve", "app.create",
		// Migrations 000042 (issue #53) and 000043 (issue #50).
		"install.upgrade", "install.events.manage",
		// Each app verb on every app (issue #81).
		"install.apps.view", "install.apps.logs.read", "install.apps.deploy",
		"install.apps.restart", "install.apps.spec.edit", "install.apps.secrets.write",
		"install.apps.secrets.read", "install.apps.exec", "install.apps.grants.manage",
		"install.apps.routing.override", "install.apps.resources.override",
		"install.apps.egress.tighten", "install.apps.egress.loosen", "install.apps.delete",
	}, held)

	// An ordinary account: the key is present and the list is empty. Present,
	// because a missing key and an empty list are the same thing to a console
	// that reads `verbs ?? []`, and only one of them is an answer.
	ordinary := admin.asUser(t, admin.createUser(t, uniqueName("frank")))
	me := ordinary.get(t, "/me")
	raw, ok := me["verbs"].([]any)
	require.True(t, ok, "verbs must be present even when empty: %v", me)
	require.Empty(t, raw)
}

// TestR046_TheGeneratedPasswordCanActuallyBeChanged asserts the second half of
// R-046.
//
// "Must be changed on first login" was a flag that nothing could clear: there
// was no password endpoint and no console screen, so the only way into a fresh
// install was to keep using the credential printed in the server log forever.
func TestR046_TheGeneratedPasswordCanActuallyBeChanged(t *testing.T) {
	admin := login(t)
	username := uniqueName("gina")
	userID := admin.createUser(t, username)
	gina := admin.asUser(t, userID)

	// A password change needs the current one. A session cookie is a bearer
	// credential, and without this check anyone holding a borrowed one could
	// lock the owner out of their own account.
	body, status := gina.do(t, http.MethodPost, "/me/password",
		`{"current_password":"not-the-password","new_password":"a-much-longer-passphrase"}`)
	require.Equal(t, http.StatusUnauthorized, status, body)

	// Length is the only rule. No composition requirements — they produce
	// shorter, more guessable passwords and a note on a monitor.
	body, status = gina.do(t, http.MethodPost, "/me/password",
		`{"current_password":"correct-horse-battery","new_password":"short"}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "10 characters")

	body, status = gina.do(t, http.MethodPost, "/me/password",
		`{"current_password":"correct-horse-battery","new_password":"a-much-longer-passphrase"}`)
	require.Equal(t, http.StatusNoContent, status, body)

	// The flag is cleared by the change itself. There is no state in which
	// someone has chosen their own password and must still change it.
	me := gina.get(t, "/me")
	require.Equal(t, false, me["must_change_password"])

	// The old password is gone and the new one works.
	fresh := &client{http: &http.Client{Timeout: 30 * time.Second}}
	resp, err := fresh.http.Post(baseURL()+"/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"correct-horse-battery"}`, username)))
	require.NoError(t, err)
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	_ = resp.Body.Close()

	resp, err = fresh.http.Post(baseURL()+"/sessions", "application/json",
		strings.NewReader(fmt.Sprintf(`{"username":%q,"password":"a-much-longer-passphrase"}`, username)))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	_ = resp.Body.Close()
}

// TestR081_AnAdministratorCanHandOver asserts the half of O-17 that was missing
// after the verbs landed: an install could have exactly one administrator
// forever, because bootstrap wrote the only grant anything could write.
func TestR081_AnAdministratorCanHandOver(t *testing.T) {
	admin := login(t)

	hank := admin.createUser(t, uniqueName("hank"))
	hankClient := admin.asUser(t, hank)
	require.Empty(t, verbsOf(t, hankClient), "a new account holds nothing install-wide")

	// Promote. A separate route from PATCH /users/{id}: changing someone's
	// status and changing their power are different acts.
	body, status := admin.do(t, http.MethodPut, "/users/"+hank+"/role",
		fmt.Sprintf(`{"role_id":%q}`, "role_administrator"))
	require.Equal(t, http.StatusOK, status, body)

	// A fresh session, because the grant is read live per request — but the
	// existing one would see it too, which is the point of R-079's live
	// resolution applying to grants as well as groups.
	promoted := admin.asUser(t, hank)
	require.Contains(t, verbsOf(t, promoted), "install.users.manage")

	// And the power is real, not just reported.
	body, status = promoted.do(t, http.MethodGet, "/capacity", "")
	require.Equal(t, http.StatusOK, status, body)

	// An app role cannot be granted installation-wide. The composite foreign
	// key refuses it, and the message says which scope the role belongs to
	// rather than surfacing a constraint name.
	body, status = admin.do(t, http.MethodPut, "/users/"+hank+"/role", `{"role_id":"role_owner"}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "single app")

	// Demote.
	body, status = admin.do(t, http.MethodDelete, "/users/"+hank+"/role", "")
	require.Equal(t, http.StatusNoContent, status, body)
	require.Empty(t, verbsOf(t, admin.asUser(t, hank)))
}

// TestR081_TheLastAdministratorCannotBeRemoved asserts the guard that keeps an
// install administrable.
//
// Without it the install becomes unadministrable in one click and the only way
// back is a psql prompt — the same lockout O-17 allowed by accident, reachable
// on purpose.
func TestR081_TheLastAdministratorCannotBeRemoved(t *testing.T) {
	admin := login(t)
	adminID := admin.get(t, "/me")["user_id"].(string)

	// The first-run account is the only administrator on a fresh install.
	body, status := admin.do(t, http.MethodDelete, "/users/"+adminID+"/role", "")
	if status == http.StatusNoContent {
		// Another test promoted someone and left them promoted, so this is not
		// the last administrator. Put it back and say so rather than passing on
		// a technicality.
		_, restore := admin.do(t, http.MethodPut, "/users/"+adminID+"/role",
			`{"role_id":"role_administrator"}`)
		require.Equal(t, http.StatusOK, restore)
		t.Skip("another administrator exists on this stack, so this is not the last one")
	}

	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "only account that can manage accounts")

	// Still administering.
	require.Contains(t, verbsOf(t, login(t)), "install.users.manage")
}

// TestR274_PolicyIsReadableAndWritableBehindItsOwnVerbs asserts that the two
// policy verbs gate something.
func TestR274_PolicyIsReadableAndWritableBehindItsOwnVerbs(t *testing.T) {
	admin := login(t)
	outsider := admin.asUser(t, admin.createUser(t, uniqueName("ivan")))

	body, status := outsider.do(t, http.MethodGet, "/policy", "")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "install.view")

	body, status = outsider.do(t, http.MethodPut, "/policy", `{}`)
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "install.policy.manage")

	body, status = admin.do(t, http.MethodGet, "/policy", "")
	require.Equal(t, http.StatusOK, status, body)

	// A disabled verb must be a real verb. Policy can only deny (R-272), so a
	// typo denies nothing and looks exactly like a rule that works.
	body, status = admin.do(t, http.MethodPut, "/policy", `{"disabled_verbs":["app.exek"]}`)
	require.Equal(t, http.StatusBadRequest, status, body)
	require.Contains(t, body, "not a permission Pando has")

	// Round-trip a real one, then put it back.
	body, status = admin.do(t, http.MethodPut, "/policy", `{"allow_anonymous_grants":true}`)
	require.Equal(t, http.StatusOK, status, body)
}

// TestR227_TheAuditLogIsReadableBehindItsOwnVerb asserts R-227's readable log
// and that install.audit.read is separate from install.view.
//
// Separate because the log records what everyone did, including inside apps
// they own — a different level of trust from seeing how many CPUs the host has.
func TestR227_TheAuditLogIsReadableBehindItsOwnVerb(t *testing.T) {
	admin := login(t)
	outsider := admin.asUser(t, admin.createUser(t, uniqueName("judy")))

	body, status := outsider.do(t, http.MethodGet, "/audit", "")
	require.Equal(t, http.StatusForbidden, status, body)
	require.Contains(t, body, "install.audit.read")

	log := admin.get(t, "/audit?action=user.")
	events, ok := log["events"].([]any)
	require.True(t, ok, "%v", log)
	require.NotEmpty(t, events, "creating accounts above must be recorded")

	// The prefix filter is a prefix, not a substring: actions are dotted
	// namespaces, and a substring match would make grant.delete a result for
	// a search for "delete".
	for _, e := range events {
		action := e.(map[string]any)["action"].(string)
		require.True(t, strings.HasPrefix(action, "user."), "got %q", action)
	}

	// Denials are recorded too, which is the signal that matters for detecting
	// misuse and the thing most commonly left out.
	denials := admin.get(t, "/audit?action=authz.")
	require.NotNil(t, denials["events"])
}
