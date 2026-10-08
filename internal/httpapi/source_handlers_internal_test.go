package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/generic"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// --- fakes ------------------------------------------------------------------

type srcConfigs []state.AdapterConfig

func (c srcConfigs) List(context.Context) ([]state.AdapterConfig, error) { return c, nil }

// srcCreds is a credential store in memory.
type srcCreds map[string]map[string]secret.Value

func (c srcCreds) Resolve(_ context.Context, id string) (map[string]secret.Value, error) {
	out := map[string]secret.Value{}
	for k, v := range c[id] {
		out[k] = v
	}
	return out, nil
}

func (c srcCreds) Put(_ context.Context, id, field string, v secret.Value) error {
	if c[id] == nil {
		c[id] = map[string]secret.Value{}
	}
	c[id][field] = v
	return nil
}

func (c srcCreds) Delete(_ context.Context, id, field string) error {
	delete(c[id], field)
	return nil
}

// forge is an OAuth source adapter for forge.example. It is authorized once
// it holds an access token, finishes a device authorization on the second
// poll, and puts the callback address it was given in its authorize URL.
type forge struct {
	cfg struct {
		Credentials map[string]secret.Value `json:"credentials"`
	}
	polls  *int
	listed *api.ListRepositoriesRequest
}

func (f *forge) Kind() string                      { return "forge" }
func (f *forge) Category() api.Category            { return api.CategorySource }
func (f *forge) HealthCheck(context.Context) error { return nil }
func (f *forge) Configure(_ context.Context, raw json.RawMessage) error {
	return json.Unmarshal(raw, &f.cfg)
}
func (f *forge) SourceCapabilities() api.SourceCapabilities {
	return api.SourceCapabilities{Method: "oauth", Host: "forge.example", ListRepositories: true,
		DeviceAuthorization: true, WebAuthorization: true,
		Authorized: !f.cfg.Credentials["access_token"].IsZero()}
}
func (f *forge) Covers(u string) int {
	if strings.Contains(u, "forge.example") {
		return 1
	}
	return 0
}
func (f *forge) GitCredential(context.Context, string) (api.GitCredential, error) {
	return api.GitCredential{Password: f.cfg.Credentials["access_token"]}, nil
}
func (f *forge) Refresh(context.Context) (map[string]secret.Value, error) { return nil, nil }
func (f *forge) ListRepositories(_ context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	*f.listed = req
	return []api.Repository{{URL: "https://forge.example/acme/api", FullName: "acme/api"}}, nil
}
func (f *forge) ListBranches(_ context.Context, u string) ([]string, error) {
	if strings.HasSuffix(u, "/empty") {
		return nil, nil
	}
	return []string{"main", "dev"}, nil
}
func (f *forge) BeginAuthorization(_ context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if req.Mode == api.AuthorizationWeb {
		return api.Authorization{Mode: req.Mode, Flow: secret.New("verifier"),
			AuthorizeURL: "https://forge.example/authorize?" + url.Values{
				"state": {req.State}, "redirect_uri": {req.RedirectURL}}.Encode()}, nil
	}
	return api.Authorization{Mode: req.Mode, UserCode: "ABCD-1234", VerificationURL: "https://forge.example/device",
		Flow: secret.New("device-code")}, nil
}
func (f *forge) CompleteAuthorization(_ context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	*f.polls++
	if req.Mode == api.AuthorizationDevice && *f.polls == 1 {
		return api.AuthorizationResult{Pending: true}, nil
	}
	return api.AuthorizationResult{Credentials: map[string]secret.Value{"access_token": secret.New("good")}}, nil
}

func srcRow(id, kind string, cfg string) state.AdapterConfig {
	return state.AdapterConfig{ID: id, Category: string(api.CategorySource), Kind: kind, Name: id,
		Config: json.RawMessage(cfg), Enabled: true}
}

type sourcesFixture struct {
	srv    *Server
	store  srcCreds
	listed api.ListRepositoriesRequest
}

