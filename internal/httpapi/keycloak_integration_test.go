//go:build integration

package httpapi_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/trypando/pando/internal/httpapi"
)

// External identity against a real identity provider (issue #51).
//
// Keycloak speaks both OpenID Connect and SAML, runs in a container, and is
// configured here through its admin API the way an administrator would
// configure Okta: a client for Pando, a user, a group, and a mapper that puts
// the groups in the token. The browser is an http.Client with a cookie jar
// that fills in Keycloak's login form and submits its SAML auto-post form, so
// every redirect, cookie and signature is the real one.

var (
	keycloakOnce sync.Once
	keycloakURL  string
	keycloakErr  error
)

// keycloak starts one Keycloak for the package.
func keycloak(t *testing.T) string {
	t.Helper()
	keycloakOnce.Do(func() {
		ctx := context.Background()
		c, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
			ContainerRequest: testcontainers.ContainerRequest{
				Image:        "keycloak/keycloak:26.4",
				Cmd:          []string{"start-dev"},
				ExposedPorts: []string{"8080/tcp"},
				Env: map[string]string{
					"KC_BOOTSTRAP_ADMIN_USERNAME": "admin",
					"KC_BOOTSTRAP_ADMIN_PASSWORD": "admin",
				},
				WaitingFor: wait.ForHTTP("/realms/master").WithPort("8080/tcp").WithStartupTimeout(4 * time.Minute),
			},
			Started: true,
		})
		if err != nil {
			keycloakErr = err
			return
		}
		// Plain HTTP from the test, wherever the test runs. Keycloak's master
		// realm requires HTTPS for any client it does not see as local or on
		// a private network ("sslRequired": "external"). Behind a Linux
		// runner's port mapping a request arrives from the bridge gateway,
		// which is private, so this never came up in CI; behind Docker
		// Desktop's it does not, and every admin token request was refused
		// with "HTTPS required". Inside the container the request is local.
		for _, cmd := range [][]string{
			{"/opt/keycloak/bin/kcadm.sh", "config", "credentials", "--server", "http://localhost:8080",
				"--realm", "master", "--user", "admin", "--password", "admin"},
			{"/opt/keycloak/bin/kcadm.sh", "update", "realms/master", "-s", "sslRequired=NONE"},
		} {
			code, out, err := c.Exec(ctx, cmd)
			if err == nil && code != 0 {
				b, _ := io.ReadAll(out)
				err = fmt.Errorf("%s exited %d: %s", strings.Join(cmd[1:3], " "), code, b)
			}
			if err != nil {
				keycloakErr = err
				return
			}
		}
		port, err := c.MappedPort(ctx, "8080/tcp")
		if err != nil {
			keycloakErr = err
			return
		}
		// localhost, not the container host: Keycloak names its issuer after
		// the host it was reached at, and both Pando and the browser reach it
		// here, so the two agree.
		keycloakURL = "http://localhost:" + port.Port()
	})
	require.NoError(t, keycloakErr, "Keycloak did not start")
	return keycloakURL
}

// kcAdmin is Keycloak's admin API.
type kcAdmin struct {
	t     *testing.T
	base  string
	token string
}

func newKCAdmin(t *testing.T, base string) *kcAdmin {
	t.Helper()
	resp, err := http.PostForm(base+"/realms/master/protocol/openid-connect/token", url.Values{
		"grant_type": {"password"}, "client_id": {"admin-cli"}, "username": {"admin"}, "password": {"admin"},
	})
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&tok))
	require.NotEmpty(t, tok.AccessToken)
	return &kcAdmin{t: t, base: base, token: tok.AccessToken}
}

