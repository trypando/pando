package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// fakeGitHub answers the calls creating and installing an App makes: an
// owner's type, the manifest's exchange for code "good", and installations 7
// (on acme) and 8 (on someone else) of the App it created.
func fakeGitHub(t *testing.T, keyPEM string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/users/acme":
			_, _ = w.Write([]byte(`{"type":"Organization"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/users/bob":
			_, _ = w.Write([]byte(`{"type":"User"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/app-manifests/good/conversions":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("the exchange carried a credential: %s", r.Header.Get("Authorization"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": 42, "slug": "pando-acme", "pem": keyPEM, "webhook_secret": "whsec",
				"owner":     map[string]string{"login": "acme", "type": "Organization"},
				"client_id": "Iv1.x", "client_secret": "not kept",
			})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/app/installations/"):
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			switch strings.TrimPrefix(r.URL.Path, "/app/installations/") {
			case "7":
				_, _ = w.Write([]byte(`{"id":7,"account":{"login":"Acme"}}`))
			case "8":
				_, _ = w.Write([]byte(`{"id":8,"account":{"login":"someone-else"}}`))
			default:
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func appRequest() api.AuthorizationRequest {
	return api.AuthorizationRequest{
		Mode:        api.AuthorizationWeb,
		State:       "src_gh.abc",
		RedirectURL: "https://pando.acme.com/api/v1/sources/callback",
		SetupURL:    "https://pando.acme.com/api/v1/sources/src_gh/installed",
		WebhookURL:  "https://pando.acme.com/api/v1/sources/src_gh/webhook",
		HomepageURL: "https://pando.acme.com",
	}
}

func readManifest(t *testing.T, a api.Authorization) manifest {
	t.Helper()
	require.NotNil(t, a.Form, "creating an App is a form the browser posts")
	var m manifest
	require.NoError(t, json.Unmarshal([]byte(a.Form.Fields["manifest"]), &m))
	return m
}

// TestR091_AGitHubAppIsCreatedFromAManifestAndInstalled asserts the GitHub App
// method needs nothing copied by hand: authorizing a connection with no App
// posts a manifest asking only to read contents, exchanges the code GitHub
// returns for the App's ID and key, and keeps an installation only once the
// App's own token reads it as one of its own on the connection's owner.
func TestR091_AGitHubAppIsCreatedFromAManifestAndInstalled(t *testing.T) {
	_, keyPEM := rsaKey(t)
	srv := fakeGitHub(t, keyPEM)
	ctx := context.Background()
	cfg := map[string]any{"method": "app", "scope": "acme", "api_url": srv.URL}

	// Not created: configured, not authorized, and authorized in a browser only.
	a := mustConfigure(t, cfg)
	caps := a.SourceCapabilities()
	require.False(t, caps.Authorized)
	require.True(t, caps.WebAuthorization)
	require.False(t, caps.DeviceAuthorization)
	require.False(t, caps.Webhook)
	_, err := a.BeginAuthorization(ctx, api.AuthorizationRequest{Mode: api.AuthorizationDevice})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	begun, err := a.BeginAuthorization(ctx, appRequest())
	require.NoError(t, err)
	require.Equal(t, "https://github.com/organizations/acme/settings/apps/new?state=src_gh.abc", begun.Form.Action,
		"an organization's App is created from the organization's settings")
	require.Equal(t, flowManifest, begun.Flow.Reveal())
	m := readManifest(t, begun)
	require.Equal(t, map[string]string{"contents": "read", "metadata": "read"}, m.DefaultPermissions)
	require.False(t, m.Public)
	require.Equal(t, "Pando pando-acme-com", m.Name)
	require.Equal(t, appRequest().RedirectURL, m.RedirectURL)
	require.Equal(t, appRequest().SetupURL, m.SetupURL)
	require.NotNil(t, m.HookAttributes, "the push webhook is on unless turned off")
	require.Equal(t, appRequest().WebhookURL, m.HookAttributes.URL)
	require.ElementsMatch(t, []string{"push", "create", "release"}, m.DefaultEvents)

	// The code GitHub returned is exchanged for the App, and installing it is next.
	_, err = a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationWeb, Flow: begun.Flow, Code: "stale"})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "good for an hour")
	res, err := a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationWeb, Flow: begun.Flow, Code: "good"})
	require.NoError(t, err)
	require.Equal(t, "https://github.com/apps/pando-acme/installations/new", res.ContinueURL)
	require.Equal(t, "42", res.Credentials[FieldAppID].Reveal())
	require.Equal(t, "pando-acme", res.Credentials[FieldAppSlug].Reveal())
	require.Equal(t, keyPEM, res.Credentials[FieldAppPrivateKey].Reveal())
	require.Equal(t, "whsec", res.Credentials[api.CredentialWebhookSecret].Reveal())
	require.NotContains(t, res.Credentials, "client_secret", "only what Pando uses is kept")

	// Created: still not authorized until it is installed, and authorizing
	// it again opens the installation page.
	cfg["credentials"] = revealed(res.Credentials)
	a = mustConfigure(t, cfg)
	caps = a.SourceCapabilities()
	require.True(t, caps.Webhook)
	require.Equal(t, "https://github.com/organizations/acme/settings/apps/pando-acme", caps.ManageURL,
		"where the App's logo is uploaded")

	// Deploy on push turned off: Pando no longer acts on the App's webhook.
	cfg["push_webhook"] = false
	require.False(t, mustConfigure(t, cfg).SourceCapabilities().Webhook)
	delete(cfg, "push_webhook")
	require.True(t, caps.WebAuthorization)
	again, err := a.BeginAuthorization(ctx, appRequest())
	require.NoError(t, err)
	require.Nil(t, again.Form)
	require.Equal(t, "https://github.com/apps/pando-acme/installations/new?state=src_gh.abc", again.AuthorizeURL,
		"with the state GitHub hands on to the setup address")

	// The installation's ID came through the browser: one that is not this
	// App's, or is on another owner, is refused.
	for id, want := range map[string]string{"99": "not an installation of this connection's GitHub App",
		"8": "installed on someone-else", "x": "without the ID"} {
		_, err := a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationInstall, Code: id})
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), id)
		require.Contains(t, err.Error(), want, id)
	}
	done, err := a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationInstall, Code: "7"})
	require.NoError(t, err)
	require.Equal(t, map[string]secret.Value{FieldInstallationID: secret.New("7")}, done.Credentials)

	cfg["credentials"].(map[string]string)[FieldInstallationID] = "7"
	a = mustConfigure(t, cfg)
	require.True(t, a.SourceCapabilities().Authorized)
	require.Positive(t, a.Covers("https://github.com/acme/api"))
}

