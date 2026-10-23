//go:build integration

package httpapi_test

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The refusals and the less-travelled paths of external identity (issue #51):
// what each surface says when a request is wrong, and that each says it in a
// sentence someone can act on (R-105).

// oidcProvider adds an OIDC provider that is never contacted.
func oidcProvider(t *testing.T, i *install, admin *session, name string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{
		"kind": "oidc", "name": name,
		"config": map[string]any{"issuer": "https://idp.example.com", "client_id": "pando"},
	}
	for k, v := range extra {
		body[k] = v
	}
	created := i.do(admin, http.MethodPost, "/identity-providers", body)
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)
	return p.ID
}

// TestR043_AProviderIsRefusedWhatItCannotUse asserts that a provider's
// settings are checked when they are saved, not at someone's sign-in.
func TestR043_AProviderIsRefusedWhatItCannotUse(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	for name, body := range map[string]map[string]any{
		"no name":          {"kind": "oidc", "config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x"}},
		"no issuer":        {"kind": "oidc", "name": "A", "config": map[string]any{"client_id": "x"}},
		"bad scim":         {"kind": "oidc", "name": "B", "scim_identity_attribute": "email", "config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x"}},
		"unknown secret":   {"kind": "oidc", "name": "C", "config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x"}, "credentials": map[string]string{"api_key": "x"}},
		"not an object":    {"kind": "oidc", "name": "D", "config": []string{"x"}},
		"saml no metadata": {"kind": "saml", "name": "E", "config": map[string]any{}},
	} {
		got := i.do(admin, http.MethodPost, "/identity-providers", body)
		require.Equal(t, http.StatusBadRequest, got.Code, "%s: %s", name, got.String())
	}

	id := oidcProvider(t, i, admin, "Okta", map[string]any{"credentials": map[string]string{"client_secret": "one"}})
	dup := i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": " okta ", "config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x"},
	})
	require.Equal(t, http.StatusBadRequest, dup.Code)
	require.Contains(t, dup.String(), "already an identity provider called")

	// Changing settings, a secret, and removing a secret.
	changed := i.do(admin, http.MethodPatch, "/identity-providers/"+id, map[string]any{
		"name":             "Okta prod",
		"config":           map[string]any{"issuer": "https://idp2.example.com", "client_id": "pando2", "groups_claim": "roles"},
		"credentials":      map[string]string{"client_secret": "two"},
		"jit_provisioning": true, "link_by_email": true, "scim_identity_attribute": "userName",
	})
	require.Equal(t, http.StatusOK, changed.Code, changed.String())
	require.Contains(t, changed.String(), `"issuer":"https://idp2.example.com"`)
	require.Contains(t, changed.String(), `"scim_identity_attribute":"userName"`)
	require.NotContains(t, changed.String(), "two")
	cleared := i.do(admin, http.MethodPatch, "/identity-providers/"+id, map[string]any{"credentials": map[string]string{"client_secret": ""}})
	require.Equal(t, http.StatusOK, cleared.Code, cleared.String())
	require.Contains(t, cleared.String(), `"credentials":[]`)

	for name, body := range map[string]map[string]any{
		"empty name":    {"name": " "},
		"bad scim":      {"scim_identity_attribute": "id"},
		"bad config":    {"config": map[string]any{"client_id": "x"}},
		"secret in cfg": {"config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x", "client_secret": "x"}},
		"unknown cred":  {"credentials": map[string]string{"password": "x"}},
	} {
		got := i.do(admin, http.MethodPatch, "/identity-providers/"+id, body)
		require.Equal(t, http.StatusBadRequest, got.Code, "%s: %s", name, got.String())
	}
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodPatch, "/identity-providers/idp_nope", map[string]any{"name": "x"}).Code)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodGet, "/identity-providers/idp_nope", nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodDelete, "/identity-providers/idp_nope", nil).Code)

	// Local accounts take a name and nothing else.
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodPatch, "/identity-providers/idp_local",
		map[string]any{"enabled": false}).Code)
	renamed := i.do(admin, http.MethodPatch, "/identity-providers/idp_local", map[string]any{"name": "Pando accounts"})
	require.Equal(t, http.StatusOK, renamed.Code, renamed.String())
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodPost, "/identity-providers/idp_local/scim-token", nil).Code)

	// A provider that does not answer says so, and why.
	unreachable := oidcProvider(t, i, admin, "Down", map[string]any{
		"config": map[string]any{"issuer": "https://127.0.0.1:1", "client_id": "x"},
	})
	check := i.do(admin, http.MethodPost, "/identity-providers/"+unreachable+"/check", nil)
	require.Equal(t, http.StatusOK, check.Code, check.String())
	require.Contains(t, check.String(), `"ok":false`)
	require.Contains(t, check.String(), "discovery document")
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodPost, "/identity-providers/idp_nope/check", nil).Code)

	// OIDC has no SAML metadata to publish.
	md := i.anon(http.MethodGet, "/auth/providers/"+id+"/metadata", nil)
	require.Equal(t, http.StatusNotFound, md.Code)
	require.Contains(t, md.String(), "Only SAML providers")
}