func (k *kcAdmin) do(method, path string, body any) (int, []byte) {
	k.t.Helper()
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(k.t, err)
		r = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, k.base+"/admin"+path, r)
	require.NoError(k.t, err)
	req.Header.Set("Authorization", "Bearer "+k.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	require.NoError(k.t, err)
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (k *kcAdmin) must(method, path string, body any) {
	k.t.Helper()
	code, b := k.do(method, path, body)
	require.Less(k.t, code, 300, "%s %s: %s", method, path, b)
}

// realm makes a realm with one user, alice, in one group, engineering.
func (k *kcAdmin) realm(name string) {
	// Plain HTTP, for the reason keycloak() sets the master realm to it.
	k.must(http.MethodPost, "/realms", map[string]any{"realm": name, "enabled": true, "sslRequired": "none"})
	k.must(http.MethodPost, "/realms/"+name+"/groups", map[string]any{"name": "engineering"})
	k.must(http.MethodPost, "/realms/"+name+"/users", map[string]any{
		"username": "alice", "email": "alice@example.com", "emailVerified": true, "enabled": true,
		"firstName": "Alice", "lastName": "Liddell", "groups": []string{"/engineering"},
		"credentials": []map[string]any{{"type": "password", "value": "alice-password", "temporary": false}},
	})
}

// browser follows redirects with cookies, signs in at Keycloak's form, and
// submits a SAML auto-post form, until it lands somewhere that is neither.
type browser struct {
	t      *testing.T
	client *http.Client
	last   *http.Response
	body   string
}

func newBrowser(t *testing.T) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, client: &http.Client{Jar: jar, Timeout: 30 * time.Second}}
}

var (
	formAction = regexp.MustCompile(`(?s)<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"`)
	postForm   = regexp.MustCompile(`(?s)<form[^>]*method="post"[^>]*action="([^"]+)"`)
	hidden     = regexp.MustCompile(`<input[^>]*type="hidden"[^>]*name="([^"]+)"[^>]*value="([^"]*)"`)
)

func (b *browser) get(u string) {
	b.t.Helper()
	resp, err := b.client.Get(u)
	require.NoError(b.t, err)
	b.read(resp)
}

func (b *browser) post(u string, form url.Values) {
	b.t.Helper()
	resp, err := b.client.PostForm(u, form)
	require.NoError(b.t, err)
	b.read(resp)
}

func (b *browser) read(resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	b.last, b.body = resp, string(body)
}

// through signs in as username at Keycloak and follows everything after.
func (b *browser) through(username, password string) {
	b.t.Helper()
	for i := 0; i < 5; i++ {
		if m := formAction.FindStringSubmatch(b.body); m != nil {
			b.post(html.UnescapeString(m[1]), url.Values{"username": {username}, "password": {password}})
			continue
		}
		if strings.Contains(b.body, "SAMLResponse") {
			m := postForm.FindStringSubmatch(strings.ToLower(b.body[:0]) + b.body)
			require.NotNil(b.t, m, "a SAML auto-post form: %s", b.body)
			form := url.Values{}
			for _, h := range hidden.FindAllStringSubmatch(b.body, -1) {
				form.Set(h[1], html.UnescapeString(h[2]))
			}
			b.post(html.UnescapeString(m[1]), form)
			continue
		}
		return
	}
	b.t.Fatalf("the sign-in did not settle: %s", b.body)
}

