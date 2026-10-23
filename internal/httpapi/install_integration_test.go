//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Paging is by cursor rather than offset: the log is append-only with
// monotonic IDs, so "before this ID" is a stable boundary in a way an offset is
// not — with an offset, events arriving between requests shift every later page.
func TestTheAuditLogPagesByCursor(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	for n := range 4 {
		i.createApp(admin, fmt.Sprintf("app-%d", n))
	}

	first := i.do(admin, http.MethodGet, "/audit?limit=2", nil)
	require.Equal(t, http.StatusOK, first.Code, first.String())

	var page struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
		NextBefore string `json:"next_before"`
	}
	first.JSON(t, &page)
	require.Len(t, page.Events, 2)
	require.NotEmpty(t, page.NextBefore, "a full page names where the next one starts")

	second := i.do(admin, http.MethodGet, "/audit?limit=2&before="+page.NextBefore, nil)
	require.Equal(t, http.StatusOK, second.Code, second.String())

	var next struct {
		Events []struct {
			ID int64 `json:"id"`
		} `json:"events"`
	}
	second.JSON(t, &next)
	require.NotEmpty(t, next.Events)
	require.Less(t, next.Events[0].ID, page.Events[1].ID, "the second page is older")
}

func TestABadAuditCursorOrLimitIsRefusedReadably(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	for _, query := range []string{"?limit=0", "?limit=-1", "?limit=many", "?before=0", "?before=soon"} {
		got := i.do(admin, http.MethodGet, "/audit"+query, nil)
		require.Equal(t, http.StatusBadRequest, got.Code, "%s: %s", query, got.String())
		require.Equal(t, string(errs.ValidInvalid), got.ErrorCode(), query)
	}
}

// --- host policy -----------------------------------------------------------

// R-274: seeing the rules you work under is not the same privilege as changing
// them. Read is behind install.view; write is behind install.policy.manage.
func TestR274_ReadingPolicyAndChangingItAreDifferentPrivileges(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")

	read := i.do(admin, http.MethodGet, "/policy", nil)
	require.Equal(t, http.StatusOK, read.Code, read.String())

	denied := i.do(other, http.MethodPut, "/policy", map[string]any{"disabled_verbs": []string{"app.exec"}})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	// And the policy is unchanged.
	after := i.do(admin, http.MethodGet, "/policy", nil)
	require.NotContains(t, after.String(), "app.exec")
}