// TestR043_ASignInThatCannotHappenSaysWhy asserts that every way a redirect
// sign-in can fail ends on the sign-in page with a sentence, never a raw error.
func TestR043_ASignInThatCannotHappenSaysWhy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	off := oidcProvider(t, i, admin, "Off", nil)

	location := func(r reply) *url.URL {
		u, err := url.Parse(r.Hdr.Get("Location"))
		require.NoError(t, err)
		return u
	}

	for _, id := range []string{off, "idp_nope", "idp_local"} {
		start := i.anon(http.MethodGet, "/auth/providers/"+id+"/start?next=/x", nil)
		require.Equal(t, http.StatusFound, start.Code, start.String())
		require.Equal(t, "unavailable", location(start).Query().Get("sso_error"))
		require.Equal(t, "/x", location(start).Query().Get("next"))
	}
	fixed := i.anon(http.MethodGet, "/auth/failures/unavailable", nil)
	require.Equal(t, http.StatusOK, fixed.Code)
	require.Contains(t, fixed.String(), "could not start signing you in")
	require.Equal(t, http.StatusNotFound, i.anon(http.MethodGet, "/auth/failures/notaflow", nil).Code)

	// A callback nobody started, or one already spent.
	cb := i.anon(http.MethodGet, "/auth/providers/"+off+"/callback?state=nope&code=x", nil)
	require.Equal(t, http.StatusSeeOther, cb.Code)
	require.Equal(t, "expired", location(cb).Query().Get("sso_error"))

	// A SAML response posted to a provider that is not SAML is refused too.
	post := i.raw(nil, http.MethodPost, "/auth/providers/"+off+"/callback",
		"application/x-www-form-urlencoded", []byte("SAMLResponse=PHg%2B"))
	require.Equal(t, http.StatusSeeOther, post.Code, post.String())
	require.Contains(t, post.Hdr.Get("Location"), "sso_error=")

	complete := i.anon(http.MethodGet, "/auth/complete?code=forged", nil)
	require.Equal(t, http.StatusFound, complete.Code)
	require.Equal(t, "expired", location(complete).Query().Get("sso_error"))

	// A test sign-in's report is its administrator's, and only once it has
	// come back from the provider.
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodGet, "/identity-providers/"+off+"/tests/nope", nil).Code)
	require.Equal(t, http.StatusBadRequest, i.do(admin, http.MethodGet, "/identity-providers/idp_local/test", nil).Code)
}

// TestO1_LinkingIsRefusedWhereItMeansNothing asserts the refusals of linking.
func TestO1_LinkingIsRefusedWhereItMeansNothing(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	p := oidcProvider(t, i, admin, "Okta", nil)
	me := i.AdminID

	for name, c := range map[string]struct {
		user, adapter, external string
		code                    int
	}{
		"no such account":  {"usr_nope", p, "x", http.StatusNotFound},
		"no such provider": {me, "idp_nope", "x", http.StatusNotFound},
		"local":            {me, "idp_local", "x", http.StatusBadRequest},
		"empty identity":   {me, p, " ", http.StatusBadRequest},
	} {
		got := i.do(admin, http.MethodPost, "/users/"+c.user+"/identities",
			map[string]any{"adapter_id": c.adapter, "external_id": c.external})
		require.Equal(t, c.code, got.Code, "%s: %s", name, got.String())
	}
	require.Equal(t, http.StatusCreated, i.do(admin, http.MethodPost, "/users/"+me+"/identities",
		map[string]any{"adapter_id": p, "external_id": "00u1"}).Code)
	again := i.do(admin, http.MethodPost, "/users/"+me+"/identities",
		map[string]any{"adapter_id": p, "external_id": "00u1"})
	require.Equal(t, http.StatusCreated, again.Code, "linking what is already linked holds")
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodDelete,
		"/users/"+me+"/identities?adapter_id="+p+"&external_id=other", nil).Code)

	var native struct {
		ID string `json:"id"`
	}
	i.do(admin, http.MethodPost, "/groups", map[string]any{"name": "Team"}).JSON(t, &native)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodDelete, "/groups/"+native.ID+"/links/grp_nope", nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodPut, "/groups/"+native.ID+"/links/grp_nope", nil).Code)

	// An account from a provider has no Pando password to change.
	var dana struct {
		ID string `json:"id"`
	}
	_, okta := scimProvider(t, i, admin, "Okta SCIM", nil)
	okta.do(http.MethodPost, "/Users", map[string]any{"userName": "dana", "externalId": "00ud", "active": true}).JSON(t, &dana)
	sess, err := i.Sessions.Create(t.Context(), dana.ID, "idp_local", 3600e9, "test", "")
	require.NoError(t, err)
	pw := i.do(&session{cookie: sess.Cookie.Reveal()}, http.MethodPost, "/me/password",
		map[string]string{"current_password": "x", "new_password": "a-long-enough-password"})
	require.Equal(t, http.StatusBadRequest, pw.Code, pw.String())
	require.Contains(t, pw.String(), "identity provider")
}