func (b *browser) cookie(u, name string) string {
	parsed, _ := url.Parse(u)
	for _, c := range b.client.Jar.Cookies(parsed) {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}

// served runs an install behind a real listener, which a provider can
// redirect a browser to.
func served(t *testing.T) (*install, string) {
	t.Helper()
	inst := newInstall(t)
	srv := httptest.NewServer(inst.handler)
	t.Cleanup(srv.Close)
	// As an operator would: the address the provider sends people back to
	// is stated, not inferred from whoever asked.
	external := mustURL(srv.URL)
	inst.Server.ExternalURL = external
	inst.Server.IDP.ExternalURL = external
	return inst, srv.URL
}

// TestR043_SignInThroughKeycloakWithOIDCAndSAML asserts R-043: an administrator
// connects a real provider over OpenID Connect and over SAML, tests it, and
// people sign in through it — with just-in-time accounts and groups from the
// provider — with nothing configured but the provider.
func TestR043_SignInThroughKeycloakWithOIDCAndSAML(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	admin := inst.admin()

	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("pando%d", time.Now().UnixNano())
	kca.realm(realm)

	// --- OpenID Connect -------------------------------------------------
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": "pando", "enabled": true, "protocol": "openid-connect", "publicClient": false,
		"secret": "pando-client-secret", "standardFlowEnabled": true, "redirectUris": []string{pando + "/*"},
		"protocolMappers": []map[string]any{{
			"name": "groups", "protocol": "openid-connect", "protocolMapper": "oidc-group-membership-mapper",
			"config": map[string]string{"full.path": "false", "id.token.claim": "true", "access.token.claim": "true",
				"userinfo.token.claim": "true", "claim.name": "groups"},
		}},
	})

	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Keycloak", "jit_provisioning": true,
		"config":      map[string]any{"issuer": kc + "/realms/" + realm, "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "pando-client-secret"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var oidcProvider struct {
		ID          string   `json:"id"`
		Enabled     bool     `json:"enabled"`
		CallbackURL string   `json:"callback_url"`
		Credentials []string `json:"credentials"`
		Config      map[string]any
		Revocation  struct {
			Mode          string `json:"mode"`
			WindowSeconds int64  `json:"window_seconds"`
		} `json:"revocation"`
	}
	created.JSON(t, &oidcProvider)
	require.False(t, oidcProvider.Enabled, "a new provider is off until it is tested and turned on")
	require.Equal(t, pando+"/api/v1/auth/providers/"+oidcProvider.ID+"/callback", oidcProvider.CallbackURL)
	require.Equal(t, []string{"client_secret"}, oidcProvider.Credentials, "the secret is named, never shown")
	require.NotContains(t, created.String(), "pando-client-secret")
	require.Equal(t, "expiry_only", oidcProvider.Revocation.Mode, "without SCIM the session lifetime is the window (R-050)")
	require.Equal(t, int64(12*3600), oidcProvider.Revocation.WindowSeconds)

	check := inst.do(admin, http.MethodPost, "/identity-providers/"+oidcProvider.ID+"/check", nil)
	require.Contains(t, check.String(), `"ok":true`)

	// A test sign-in works while the provider is off, and reports the claims.
	tester := newBrowser(t)
	tester.client.Jar.SetCookies(mustURL(pando), []*http.Cookie{{Name: httpapi.SessionCookie, Value: admin.cookie, Path: "/"}})
	tester.get(pando + "/api/v1/identity-providers/" + oidcProvider.ID + "/test")
	tester.through("alice", "alice-password")
	require.Contains(t, tester.last.Request.URL.String(), "/admin/sign-in?provider="+oidcProvider.ID+"&test=")
	testID := tester.last.Request.URL.Query().Get("test")
	report := inst.do(admin, http.MethodGet, "/identity-providers/"+oidcProvider.ID+"/tests/"+testID, nil)
	require.Equal(t, http.StatusOK, report.Code, report.String())
	var rep struct {
		OK      bool `json:"ok"`
		Subject struct {
			ExternalID    string   `json:"external_id"`
			Email         string   `json:"email"`
			EmailVerified bool     `json:"email_verified"`
			DisplayName   string   `json:"display_name"`
			Groups        []string `json:"groups"`
		} `json:"subject"`
		Attributes map[string][]string `json:"attributes"`
		Outcome    struct {
			Kind string `json:"kind"`
		} `json:"outcome"`
	}
	report.JSON(t, &rep)
	require.True(t, rep.OK, report.String())
	require.Equal(t, "alice@example.com", rep.Subject.Email)
	require.True(t, rep.Subject.EmailVerified)
	require.Equal(t, "Alice Liddell", rep.Subject.DisplayName)
	require.Equal(t, []string{"engineering"}, rep.Subject.Groups)
	require.Equal(t, "created", rep.Outcome.Kind, "the report says what a real sign-in would do")
	require.Equal(t, []string{"alice"}, rep.Attributes["preferred_username"])
	users := inst.do(admin, http.MethodGet, "/users", nil)
	require.NotContains(t, users.String(), "alice@example.com", "a test sign-in makes nobody")

	// Off, it is not offered and cannot be used.
	opts := inst.anon(http.MethodGet, "/auth/options", nil)
	require.NotContains(t, opts.String(), oidcProvider.ID)

	on := inst.do(admin, http.MethodPatch, "/identity-providers/"+oidcProvider.ID, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, on.Code, on.String())
	opts = inst.anon(http.MethodGet, "/auth/options", nil)
	require.Contains(t, opts.String(), `"name":"Keycloak"`)

	// A real sign-in, started on the sign-in page with somewhere to go after.
	alice := newBrowser(t)
	alice.get(pando + "/api/v1/auth/providers/" + oidcProvider.ID + "/start?next=/apps/notes")
	alice.through("alice", "alice-password")
	require.Equal(t, "/apps/notes", alice.last.Request.URL.Path, "the browser lands where it was going")
	sessionID := alice.cookie(pando, httpapi.SessionCookie)
	require.NotEmpty(t, sessionID)

	me := inst.do(&session{cookie: sessionID}, http.MethodGet, "/me", nil)
	require.Equal(t, http.StatusOK, me.Code, me.String())
	var who struct {
		UserID      string   `json:"user_id"`
		Email       string   `json:"email"`
		DisplayName string   `json:"display_name"`
		Groups      []string `json:"groups"`
	}
	me.JSON(t, &who)
	require.Equal(t, "alice@example.com", who.Email)
	require.Equal(t, "Alice Liddell", who.DisplayName)
	require.Len(t, who.Groups, 1, "the provider's group is synced and held live (R-079)")

	groups := inst.do(admin, http.MethodGet, "/groups", nil)
	require.Contains(t, groups.String(), `"name":"engineering"`)
	require.Contains(t, groups.String(), `"source":"`+oidcProvider.ID+`"`)

	ids := inst.do(admin, http.MethodGet, "/users/"+who.UserID+"/identities", nil)
	require.Contains(t, ids.String(), `"origin":true`)

	// --- SAML -----------------------------------------------------------
	samlCreated := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "saml", "name": "Keycloak SAML", "enabled": true, "link_by_email": true,
		"config": map[string]any{
			"idp_metadata_url": kc + "/realms/" + realm + "/protocol/saml/descriptor",
			"name_id_format":   "persistent", "groups_attribute": "groups", "trust_email": true,
		},
	})
	require.Equal(t, http.StatusCreated, samlCreated.Code, samlCreated.String())
	var samlProvider struct {
		ID          string `json:"id"`
		CallbackURL string `json:"callback_url"`
		EntityID    string `json:"entity_id"`
	}
	samlCreated.JSON(t, &samlProvider)

	md, err := http.Get(samlProvider.EntityID)
	require.NoError(t, err)
	mdBody, _ := io.ReadAll(md.Body)
	_ = md.Body.Close()
	require.Equal(t, http.StatusOK, md.StatusCode)
	require.Contains(t, string(mdBody), samlProvider.CallbackURL, "the metadata names the ACS URL")
	require.NotContains(t, string(mdBody), "HTTP-Artifact", "only HTTP-POST is answered")

	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": samlProvider.EntityID, "enabled": true, "protocol": "saml",
		"redirectUris": []string{pando + "/*"},
		"attributes": map[string]string{
			"saml.assertion.signature": "true", "saml.server.signature": "true", "saml.client.signature": "false",
			"saml_force_name_id_format": "true", "saml_name_id_format": "persistent",
			"saml_assertion_consumer_url_post": samlProvider.CallbackURL,
		},
		"protocolMappers": []map[string]any{
			{"name": "groups", "protocol": "saml", "protocolMapper": "saml-group-membership-mapper",
				"config": map[string]string{"attribute.name": "groups", "full.path": "false", "single": "false",
					"attribute.nameformat": "Basic"}},
			{"name": "email", "protocol": "saml", "protocolMapper": "saml-user-property-mapper",
				"config": map[string]string{"user.attribute": "email", "attribute.name": "email", "attribute.nameformat": "Basic"}},
		},
	})

	// The same person through SAML: a different identity, matched to her
	// account by the email SAML's provider is trusted to vouch for.
	alice.get(pando + "/api/v1/auth/providers/" + samlProvider.ID + "/start?next=/after-saml")
	alice.through("alice", "alice-password")
	require.Equal(t, "/after-saml", alice.last.Request.URL.Path, alice.body)
	samlSession := alice.cookie(pando, httpapi.SessionCookie)
	require.NotEqual(t, sessionID, samlSession)
	me = inst.do(&session{cookie: samlSession}, http.MethodGet, "/me", nil)
	var again struct {
		UserID string   `json:"user_id"`
		Groups []string `json:"groups"`
	}
	me.JSON(t, &again)
	require.Equal(t, who.UserID, again.UserID, "linked by verified email to the same account (O-1)")
	require.Len(t, again.Groups, 2, "engineering at each provider is its own synced group")

	ids = inst.do(admin, http.MethodGet, "/users/"+who.UserID+"/identities", nil)
	require.Contains(t, ids.String(), samlProvider.ID)
}

