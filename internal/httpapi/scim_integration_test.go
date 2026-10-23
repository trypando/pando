//go:build integration

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// SCIM provisioning (R-048) as Okta and Microsoft Entra ID send it. Each
// request body here is the shape that provider's provisioning client sends —
// Okta replaces with a value object and no path; Entra capitalizes its ops,
// sends booleans as strings and addresses emails by filter — because a SCIM
// server that only accepts the RFC's own examples works with neither.

type scimClient struct {
	t     *testing.T
	inst  *install
	token string
}

func (c *scimClient) do(method, path string, body any) reply {
	c.t.Helper()
	var r io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			r = strings.NewReader(b)
		default:
			enc, err := json.Marshal(b)
			require.NoError(c.t, err)
			r = bytes.NewReader(enc)
		}
	}
	req := httptest.NewRequest(method, "/api/v1/scim/v2"+path, r)
	req.Header.Set("Content-Type", "application/scim+json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	rec := httptest.NewRecorder()
	c.inst.handler.ServeHTTP(rec, req)
	return reply{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

// scimProvider adds an OIDC provider and turns SCIM on for it.
func scimProvider(t *testing.T, inst *install, admin *session, name string, extra map[string]any) (string, *scimClient) {
	t.Helper()
	body := map[string]any{
		"kind": "oidc", "name": name, "enabled": true,
		"config": map[string]any{"issuer": "https://idp.example.com", "client_id": "pando"},
	}
	for k, v := range extra {
		body[k] = v
	}
	created := inst.do(admin, http.MethodPost, "/identity-providers", body)
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)
	tok := inst.do(admin, http.MethodPost, "/identity-providers/"+p.ID+"/scim-token", nil)
	require.Equal(t, http.StatusOK, tok.Code, tok.String())
	var got struct {
		Token   string `json:"token"`
		BaseURL string `json:"scim_base_url"`
	}
	tok.JSON(t, &got)
	require.True(t, strings.HasPrefix(got.Token, "pando_scim_"))
	require.True(t, strings.HasSuffix(got.BaseURL, "/api/v1/scim/v2"))
	return p.ID, &scimClient{t: t, inst: inst, token: got.Token}
}

// TestR048_OktaProvisionsSuspendsAndGroupsPeople asserts R-048 with Okta's
// requests: create, find by userName, push a group with members, deactivate —
// which ends the person's sessions at once — and reactivate.
func TestR048_OktaProvisionsSuspendsAndGroupsPeople(t *testing.T) {
	inst := newInstall(t)
	admin := inst.admin()
	providerID, okta := scimProvider(t, inst, admin, "Okta", nil)

	// Okta checks first, then creates.
	none := okta.do(http.MethodGet, `/Users?filter=userName%20eq%20%22dana%40example.com%22&startIndex=1&count=100`, nil)
	require.Equal(t, http.StatusOK, none.Code, none.String())
	require.Contains(t, none.String(), `"totalResults":0`)
	require.Equal(t, "application/scim+json", none.Hdr.Get("Content-Type"))

	created := okta.do(http.MethodPost, "/Users", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User"],
		"userName": "dana@example.com",
		"name": {"givenName": "Dana", "familyName": "Scully"},
		"emails": [{"primary": true, "value": "dana@example.com", "type": "work"}],
		"displayName": "Dana Scully",
		"locale": "en-US",
		"externalId": "00ujl29u0le5T6Aj10h7",
		"groups": [],
		"password": "1mz050nq",
		"active": true
	}`)
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	require.NotContains(t, created.String(), "1mz050nq", "a password is never kept or returned")
	var dana struct {
		ID     string `json:"id"`
		Active bool   `json:"active"`
	}
	created.JSON(t, &dana)
	require.True(t, dana.Active)
	require.True(t, strings.HasPrefix(dana.ID, "usr_"))

	found := okta.do(http.MethodGet, `/Users?filter=userName%20eq%20%22DANA%40example.com%22`, nil)
	require.Contains(t, found.String(), `"totalResults":1`, "userName is matched without regard to case")

	dup := okta.do(http.MethodPost, "/Users", map[string]any{"userName": "dana@example.com", "externalId": "00uOTHER"})
	require.Equal(t, http.StatusConflict, dup.Code, dup.String())
	require.Contains(t, dup.String(), `"scimType":"uniqueness"`)

	// The account is a Pando account, from this provider, and the identity it
	// signs in with is the externalId Okta sends as the sub claim.
	ids := inst.do(admin, http.MethodGet, "/users/"+dana.ID+"/identities", nil)
	require.Contains(t, ids.String(), `"external_id":"00ujl29u0le5T6Aj10h7"`)
	require.Contains(t, ids.String(), `"scim":true`)

	// A pushed group, with no access until Pando gives it some (R-078).
	grp := okta.do(http.MethodPost, "/Groups", map[string]any{
		"schemas": []string{"urn:ietf:params:scim:schemas:core:2.0:Group"}, "displayName": "Engineering",
		"members": []map[string]string{{"value": dana.ID, "display": "dana@example.com"}},
	})
	require.Equal(t, http.StatusCreated, grp.Code, grp.String())
	var group struct {
		ID string `json:"id"`
	}
	grp.JSON(t, &group)

	// Okta renames with a replace carrying the whole value.
	rename := okta.do(http.MethodPatch, "/Groups/"+group.ID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "replace", "value": {"id": "`+group.ID+`", "displayName": "Engineering Team"}}]
	}`)
	require.Equal(t, http.StatusNoContent, rename.Code, rename.String())
	got := okta.do(http.MethodGet, "/Groups/"+group.ID, nil)
	require.Contains(t, got.String(), `"displayName":"Engineering Team"`)
	require.Contains(t, got.String(), dana.ID)

	listed := inst.do(admin, http.MethodGet, "/groups", nil)
	require.Contains(t, listed.String(), `"source":"`+providerID+`"`)
	require.Contains(t, listed.String(), `"source_name":"Okta"`)

	// A pushed group's members are the provider's to change, not Pando's.
	refused := inst.do(admin, http.MethodPut, "/groups/"+group.ID+"/members/"+inst.AdminID, nil)
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "changed there")

	// Give Dana a session, then deactivate her the way Okta does.
	sess, err := inst.Sessions.Create(t.Context(), dana.ID, providerID, 3600e9, "test", "")
	require.NoError(t, err)
	me := inst.do(&session{cookie: sess.Cookie.Reveal()}, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusOK, me.Code)

	off := okta.do(http.MethodPatch, "/Users/"+dana.ID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "replace", "value": {"active": false}}]
	}`)
	require.Equal(t, http.StatusOK, off.Code, off.String())
	require.Contains(t, off.String(), `"active":false`)
	me = inst.do(&session{cookie: sess.Cookie.Reveal()}, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusUnauthorized, me.Code, "a deactivation ends the session at once (R-048)")

	account := inst.do(admin, http.MethodGet, "/users/"+dana.ID, nil)
	require.Contains(t, account.String(), `"status":"suspended"`, "suspended, not deleted (R-049)")

	on := okta.do(http.MethodPatch, "/Users/"+dana.ID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [{"op": "replace", "value": {"active": true}}]
	}`)
	require.Equal(t, http.StatusOK, on.Code, on.String())
	account = inst.do(admin, http.MethodGet, "/users/"+dana.ID, nil)
	require.Contains(t, account.String(), `"status":"active"`)
}