// R-270: Pando ships permissive and is narrowed deliberately.
func TestR270_AFreshInstallShipsPermissive(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	got := i.do(i.admin(), http.MethodGet, "/policy", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var doc map[string]any
	got.JSON(t, &doc)
	require.Empty(t, doc["source_allowlist"], "apps may be created from anywhere")
	require.Empty(t, doc["disabled_verbs"], "nothing is turned off install-wide")
}

// R-085: host policy may disable exec install-wide, and it denies the owner too
// — policy is a floor evaluated before grants, not something a grant outranks.
func TestR085_DisablingExecInstallWideDeniesTheAppsOwner(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	saved := i.do(admin, http.MethodPut, "/policy", map[string]any{
		"disabled_verbs": []string{string(authz.AppExec)},
	})
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	// The administrator owns this app and is still refused.
	got := i.do(admin, http.MethodGet, "/apps/"+id+"/exec", nil)
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
	if got.ErrorCode() != "" {
		require.Equal(t, string(errs.PolicyExecDisabled), got.ErrorCode(), got.String())
	}
}

// TestR272_APolicyRefusalDoesNotRevealAnAppTheCallerCannotSee asserts that
// host policy, evaluated before grants (R-272), does not turn into an
// existence oracle: somebody with no grant on an app is told it does not
// exist whether or not policy also forbids the action, exactly as for an app
// that really does not exist. Someone who can see it is still told why.
func TestR272_APolicyRefusalDoesNotRevealAnAppTheCallerCannotSee(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	bob := i.user("bob")

	saved := i.do(admin, http.MethodPut, "/policy", map[string]any{
		"disabled_verbs": []string{string(authz.AppRestart)},
	})
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	existing := i.do(bob, http.MethodPost, "/apps/"+appID+"/restart", nil)
	missing := i.do(bob, http.MethodPost, "/apps/"+id.New(id.App)+"/restart", nil)
	require.Equal(t, http.StatusNotFound, existing.Code, existing.String())
	require.Equal(t, string(errs.NotFound), existing.ErrorCode())
	require.Equal(t, missing.Code, existing.Code, "an app bob cannot see answers as one that does not exist")
	require.Equal(t, missing.ErrorCode(), existing.ErrorCode())

	// The owner can see it, so the owner is told it is policy.
	own := i.do(admin, http.MethodPost, "/apps/"+appID+"/restart", nil)
	require.Equal(t, string(errs.PolicyExecDisabled), own.ErrorCode(), own.String())
}

// R-092: the source allowlist is evaluated before anything touches disk.
func TestR092_ASourceOutsideTheAllowlistIsRefusedAtCreation(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	saved := i.do(admin, http.MethodPut, "/policy", map[string]any{
		"source_allowlist": []string{"github.com"},
	})
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	blocked := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name":   "elsewhere",
		"source": map[string]string{"type": "git", "url": "https://gitlab.com/acme/notes"},
	})
	require.Equal(t, string(errs.PolicySourceNotAllowed), blocked.ErrorCode(), blocked.String())

	allowed := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name":   "approved",
		"source": map[string]string{"type": "git", "url": "https://github.com/acme/notes"},
	})
	require.Equal(t, http.StatusAccepted, allowed.Code, allowed.String())
}

// TestR092_AnAddressPandoWillNotFetchIsRefusedAtCreation asserts R-092's
// schemes and R-190's rule on credentials where a person gives the address:
// a path on Pando's disk, git://, and an address with a token in it are
// refused before an app exists, rather than created and left to fail at
// detection (2026-10-16 assessment, finding 3). Where an address resolves to
// is the fetch's to refuse (source.TestR092_AFetchOfAMetadataAddressIsRefusedInWords).
func TestR092_AnAddressPandoWillNotFetchIsRefusedAtCreation(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	for _, addr := range []string{
		"file:///etc/hostname",
		"/etc/hostname",
		"git://github.com/acme/notes.git",
		"https://ghp_abc123@github.com/acme/notes",
	} {
		got := i.do(admin, http.MethodPost, "/apps", map[string]any{
			"name":   "refused",
			"source": map[string]string{"type": "git", "url": addr},
		})
		require.Equal(t, string(errs.ValidInvalid), got.ErrorCode(), "%s: %s", addr, got.String())
	}
}

// TestR092_ASpecCannotMoveAnAppToAnAddressPandoWillNotFetch asserts the same
// rule for a spec written whole: POST /specs that changes the repository to a
// path on Pando's disk is refused, as creating an app with it would be.
func TestR092_ASpecCannotMoveAnAppToAnAddressPandoWillNotFetch(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "moved")

	body := minimalSpec()
	body["source"] = map[string]any{"type": "git", "url": "file:///etc"}
	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/specs", body)
	require.Equal(t, string(errs.ValidInvalid), got.ErrorCode(), got.String())
	require.Contains(t, got.String(), "https, http or ssh")
}

// TestR092_ASpecCannotTurnAnAddressIntoARepositorysByChangingItsType asserts
// that the check follows the source's type as well as its address: an app
// whose stored address is a local path, kept as it is while the source
// becomes a repository, is refused.
func TestR092_ASpecCannotTurnAnAddressIntoARepositorysByChangingItsType(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/apps", map[string]any{
		"name":   "retyped",
		"source": map[string]string{"type": "image", "image": "nginx:alpine", "url": "file:///etc"},
	})
	require.Contains(t, []int{http.StatusCreated, http.StatusAccepted}, created.Code, created.String())
	var app struct {
		ID     string `json:"id"`
		Source struct {
			URL string `json:"url"`
		} `json:"source"`
	}
	created.JSON(t, &app)
	require.Equal(t, "file:///etc", app.Source.URL, "the stored address the spec keeps")

	body := minimalSpec()
	body["source"] = map[string]any{"type": "git", "url": "file:///etc"}
	got := i.do(admin, http.MethodPost, "/apps/"+app.ID+"/specs", body)
	require.Equal(t, string(errs.ValidInvalid), got.ErrorCode(), got.String())
	require.Contains(t, got.String(), "https, http or ssh")
}