// TestR045_NoAccountWithoutJITOrALink asserts R-045 and the O-1 resolution: a
// provider that does not make accounts refuses someone Pando does not know,
// in a sentence that says what to do; an administrator links the identity to
// an existing account; and from then on it signs in there.
func TestR045_NoAccountWithoutJITOrALink(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	admin := inst.admin()

	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("link%d", time.Now().UnixNano())
	kca.realm(realm)
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": "pando", "enabled": true, "protocol": "openid-connect", "publicClient": false,
		"secret": "s3cret", "standardFlowEnabled": true, "redirectUris": []string{pando + "/*"},
	})
	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Corp", "enabled": true,
		"config":      map[string]any{"issuer": kc + "/realms/" + realm, "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "s3cret"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)

	b := newBrowser(t)
	b.get(pando + "/api/v1/auth/providers/" + p.ID + "/start")
	b.through("alice", "alice-password")
	require.Equal(t, "/.pando/login", b.last.Request.URL.Path)
	flowID := b.last.Request.URL.Query().Get("sso_error")
	require.NotEmpty(t, flowID)
	failure := inst.anon(http.MethodGet, "/auth/failures/"+url.PathEscape(flowID), nil)
	require.Equal(t, http.StatusOK, failure.Code, failure.String())
	require.Contains(t, failure.String(), "do not have a Pando account yet")
	require.Contains(t, failure.String(), "Ask an administrator")
	require.Empty(t, b.cookie(pando, httpapi.SessionCookie))

	// The failure names the provider's ID for the person, which is what an
	// administrator links.
	var problem struct {
		Remedy string `json:"remedy"`
	}
	failure.JSON(t, &problem)
	sub := strings.TrimSuffix(problem.Remedy[strings.LastIndex(problem.Remedy, ": ")+2:], ".")

	inst.user("alice")
	users := inst.do(admin, http.MethodGet, "/users", nil)
	var list struct {
		Users []struct {
			ID         string `json:"id"`
			ExternalID string `json:"external_id"`
		} `json:"users"`
	}
	users.JSON(t, &list)
	aliceID := ""
	for _, u := range list.Users {
		if u.ExternalID == "alice" {
			aliceID = u.ID
		}
	}
	require.NotEmpty(t, aliceID)
	link := inst.do(admin, http.MethodPost, "/users/"+aliceID+"/identities",
		map[string]any{"adapter_id": p.ID, "external_id": sub})
	require.Equal(t, http.StatusCreated, link.Code, link.String())

	b.get(pando + "/api/v1/auth/providers/" + p.ID + "/start?next=/in")
	b.through("alice", "alice-password")
	require.Equal(t, "/in", b.last.Request.URL.Path, b.body)
	me := inst.do(&session{cookie: b.cookie(pando, httpapi.SessionCookie)}, http.MethodGet, "/me", nil)
	require.Contains(t, me.String(), `"user_id":"`+aliceID+`"`, "the local account, through the linked identity")
	require.Contains(t, me.String(), `"username":"alice"`)
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}

