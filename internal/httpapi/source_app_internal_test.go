package httpapi

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/github"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/errs"
)

// appFixture is a server with one GitHub App connection, src_gh, for the
// organization acme, whose App is not created yet, against a GitHub that
// creates App 42 for code "good" and knows its installation 7 on acme.
func appFixture(t *testing.T) (*Server, srcCreds) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	keyPEM := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users/acme":
			_, _ = w.Write([]byte(`{"type":"Organization"}`))
		case "/app-manifests/good/conversions":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 42, "slug": "pando-acme", "pem": keyPEM, "webhook_secret": "whsec"})
		case "/app/installations/7":
			_, _ = w.Write([]byte(`{"id":7,"account":{"login":"acme"}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(gh.Close)

	store := srcCreds{}
	conns := &sourceconn.Service{
		Configs:        srcConfigs{srcRow("src_gh", github.Kind, `{"method":"app","scope":"acme","api_url":"`+gh.URL+`"}`)},
		Credentials:    store,
		Authorizations: srcCreds{},
		New: func(kind string) api.SourceAdapter {
			if kind == github.Kind {
				return github.New()
			}
			return nil
		},
	}
	srv := &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil), SourceConnections: conns,
		ExternalURL: &url.URL{Scheme: "https", Host: "pando.acme.com"}}
	return srv, store
}

// TestR091_AGitHubAppIsConnectedWithoutCopyingAnythingByHand asserts the
// browser's whole trip: the authorize answer is a form for GitHub with the
// addresses Pando is reached at, and a console page for a client that cannot
// post one; GitHub's return exchanges the code and goes on to the
// installation; and the installation's return is kept and lands on the
// console, the connection ready.
func TestR091_AGitHubAppIsConnectedWithoutCopyingAnythingByHand(t *testing.T) {
	srv, store := appFixture(t)
	ghID := map[string]string{"sourceID": "src_gh"}

	w := asSystem(srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_gh/authorize", `{"mode":"web"}`, ghID)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var begun struct {
		AuthorizeURL string `json:"authorize_url"`
		Form         struct {
			Action string            `json:"action"`
			Fields map[string]string `json:"fields"`
		} `json:"form"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &begun))
	require.Equal(t, "https://pando.acme.com/admin/system/adapters?authorize=src_gh", begun.AuthorizeURL)
	action, err := url.Parse(begun.Form.Action)
	require.NoError(t, err)
	require.Equal(t, "/organizations/acme/settings/apps/new", action.Path)
	state := action.Query().Get("state")
	require.True(t, strings.HasPrefix(state, "src_gh."))
	var manifest struct {
		RedirectURL    string `json:"redirect_url"`
		SetupURL       string `json:"setup_url"`
		HookAttributes struct {
			URL string `json:"url"`
		} `json:"hook_attributes"`
	}
	require.NoError(t, json.Unmarshal([]byte(begun.Form.Fields["manifest"]), &manifest))
	require.Equal(t, "https://pando.acme.com/api/v1/sources/callback", manifest.RedirectURL)
	require.Equal(t, "https://pando.acme.com/api/v1/sources/src_gh/installed", manifest.SetupURL)
	require.Equal(t, "https://pando.acme.com/api/v1/sources/src_gh/webhook", manifest.HookAttributes.URL)

	// GitHub created the App: the code is exchanged, and the browser goes on
	// to install it.
	w = asSystem(srv.handleSourceCallback, http.MethodGet,
		"/api/v1/sources/callback?"+url.Values{"state": {state}, "code": {"good"}}.Encode(), "", nil)
	require.Equal(t, http.StatusFound, w.Code, w.Body.String())
	next, err := url.Parse(w.Header().Get("Location"))
	require.NoError(t, err)
	installState := next.Query().Get("state")
	require.NotEmpty(t, installState, "the installation page carries a state GitHub hands back")
	next.RawQuery = ""
	require.Equal(t, "https://github.com/apps/pando-acme/installations/new", next.String())
	require.Equal(t, "42", store["src_gh"]["app_id"].Reveal())
	require.False(t, store["src_gh"]["app_private_key"].IsZero())
	c, err := srv.SourceConnections.Get(t.Context(), "src_gh")
	require.NoError(t, err)
	require.False(t, c.Capabilities.Authorized, "created and not installed")

	back := func(w *httptest.ResponseRecorder) url.Values {
		t.Helper()
		require.Equal(t, http.StatusFound, w.Code, w.Body.String())
		u, err := url.Parse(w.Header().Get("Location"))
		require.NoError(t, err)
		require.Equal(t, sourcesPage, u.Path)
		return u.Query()
	}
	// A link to the setup address without the state, as somebody else could
	// send an administrator, changes nothing (R-091).
	q := back(asSystem(srv.handleSourceInstalled, http.MethodGet, "/api/v1/sources/src_gh/installed?installation_id=7", "", ghID))
	require.Contains(t, q.Get("error"), "not one Pando started")
	require.True(t, store["src_gh"]["installation_id"].IsZero())

	installed := func(id string) string {
		return "/api/v1/sources/src_gh/installed?" + url.Values{
			"installation_id": {id}, "setup_action": {"install"}, "state": {installState},
		}.Encode()
	}
	q = back(asSystem(srv.handleSourceInstalled, http.MethodGet, installed("9"), "", ghID))
	require.Contains(t, q.Get("error"), "not an installation of this connection's GitHub App")
	require.True(t, store["src_gh"]["installation_id"].IsZero())

	q = back(asSystem(srv.handleSourceInstalled, http.MethodGet, installed("7"), "", ghID))
	require.Equal(t, "src_gh", q.Get("authorized"))
	require.Equal(t, "7", store["src_gh"]["installation_id"].Reveal())
	c, err = srv.SourceConnections.Get(t.Context(), "src_gh")
	require.NoError(t, err)
	require.True(t, c.Usable())
	require.True(t, c.Capabilities.Webhook)

	secret, found, err := srv.SourceConnections.WebhookSecret(t.Context(), "src_gh")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "whsec", secret.Reveal())

	// Signing in is required to finish an installation.
	w = serve(srv.handleSourceInstalled, authz.Anonymous(), http.MethodGet, "/api/v1/sources/src_gh/installed?installation_id=7", "", ghID)
	requireCode(t, w, errs.AuthRequired)
}

// TestTheAppIsGivenNoWebhookOnAPrivateAddress asserts that an installation
// GitHub cannot reach creates its App without a webhook, which GitHub would
// refuse, and the App with it.
func TestTheAppIsGivenNoWebhookOnAPrivateAddress(t *testing.T) {
	srv, _ := appFixture(t)
	srv.ExternalURL = &url.URL{Scheme: "http", Host: "localhost:8080"}
	w := asSystem(srv.handleBeginSourceAuthorization, http.MethodPost, "/api/v1/sources/src_gh/authorize", `{"mode":"web"}`,
		map[string]string{"sourceID": "src_gh"})
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.NotContains(t, w.Body.String(), "hook_attributes")
	require.Contains(t, w.Body.String(), `http://localhost:8080/api/v1/sources/src_gh/installed`)
}