// TestR048_SCIMRefusesWhatItCannotApplyAndReplacesWhole asserts SCIM's
// refusals and its PUT: a replacement is whole, and a request Pando cannot
// apply is answered in SCIM's own error format.
func TestR048_SCIMRefusesWhatItCannotApplyAndReplacesWhole(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	providerID, c := scimProvider(t, i, admin, "Okta", nil)

	scimType := func(r reply) string {
		var e struct {
			Type string `json:"scimType"`
		}
		r.JSON(t, &e)
		return e.Type
	}
	require.Equal(t, "invalidSyntax", scimType(c.do(http.MethodPost, "/Users", "{not json")))
	require.Equal(t, "invalidValue", scimType(c.do(http.MethodPost, "/Users", map[string]any{"externalId": "x"})))
	noExt := c.do(http.MethodPost, "/Users", map[string]any{"userName": "a"})
	require.Equal(t, "invalidValue", scimType(noExt))
	require.Contains(t, noExt.String(), "externalId")
	require.Equal(t, "invalidValue", scimType(c.do(http.MethodPost, "/Users",
		map[string]any{"userName": "a", "externalId": "x", "active": "maybe"})))
	for _, path := range []string{"/Users/usr_nope", "/Groups/grp_nope"} {
		require.Equal(t, http.StatusNotFound, c.do(http.MethodGet, path, nil).Code)
		require.Equal(t, http.StatusNotFound, c.do(http.MethodDelete, path, nil).Code)
		require.Equal(t, http.StatusNotFound, c.do(http.MethodPut, path, map[string]any{"userName": "x", "externalId": "x", "displayName": "x"}).Code)
		require.Equal(t, http.StatusNotFound, c.do(http.MethodPatch, path, `{"Operations":[]}`).Code)
	}

	// An account an administrator linked is adopted by the first push for it.
	i.do(admin, http.MethodPost, "/users/"+i.AdminID+"/identities",
		map[string]any{"adapter_id": providerID, "external_id": "00uadmin"})
	adopted := c.do(http.MethodPost, "/Users", map[string]any{"userName": "admin@example.com", "externalId": "00uadmin",
		"name": map[string]any{"givenName": "Ada", "familyName": "Admin"}})
	require.Equal(t, http.StatusCreated, adopted.Code, adopted.String())
	require.Contains(t, adopted.String(), `"id":"`+i.AdminID+`"`)
	require.Contains(t, i.do(admin, http.MethodGet, "/audit?limit=100", nil).String(), "user.scim.adopt")

	var u struct {
		ID string `json:"id"`
	}
	c.do(http.MethodPost, "/Users", map[string]any{"userName": "bo", "externalId": "00ubo",
		"emails": []map[string]any{{"value": "bo@x.com"}, {"value": "bo@y.com", "primary": true}}}).JSON(t, &u)
	byEmail := c.do(http.MethodGet, `/Users?filter=emails.value+eq+%22bo%40y.com%22`, nil)
	require.Contains(t, byEmail.String(), `"totalResults":1`, "the primary email is the account's")

	put := c.do(http.MethodPut, "/Users/"+u.ID, map[string]any{"userName": "bo2", "externalId": "00ubo",
		"displayName": "Bo Two", "active": false})
	require.Equal(t, http.StatusOK, put.Code, put.String())
	require.Contains(t, put.String(), `"userName":"bo2"`)
	require.Contains(t, put.String(), `"active":false`)
	require.NotContains(t, put.String(), "bo@y.com", "a PUT replaces whole")
	require.Equal(t, "invalidSyntax", scimType(c.do(http.MethodPatch, "/Users/"+u.ID, "{")))
	require.Equal(t, "invalidSyntax", scimType(c.do(http.MethodPatch, "/Users/"+u.ID,
		`{"Operations":[{"op":"copy","path":"title"}]}`)))
	require.Equal(t, "uniqueness", scimType(c.do(http.MethodPut, "/Users/"+u.ID,
		map[string]any{"userName": "admin@example.com", "externalId": "00ubo"})))

	// Groups: a whole replacement, a paged list, a rename by path, emptying.
	var g struct {
		ID string `json:"id"`
	}
	c.do(http.MethodPost, "/Groups", map[string]any{"displayName": "Ops", "externalId": "g-ops"}).JSON(t, &g)
	require.Equal(t, "uniqueness", scimType(c.do(http.MethodPost, "/Groups", map[string]any{"displayName": "Other", "externalId": "g-ops"})))
	require.Equal(t, "invalidValue", scimType(c.do(http.MethodPost, "/Groups", map[string]any{"externalId": "x"})))
	require.Equal(t, "invalidSyntax", scimType(c.do(http.MethodPost, "/Groups", "[")))
	replaced := c.do(http.MethodPut, "/Groups/"+g.ID, map[string]any{"displayName": "Operations",
		"members": []map[string]string{{"value": u.ID}}})
	require.Equal(t, http.StatusOK, replaced.Code, replaced.String())
	require.Contains(t, c.do(http.MethodGet, "/Groups/"+g.ID, nil).String(), u.ID)
	require.NotContains(t, c.do(http.MethodGet, "/Groups/"+g.ID+"?excludedAttributes=members", nil).String(), u.ID)
	page := c.do(http.MethodGet, "/Groups?startIndex=1&count=1&filter=displayName+eq+%22Operations%22", nil)
	require.Contains(t, page.String(), `"itemsPerPage":1`)

	rename := c.do(http.MethodPatch, "/Groups/"+g.ID, `{"Operations":[{"op":"replace","path":"displayName","value":"Ops team"}]}`)
	require.Equal(t, http.StatusNoContent, rename.Code, rename.String())
	require.Contains(t, c.do(http.MethodGet, "/Groups/"+g.ID, nil).String(), `"displayName":"Ops team"`)
	empty := c.do(http.MethodPatch, "/Groups/"+g.ID, `{"Operations":[{"op":"remove","path":"members"}]}`)
	require.Equal(t, http.StatusNoContent, empty.Code)
	require.NotContains(t, c.do(http.MethodGet, "/Groups/"+g.ID, nil).String(), u.ID)
	require.Equal(t, "invalidSyntax", scimType(c.do(http.MethodPatch, "/Groups/"+g.ID, "{")))
	require.Equal(t, "invalidPath", scimType(c.do(http.MethodPatch, "/Groups/"+g.ID,
		`{"Operations":[{"op":"add","path":"members[value eq \"x\"]"}]}`)))

	// A provider turned off provisions nobody.
	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPatch, "/identity-providers/"+providerID,
		map[string]any{"enabled": false}).Code)
	offline := c.do(http.MethodGet, "/Users", nil)
	require.Equal(t, http.StatusForbidden, offline.Code)
	require.True(t, strings.Contains(offline.String(), "turned off"), offline.String())
}