// samlForm signs in at Keycloak and returns the SAML auto-post form it
// answers with, without submitting it.
func (b *browser) samlForm(username, password string) (string, url.Values) {
	b.t.Helper()
	for i := 0; i < 3; i++ {
		if m := formAction.FindStringSubmatch(b.body); m != nil {
			b.post(html.UnescapeString(m[1]), url.Values{"username": {username}, "password": {password}})
			continue
		}
		break
	}
	m := postForm.FindStringSubmatch(b.body)
	require.NotNil(b.t, m, "a SAML auto-post form: %s", b.body)
	form := url.Values{}
	for _, h := range hidden.FindAllStringSubmatch(b.body, -1) {
		form.Set(h[1], html.UnescapeString(h[2]))
	}
	require.NotEmpty(b.t, form.Get("SAMLResponse"))
	return html.UnescapeString(m[1]), form
}

// TestR043_SAMLRefusesForgedReplayedAndUnsolicitedResponses asserts the three
// properties that make SAML sign-in safe: a response whose signed content was
// changed is refused, a valid response is accepted once and never again, and
// a response nobody asked for is refused unless the provider allows it.
func TestR043_SAMLRefusesForgedReplayedAndUnsolicitedResponses(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	admin := inst.admin()
	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("saml%d", time.Now().UnixNano())
	kca.realm(realm)

	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "saml", "name": "SAML", "enabled": true, "jit_provisioning": true,
		"config": map[string]any{"idp_metadata_url": kc + "/realms/" + realm + "/protocol/saml/descriptor",
			"name_id_format": "persistent"},
	})
	require.Equal(t, http.StatusCreated, created.Code, created.String())
	var p struct {
		ID          string `json:"id"`
		CallbackURL string `json:"callback_url"`
		EntityID    string `json:"entity_id"`
	}
	created.JSON(t, &p)
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": p.EntityID, "enabled": true, "protocol": "saml", "redirectUris": []string{pando + "/*"},
		"attributes": map[string]string{
			"saml.assertion.signature": "true", "saml.server.signature": "true", "saml.client.signature": "false",
			"saml_force_name_id_format": "true", "saml_name_id_format": "persistent",
			"saml_assertion_consumer_url_post": p.CallbackURL,
		},
	})

	// Forged: the NameID changed after signing.
	b := newBrowser(t)
	b.get(pando + "/api/v1/auth/providers/" + p.ID + "/start")
	acs, form := b.samlForm("alice", "alice-password")
	raw, err := base64.StdEncoding.DecodeString(form.Get("SAMLResponse"))
	require.NoError(t, err)
	nameID := regexp.MustCompile(`(<saml:NameID[^>]*>)([^<]+)(</saml:NameID>)`)
	require.True(t, nameID.Match(raw), string(raw))
	forged := nameID.ReplaceAll(raw, []byte("${1}someone-else${3}"))
	tampered := url.Values{"SAMLResponse": {base64.StdEncoding.EncodeToString(forged)}, "RelayState": {form.Get("RelayState")}}
	b.post(acs, tampered)
	require.Equal(t, "/.pando/login", b.last.Request.URL.Path)
	failure := inst.anon(http.MethodGet, "/auth/failures/"+url.PathEscape(b.last.Request.URL.Query().Get("sso_error")), nil)
	require.Contains(t, failure.String(), "did not verify", failure.String())
	require.Empty(t, b.cookie(pando, httpapi.SessionCookie))

	// Genuine, once.
	b.get(pando + "/api/v1/auth/providers/" + p.ID + "/start?next=/ok")
	acs, form = b.samlForm("alice", "alice-password")
	b.post(acs, form)
	require.Equal(t, "/ok", b.last.Request.URL.Path, b.body)
	require.NotEmpty(t, b.cookie(pando, httpapi.SessionCookie))

	// Replayed by someone else: the flow is spent, and a response nobody asked
	// for is refused while the provider does not allow them.
	attacker := newBrowser(t)
	attacker.post(acs, form)
	require.Equal(t, "/.pando/login", attacker.last.Request.URL.Path)
	require.Empty(t, attacker.cookie(pando, httpapi.SessionCookie))
	unsolicited := url.Values{"SAMLResponse": {form.Get("SAMLResponse")}}
	attacker.post(acs, unsolicited)
	require.Empty(t, attacker.cookie(pando, httpapi.SessionCookie))

	// Allowing IdP-initiated sign-in does not let the same response in twice.
	allow := inst.do(admin, http.MethodPatch, "/identity-providers/"+p.ID, map[string]any{
		"config": map[string]any{"idp_metadata_url": kc + "/realms/" + realm + "/protocol/saml/descriptor",
			"name_id_format": "persistent", "allow_idp_initiated": true},
	})
	require.Equal(t, http.StatusOK, allow.Code, allow.String())
	attacker.post(acs, unsolicited)
	require.Empty(t, attacker.cookie(pando, httpapi.SessionCookie), "a SAML assertion is used once (sso_replay)")
}

// TestR043_ASignInFinishesOnlyInTheBrowserThatStartedIt asserts the login-CSRF
// defence: the one-time code the callback issues is useless in another browser.
func TestR043_ASignInFinishesOnlyInTheBrowserThatStartedIt(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	admin := inst.admin()
	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("csrf%d", time.Now().UnixNano())
	kca.realm(realm)
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": "pando", "enabled": true, "protocol": "openid-connect", "publicClient": false,
		"secret": "s3cret", "standardFlowEnabled": true, "redirectUris": []string{pando + "/*"},
	})
	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Corp", "enabled": true, "jit_provisioning": true,
		"config":      map[string]any{"issuer": kc + "/realms/" + realm, "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "s3cret"},
	})
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)

	// The attacker signs in as themselves, and stops before completing.
	attacker := newBrowser(t)
	attacker.client.CheckRedirect = func(req *http.Request, _ []*http.Request) error {
		if strings.Contains(req.URL.Path, "/auth/complete") {
			return http.ErrUseLastResponse
		}
		return nil
	}
	attacker.get(pando + "/api/v1/auth/providers/" + p.ID + "/start")
	attacker.through("alice", "alice-password")
	completeURL := attacker.last.Header.Get("Location")
	require.Contains(t, completeURL, "/auth/complete?code=")

	// The victim is lured to it.
	victim := newBrowser(t)
	victim.get(completeURL)
	require.Equal(t, "/.pando/login", victim.last.Request.URL.Path)
	require.Equal(t, "browser", victim.last.Request.URL.Query().Get("sso_error"))
	require.Empty(t, victim.cookie(pando, httpapi.SessionCookie))

	// And the code is spent: the attacker cannot use it after either.
	attacker.client.CheckRedirect = nil
	attacker.get(completeURL)
	require.Equal(t, "expired", attacker.last.Request.URL.Query().Get("sso_error"))
}

// TestR047_JITFollowsHostPolicyAndPasswordSignInCanBeTurnedOff asserts the
// host-policy floors over external identity: disable_jit_provisioning refuses
// a first sign-in whatever the provider says, and disable_password_sign_in
// refuses a password — refused while no provider is on, so it cannot lock
// everyone out.
func TestR047_JITFollowsHostPolicyAndPasswordSignInCanBeTurnedOff(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	admin := inst.admin()

	policy := inst.do(admin, http.MethodGet, "/policy", nil)
	var doc map[string]any
	policy.JSON(t, &doc)
	doc["disable_password_sign_in"] = true
	refused := inst.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "nobody could sign in")

	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("policy%d", time.Now().UnixNano())
	kca.realm(realm)
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": "pando", "enabled": true, "protocol": "openid-connect", "publicClient": false,
		"secret": "s3cret", "standardFlowEnabled": true, "redirectUris": []string{pando + "/*"},
	})
	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Corp", "enabled": true, "jit_provisioning": true,
		"config":      map[string]any{"issuer": kc + "/realms/" + realm, "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "s3cret"},
	})
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)

	doc["disable_jit_provisioning"] = true
	saved := inst.do(admin, http.MethodPut, "/policy", doc)
	require.Equal(t, http.StatusOK, saved.Code, saved.String())

	b := newBrowser(t)
	b.get(pando + "/api/v1/auth/providers/" + p.ID + "/start")
	b.through("alice", "alice-password")
	require.Equal(t, "/.pando/login", b.last.Request.URL.Path, "host policy is a floor over the provider's setting")

	_, pw := inst.signIn("admin", inst.adminPassword)
	require.Equal(t, http.StatusUnauthorized, pw.Code, pw.String())
	require.Contains(t, pw.String(), "Password sign-in is turned off")
	opts := inst.anon(http.MethodGet, "/auth/options", nil)
	require.Contains(t, opts.String(), `"password_sign_in":false`)

	// Local accounts read as off everywhere the setting is shown: the provider
	// list agrees with the sign-in page, which offers only the provider.
	local := inst.do(admin, http.MethodGet, "/identity-providers/idp_local", nil)
	require.Contains(t, local.String(), `"enabled":false`, local.String())
	require.Contains(t, opts.String(), `"name":"Corp"`)

	// With password sign-in off, the last provider cannot be turned off.
	off := inst.do(admin, http.MethodPatch, "/identity-providers/"+p.ID, map[string]any{"enabled": false})
	require.Equal(t, http.StatusBadRequest, off.Code, off.String())
	require.Contains(t, off.String(), "last identity provider")
}

// localtestHosts says every *.localtest.me hostname belongs to an app.
type localtestHosts struct{}

func (localtestHosts) IsAppHostname(_ context.Context, host string) (bool, error) {
	return strings.HasSuffix(host, ".localtest.me"), nil
}

// TestR172_ASignInStartedOnAnAppsHostnameFinishesThere asserts that a person
// sent to sign in from an app's own hostname (R-172) comes back signed in on
// that hostname — though the provider can only return them to the one
// callback address registered with it, on Pando's own.
func TestR172_ASignInStartedOnAnAppsHostnameFinishesThere(t *testing.T) {
	kc := keycloak(t)
	inst, pando := served(t)
	inst.Server.AppHosts = localtestHosts{}
	admin := inst.admin()
	kca := newKCAdmin(t, kc)
	realm := fmt.Sprintf("hosts%d", time.Now().UnixNano())
	kca.realm(realm)
	kca.must(http.MethodPost, "/realms/"+realm+"/clients", map[string]any{
		"clientId": "pando", "enabled": true, "protocol": "openid-connect", "publicClient": false,
		"secret": "s3cret", "standardFlowEnabled": true, "redirectUris": []string{pando + "/*"},
	})
	created := inst.do(admin, http.MethodPost, "/identity-providers", map[string]any{
		"kind": "oidc", "name": "Corp", "enabled": true, "jit_provisioning": true,
		"config":      map[string]any{"issuer": kc + "/realms/" + realm, "client_id": "pando"},
		"credentials": map[string]string{"client_secret": "s3cret"},
	})
	var p struct {
		ID string `json:"id"`
	}
	created.JSON(t, &p)

	// A browser that reaches notes.localtest.me at Pando's listener, as DNS
	// would send it there.
	listener := mustURL(pando).Host
	b := newBrowser(t)
	b.client.Transport = &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		if strings.HasPrefix(addr, "notes.localtest.me:") || strings.HasPrefix(addr, "evil.example:") {
			addr = listener
		}
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}}
	app := "http://notes.localtest.me:" + mustURL(pando).Port()

	b.get(app + "/.pando/api/v1/auth/providers/" + p.ID + "/start?next=/dashboard")
	b.through("alice", "alice-password")
	require.Equal(t, "notes.localtest.me:"+mustURL(pando).Port(), b.last.Request.URL.Host, "back on the app's hostname")
	require.Equal(t, "/dashboard", b.last.Request.URL.Path)
	require.NotEmpty(t, b.cookie(app, httpapi.SessionCookie), "the session is on the app's hostname")
	require.Empty(t, b.cookie(pando, httpapi.SessionCookie), "and nowhere else")

	// A hostname Pando does not serve cannot be where a sign-in finishes.
	b.get("http://evil.example:" + mustURL(pando).Port() + "/.pando/api/v1/auth/providers/" + p.ID + "/start")
	require.Equal(t, http.StatusBadRequest, b.last.StatusCode, b.body)
}