// newSourcesFixture is a server with two connections: src_forge, an OAuth
// connection not yet authorized, and src_git, a token connection to
// git.example.com.
func newSourcesFixture() *sourcesFixture {
	f := &sourcesFixture{store: srcCreds{"src_git": {"token": secret.New("t")}}}
	polls := new(int)
	conns := &sourceconn.Service{
		Configs: srcConfigs{
			srcRow("src_forge", "forge", `{}`),
			srcRow("src_git", generic.Kind, `{"method":"token","host":"git.example.com"}`),
		},
		Credentials:    f.store,
		Authorizations: srcCreds{},
		New: func(kind string) api.SourceAdapter {
			switch kind {
			case "forge":
				return &forge{polls: polls, listed: &f.listed}
			case generic.Kind:
				return generic.New()
			}
			return nil
		},
	}
	f.srv = &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil), SourceConnections: conns}
	return f
}

// serve calls one handler as the router would: with the route's parameters
// and the principal the authentication middleware would have set.
func serve(h http.HandlerFunc, p authz.Principal, method, target, body string, params map[string]string) *httptest.ResponseRecorder {
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, target, nil)
	} else {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	}
	rc := chi.NewRouteContext()
	for k, v := range params {
		rc.URLParams.Add(k, v)
	}
	ctx := context.WithValue(r.Context(), chi.RouteCtxKey, rc)
	ctx = context.WithValue(ctx, ctxKeyPrincipal{}, p)
	w := httptest.NewRecorder()
	h(w, r.WithContext(ctx))
	return w
}

func asSystem(h http.HandlerFunc, method, target, body string, params map[string]string) *httptest.ResponseRecorder {
	return serve(h, authz.System(), method, target, body, params)
}

func body(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out), w.Body.String())
	return out
}

func requireCode(t *testing.T, w *httptest.ResponseRecorder, code errs.Code) map[string]any {
	t.Helper()
	b := body(t, w)
	require.Equal(t, string(code), b["code"], w.Body.String())
	return b
}

var forgeID = map[string]string{"sourceID": "src_forge"}

// --- tests ------------------------------------------------------------------

