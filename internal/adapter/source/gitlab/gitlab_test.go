package gitlab

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

const (
	tokenSecret   = "glpat-s3cret-token"
	clientSecret  = "gloas-client-s3cret"
	accessSecret  = "access-s3cret"
	refreshSecret = "refresh-s3cret"
)

var secrets = []string{tokenSecret, clientSecret, accessSecret, refreshSecret}

func TestInfoValidates(t *testing.T) {
	if err := Info().Validate(); err != nil {
		t.Fatal(err)
	}
}

func configure(t *testing.T, cfg map[string]any) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := New()
	return a, a.Configure(context.Background(), raw)
}

func mustConfigure(t *testing.T, cfg map[string]any) *Adapter {
	t.Helper()
	a, err := configure(t, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// point sends the adapter's web and API calls to srv.
func point(a *Adapter, srv *httptest.Server) {
	a.web = srv.URL
	a.api = srv.URL + "/api/v4"
}

func noSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	msg := err.Error()
	if e := errs.As(err); e != nil {
		msg += " " + e.Remedy
	}
	for _, s := range secrets {
		if strings.Contains(msg, s) {
			t.Fatalf("error carries a secret: %s", msg)
		}
	}
}

func testKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestCoversNestedGroupScope(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme/platform",
		"credentials": map[string]string{"token": tokenSecret},
	})
	for u, want := range map[string]int{
		"https://gitlab.com/acme/platform/api.git":      3,
		"https://gitlab.com/ACME/Platform/api":          3,
		"https://gitlab.com/acme/platform/team/api.git": 3,
		"git@gitlab.com:acme/platform/api.git":          3,
		"https://gitlab.com/acme/other/api.git":         0,
		"https://gitlab.com/acme/platform":              0,
		"https://gitlab.com/acme/api.git":               0,
		"https://gitlab.example.com/acme/platform/api":  0,
		"not an address":                                0,
	} {
		if got := a.Covers(u); got != want {
			t.Errorf("Covers(%q) = %d, want %d", u, got, want)
		}
	}

	whole := mustConfigure(t, map[string]any{
		"method": "token", "credentials": map[string]string{"token": tokenSecret},
	})
	if got := whole.Covers("https://gitlab.com/acme/platform/api"); got != 1 {
		t.Errorf("an unscoped connection scored %d, want 1", got)
	}
	selfManaged := mustConfigure(t, map[string]any{
		"method": "token", "host": "https://gitlab.acme.internal:8443",
		"credentials": map[string]string{"token": tokenSecret},
	})
	if got := selfManaged.Covers("https://gitlab.acme.internal:8443/acme/api.git"); got != 1 {
		t.Errorf("a self-managed connection scored %d, want 1", got)
	}
	if got := selfManaged.Covers("https://gitlab.com/acme/api.git"); got != 0 {
		t.Errorf("a self-managed connection covered gitlab.com: %d", got)
	}
}

func TestAccessTokenCredential(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "credentials": map[string]string{"token": tokenSecret},
	})
	cred, err := a.GitCredential(context.Background(), "https://gitlab.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "oauth2" || cred.Password.Reveal() != tokenSecret || cred.URL != "" {
		t.Fatalf("credential = %+v", cred)
	}
	if !a.SourceCapabilities().ListRepositories || !a.SourceCapabilities().Authorized {
		t.Fatalf("capabilities = %+v", a.SourceCapabilities())
	}
	_, err = a.GitCredential(context.Background(), "git@gitlab.com:acme/api.git")
	if err == nil {
		t.Fatal("a token connection handed out a credential for an SSH address")
	}
	noSecret(t, err)
}

