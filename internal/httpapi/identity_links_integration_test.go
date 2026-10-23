//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR078_AProvidersGroupCanStandInForAPandoGroup asserts the mapping half
// of R-078: a group the provider pushes, linked to a Pando group, gives its
// members that group's access live — and unlinking takes it away.
func TestR078_AProvidersGroupCanStandInForAPandoGroup(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "wiki")
	_, okta := scimProvider(t, i, admin, "Okta", nil)

	var team struct {
		ID string `json:"id"`
	}
	i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "Wiki editors"}).JSON(t, &team)
	require.Equal(t, http.StatusCreated, i.do(admin, http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "group", "principal_id": team.ID, "role_id": "role_operator",
	}).Code)

	var eve struct {
		ID string `json:"id"`
	}
	okta.do(http.MethodPost, "/Users", map[string]any{"userName": "eve", "externalId": "00ueve", "active": true}).JSON(t, &eve)
	var eng struct {
		ID string `json:"id"`
	}
	okta.do(http.MethodPost, "/Groups", map[string]any{"displayName": "Engineering",
		"members": []map[string]string{{"value": eve.ID}}}).JSON(t, &eng)

	sess, err := i.Sessions.Create(t.Context(), eve.ID, "idp_local", 3600e9, "test", "")
	require.NoError(t, err)
	as := &session{cookie: sess.Cookie.Reveal()}
	require.Equal(t, http.StatusNotFound, i.do(as, http.MethodGet, "/apps/"+appID, nil).Code)

	// The link runs only from a synced group to a Pando one.
	backwards := i.do(admin, http.MethodPut, "/groups/"+eng.ID+"/links/"+team.ID, nil)
	require.Equal(t, http.StatusBadRequest, backwards.Code, backwards.String())

	linked := i.do(admin, http.MethodPut, "/groups/"+team.ID+"/links/"+eng.ID, nil)
	require.Equal(t, http.StatusNoContent, linked.Code, linked.String())
	require.Equal(t, http.StatusOK, i.do(as, http.MethodGet, "/apps/"+appID, nil).Code,
		"Engineering's members are Wiki editors now")

	groups := i.do(admin, http.MethodGet, "/groups", nil)
	require.Contains(t, groups.String(), `"linked_from":["`+eng.ID+`"]`)
	require.Contains(t, groups.String(), `"links_to":["`+team.ID+`"]`)

	// Okta takes Eve out of Engineering, and with it the access (R-079).
	okta.do(http.MethodPatch, "/Groups/"+eng.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"remove","path":"members[value eq \"`+eve.ID+`\"]"}]}`)
	require.Equal(t, http.StatusNotFound, i.do(as, http.MethodGet, "/apps/"+appID, nil).Code)

	okta.do(http.MethodPatch, "/Groups/"+eng.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"add","path":"members","value":[{"value":"`+eve.ID+`"}]}]}`)
	require.Equal(t, http.StatusOK, i.do(as, http.MethodGet, "/apps/"+appID, nil).Code)
	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodDelete, "/groups/"+team.ID+"/links/"+eng.ID, nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(as, http.MethodGet, "/apps/"+appID, nil).Code)
}