// Replaces rather than merges: a merge would make it impossible to remove a rule.
func TestSavingPolicyReplacesTheDocument(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, "/policy", map[string]any{
		"source_allowlist": []string{"github.com"},
		"disabled_verbs":   []string{string(authz.AppExec)},
	}).Code)

	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, "/policy", map[string]any{
		"source_allowlist": []string{"github.com"},
	}).Code)

	got := i.do(admin, http.MethodGet, "/policy", nil)
	var doc map[string]any
	got.JSON(t, &doc)
	require.Empty(t, doc["disabled_verbs"], "a rule left out of the document is removed")
}

// Behind the write verb rather than install.view: the body is a policy someone
// is composing, and answering "which apps does this break" for anyone who can
// read policy hands them a probe for the whole install's shape.
func TestPolicyPreviewIsBehindTheWriteVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")

	denied := i.do(other, http.MethodPost, "/policy/preview", map[string]any{})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	allowed := i.do(admin, http.MethodPost, "/policy/preview", map[string]any{
		"disabled_verbs": []string{string(authz.AppExec)},
	})
	require.Equal(t, http.StatusOK, allowed.Code, allowed.String())
	require.Contains(t, allowed.String(), "violations")
}

// --- the audit log ---------------------------------------------------------

// R-227: the audit log has its own verb, because it records what everyone did,
// including inside apps they own.
func TestR227_TheAuditLogIsBehindItsOwnVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	other := i.user("ordinary")

	denied := i.do(other, http.MethodGet, "/audit", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	allowed := i.do(i.admin(), http.MethodGet, "/audit", nil)
	require.Equal(t, http.StatusOK, allowed.Code, allowed.String())
}

// Every action lands in the log under the principal that took it.
func TestActionsAreRecordedInTheAuditLog(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodGet, "/audit", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Events []struct {
			Action      string `json:"action"`
			PrincipalID string `json:"principal_id"`
			AppID       string `json:"app_id"`
		} `json:"events"`
	}
	got.JSON(t, &body)
	require.NotEmpty(t, body.Events)

	var sawCreate bool
	for _, e := range body.Events {
		if e.Action == "app.create" && e.AppID == id {
			sawCreate = true
			require.Equal(t, i.AdminID, e.PrincipalID)
		}
	}
	require.True(t, sawCreate, "creating an app is recorded: %s", got.String())
}

func TestTheAuditLogIsFilterable(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	byAction := i.do(admin, http.MethodGet, "/audit?action=app.create", nil)
	require.Equal(t, http.StatusOK, byAction.Code, byAction.String())
	require.Contains(t, byAction.String(), "app.create")

	byApp := i.do(admin, http.MethodGet, "/audit?app_id="+id, nil)
	require.Equal(t, http.StatusOK, byApp.Code, byApp.String())

	byPrincipal := i.do(admin, http.MethodGet, "/audit?principal_id="+i.AdminID, nil)
	require.Equal(t, http.StatusOK, byPrincipal.Code, byPrincipal.String())

	limited := i.do(admin, http.MethodGet, "/audit?limit=1", nil)
	require.Equal(t, http.StatusOK, limited.Code)
	var body struct {
		Events []map[string]any `json:"events"`
	}
	limited.JSON(t, &body)
	require.LessOrEqual(t, len(body.Events), 1)

	// A filter that matches nothing is an empty page, not an error — which is
	// the answer a search UI produces most often.
	none := i.do(admin, http.MethodGet, "/audit?action=nothing.happened", nil)
	require.Equal(t, http.StatusOK, none.Code, none.String())

	var empty struct {
		Events     []map[string]any `json:"events"`
		NextBefore string           `json:"next_before"`
	}
	none.JSON(t, &empty)
	require.Empty(t, empty.Events)
	require.Empty(t, empty.NextBefore, "there is no next page of nothing")
}