func TestDeployTokenNeedsUsernameAndCannotList(t *testing.T) {
	_, err := configure(t, map[string]any{
		"method": "token", "token_type": "deploy_token",
		"credentials": map[string]string{"token": tokenSecret},
	})
	if err == nil {
		t.Fatal("a deploy token connection was configured without its username")
	}
	noSecret(t, err)
	if _, err := configure(t, map[string]any{
		"method": "token", "token_type": "sideways",
		"credentials": map[string]string{"token": tokenSecret},
	}); err == nil {
		t.Fatal("an unknown token type was accepted")
	}

	a := mustConfigure(t, map[string]any{
		"method": "token", "token_type": "deploy_token", "username": "gitlab+deploy-token-12",
		"credentials": map[string]string{"token": tokenSecret},
	})
	cred, err := a.GitCredential(context.Background(), "https://gitlab.com/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "gitlab+deploy-token-12" || cred.Password.Reveal() != tokenSecret {
		t.Fatalf("credential = %+v", cred)
	}
	if a.SourceCapabilities().ListRepositories {
		t.Fatal("a deploy token connection claims it can list repositories")
	}
	_, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err == nil || !strings.Contains(err.Error(), "deploy token") {
		t.Fatalf("listing on a deploy token: %v", err)
	}
	if _, err := a.ListBranches(context.Background(), "https://gitlab.com/acme/api"); err == nil {
		t.Fatal("listing branches on a deploy token succeeded")
	}
}

func TestSSHKnownHostsDefaultForGitLabCom(t *testing.T) {
	key := testKey(t)
	a := mustConfigure(t, map[string]any{
		"method": "ssh", "credentials": map[string]string{"ssh_private_key": key},
	})
	cred, err := a.GitCredential(context.Background(), "https://gitlab.com/acme/platform/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "git@gitlab.com:acme/platform/api.git" || cred.SSHUser != "git" ||
		!strings.Contains(cred.KnownHosts, "gitlab.com ssh-ed25519 ") || cred.SSHPrivateKey.Reveal() != key {
		t.Fatalf("credential = %+v", cred)
	}
	if c := a.SourceCapabilities(); c.ListRepositories || c.DeviceAuthorization || c.WebAuthorization || !c.Authorized {
		t.Fatalf("capabilities = %+v", c)
	}

	if _, err := configure(t, map[string]any{
		"method": "ssh", "host": "gitlab.acme.internal",
		"credentials": map[string]string{"ssh_private_key": key},
	}); err == nil {
		t.Fatal("a self-managed SSH connection was configured without known hosts")
	}
}

func TestOAuthNeedsClientIDAndAuthorization(t *testing.T) {
	if _, err := configure(t, map[string]any{"method": "oauth"}); err == nil {
		t.Fatal("an OAuth connection was configured without a client ID")
	}
	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id"})
	c := a.SourceCapabilities()
	if c.Authorized || c.ListRepositories || !c.DeviceAuthorization || c.WebAuthorization {
		t.Fatalf("capabilities before authorization = %+v", c)
	}
	if got := a.Covers("https://gitlab.com/acme/api"); got != 0 {
		t.Fatalf("an unauthorized connection covers a repository: %d", got)
	}
	if _, err := a.GitCredential(context.Background(), "https://gitlab.com/acme/api"); err == nil {
		t.Fatal("an unauthorized connection handed out a credential")
	}
	if _, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "st",
	}); err == nil {
		t.Fatal("web authorization began without a client secret")
	}
}

func TestOAuthDeviceFlow(t *testing.T) {
	polls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/oauth/authorize_device":
			if r.Form.Get("client_id") != "app-id" || r.Form.Get("scope") != "read_repository read_api" {
				t.Errorf("device request form = %v", r.Form)
			}
			_, _ = fmt.Fprint(w, `{"device_code":"dev-code","user_code":"ABCD-EFGH","verification_uri":"https://gitlab.com/oauth/device","expires_in":300,"interval":5}`)
		case "/oauth/token":
			if r.Form.Get("device_code") != "dev-code" ||
				r.Form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" {
				t.Errorf("token request form = %v", r.Form)
			}
			polls++
			if polls == 1 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprint(w, `{"error":"authorization_pending"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":7200,"token_type":"Bearer"}`, accessSecret, refreshSecret)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id"})
	point(a, srv)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }

	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice})
	if err != nil {
		t.Fatal(err)
	}
	if auth.UserCode != "ABCD-EFGH" || auth.VerificationURL != "https://gitlab.com/oauth/device" ||
		auth.Flow.Reveal() != "dev-code" || auth.Interval != 5*time.Second || !auth.ExpiresAt.Equal(now.Add(5*time.Minute)) {
		t.Fatalf("authorization = %+v", auth)
	}

	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Pending || res.Credentials != nil {
		t.Fatalf("first poll = %+v", res)
	}
	res, err = a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pending || res.Credentials["access_token"].Reveal() != accessSecret ||
		res.Credentials["refresh_token"].Reveal() != refreshSecret || !res.ExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("second poll = %+v", res)
	}
}