// TestR091_SourceConnectionsAreListedForWhoeverAddsApps asserts the listing:
// every connection, whether it can be used, and an empty list as a list.
func TestR091_SourceConnectionsAreListedForWhoeverAddsApps(t *testing.T) {
	f := newSourcesFixture()
	w := asSystem(f.srv.handleListSources, http.MethodGet, "/api/v1/sources", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	var got struct {
		Sources []struct {
			ID           string `json:"id"`
			Capabilities struct {
				Authorized bool `json:"authorized"`
			} `json:"capabilities"`
		} `json:"sources"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.Sources, 2)
	require.Equal(t, "src_forge", got.Sources[0].ID)
	require.False(t, got.Sources[0].Capabilities.Authorized)
	require.True(t, got.Sources[1].Capabilities.Authorized)

	f.srv.SourceConnections = &sourceconn.Service{Configs: srcConfigs{}}
	w = asSystem(f.srv.handleListSources, http.MethodGet, "/api/v1/sources", "", nil)
	require.JSONEq(t, `{"sources":[]}`, w.Body.String())

	// Anonymous is asked to sign in.
	w = serve(f.srv.handleListSources, authz.Anonymous(), http.MethodGet, "/api/v1/sources", "", nil)
	requireCode(t, w, errs.AuthRequired)
}

// TestR091_ARepositoryIsPickedThroughItsConnection asserts listing a
// connection's repositories and a repository's branches, and the refusals.
func TestR091_ARepositoryIsPickedThroughItsConnection(t *testing.T) {
	f := newSourcesFixture()
	f.store["src_forge"] = map[string]secret.Value{"access_token": secret.New("good")}

	w := asSystem(f.srv.handleListSourceRepositories, http.MethodGet,
		"/api/v1/sources/src_forge/repositories?q=api&limit=5", "", forgeID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"repositories":[{"url":"https://forge.example/acme/api","full_name":"acme/api","private":false}]}`,
		w.Body.String())
	require.Equal(t, api.ListRepositoriesRequest{Query: "api", Limit: 5}, f.listed)

	w = asSystem(f.srv.handleListSourceRepositories, http.MethodGet, "/api/v1/sources/src_gone/repositories", "",
		map[string]string{"sourceID": "src_gone"})
	requireCode(t, w, errs.NotFound)

	w = asSystem(f.srv.handleListSourceBranches, http.MethodGet,
		"/api/v1/sources/src_forge/branches?url="+url.QueryEscape("https://forge.example/acme/api"), "", forgeID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"branches":["main","dev"]}`, w.Body.String())

	w = asSystem(f.srv.handleListSourceBranches, http.MethodGet,
		"/api/v1/sources/src_forge/branches?url="+url.QueryEscape("https://forge.example/acme/empty"), "", forgeID)
	require.JSONEq(t, `{"branches":[]}`, w.Body.String(), "no branches is an empty list, not null")

	w = asSystem(f.srv.handleListSourceBranches, http.MethodGet, "/api/v1/sources/src_forge/branches", "", forgeID)
	requireCode(t, w, errs.ValidInvalid)
}

// TestR091_AConnectionIsAuthorizedWithADeviceCode asserts beginning a device
// authorization — the default when no mode is given — and polling it.
func TestR091_AConnectionIsAuthorizedWithADeviceCode(t *testing.T) {
	f := newSourcesFixture()

	w := asSystem(f.srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize", `{}`, forgeID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	b := body(t, w)
	require.Equal(t, "device", b["mode"])
	require.Equal(t, "ABCD-1234", b["user_code"])

	w = asSystem(f.srv.handlePollSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize/poll", "", forgeID)
	require.JSONEq(t, `{"status":"pending"}`, w.Body.String())
	w = asSystem(f.srv.handlePollSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize/poll", "", forgeID)
	require.JSONEq(t, `{"status":"authorized"}`, w.Body.String())
	require.Equal(t, "good", f.store["src_forge"]["access_token"].Reveal())

	w = asSystem(f.srv.handlePollSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize/poll", "", forgeID)
	requireCode(t, w, errs.StateInvalid)

	w = asSystem(f.srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize", `nope`, forgeID)
	b = requireCode(t, w, errs.ValidInvalid)
	require.Contains(t, b["remedy"], `"mode": "device"`)

	// A token connection has nothing to authorize, and says so.
	w = asSystem(f.srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_git/authorize",
		`{"mode":"device"}`, map[string]string{"sourceID": "src_git"})
	requireCode(t, w, errs.ValidInvalid)
}

// TestR091_ABrowserAuthorizationReturnsToTheConsole asserts the web flow:
// the provider is sent Pando's own callback, and the callback redirects to
// the console saying how it went — authorized, refused by the provider, or
// not an authorization Pando started.
func TestR091_ABrowserAuthorizationReturnsToTheConsole(t *testing.T) {
	f := newSourcesFixture()
	f.srv.ExternalURL = &url.URL{Scheme: "https", Host: "pando.example", Path: "/somewhere", RawQuery: "x=1"}

	w := asSystem(f.srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_forge/authorize", `{"mode":"web"}`, forgeID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	authorize, err := url.Parse(body(t, w)["authorize_url"].(string))
	require.NoError(t, err)
	require.Equal(t, "https://pando.example/api/v1/sources/callback", authorize.Query().Get("redirect_uri"))
	stateParam := authorize.Query().Get("state")

	location := func(w *httptest.ResponseRecorder) url.Values {
		t.Helper()
		require.Equal(t, http.StatusFound, w.Code, w.Body.String())
		u, err := url.Parse(w.Header().Get("Location"))
		require.NoError(t, err)
		require.Equal(t, sourcesPage, u.Path)
		return u.Query()
	}

	q := location(asSystem(f.srv.handleSourceCallback, http.MethodGet, "/api/v1/sources/callback?error=access_denied", "", nil))
	require.Contains(t, q.Get("error"), "did not authorize the connection (access_denied)")

	q = location(asSystem(f.srv.handleSourceCallback, http.MethodGet, "/api/v1/sources/callback?state=forged&code=c", "", nil))
	require.Contains(t, q.Get("error"), "not one Pando started")

	q = location(asSystem(f.srv.handleSourceCallback, http.MethodGet,
		"/api/v1/sources/callback?"+url.Values{"state": {stateParam}, "code": {"c"}}.Encode(), "", nil))
	require.Equal(t, "src_forge", q.Get("authorized"))
	require.Empty(t, q.Get("error"))
	require.Equal(t, "good", f.store["src_forge"]["access_token"].Reveal())

	// Signing in is still required to finish one.
	w = serve(f.srv.handleSourceCallback, authz.Anonymous(), http.MethodGet, "/api/v1/sources/callback?state=x", "", nil)
	requireCode(t, w, errs.AuthRequired)
}

// The callback is the external address when one is set, and otherwise the
// address the request came in on.
func TestTheSourceCallbackIsOnTheAddressABrowserReachesPandoBy(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "http://pando.local:8080/api/v1/sources/x/authorize", nil)
	require.Equal(t, "http://pando.local:8080/api/v1/sources/callback", (&Server{}).sourceCallbackURL(r))

	s := &Server{ExternalURL: &url.URL{Scheme: "https", Host: "pando.example", Path: "/x", RawQuery: "a=b"}}
	require.Equal(t, "https://pando.example/api/v1/sources/callback", s.sourceCallbackURL(r))

	// An external address with no host is not one.
	s = &Server{ExternalURL: &url.URL{Path: "/relative"}}
	require.Equal(t, "http://pando.local:8080/api/v1/sources/callback", s.sourceCallbackURL(r))
}

// TestR091_AnInstallationWithoutSourceConnectionsSaysSo asserts every source
// route on a server without the service answers with a reason rather than
// failing on a nil.
func TestR091_AnInstallationWithoutSourceConnectionsSaysSo(t *testing.T) {
	s := &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil)}
	for name, h := range map[string]http.HandlerFunc{
		"list":         s.handleListSources,
		"repositories": s.handleListSourceRepositories,
		"branches":     s.handleListSourceBranches,
		"authorize":    s.handleBeginSourceAuthorization,
		"poll":         s.handlePollSourceAuthorization,
		"callback":     s.handleSourceCallback,
		"delete":       s.handleDeleteSource,
	} {
		w := asSystem(h, http.MethodPost, "/api/v1/sources/src_forge", `{}`, forgeID)
		b := requireCode(t, w, errs.StateInvalid)
		require.Contains(t, b["message"], "cannot use source connections", name)
	}

	w := serve(s.handleDeleteSource, authz.Anonymous(), http.MethodDelete, "/api/v1/sources/src_forge", "", forgeID)
	requireCode(t, w, errs.AuthRequired)
}

// An app's creation records the connection its repository is read with, and
// nothing for one read anonymously or not built from a repository.
func TestAnAppsCreationRecordsItsSourceConnection(t *testing.T) {
	require.Equal(t, map[string]any{"source_connection": "src_git"},
		createDetail(spec.Source{Type: spec.SourceGit, CredentialRef: "src_git"}))
	require.Nil(t, createDetail(spec.Source{Type: spec.SourceGit}))
	require.Nil(t, createDetail(spec.Source{Type: spec.SourceImage, CredentialRef: registryCredentialRef}))
}

// TestR091_CreatingAnAppRefusesAConnectionItCannotUse asserts the checks
// creation makes of a source connection before the app exists: none for a
// source that is not a repository, and the service's refusal returned as it
// is.
func TestR091_CreatingAnAppRefusesAConnectionItCannotUse(t *testing.T) {
	f := newSourcesFixture()

	w := asSystem(f.srv.handleCreateApp, http.MethodPost, "/api/v1/apps",
		`{"name":"web","source":{"type":"image","image":"nginx:1","connection":"src_git"}}`, nil)
	b := requireCode(t, w, errs.ValidInvalid)
	require.Contains(t, b["message"], "not built from one")

	w = asSystem(f.srv.handleCreateApp, http.MethodPost, "/api/v1/apps",
		`{"name":"web","source":{"type":"git","url":"https://git.example.com/a/b.git","connection":"src_gone"}}`, nil)
	requireCode(t, w, errs.NotFound)

	w = asSystem(f.srv.handleCreateApp, http.MethodPost, "/api/v1/apps",
		`{"name":"web","source":{"type":"git","url":"https://bob:ghp_x@git.example.com/a/b.git"}}`, nil)
	b = requireCode(t, w, errs.ValidInvalid)
	require.NotContains(t, w.Body.String(), "ghp_x", "R-190")
	require.Contains(t, b["message"], "password or token")
}

// TestR091_AConnectionWhoseSettingsAreRefusedIsNotSaved asserts that saving a
// source connection configures it first and returns the adapter's refusal.
func TestR091_AConnectionWhoseSettingsAreRefusedIsNotSaved(t *testing.T) {
	f := newSourcesFixture()
	w := asSystem(f.srv.handleCreateAdapter, http.MethodPost, "/api/v1/adapters",
		`{"id":"src_new","category":"source","kind":"git","is_default":true,"config":{"method":"token","host":"git.example.com"}}`, nil)
	b := requireCode(t, w, errs.ValidInvalid)
	require.Contains(t, b["message"], "needs a token")
}