// A wildcard in a user-supplied prefix must not become a wildcard in the query.
func TestAnAuditFilterCannotSmuggleAWildcard(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.createApp(admin, "notes")

	got := i.do(admin, http.MethodGet, "/audit?action=%25", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Events []map[string]any `json:"events"`
	}
	got.JSON(t, &body)
	require.Empty(t, body.Events, "a literal %% matches no action, rather than every one")
}

// --- the inventory ---------------------------------------------------------

// GET /adapters returns live capabilities, not stored config, so the console
// can grey out choices that would fail at plan time (R-254).
func TestTheAdapterInventoryIsBehindInstallView(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	other := i.user("ordinary")

	denied := i.do(other, http.MethodGet, "/adapters", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	// The inventory is what the install has *configured* — the adapter_configs
	// rows — rather than what happens to be registered in this process. A test
	// install registers its adapters in memory and stores none, so the list is
	// empty and that is the honest answer.
	allowed := i.do(i.admin(), http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, allowed.Code, allowed.String())
	require.Contains(t, allowed.String(), "adapters")
}

func TestCapacityIsBehindInstallView(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	denied := i.do(i.user("ordinary"), http.MethodGet, "/capacity", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())

	// With no runtime configured this reports what it can rather than failing:
	// R-243 says capacity is adapter-reported, and there is no adapter to ask.
	allowed := i.do(i.admin(), http.MethodGet, "/capacity", nil)
	require.NotEqual(t, http.StatusInternalServerError, allowed.Code, allowed.String())
}

func TestRegisteringAnAdapterIsAdministration(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	denied := i.do(i.user("ordinary"), http.MethodPost, "/adapters", map[string]any{
		"id": "rt_evil", "category": "runtime", "kind": "docker",
	})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())
}

// TestR253_AnAdapterSavedAfterStartupIsMarkedUntilARestart asserts R-253's
// consequence as the API reports it: a new adapter is saved but not running,
// and says so, rather than reading as reachable.
func TestR253_AnAdapterSavedAfterStartupIsMarkedUntilARestart(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic", "name": "Anthropic",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.Contains(t, listed.String(), `"restart_needed":true`)
	require.Contains(t, listed.String(), `"pending_restart":true`)
	require.Contains(t, listed.String(), `"started_at":`)
}

// TestR253_RestartingIsForWhoeverMayChangeAdapters asserts that POST /restart
// — how a saved adapter is put into effect — is behind the same verb as saving
// one, and is audited.
func TestR253_RestartingIsForWhoeverMayChangeAdapters(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	denied := i.do(i.user("ordinary"), http.MethodPost, "/restart", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())
	require.Zero(t, i.Restarts.Load(), "a refused restart does not restart")

	admin := i.admin()
	accepted := i.do(admin, http.MethodPost, "/restart", nil)
	require.Equal(t, http.StatusAccepted, accepted.Code, accepted.String())
	require.Equal(t, int32(1), i.Restarts.Load())

	audited := i.do(admin, http.MethodGet, "/audit?action=install.restart", nil)
	require.Equal(t, http.StatusOK, audited.Code, audited.String())
	require.Contains(t, audited.String(), `"install.restart"`)
}