func TestOAuthWebFlow(t *testing.T) {
	var verifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/oauth/token" {
			http.NotFound(w, r)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" ||
			r.Form.Get("code_verifier") != verifier || r.Form.Get("client_secret") != clientSecret ||
			r.Form.Get("redirect_uri") != "https://pando.example/cb" {
			t.Errorf("exchange form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":7200}`, accessSecret, refreshSecret)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "oauth", "client_id": "app-id", "credentials": map[string]string{"client_secret": clientSecret},
	})
	if !a.SourceCapabilities().WebAuthorization {
		t.Fatal("web authorization not offered with a client secret")
	}
	point(a, srv)
	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "state-123",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(auth.AuthorizeURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if u.Path != "/oauth/authorize" || q.Get("state") != "state-123" || q.Get("code_challenge") == "" ||
		q.Get("code_challenge_method") != "S256" || q.Get("client_id") != "app-id" ||
		q.Get("scope") != "read_repository read_api" {
		t.Fatalf("authorize URL = %s", auth.AuthorizeURL)
	}
	if strings.Contains(auth.AuthorizeURL, clientSecret) {
		t.Fatal("the authorize URL carries the client secret")
	}
	verifier = auth.Flow.Reveal()
	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: auth.Flow, Code: "the-code", RedirectURL: "https://pando.example/cb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials["access_token"].Reveal() != accessSecret {
		t.Fatalf("result = %+v", res)
	}
}

func TestOAuthRefreshOnExpirySetsRotated(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != refreshSecret {
			t.Errorf("refresh form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":7200}`)
	}))
	defer srv.Close()

	creds := map[string]string{
		"access_token": accessSecret, "refresh_token": refreshSecret,
		"token_expires_at": now.Add(-time.Minute).Format(time.RFC3339),
	}
	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id", "credentials": creds})
	point(a, srv)
	a.now = func() time.Time { return now }

	cred, err := a.GitCredential(context.Background(), "https://gitlab.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "oauth2" || cred.Password.Reveal() != "new-access" || !cred.ExpiresAt.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.Rotated["access_token"].Reveal() != "new-access" || cred.Rotated["refresh_token"].Reveal() != "new-refresh" ||
		cred.Rotated["token_expires_at"].Reveal() != now.Add(2*time.Hour).Format(time.RFC3339) {
		t.Fatalf("rotated = %v", cred.Rotated)
	}

	// Not expired: no call, nothing rotated.
	creds["token_expires_at"] = now.Add(time.Hour).Format(time.RFC3339)
	b := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id", "credentials": creds})
	b.web = "http://127.0.0.1:1" // would fail if called
	b.now = func() time.Time { return now }
	cred, err = b.GitCredential(context.Background(), "https://gitlab.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Rotated != nil || cred.Password.Reveal() != accessSecret {
		t.Fatalf("credential = %+v", cred)
	}

	// Expired with no refresh token: an error naming what to do.
	delete(creds, "refresh_token")
	creds["token_expires_at"] = now.Add(-time.Hour).Format(time.RFC3339)
	c := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "app-id", "credentials": creds})
	c.now = func() time.Time { return now }
	_, err = c.GitCredential(context.Background(), "https://gitlab.com/acme/api.git")
	if err == nil {
		t.Fatal("an expired token with no refresh token produced a credential")
	}
	noSecret(t, err)
	if _, err := c.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err == nil {
		t.Fatal("listing with an expired token succeeded")
	}
}

func projects(from, n int, prefix string) string {
	var out []string
	for i := from; i < from+n; i++ {
		out = append(out, fmt.Sprintf(`{"path_with_namespace":"%s/p%d","http_url_to_repo":"https://gitlab.com/%s/p%d.git","default_branch":"main","visibility":%q}`,
			prefix, i, prefix, i, map[bool]string{true: "public", false: "private"}[i%2 == 0]))
	}
	return "[" + strings.Join(out, ",") + "]"
}

