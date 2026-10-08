package bitbucket

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
	tok          = "bb-s3cret-token"
	clientSecret = "consumer-s3cret"
)

var fixedNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func configure(t *testing.T, cfg map[string]any, oauthBase string) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := New()
	a.now = func() time.Time { return fixedNow }
	if oauthBase != "" {
		a.oauthBase = oauthBase
	}
	return a, a.Configure(context.Background(), raw)
}

func mustConfigure(t *testing.T, cfg map[string]any, oauthBase string) *Adapter {
	t.Helper()
	a, err := configure(t, cfg, oauthBase)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func noSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	for _, s := range []string{tok, clientSecret, "access-1", "refresh-1"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q contains a secret", err)
		}
	}
}

func testKey(t *testing.T, host string) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(block)), host + " " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestInfoValidates(t *testing.T) {
	if err := Info().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestCloudCoversEveryAddressForm(t *testing.T) {
	host := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tok}}, "")
	ws := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "credentials": map[string]string{"token": tok}}, "")
	for u, want := range map[string][2]int{
		"https://bitbucket.org/acme/api.git":           {1, 2},
		"https://alice@bitbucket.org/acme/api.git":     {1, 2},
		"https://bitbucket.org/ACME/api":               {1, 2},
		"https://bitbucket.org/acme/api/src/main/x.go": {1, 2},
		"git@bitbucket.org:acme/api.git":               {1, 2},
		"https://bitbucket.org/other/api.git":          {1, 0},
		"https://bitbucket.org/acme":                   {0, 0},
		"https://git.acme.internal/scm/pay/api.git":    {0, 0},
		"https://github.com/acme/api.git":              {0, 0},
		"not an address":                               {0, 0},
	} {
		if got := host.Covers(u); got != want[0] {
			t.Errorf("host connection: Covers(%q) = %d, want %d", u, got, want[0])
		}
		if got := ws.Covers(u); got != want[1] {
			t.Errorf("workspace connection: Covers(%q) = %d, want %d", u, got, want[1])
		}
	}
}

func TestDataCenterCoversEveryAddressForm(t *testing.T) {
	a := mustConfigure(t, map[string]any{"method": "token", "host": "git.acme.internal", "scope": "PAY",
		"credentials": map[string]string{"token": tok}}, "")
	for u, want := range map[string]int{
		"https://git.acme.internal/scm/pay/api.git":               2,
		"https://alice@git.acme.internal/scm/PAY/api.git":         2,
		"https://git.acme.internal/bitbucket/scm/pay/api.git":     2,
		"https://git.acme.internal/projects/PAY/repos/api/browse": 2,
		"ssh://git@git.acme.internal:7999/pay/api.git":            2,
		"https://git.acme.internal/scm/ops/api.git":               0,
		"https://git.acme.internal/pay/api.git":                   0,
		"https://bitbucket.org/pay/api.git":                       0,
		"ssh://git@git.acme.internal:7999/pay":                    0,
	} {
		if got := a.Covers(u); got != want {
			t.Errorf("Covers(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestTokenCredentials(t *testing.T) {
	ctx := context.Background()
	access := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tok}}, "")
	cred, err := access.GitCredential(ctx, "https://bitbucket.org/acme/api.git")
	if err != nil || cred.Username != "x-token-auth" || cred.Password.Reveal() != tok {
		t.Fatalf("access token credential = %+v, %v", cred, err)
	}

	if _, err := configure(t, map[string]any{"method": "token", "token_type": "api_token",
		"credentials": map[string]string{"token": tok}}, ""); err == nil {
		t.Fatal("an API token was configured without a username")
	}
	apiTok := mustConfigure(t, map[string]any{"method": "token", "token_type": "api_token", "username": "alice",
		"credentials": map[string]string{"token": tok}}, "")
	cred, err = apiTok.GitCredential(ctx, "https://bitbucket.org/acme/api.git")
	if err != nil || cred.Username != "alice" || cred.Password.Reveal() != tok {
		t.Fatalf("API token credential = %+v, %v", cred, err)
	}

	dc := mustConfigure(t, map[string]any{"method": "token", "host": "git.acme.internal", "credentials": map[string]string{"token": tok}}, "")
	cred, err = dc.GitCredential(ctx, "https://git.acme.internal/scm/pay/api.git")
	if err != nil || cred.Username != "x-token-auth" {
		t.Fatalf("Data Center credential = %+v, %v", cred, err)
	}
	dcUser := mustConfigure(t, map[string]any{"method": "token", "host": "git.acme.internal", "username": "bob",
		"credentials": map[string]string{"token": tok}}, "")
	if cred, _ := dcUser.GitCredential(ctx, "https://git.acme.internal/scm/pay/api.git"); cred.Username != "bob" {
		t.Fatalf("Data Center username = %q", cred.Username)
	}

	_, err = access.GitCredential(ctx, "git@bitbucket.org:acme/api.git")
	if err == nil {
		t.Fatal("a token connection handed out a credential for an SSH address")
	}
	noSecret(t, err)
}