// TestO1_LinkingAliasesAndNeverMerges asserts O-1's resolution: moving an
// identity to another account keeps the account it left — suspended, as an
// alias, never deleted — because its ID may be an assertion subject an app
// holds data under (R-054).
func TestO1_LinkingAliasesAndNeverMerges(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	providerID, okta := scimProvider(t, i, admin, "Okta", nil)

	var provisioned struct {
		ID string `json:"id"`
	}
	okta.do(http.MethodPost, "/Users", map[string]any{"userName": "sam", "externalId": "00usam", "active": true}).JSON(t, &provisioned)
	sam := i.user("sam")
	samID := i.userID(sam)

	taken := i.do(admin, http.MethodPost, "/users/"+samID+"/identities",
		map[string]any{"adapter_id": providerID, "external_id": "00usam"})
	require.Equal(t, http.StatusBadRequest, taken.Code, taken.String())
	require.Contains(t, taken.String(), "replace_account")

	moved := i.do(admin, http.MethodPost, "/users/"+samID+"/identities",
		map[string]any{"adapter_id": providerID, "external_id": "00usam", "replace_account": true})
	require.Equal(t, http.StatusCreated, moved.Code, moved.String())
	require.Contains(t, moved.String(), `"aliased_user_id":"`+provisioned.ID+`"`)

	alias := i.do(admin, http.MethodGet, "/users/"+provisioned.ID, nil)
	require.Equal(t, http.StatusOK, alias.Code, "the alias is kept, not deleted")
	require.Contains(t, alias.String(), `"status":"suspended"`)
	require.Contains(t, alias.String(), `"alias_of":"`+samID+`"`)
	require.NotContains(t, i.do(admin, http.MethodGet, "/users", nil).String(), provisioned.ID,
		"an alias is not listed as an account of its own")

	reactivate := i.do(admin, http.MethodPatch, "/users/"+provisioned.ID, map[string]any{"status": "active"})
	require.Equal(t, http.StatusBadRequest, reactivate.Code, reactivate.String())

	ids := i.do(sam, http.MethodGet, "/users/"+samID+"/identities", nil)
	require.Contains(t, ids.String(), `"external_id":"00usam"`, "an account sees its own identities")

	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodDelete,
		"/users/"+samID+"/identities?adapter_id="+providerID+"&external_id=00usam", nil).Code)
	require.NotContains(t, i.do(admin, http.MethodGet, "/users/"+samID+"/identities", nil).String(), "00usam")

	// Linking is administration.
	refused := i.do(sam, http.MethodPost, "/users/"+samID+"/identities",
		map[string]any{"adapter_id": providerID, "external_id": "00usam"})
	require.Equal(t, http.StatusForbidden, refused.Code, refused.String())
}

// TestR190_AProvidersSecretIsNeverStoredInTheClear asserts R-190 for identity
// providers: a client secret in the plain settings is refused by the API and
// by the database, is stored sealed, and is never returned.
func TestR190_AProvidersSecretIsNeverStoredInTheClear(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	inline := i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Okta",
		"config": map[string]any{"issuer": "https://idp.example.com", "client_id": "pando", "client_secret": "hunter2"},
	})
	require.Equal(t, http.StatusBadRequest, inline.Code, inline.String())
	require.Contains(t, inline.String(), "credentials")

	_, err := i.db.Exec(t.Context(), `
		INSERT INTO identity_adapters (id, kind, name, config)
		VALUES ('idp_x', 'oidc', 'X', '{"client_secret":"hunter2"}')`)
	require.Error(t, err, "the database refuses it too")

	created := i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Okta",
		"config":      map[string]any{"issuer": "https://idp.example.com", "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "hunter2"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	require.NotContains(t, created.String(), "hunter2")
	var stored []byte
	require.NoError(t, i.db.QueryRow(t.Context(),
		`SELECT ciphertext FROM identity_adapter_credentials WHERE field = 'client_secret'`).Scan(&stored))
	require.NotContains(t, string(stored), "hunter2")

	unknown := i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "ldap", "name": "LDAP", "config": map[string]any{},
	})
	require.Equal(t, http.StatusBadRequest, unknown.Code, unknown.String())

	// Only an administrator of adapters may add one.
	someone := i.user("someone")
	require.Equal(t, http.StatusForbidden, i.do(someone, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Mine", "config": map[string]any{"issuer": "https://x.example.com", "client_id": "x"},
	}).Code)
	require.Equal(t, http.StatusForbidden, i.do(someone, http.MethodGet, "/identity-providers", nil).Code)
}

// TestR045_AProviderSomeoneSignedInThroughIsKept asserts that a provider with
// accounts cannot be deleted — its identities would be free for reuse — while
// an unused one can.
func TestR045_AProviderSomeoneSignedInThroughIsKept(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	used, okta := scimProvider(t, i, admin, "Okta", nil)
	okta.do(http.MethodPost, "/Users", map[string]any{"userName": "x", "externalId": "00ux", "active": true})

	refused := i.do(admin, http.MethodDelete, "/identity-providers/"+used, nil)
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "Turn it off instead")

	var unused struct {
		ID string `json:"id"`
	}
	i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "saml", "name": "Spare",
		"config": map[string]any{"idp_metadata_url": "https://idp.example.com/metadata"},
	}).JSON(t, &unused)
	require.Equal(t, http.StatusNoContent, i.do(admin, http.MethodDelete, "/identity-providers/"+unused.ID, nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodDelete, "/identity-providers/idp_local", nil).Code)
}