// TestR190_AnAdapterCredentialGoesInEncryptedAndNeverComesBack asserts O-20's
// resolution end to end: sent as a credential, stored as ciphertext, listed by
// name, never returned.
func TestR190_AnAdapterCredentialGoesInEncryptedAndNeverComesBack(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	const key = "sk-ant-integration-must-not-leak"

	created := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic", "name": "Anthropic",
		"config":      map[string]any{"model": "claude-opus-5"},
		"credentials": map[string]any{"api_key": key},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())

	var ciphertext []byte
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT ciphertext FROM adapter_credentials WHERE adapter_id = 'ai_anthropic' AND field = 'api_key'`).
		Scan(&ciphertext))
	require.NotContains(t, string(ciphertext), key)

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.NotContains(t, listed.String(), key)
	require.Contains(t, listed.String(), `"credentials_set":["api_key"]`)

	audited := i.do(admin, http.MethodGet, "/audit?action=adapter.configure", nil)
	require.NotContains(t, audited.String(), key, "the audit event names the field, not the value")
}

// TestO20_TheAPIRefusesACredentialInPlainConfiguration asserts O-20.
func TestO20_TheAPIRefusesACredentialInPlainConfiguration(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	refused := i.do(i.admin(), http.MethodPost, "/adapters", map[string]any{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic",
		"config": map[string]any{"api_key": "sk-ant-plain"},
	})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "credentials")
	require.NotContains(t, refused.String(), "sk-ant-plain")
}

// O-20: a secrets adapter is what would encrypt credentials, so it cannot be
// given any.
func TestO20_ASecretsAdapterCannotBeGivenCredentials(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	refused := i.do(i.admin(), http.MethodPost, "/adapters", map[string]any{
		"id": "sek_other", "category": "secrets", "kind": "local",
		"credentials": map[string]any{"passphrase": "x"},
	})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "encrypt")
}

// --- promotion and demotion ------------------------------------------------

// Changing someone's status and changing their power are different acts, and
// folding them into one body is how a status update quietly becomes a promotion.
func TestPromotionIsItsOwnRouteRatherThanAFieldOnPatch(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/users", map[string]any{
		"username": "deputy", "password": "a-long-enough-password", "display_name": "Deputy",
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var user struct {
		ID string `json:"id"`
	}
	created.JSON(t, &user)

	// A role named in a PATCH body is not a promotion.
	patched := i.do(admin, http.MethodPatch, "/users/"+user.ID, map[string]any{
		"status": "active", "install_role_id": "role_administrator",
	})
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, patched.Code, patched.String())

	after := i.do(admin, http.MethodGet, "/users/"+user.ID, nil)
	var got map[string]any
	after.JSON(t, &got)
	require.Empty(t, got["install_role_id"], "PATCH does not promote")

	// The dedicated route does.
	promoted := i.do(admin, http.MethodPut, "/users/"+user.ID+"/role",
		map[string]any{"role_id": "role_administrator"})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent},
		promoted.Code, promoted.String())

	// And the promoted account can now administer.
	deputy, in := i.signIn("deputy", "a-long-enough-password")
	require.Equal(t, http.StatusOK, in.Code, in.String())
	i.keepPassword(deputy, in, "a-long-enough-password")
	require.Equal(t, http.StatusOK, i.do(deputy, http.MethodGet, "/users", nil).Code)

	demoted := i.do(admin, http.MethodDelete, "/users/"+user.ID+"/role", nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, demoted.Code, demoted.String())

	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound},
		i.do(deputy, http.MethodGet, "/users", nil).Code, "demotion takes effect immediately")
}

// R-080: an app role cannot be granted install-wide.
func TestR080_AnAppRoleCannotBeGrantedAcrossTheInstallation(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	created := i.do(admin, http.MethodPost, "/users", map[string]any{
		"username": "deputy", "password": "a-long-enough-password", "display_name": "Deputy",
	})
	var user struct {
		ID string `json:"id"`
	}
	created.JSON(t, &user)

	got := i.do(admin, http.MethodPut, "/users/"+user.ID+"/role",
		map[string]any{"role_id": "role_app_owner"})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

// --- slots and volumes -----------------------------------------------------

func TestAnAppWithNoSpecHasNoSlots(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/slots", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Slots []map[string]any `json:"slots"`
	}
	got.JSON(t, &body)
	require.Empty(t, body.Slots)
}

func TestSettingASlotNeedsExactlyOneResolution(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodPut, "/apps/"+id+"/slots/DATABASE_URL", map[string]any{})
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	require.NotEqual(t, http.StatusInternalServerError, got.Code)
}

// R-204: volumes survive app deletion, which is why they are their own object.
func TestR204_VolumesAreListedPerApp(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/volumes", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Volumes []map[string]any `json:"volumes"`
	}
	got.JSON(t, &body)
	require.Empty(t, body.Volumes, "a new app has none yet")
}

// R-083: rotating a credential and reading it are different levels of trust, so
// reading a value has its own verb.
func TestR083_ReadingASecretsValueIsItsOwnVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")
	id := i.createApp(admin, "notes")

	set := i.do(admin, http.MethodPut, "/apps/"+id+"/secrets/API_KEY",
		map[string]any{"value": "s3cr3t-value"})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent},
		set.Code, set.String())

	// The list says which are set, and never what they are (R-194).
	listed := i.do(admin, http.MethodGet, "/apps/"+id+"/secrets", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	require.Contains(t, listed.String(), "API_KEY")
	require.NotContains(t, listed.String(), "s3cr3t-value")

	denied := i.do(other, http.MethodGet, "/apps/"+id+"/secrets/API_KEY/value", nil)
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())
}

// R-190/R-191: core never encrypts — it stores what the adapter hands back, and
// what comes out is what went in.
func TestR190_ASecretRoundTripsThroughTheAdapter(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent},
		i.do(admin, http.MethodPut, "/apps/"+id+"/secrets/API_KEY",
			map[string]any{"value": "s3cr3t-value"}).Code)

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/secrets/API_KEY/value", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	var body struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	got.JSON(t, &body)
	require.Equal(t, "API_KEY", body.Key)
	require.Equal(t, "s3cr3t-value", body.Value)
}

func TestASecretCanBeRemoved(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	require.Contains(t, []int{http.StatusOK, http.StatusCreated, http.StatusNoContent},
		i.do(admin, http.MethodPut, "/apps/"+id+"/secrets/API_KEY",
			map[string]any{"value": "s3cr3t-value"}).Code)

	deleted := i.do(admin, http.MethodDelete, "/apps/"+id+"/secrets/API_KEY", nil)
	require.Contains(t, []int{http.StatusOK, http.StatusNoContent}, deleted.Code, deleted.String())

	listed := i.do(admin, http.MethodGet, "/apps/"+id+"/secrets", nil)
	require.NotContains(t, listed.String(), "API_KEY")
}

// --- detection -------------------------------------------------------------

// R-022: nothing re-detects on its own, because a spec that changed under
// someone because a file moved in their repository is a spec they did not write.
func TestR022_DetectionEndpointsAnswerForAnAppThatHasNotDetectedYet(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id := i.createApp(admin, "notes")

	for _, path := range []string{"/detection", "/detection/diff"} {
		got := i.do(admin, http.MethodGet, "/apps/"+id+path, nil)
		require.NotEqual(t, http.StatusInternalServerError, got.Code, "%s: %s", path, got.String())
	}

	// Accepting a proposal that does not exist is refused readably rather than
	// pinning an empty spec.
	accepted := i.do(admin, http.MethodPost, "/apps/"+id+"/detection/accept", map[string]any{})
	require.GreaterOrEqual(t, accepted.Code, 400, accepted.String())
	require.NotEqual(t, http.StatusInternalServerError, accepted.Code)
}

func TestDetectionAnswersAreBehindTheSpecEditVerb(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	other := i.user("ordinary")
	id := i.createApp(admin, "notes")

	denied := i.do(other, http.MethodPost, "/apps/"+id+"/detection/answers",
		map[string]any{"answers": map[string]string{"primary_port": "3000"}})
	require.Contains(t, []int{http.StatusForbidden, http.StatusNotFound}, denied.Code, denied.String())
}