func TestSSHCredentials(t *testing.T) {
	ctx := context.Background()
	key, known := testKey(t, "bitbucket.org")
	if _, err := configure(t, map[string]any{"method": "ssh", "credentials": map[string]string{"ssh_private_key": key}}, ""); err == nil {
		t.Fatal("an SSH connection was configured without known hosts")
	}
	cloud := mustConfigure(t, map[string]any{"method": "ssh", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key}}, "")
	cred, err := cloud.GitCredential(ctx, "https://bitbucket.org/acme/api.git")
	if err != nil || cred.URL != "git@bitbucket.org:acme/api.git" || cred.KnownHosts != known {
		t.Fatalf("Cloud SSH credential = %+v, %v", cred, err)
	}

	_, dcKnown := testKey(t, "[git.acme.internal]:7999")
	dc := mustConfigure(t, map[string]any{"method": "ssh", "host": "git.acme.internal", "known_hosts": dcKnown,
		"credentials": map[string]string{"ssh_private_key": key}}, "")
	cred, err = dc.GitCredential(ctx, "https://git.acme.internal/scm/pay/api.git")
	if err != nil || cred.URL != "ssh://git@git.acme.internal:7999/pay/api.git" {
		t.Fatalf("Data Center SSH credential = %+v, %v", cred, err)
	}
	dc2 := mustConfigure(t, map[string]any{"method": "ssh", "host": "git.acme.internal", "known_hosts": dcKnown, "ssh_port": "2222",
		"credentials": map[string]string{"ssh_private_key": key}}, "")
	if cred, _ := dc2.GitCredential(ctx, "https://git.acme.internal/scm/pay/api.git"); cred.URL != "ssh://git@git.acme.internal:2222/pay/api.git" {
		t.Fatalf("custom port URL = %q", cred.URL)
	}
	if cred, _ := dc.GitCredential(ctx, "ssh://git@git.acme.internal:7999/pay/api.git"); cred.URL != "" {
		t.Fatalf("an SSH address was rewritten to %q", cred.URL)
	}
	if dc.SourceCapabilities().ListRepositories {
		t.Fatal("an SSH connection claims it can list repositories")
	}
}

func TestOAuthIsCloudOnly(t *testing.T) {
	_, err := configure(t, map[string]any{"method": "oauth", "host": "git.acme.internal", "client_id": "k",
		"credentials": map[string]string{"client_secret": clientSecret}}, "")
	if err == nil || !strings.Contains(errs.As(err).Message, "Data Center") {
		t.Fatalf("err = %v", err)
	}
	noSecret(t, err)
	if _, err := configure(t, map[string]any{"method": "oauth", "client_id": "k"}, ""); err == nil {
		t.Fatal("an OAuth connection was configured without a consumer secret")
	}
}

func oauthServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/access_token" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		id, sec, ok := r.BasicAuth()
		if !ok || id != "key-1" || sec != clientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"Invalid OAuth client credentials"}`))
			return
		}
		_ = r.ParseForm()
		w.Header().Set("Content-Type", "application/json")
		switch r.PostForm.Get("grant_type") {
		case "authorization_code":
			if r.PostForm.Get("code") != "the-code" || r.PostForm.Get("code_verifier") == "" {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
				return
			}
			_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","expires_in":7200,"token_type":"bearer"}`))
		case "refresh_token":
			_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-1","expires_in":7200}`))
		}
	}))
}

func TestOAuthWebFlowUsesBasicClientAuth(t *testing.T) {
	srv := oauthServer(t)
	defer srv.Close()
	cfg := map[string]any{"method": "oauth", "scope": "acme", "client_id": "key-1",
		"credentials": map[string]string{"client_secret": clientSecret}}
	a := mustConfigure(t, cfg, srv.URL)
	caps := a.SourceCapabilities()
	if !caps.WebAuthorization || caps.DeviceAuthorization || caps.Authorized {
		t.Fatalf("capabilities = %+v", caps)
	}
	if _, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice}); err == nil {
		t.Fatal("Bitbucket began a device authorization")
	}
	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "st"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth.AuthorizeURL, srv.URL+"/authorize?") || !strings.Contains(auth.AuthorizeURL, "client_id=key-1") {
		t.Fatalf("authorize URL = %s", auth.AuthorizeURL)
	}
	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: auth.Flow, Code: "the-code", RedirectURL: "https://pando.example/cb"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials["access_token"].Reveal() != "access-1" || !res.ExpiresAt.Equal(fixedNow.Add(2*time.Hour)) {
		t.Fatalf("result = %+v", res)
	}

	cfg["credentials"] = map[string]string{"client_secret": "wrong"}
	bad := mustConfigure(t, cfg, srv.URL)
	_, err = bad.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: auth.Flow, Code: "the-code", RedirectURL: "https://pando.example/cb"})
	if err == nil {
		t.Fatal("a wrong consumer secret completed the authorization")
	}
	noSecret(t, err)
}