func TestListRepositoriesScopedFollowsLink(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != tokenSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.EscapedPath() != "/api/v4/groups/acme%2Fplatform/projects" {
			t.Errorf("path = %s", r.URL.EscapedPath())
			http.NotFound(w, r)
			return
		}
		q := r.URL.Query()
		if q.Get("include_subgroups") != "true" || q.Get("search") != "api" || q.Has("simple") || q.Get("per_page") != "100" {
			t.Errorf("query = %v", q)
		}
		if q.Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/groups/acme%%2Fplatform/projects?include_subgroups=true&page=2&per_page=100&search=api>; rel="next"`, srv.URL))
			_, _ = fmt.Fprint(w, projects(0, 2, "acme/platform"))
			return
		}
		_, _ = fmt.Fprint(w, projects(2, 2, "acme/platform"))
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme/platform", "credentials": map[string]string{"token": tokenSecret},
	})
	point(a, srv)
	repos, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Query: "api", Limit: 3})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 3 {
		t.Fatalf("got %d repositories, want 3 (the limit): %+v", len(repos), repos)
	}
	want := api.Repository{URL: "https://gitlab.com/acme/platform/p1.git", FullName: "acme/platform/p1", DefaultBranch: "main", Private: true}
	if repos[1] != want || repos[0].Private || repos[2].FullName != "acme/platform/p2" {
		t.Fatalf("repositories = %+v", repos)
	}
}

func TestListRepositoriesUnscopedPagesByNumber(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Authorization") != "Bearer "+accessSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		q := r.URL.Query()
		if r.URL.Path != "/api/v4/projects" || q.Get("membership") != "true" || q.Get("order_by") != "last_activity_at" || q.Has("search") {
			t.Errorf("request = %s", r.URL)
		}
		switch q.Get("page") {
		case "":
			// A full page with no Link: ask for page 2. A Link to another
			// host is not followed.
			w.Header().Set("Link", `<https://evil.example/api/v4/projects?page=2>; rel="next"`)
			_, _ = fmt.Fprint(w, projects(0, 100, "acme"))
		case "2":
			_, _ = fmt.Fprint(w, projects(100, 5, "acme"))
		default:
			t.Errorf("unexpected page %s", q.Get("page"))
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "oauth", "client_id": "app-id", "credentials": map[string]string{"access_token": accessSecret},
	})
	point(a, srv)
	repos, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Limit: 500})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 105 || calls != 2 {
		t.Fatalf("got %d repositories in %d calls", len(repos), calls)
	}
}

func TestListBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/acme%2Fplatform%2Fapi/repository/branches" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("request = %s", r.URL)
		}
		_, _ = fmt.Fprint(w, `[{"name":"main"},{"name":"release/1.0"}]`)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tokenSecret}})
	point(a, srv)
	names, err := a.ListBranches(context.Background(), "git@gitlab.com:acme/platform/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "main,release/1.0" {
		t.Fatalf("branches = %v", names)
	}
	if _, err := a.ListBranches(context.Background(), "https://github.com/acme/api"); err == nil {
		t.Fatal("listed branches for a repository on another host")
	}
}

func TestUnauthorizedIsActionableAndCarriesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tokenSecret}})
	point(a, srv)
	_, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err == nil {
		t.Fatal("a 401 was not an error")
	}
	e := errs.As(err)
	if e == nil || e.Code != errs.AdapterFailed || !strings.Contains(e.Message, "did not accept") || e.Remedy == "" {
		t.Fatalf("error = %v", err)
	}
	noSecret(t, err)
	_, err = a.ListBranches(context.Background(), "https://gitlab.com/acme/api")
	if err == nil {
		t.Fatal("a 401 was not an error")
	}
	noSecret(t, err)
}

func TestSecretsDoNotRender(t *testing.T) {
	a := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tokenSecret}})
	if s := fmt.Sprintf("%+v %v", a.cfg, a.cfg.Credentials); strings.Contains(s, tokenSecret) {
		t.Fatal("the token rendered")
	}
}