// TestR049_EntraCannotLiftAnAdministratorsSuspension asserts R-049 against
// Entra's provisioning cycle, which resends active=true every forty minutes:
// a suspension an administrator made stands, one Entra made is Entra's to
// lift, and a delete is a suspension.
func TestR049_EntraCannotLiftAnAdministratorsSuspension(t *testing.T) {
	inst := newInstall(t)
	admin := inst.admin()
	_, entra := scimProvider(t, inst, admin, "Entra", map[string]any{"scim_identity_attribute": "externalId"})

	created := entra.do(http.MethodPost, "/Users", `{
		"schemas": ["urn:ietf:params:scim:schemas:core:2.0:User",
		            "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User"],
		"externalId": "0a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef",
		"userName": "Test_User_ab6490ee-1e48-479e-a20b-2d77186b5dd1",
		"active": true,
		"emails": [{"primary": true, "type": "work", "value": "Test_User_fd0ea19b@testuser.com"}],
		"meta": {"resourceType": "User"},
		"name": {"formatted": "givenName familyName", "familyName": "familyName", "givenName": "givenName"},
		"roles": [],
		"urn:ietf:params:scim:schemas:extension:enterprise:2.0:User": {"employeeNumber": "123"}
	}`)
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var u struct {
		ID string `json:"id"`
	}
	created.JSON(t, &u)
	require.Contains(t, created.String(), `"employeeNumber":"123"`, "what the client sent comes back")

	// Entra's own filter shapes.
	byExt := entra.do(http.MethodGet, `/Users?filter=externalId+eq+%220a21f0f2-8d2a-4f8e-bf98-7363c4aed4ef%22`, nil)
	require.Contains(t, byExt.String(), `"totalResults":1`, byExt.String())
	bad := entra.do(http.MethodGet, `/Users?filter=title+co+%22x%22`, nil)
	require.Equal(t, http.StatusBadRequest, bad.Code)
	require.Contains(t, bad.String(), `"scimType":"invalidFilter"`)

	// Entra's PATCH: capitalized ops, a filtered path, a string boolean.
	patched := entra.do(http.MethodPatch, "/Users/"+u.ID, `{
		"schemas": ["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations": [
			{"op": "Replace", "path": "emails[type eq \"work\"].value", "value": "updated@testuser.com"},
			{"op": "Replace", "path": "name.familyName", "value": "updatedFamilyName"},
			{"op": "Add", "path": "urn:ietf:params:scim:schemas:extension:enterprise:2.0:User:department", "value": "Research"},
			{"op": "Replace", "path": "active", "value": "False"}
		]
	}`)
	require.Equal(t, http.StatusOK, patched.Code, patched.String())
	require.Contains(t, patched.String(), "updated@testuser.com")
	require.Contains(t, patched.String(), "updatedFamilyName")
	require.Contains(t, patched.String(), `"department":"Research"`)
	require.Contains(t, patched.String(), `"active":false`)

	// Entra reactivates: its own suspension lifts.
	on := entra.do(http.MethodPatch, "/Users/"+u.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Replace","path":"active","value":"True"}]}`)
	require.Contains(t, on.String(), `"active":true`, on.String())

	// An administrator suspends; Entra's next cycle does not undo it.
	suspend := inst.do(admin, http.MethodPatch, "/users/"+u.ID, map[string]any{"status": "suspended"})
	require.Less(t, suspend.Code, 300, suspend.String())
	cycle := entra.do(http.MethodPatch, "/Users/"+u.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Replace","path":"active","value":"True"}]}`)
	require.Equal(t, http.StatusOK, cycle.Code, cycle.String())
	require.Contains(t, cycle.String(), `"active":false`, "an administrator's suspension stands")

	// A delete suspends and forgets; the account stays in Pando.
	del := entra.do(http.MethodDelete, "/Users/"+u.ID, nil)
	require.Equal(t, http.StatusNoContent, del.Code, del.String())
	gone := entra.do(http.MethodGet, "/Users/"+u.ID, nil)
	require.Equal(t, http.StatusNotFound, gone.Code)
	account := inst.do(admin, http.MethodGet, "/users/"+u.ID, nil)
	require.Equal(t, http.StatusOK, account.Code)
	require.Contains(t, account.String(), `"status":"suspended"`)

	// Entra group membership: add and remove by filter.
	other := entra.do(http.MethodPost, "/Users", map[string]any{"userName": "other", "externalId": "ext-other", "active": true})
	var o struct {
		ID string `json:"id"`
	}
	other.JSON(t, &o)
	grp := entra.do(http.MethodPost, "/Groups", map[string]any{"displayName": "Group1DisplayName", "externalId": "g-ext-1"})
	var g struct {
		ID string `json:"id"`
	}
	grp.JSON(t, &g)
	add := entra.do(http.MethodPatch, "/Groups/"+g.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Add","path":"members","value":[{"value":"`+o.ID+`"}]}]}`)
	require.Equal(t, http.StatusNoContent, add.Code, add.String())
	require.Contains(t, entra.do(http.MethodGet, "/Groups/"+g.ID, nil).String(), o.ID)
	excluded := entra.do(http.MethodGet, `/Groups?filter=displayName+eq+%22Group1DisplayName%22&excludedAttributes=members`, nil)
	require.Contains(t, excluded.String(), `"totalResults":1`)
	require.NotContains(t, excluded.String(), o.ID)
	remove := entra.do(http.MethodPatch, "/Groups/"+g.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Remove","path":"members[value eq \"`+o.ID+`\"]"}]}`)
	require.Equal(t, http.StatusNoContent, remove.Code, remove.String())
	require.NotContains(t, entra.do(http.MethodGet, "/Groups/"+g.ID, nil).String(), o.ID)

	// A member this provider does not manage is refused.
	foreign := entra.do(http.MethodPatch, "/Groups/"+g.ID, `{"schemas":["urn:ietf:params:scim:api:messages:2.0:PatchOp"],
		"Operations":[{"op":"Add","path":"members","value":[{"value":"`+inst.AdminID+`"}]}]}`)
	require.Equal(t, http.StatusNotFound, foreign.Code, foreign.String())

	require.Equal(t, http.StatusNoContent, entra.do(http.MethodDelete, "/Groups/"+g.ID, nil).Code)
	require.Equal(t, http.StatusNotFound, entra.do(http.MethodGet, "/Groups/"+g.ID, nil).Code)
}

// TestR048_ASCIMTokenIsOnlyASCIMToken asserts that a SCIM token authenticates
// SCIM and nothing else, that a Pando token does not authenticate SCIM, and
// that rotating it retires the old one.
func TestR048_ASCIMTokenIsOnlyASCIMToken(t *testing.T) {
	inst := newInstall(t)
	admin := inst.admin()
	providerID, c := scimProvider(t, inst, admin, "Okta", nil)

	require.Equal(t, http.StatusOK, c.do(http.MethodGet, "/ServiceProviderConfig", nil).Code)
	require.Contains(t, c.do(http.MethodGet, "/ResourceTypes", nil).String(), `"endpoint":"/Users"`)
	require.Contains(t, c.do(http.MethodGet, "/Schemas", nil).String(), "userName")

	noToken := &scimClient{t: t, inst: inst}
	resp := noToken.do(http.MethodGet, "/Users", nil)
	require.Equal(t, http.StatusUnauthorized, resp.Code)
	require.NotEmpty(t, resp.Hdr.Get("WWW-Authenticate"))

	// The SCIM token is not a Pando credential.
	asAPI := inst.do(&session{token: c.token}, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusUnauthorized, asAPI.Code, asAPI.String())

	// A Pando token is not a SCIM credential.
	pandoToken := inst.tokenFor(admin)
	asSCIM := (&scimClient{t: t, inst: inst, token: pandoToken.token}).do(http.MethodGet, "/Users", nil)
	require.Equal(t, http.StatusUnauthorized, asSCIM.Code)

	rotated := inst.do(admin, http.MethodPost, "/identity-providers/"+providerID+"/scim-token", nil)
	require.Equal(t, http.StatusOK, rotated.Code)
	require.Equal(t, http.StatusUnauthorized, c.do(http.MethodGet, "/Users", nil).Code, "the old token is retired")

	list := inst.do(admin, http.MethodGet, "/identity-providers", nil)
	require.NotContains(t, list.String(), c.token, "a token is shown once")
	require.Contains(t, list.String(), `"scim_enabled":true`)
	require.Contains(t, list.String(), `"mode":"push"`, "with SCIM, revocation is pushed (R-047, R-048)")
	require.Contains(t, list.String(), `"window_seconds":120`)

	require.Equal(t, http.StatusNoContent, inst.do(admin, http.MethodDelete, "/identity-providers/"+providerID+"/scim-token", nil).Code)
	list = inst.do(admin, http.MethodGet, "/identity-providers", nil)
	require.Contains(t, list.String(), `"mode":"expiry_only"`)

	audit := inst.do(admin, http.MethodGet, "/audit?limit=200", nil)
	for _, action := range []string{"identity_provider.create", "identity_provider.scim.enable",
		"identity_provider.scim.rotate", "identity_provider.scim.disable"} {
		require.Contains(t, audit.String(), action)
	}
}