// revealed is credentials as core hands them to Configure.
func revealed(creds map[string]secret.Value) map[string]string {
	out := map[string]string{}
	for k, v := range creds {
		out[k] = v.Reveal()
	}
	return out
}

// TestTheAppIsCreatedWhereThePersonIsAndWithoutAWebhookGitHubCannotReach
// asserts the manifest's two variations: a user's App is created from the
// person's own settings, and an App is given no webhook when Pando has no
// address GitHub can reach or the person turned it off.
func TestTheAppIsCreatedWhereThePersonIsAndWithoutAWebhookGitHubCannotReach(t *testing.T) {
	_, keyPEM := rsaKey(t)
	srv := fakeGitHub(t, keyPEM)
	ctx := context.Background()

	for owner, action := range map[string]string{
		"bob":     "https://github.com/settings/apps/new",
		"":        "https://github.com/settings/apps/new",
		"unknown": "https://github.com/organizations/unknown/settings/apps/new",
	} {
		a := mustConfigure(t, map[string]any{"method": "app", "scope": owner, "api_url": srv.URL})
		begun, err := a.BeginAuthorization(ctx, appRequest())
		require.NoError(t, err)
		u, err := url.Parse(begun.Form.Action)
		require.NoError(t, err)
		require.Equal(t, action, u.Scheme+"://"+u.Host+u.Path, owner)
		require.Equal(t, "src_gh.abc", u.Query().Get("state"))
	}

	unreachable := appRequest()
	unreachable.WebhookURL = ""
	a := mustConfigure(t, map[string]any{"method": "app", "api_url": srv.URL})
	begun, err := a.BeginAuthorization(ctx, unreachable)
	require.NoError(t, err)
	m := readManifest(t, begun)
	require.Nil(t, m.HookAttributes)
	require.Empty(t, m.DefaultEvents)

	a = mustConfigure(t, map[string]any{"method": "app", "api_url": srv.URL, "push_webhook": false})
	begun, err = a.BeginAuthorization(ctx, appRequest())
	require.NoError(t, err)
	require.Nil(t, readManifest(t, begun).HookAttributes, "turned off, the App has no webhook")
}