func TestOAuthRefreshSetsRotated(t *testing.T) {
	srv := oauthServer(t)
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "key-1",
		"credentials": map[string]string{
			"client_secret": clientSecret, "access_token": "access-1", "refresh_token": "refresh-1",
			"token_expires_at": fixedNow.Add(-time.Minute).Format(time.RFC3339),
		}}, srv.URL)
	if a.Covers("https://bitbucket.org/acme/api.git") != 1 {
		t.Fatal("an authorized OAuth connection does not cover its host")
	}
	cred, err := a.GitCredential(context.Background(), "https://bitbucket.org/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "x-token-auth" || cred.Password.Reveal() != "access-2" || cred.Rotated["access_token"].Reveal() != "access-2" {
		t.Fatalf("credential = %+v", cred)
	}
}

// cloudAPI is a fake Bitbucket Cloud API with two pages of repositories.
func cloudAPIServer(t *testing.T, auth func(*http.Request) bool) *httptest.Server {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !auth(r) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		switch r.URL.Path {
		case "/repositories/acme", "/repositories":
			if q.Get("role") != "member" || q.Get("pagelen") != "100" {
				t.Errorf("query = %v", q)
			}
			if r.URL.Path == "/repositories/acme" && q.Get("q") != "" && q.Get("q") != `name~"ap"` {
				t.Errorf("q = %q", q.Get("q"))
			}
			if q.Get("page") == "2" {
				_, _ = w.Write([]byte(`{"values":[{"full_name":"acme/web","is_private":false,"mainbranch":{"name":"trunk"},
					"links":{"clone":[{"name":"https","href":"https://alice@bitbucket.org/acme/web.git"}]}}]}`))
				return
			}
			next := srv.URL + r.URL.Path + "?" + q.Encode() + "&page=2"
			if r.URL.Path == "/repositories" {
				next = "https://evil.example/repositories?page=2"
			}
			_, _ = fmt.Fprintf(w, `{"values":[{"full_name":"acme/api","is_private":true,"mainbranch":{"name":"main"},
				"links":{"clone":[{"name":"ssh","href":"git@bitbucket.org:acme/api.git"},{"name":"https","href":"https://alice@bitbucket.org/acme/api.git"}]}}],
				"next":%q}`, next)
		case "/repositories/acme/api/refs/branches":
			if q.Get("page") == "2" {
				_, _ = w.Write([]byte(`{"values":[{"name":"dev"}]}`))
				return
			}
			_, _ = fmt.Fprintf(w, `{"values":[{"name":"main"}],"next":%q}`, srv.URL+r.URL.Path+"?pagelen=100&page=2")
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv
}

func TestCloudListing(t *testing.T) {
	srv := cloudAPIServer(t, func(r *http.Request) bool { return r.Header.Get("Authorization") == "Bearer "+tok })
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	ctx := context.Background()

	repos, err := a.ListRepositories(ctx, api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Repository{
		{URL: "https://bitbucket.org/acme/api.git", FullName: "acme/api", DefaultBranch: "main", Private: true},
		{URL: "https://bitbucket.org/acme/web.git", FullName: "acme/web", DefaultBranch: "trunk", Private: false},
	}
	if len(repos) != 2 || repos[0] != want[0] || repos[1] != want[1] {
		t.Fatalf("repositories = %+v", repos)
	}
	if repos, err := a.ListRepositories(ctx, api.ListRepositoriesRequest{Limit: 1}); err != nil || len(repos) != 1 {
		t.Fatalf("limit = %+v, %v", repos, err)
	}
	if repos, err := a.ListRepositories(ctx, api.ListRepositoriesRequest{Query: "ap"}); err != nil || len(repos) != 1 || repos[0].FullName != "acme/api" {
		t.Fatalf("query = %+v, %v", repos, err)
	}

	// Without a workspace, the next page points elsewhere and is not followed.
	whole := mustConfigure(t, map[string]any{"method": "token", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	if repos, err := whole.ListRepositories(ctx, api.ListRepositoriesRequest{}); err != nil || len(repos) != 1 {
		t.Fatalf("cross-origin next = %+v, %v", repos, err)
	}

	branches, err := a.ListBranches(ctx, "https://bitbucket.org/acme/api.git")
	if err != nil || strings.Join(branches, ",") != "main,dev" {
		t.Fatalf("branches = %v, %v", branches, err)
	}
}

func TestCloudAPITokenUsesBasicAuth(t *testing.T) {
	srv := cloudAPIServer(t, func(r *http.Request) bool {
		u, p, ok := r.BasicAuth()
		return ok && u == "alice@example.com" && p == tok
	})
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "token_type": "api_token", "username": "alice",
		"email": "alice@example.com", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	if _, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err != nil {
		t.Fatal(err)
	}
}

func TestDataCenterListing(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+tok {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		q := r.URL.Query()
		if q.Get("limit") != "100" {
			t.Errorf("limit = %q", q.Get("limit"))
		}
		switch r.URL.Path {
		case "/projects/PAY/repos", "/repos":
			if r.URL.Path == "/repos" && q.Get("name") != "web" {
				t.Errorf("name = %q", q.Get("name"))
			}
			if q.Get("start") == "25" {
				_, _ = w.Write([]byte(`{"values":[{"slug":"web","public":true,"project":{"key":"PAY"},
					"links":{"clone":[{"name":"http","href":"https://bob@git.acme.internal/scm/pay/web.git"}]}}],"isLastPage":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"values":[{"slug":"api","public":false,"project":{"key":"PAY"},
				"links":{"clone":[{"name":"ssh","href":"ssh://git@git.acme.internal:7999/pay/api.git"}]}}],"isLastPage":false,"nextPageStart":25}`))
		case "/projects/PAY/repos/api/branches":
			if q.Get("start") == "1" {
				_, _ = w.Write([]byte(`{"values":[{"displayId":"release/1"}],"isLastPage":true}`))
				return
			}
			_, _ = w.Write([]byte(`{"values":[{"displayId":"master"}],"isLastPage":false,"nextPageStart":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	a := mustConfigure(t, map[string]any{"method": "token", "host": "git.acme.internal", "scope": "pay", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	repos, err := a.ListRepositories(ctx, api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	want := []api.Repository{
		{URL: "https://git.acme.internal/scm/pay/api.git", FullName: "PAY/api", Private: true},
		{URL: "https://git.acme.internal/scm/pay/web.git", FullName: "PAY/web", Private: false},
	}
	if len(repos) != 2 || repos[0] != want[0] || repos[1] != want[1] {
		t.Fatalf("repositories = %+v", repos)
	}

	whole := mustConfigure(t, map[string]any{"method": "token", "host": "git.acme.internal", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	if repos, err := whole.ListRepositories(ctx, api.ListRepositoriesRequest{Query: "web"}); err != nil || len(repos) != 1 {
		t.Fatalf("query = %+v, %v", repos, err)
	}

	branches, err := a.ListBranches(ctx, "https://git.acme.internal/scm/pay/api.git")
	if err != nil || strings.Join(branches, ",") != "master,release/1" {
		t.Fatalf("branches = %v, %v", branches, err)
	}
	if _, err := a.ListBranches(ctx, "https://git.acme.internal/scm/ops/api.git"); err == nil {
		t.Fatal("listed branches of a repository the connection is not for")
	}
}

func TestRefusedTokenIsActionable(t *testing.T) {
	srv := cloudAPIServer(t, func(*http.Request) bool { return false })
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": tok}}, "")
	_, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	e := errs.As(err)
	if e == nil || !strings.Contains(e.Message, "did not accept") || e.Remedy == "" {
		t.Fatalf("err = %v", err)
	}
	noSecret(t, err)
	if _, err := a.ListBranches(context.Background(), "https://bitbucket.org/acme/api.git"); err == nil {
		t.Fatal("a refused token listed branches")
	} else {
		noSecret(t, err)
	}
}

func TestStripUser(t *testing.T) {
	if got := stripUser("https://alice@bitbucket.org/acme/api.git"); got != "https://bitbucket.org/acme/api.git" {
		t.Fatal(got)
	}
	if sameOrigin("https://evil.example/x", "https://api.bitbucket.org/2.0") {
		t.Fatal("a different host counted as the same origin")
	}
	if _, err := url.Parse("https://api.bitbucket.org/2.0"); err != nil {
		t.Fatal(err)
	}
}