// TestR261_AProviderCanBeRegisteredBeforeItIsSaved asserts R-261 for guided
// sign-in setup: the listing gives a new provider's addresses with {id} for
// its ID, and a provider created with an ID chosen ahead has exactly those
// addresses, so they could be registered with the provider first.
func TestR261_AProviderCanBeRegisteredBeforeItIsSaved(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	list := i.do(admin, http.MethodGet, "/identity-providers", nil)
	require.Equal(t, http.StatusOK, list.Code, list.String())
	var got struct {
		Endpoints struct {
			CallbackURL string `json:"callback_url"`
			EntityID    string `json:"entity_id"`
			LoginURL    string `json:"login_url"`
		} `json:"endpoints"`
	}
	list.JSON(t, &got)
	require.Contains(t, got.Endpoints.CallbackURL, "/api/v1/auth/providers/{id}/callback")
	require.Contains(t, got.Endpoints.EntityID, "/api/v1/auth/providers/{id}/metadata")
	require.Contains(t, got.Endpoints.LoginURL, "/api/v1/auth/providers/{id}/start")

	const chosen = "idp_01J9ZQ5Y8W3V7T2R6N4M1K0H8G"
	id := oidcProvider(t, i, admin, "Chosen", map[string]any{"id": chosen})
	require.Equal(t, chosen, id)
	one := i.do(admin, http.MethodGet, "/identity-providers/"+id, nil)
	require.Equal(t, http.StatusOK, one.Code, one.String())
	require.Contains(t, one.String(), strings.ReplaceAll(got.Endpoints.CallbackURL, "{id}", chosen))
	require.Contains(t, one.String(), strings.ReplaceAll(got.Endpoints.LoginURL, "{id}", chosen))

	for name, bad := range map[string]string{"taken": chosen, "not an ID": "okta", "another kind": "app_01J9ZQ5Y8W3V7T2R6N4M1K0H8G", "local": "idp_local"} {
		refused := i.do(admin, http.MethodPost, "/identity-providers", map[string]any{
			"id": bad, "kind": "oidc", "name": "Refused " + name,
			"config": map[string]any{"issuer": "https://idp.example.com", "client_id": "x"},
		})
		require.Equal(t, http.StatusBadRequest, refused.Code, "%s: %s", name, refused.String())
	}
}