// TestAppNameFollowsGitHubsRules asserts the App's name keeps to what GitHub
// documents: at most 34 characters, letters, digits, spaces and hyphens, and
// never plain "Pando", an account GitHub already has.
func TestAppNameFollowsGitHubsRules(t *testing.T) {
	for addr, want := range map[string]string{
		"https://pando.acme.com":                        "Pando pando-acme-com",
		"https://a-very-long-subdomain.example-corp.io": "Pando a-very-long-subdomain-exampl",
	} {
		require.Equal(t, want, appName(addr), addr)
	}
	allowed := regexp.MustCompile(`^[A-Za-z0-9 -]+$`)
	for _, addr := range []string{"http://localhost:8080", "http://127.0.0.1:8080", "http://203.0.113.4", "",
		"https://pando.acme.com", "https://a-very-long-subdomain.example-corp.io"} {
		got := appName(addr)
		require.LessOrEqual(t, len(got), maxAppName, addr)
		require.Regexp(t, allowed, got, addr)
		require.NotEqual(t, "Pando", got, addr)
	}
	require.Regexp(t, `^Pando [0-9a-f]{6}$`, appName("http://localhost:8080"))
	require.NotEqual(t, appName("http://localhost:8080"), appName("http://localhost:8080"),
		"two installations on localhost are not given one name")
}

// TestAnAppNotCreatedOrEnteredByHandSaysWhatToDo asserts what a connection
// whose App cannot be used yet answers instead of a token.
func TestAnAppNotCreatedOrEnteredByHandSaysWhatToDo(t *testing.T) {
	_, keyPEM := rsaKey(t)
	ctx := context.Background()

	a := mustConfigure(t, map[string]any{"method": "app"})
	_, err := a.GitCredential(ctx, "https://github.com/acme/api")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, err.Error(), "not been created")
	_, err = a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationInstall, Code: "7"})
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	_, err = a.CompleteAuthorization(ctx, api.AuthorizationCompletion{Mode: api.AuthorizationWeb, Flow: secret.New(flowInstall)})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "an installation does not come back to the callback")

	created := mustConfigure(t, map[string]any{"method": "app",
		"credentials": map[string]string{FieldAppID: "1", FieldAppSlug: "p", FieldAppPrivateKey: keyPEM}})
	require.Equal(t, "https://github.com/settings/apps/p", created.SourceCapabilities().ManageURL,
		"an App owned by a person is under their own settings")
	_, err = created.GitCredential(ctx, "https://github.com/acme/api")
	require.Contains(t, err.Error(), "not installed yet")

	// Entered by hand before Pando created Apps: it still works, and has no
	// installation page Pando can open.
	byHand := mustConfigure(t, map[string]any{"method": "app", "app_id": "1", "installation_id": "42",
		"credentials": map[string]string{FieldAppPrivateKey: keyPEM}})
	require.True(t, byHand.SourceCapabilities().Authorized)
	require.Empty(t, byHand.SourceCapabilities().ManageURL)
	require.False(t, byHand.SourceCapabilities().WebAuthorization)
	_, err = byHand.BeginAuthorization(ctx, appRequest())
	require.Contains(t, err.Error(), "entered by hand")
}
