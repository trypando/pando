package github

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
)

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

func rsaKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return k, string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}))
}

func sshKey(t *testing.T) string {
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// checkAPIHeaders fails the request unless it carries GitHub's headers and
// the expected bearer token.
func checkAPIHeaders(t *testing.T, w http.ResponseWriter, r *http.Request, token string) bool {
	t.Helper()
	if r.Header.Get("Authorization") != "Bearer "+token ||
		r.Header.Get("Accept") != "application/vnd.github+json" ||
		r.Header.Get("X-GitHub-Api-Version") != "2022-11-28" {
		w.WriteHeader(http.StatusUnauthorized)
		return false
	}
	return true
}

func TestCoversMatchesHostAndOwner(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme",
		"credentials": map[string]string{"token": "ghp_secret"},
	})
	whole := mustConfigure(t, map[string]any{
		"method": "token", "credentials": map[string]string{"token": "ghp_secret"},
	})
	for url, want := range map[string]int{
		"https://github.com/acme/api.git":       2,
		"https://GitHub.com/ACME/Api":           2,
		"git@github.com:acme/api.git":           2,
		"ssh://git@github.com/acme/api.git":     2,
		"https://github.com/other/api.git":      0,
		"https://github.com/acme":               0,
		"https://gitlab.com/acme/api.git":       0,
		"https://github.acme.internal/acme/api": 0,
		"not an address":                        0,
	} {
		if got := a.Covers(url); got != want {
			t.Errorf("Covers(%q) = %d, want %d", url, got, want)
		}
	}
	if got := whole.Covers("https://github.com/other/api"); got != 1 {
		t.Errorf("a whole-host connection scored %d, want 1", got)
	}
	if a.Covers("https://github.com/acme/api") <= whole.Covers("https://github.com/acme/api") {
		t.Error("the owner-scoped connection does not outrank the whole-host one")
	}
	if _, err := configure(t, map[string]any{
		"method": "token", "scope": "acme/api", "credentials": map[string]string{"token": "x"},
	}); err == nil {
		t.Error("a GitHub connection accepted a scope deeper than an owner")
	}
}

func TestTokenCredential(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "credentials": map[string]string{"token": "ghp_secret"},
	})
	cred, err := a.GitCredential(context.Background(), "https://github.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "x-access-token" || cred.Password.Reveal() != "ghp_secret" || cred.URL != "" {
		t.Fatalf("credential = %+v", cred)
	}
	if strings.Contains(fmt.Sprintf("%v %+v", cred, cred), "ghp_secret") {
		t.Fatal("the token rendered in a string")
	}

	// An SSH address is cloned over HTTPS with the token.
	cred, err = a.GitCredential(context.Background(), "git@github.com:acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "https://github.com/acme/api.git" {
		t.Fatalf("URL = %q", cred.URL)
	}

	if _, err := configure(t, map[string]any{"method": "token"}); err == nil {
		t.Fatal("a token connection was configured without a token")
	}
	caps := a.SourceCapabilities()
	if !caps.ListRepositories || !caps.Authorized || caps.DeviceAuthorization || caps.Host != "github.com" {
		t.Fatalf("capabilities = %+v", caps)
	}
}

func TestSSHCredentialUsesGitHubsKeysOnGitHubCom(t *testing.T) {
	key := sshKey(t)
	a := mustConfigure(t, map[string]any{
		"method": "ssh", "credentials": map[string]string{"ssh_private_key": key},
	})
	cred, err := a.GitCredential(context.Background(), "https://github.com/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "git@github.com:acme/api.git" || cred.SSHUser != "git" || cred.KnownHosts != githubKnownHosts {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.SSHPrivateKey.Reveal() != key {
		t.Fatal("the key was not handed over")
	}
	if a.SourceCapabilities().ListRepositories {
		t.Fatal("a deploy key connection claims it can list repositories")
	}
	if _, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err == nil {
		t.Fatal("listing repositories with a deploy key succeeded")
	}

	// GitHub Enterprise Server publishes nothing Pando knows; the host key
	// must be given.
	if _, err := configure(t, map[string]any{
		"method": "ssh", "host": "github.acme.internal",
		"credentials": map[string]string{"ssh_private_key": key},
	}); err == nil {
		t.Fatal("a GHES SSH connection was configured without known hosts")
	}
	known := "github.acme.internal ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	ghes := mustConfigure(t, map[string]any{
		"method": "ssh", "host": "github.acme.internal", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key},
	})
	cred, err = ghes.GitCredential(context.Background(), "https://github.acme.internal/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "git@github.acme.internal:acme/api.git" || cred.KnownHosts != known {
		t.Fatalf("credential = %+v", cred)
	}
}

// TestGitHubKnownHostsMatchPublishedFingerprints checks the embedded host
// keys against the fingerprints GitHub publishes in its documentation.
func TestGitHubKnownHostsMatchPublishedFingerprints(t *testing.T) {
	want := map[string]string{
		"ssh-ed25519":         "SHA256:+DiY3wvvV6TuJJhbpZisF/zLDA0zPMSvHdkr4UvCOqU",
		"ecdsa-sha2-nistp256": "SHA256:p2QAMXNIC1TJYWeIOttrVc98/R1BUFWu3/LiyKgUfQM",
		"ssh-rsa":             "SHA256:uNiVztksCsDhcc0u9e8BujQXVUpKZIDTMczCvj3tD2s",
	}
	rest := []byte(githubKnownHosts)
	seen := 0
	for len(strings.TrimSpace(string(rest))) > 0 {
		_, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if err != nil {
			t.Fatal(err)
		}
		if len(hosts) != 1 || hosts[0] != "github.com" {
			t.Fatalf("hosts = %v", hosts)
		}
		got := ssh.FingerprintSHA256(key)
		if got != want[key.Type()] {
			t.Errorf("%s fingerprint = %s, want %s", key.Type(), got, want[key.Type()])
		}
		seen++
		rest = next
	}
	if seen != len(want) {
		t.Fatalf("parsed %d keys, want %d", seen, len(want))
	}
}

func TestAppMintsAnInstallationTokenFoundByOwner(t *testing.T) {
	key, keyPEM := rsaKey(t)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	expires := now.Add(time.Hour)

	verify := func(w http.ResponseWriter, r *http.Request) bool {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		tok, err := jwt.ParseSigned(raw, []jose.SignatureAlgorithm{jose.RS256})
		if err != nil {
			t.Errorf("app token: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		var claims jwt.Claims
		if err := tok.Claims(&key.PublicKey, &claims); err != nil {
			t.Errorf("app token signature: %v", err)
			w.WriteHeader(http.StatusUnauthorized)
			return false
		}
		if claims.Issuer != "12345" {
			t.Errorf("iss = %q", claims.Issuer)
		}
		if err := claims.ValidateWithLeeway(jwt.Expected{Time: now}, 0); err != nil {
			t.Errorf("app token claims: %v", err)
		}
		if exp := claims.Expiry.Time(); exp.Sub(now) > 10*time.Minute || !exp.After(now) {
			t.Errorf("exp = %s, not within GitHub's ten minutes", exp)
		}
		if iat := claims.IssuedAt.Time(); !iat.Before(now) {
			t.Errorf("iat = %s, not backdated", iat)
		}
		return true
	}

	var orgLookups, userLookups, mints atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orgs/acme/installation", func(w http.ResponseWriter, r *http.Request) {
		orgLookups.Add(1)
		if verify(w, r) {
			w.WriteHeader(http.StatusNotFound)
		}
	})
	mux.HandleFunc("GET /users/acme/installation", func(w http.ResponseWriter, r *http.Request) {
		userLookups.Add(1)
		if verify(w, r) {
			writeJSON(w, map[string]any{"id": 777})
		}
	})
	mux.HandleFunc("POST /app/installations/777/access_tokens", func(w http.ResponseWriter, r *http.Request) {
		mints.Add(1)
		if verify(w, r) {
			writeJSON(w, map[string]any{"token": "ghs_installation", "expires_at": expires.Format(time.RFC3339)})
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "app", "scope": "acme", "app_id": "12345", "api_url": srv.URL,
		"credentials": map[string]string{"app_private_key": keyPEM},
	})
	a.now = func() time.Time { return now }

	cred, err := a.GitCredential(context.Background(), "https://github.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "x-access-token" || cred.Password.Reveal() != "ghs_installation" {
		t.Fatalf("credential = %+v", cred)
	}
	if !cred.ExpiresAt.Equal(expires) {
		t.Fatalf("ExpiresAt = %s, want %s", cred.ExpiresAt, expires)
	}
	if orgLookups.Load() != 1 || userLookups.Load() != 1 || mints.Load() != 1 {
		t.Fatalf("lookups org=%d user=%d mints=%d", orgLookups.Load(), userLookups.Load(), mints.Load())
	}

	// Stateless: a second credential mints again rather than reusing one.
	if _, err := a.GitCredential(context.Background(), "https://github.com/acme/api.git"); err != nil {
		t.Fatal(err)
	}
	if mints.Load() != 2 {
		t.Fatalf("mints = %d, want 2", mints.Load())
	}
}

func TestAppConfigurationIsCheckedUpFront(t *testing.T) {
	_, keyPEM := rsaKey(t)
	for name, cfg := range map[string]map[string]any{
		"no app id":           {"method": "app", "scope": "acme", "credentials": map[string]string{"app_private_key": keyPEM}},
		"no key":              {"method": "app", "scope": "acme", "app_id": "1"},
		"bad key":             {"method": "app", "scope": "acme", "app_id": "1", "credentials": map[string]string{"app_private_key": "-----BEGIN RSA PRIVATE KEY-----\nnope\n-----END RSA PRIVATE KEY-----"}},
		"no owner or install": {"method": "app", "app_id": "1", "credentials": map[string]string{"app_private_key": keyPEM}},
		"bad installation id": {"method": "app", "app_id": "1", "installation_id": "abc", "credentials": map[string]string{"app_private_key": keyPEM}},
	} {
		if _, err := configure(t, cfg); err == nil {
			t.Errorf("%s: configured", name)
		} else if strings.Contains(err.Error(), "nope") || strings.Contains(err.Error(), "PRIVATE KEY-----\n") {
			t.Errorf("%s: the key reached the error: %v", name, err)
		}
	}
	mustConfigure(t, map[string]any{"method": "app", "app_id": "1", "installation_id": "42",
		"credentials": map[string]string{"app_private_key": keyPEM}})
}

func oauthServer(t *testing.T, handler http.HandlerFunc) (*Adapter, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return nil, srv
}

func TestOAuthDeviceFlow(t *testing.T) {
	var polls atomic.Int32
	_, srv := oauthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch r.URL.Path {
		case "/login/device/code":
			if r.Form.Get("client_id") != "Iv1.client" || r.Form.Get("scope") != "repo" {
				t.Errorf("device form = %v", r.Form)
			}
			writeJSON(w, map[string]any{"device_code": "dev-code", "user_code": "ABCD-1234",
				"verification_uri": "https://github.com/login/device", "expires_in": 900, "interval": 5})
		case "/login/oauth/access_token":
			if r.Form.Get("device_code") != "dev-code" {
				t.Errorf("token form = %v", r.Form)
			}
			if polls.Add(1) == 1 {
				writeJSON(w, map[string]any{"error": "authorization_pending"})
				return
			}
			writeJSON(w, map[string]any{"access_token": "gho_access", "refresh_token": "ghr_refresh", "expires_in": 28800})
		default:
			http.NotFound(w, r)
		}
	})

	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client"})
	a.webURL = srv.URL
	caps := a.SourceCapabilities()
	if !caps.DeviceAuthorization || caps.WebAuthorization || caps.Authorized {
		t.Fatalf("capabilities = %+v", caps)
	}
	if a.Covers("https://github.com/acme/api") != 0 {
		t.Fatal("an unauthorized oauth connection covers a repository")
	}
	if _, err := a.GitCredential(context.Background(), "https://github.com/acme/api"); err == nil {
		t.Fatal("an unauthorized oauth connection handed out a credential")
	}

	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice})
	if err != nil {
		t.Fatal(err)
	}
	if auth.UserCode != "ABCD-1234" || auth.VerificationURL != "https://github.com/login/device" || auth.Flow.Reveal() != "dev-code" {
		t.Fatalf("authorization = %+v", auth)
	}

	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil || !res.Pending {
		t.Fatalf("first poll = %+v, %v", res, err)
	}
	res, err = a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil || res.Pending {
		t.Fatalf("second poll = %+v, %v", res, err)
	}
	if res.Credentials[forgekit.FieldAccessToken].Reveal() != "gho_access" || res.Credentials[forgekit.FieldRefreshToken].Reveal() != "ghr_refresh" {
		t.Fatalf("credentials = %v", res.Credentials)
	}

	// Web authorization needs the client secret.
	if _, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb"}); err == nil {
		t.Fatal("web authorization began without a client secret")
	}
	withSecret := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client",
		"credentials": map[string]string{"client_secret": "shh"}})
	if !withSecret.SourceCapabilities().WebAuthorization {
		t.Fatal("web authorization not offered with a client secret")
	}
	web, err := withSecret.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "st"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(web.AuthorizeURL, "https://github.com/login/oauth/authorize?") || strings.Contains(web.AuthorizeURL, "shh") {
		t.Fatalf("authorize URL = %s", web.AuthorizeURL)
	}

	// Other methods have nothing to authorize.
	tok := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": "x"}})
	if _, err := tok.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice}); err == nil {
		t.Fatal("a token connection began an authorization")
	}
}

func TestOAuthRefreshOnExpiryIsRotated(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	_, srv := oauthServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/login/oauth/access_token" || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "ghr_old" {
			t.Errorf("unexpected request %s %v", r.URL.Path, r.Form)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeJSON(w, map[string]any{"access_token": "gho_new", "refresh_token": "ghr_new", "expires_in": 3600})
	})
	a := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client",
		"credentials": map[string]string{
			"client_secret":    "shh",
			"access_token":     "gho_old",
			"refresh_token":    "ghr_old",
			"token_expires_at": now.Add(-time.Minute).Format(time.RFC3339),
		}})
	a.webURL = srv.URL
	a.now = func() time.Time { return now }
	if !a.SourceCapabilities().Authorized {
		t.Fatal("an authorized oauth connection reports it is not")
	}

	cred, err := a.GitCredential(context.Background(), "https://github.com/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Password.Reveal() != "gho_new" || cred.Username != "x-access-token" {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.Rotated[forgekit.FieldAccessToken].Reveal() != "gho_new" || cred.Rotated[forgekit.FieldRefreshToken].Reveal() != "ghr_new" {
		t.Fatalf("rotated = %v", cred.Rotated)
	}
	if !cred.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("ExpiresAt = %s", cred.ExpiresAt)
	}

	// A token still good is used as is.
	fresh := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client",
		"credentials": map[string]string{"access_token": "gho_live"}})
	cred, err = fresh.GitCredential(context.Background(), "https://github.com/acme/api")
	if err != nil || cred.Rotated != nil || cred.Password.Reveal() != "gho_live" {
		t.Fatalf("credential = %+v, %v", cred, err)
	}
}

func TestListRepositoriesPaginatesAndFilters(t *testing.T) {
	var srv *httptest.Server
	pages := map[string][]map[string]any{
		"1": {
			{"full_name": "acme/api", "clone_url": "https://github.com/acme/api.git", "default_branch": "main", "private": true, "owner": map[string]string{"login": "acme"}},
			{"full_name": "other/api", "clone_url": "https://github.com/other/api.git", "default_branch": "main", "owner": map[string]string{"login": "other"}},
		},
		"2": {
			{"full_name": "ACME/web", "clone_url": "https://github.com/ACME/web.git", "default_branch": "trunk", "owner": map[string]string{"login": "ACME"}},
			{"full_name": "acme/api-docs", "clone_url": "https://github.com/acme/api-docs.git", "default_branch": "main", "owner": map[string]string{"login": "acme"}},
		},
	}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/repos" {
			http.NotFound(w, r)
			return
		}
		if !checkAPIHeaders(t, w, r, "ghp_secret") {
			return
		}
		if r.URL.Query().Get("affiliation") != "owner,collaborator,organization_member" || r.URL.Query().Get("per_page") != "100" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		if page == "1" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/user/repos?per_page=100&affiliation=owner%%2Ccollaborator%%2Corganization_member&page=2>; rel="next", <%s/user/repos?page=2>; rel="last"`, srv.URL, srv.URL))
		}
		writeJSON(w, pages[page])
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": "ghp_secret"}})

	repos, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range repos {
		names = append(names, r.FullName)
	}
	if strings.Join(names, ",") != "acme/api,ACME/web,acme/api-docs" {
		t.Fatalf("repositories = %v", names)
	}
	if repos[0].URL != "https://github.com/acme/api.git" || !repos[0].Private || repos[1].DefaultBranch != "trunk" {
		t.Fatalf("repositories = %+v", repos)
	}

	repos, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Query: "API"})
	if err != nil || len(repos) != 2 {
		t.Fatalf("query: %+v, %v", repos, err)
	}
	repos, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Limit: 1})
	if err != nil || len(repos) != 1 {
		t.Fatalf("limit: %+v, %v", repos, err)
	}
}

func TestAppListsInstallationRepositories(t *testing.T) {
	_, keyPEM := rsaKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/app/installations/42/access_tokens":
			writeJSON(w, map[string]any{"token": "ghs_inst", "expires_at": time.Now().Add(time.Hour).Format(time.RFC3339)})
		case "/installation/repositories":
			if !checkAPIHeaders(t, w, r, "ghs_inst") {
				return
			}
			writeJSON(w, map[string]any{"total_count": 1, "repositories": []map[string]any{
				{"full_name": "acme/api", "clone_url": "https://github.com/acme/api.git", "default_branch": "main", "private": true, "owner": map[string]string{"login": "acme"}},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "app", "app_id": "1", "installation_id": "42", "api_url": srv.URL,
		"credentials": map[string]string{"app_private_key": keyPEM}})
	repos, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0].FullName != "acme/api" {
		t.Fatalf("repositories = %+v", repos)
	}
}

func TestListBranchesPaginates(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/api/branches" {
			http.NotFound(w, r)
			return
		}
		if !checkAPIHeaders(t, w, r, "ghp_secret") {
			return
		}
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/acme/api/branches?per_page=100&page=2>; rel="next"`, srv.URL))
			writeJSON(w, []map[string]string{{"name": "main"}, {"name": "dev"}})
			return
		}
		writeJSON(w, []map[string]string{{"name": "release"}})
	}))
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "api_url": srv.URL,
		"credentials": map[string]string{"token": "ghp_secret"}})
	branches, err := a.ListBranches(context.Background(), "git@github.com:acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(branches, ",") != "main,dev,release" {
		t.Fatalf("branches = %v", branches)
	}
}

func TestUnauthorizedIsActionableAndSecretFree(t *testing.T) {
	_, keyPEM := rsaKey(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
	}))
	defer srv.Close()

	tok := mustConfigure(t, map[string]any{"method": "token", "api_url": srv.URL,
		"credentials": map[string]string{"token": "ghp_supersecret"}})
	_, err := tok.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if errs.CodeOf(err) != errs.AdapterFailed || !strings.Contains(err.Error(), "did not accept") {
		t.Fatalf("code = %s, err = %v", errs.CodeOf(err), err)
	}
	if e := errs.As(err); e == nil || e.Remedy == "" {
		t.Fatalf("no remedy: %v", err)
	}
	_, err2 := tok.ListBranches(context.Background(), "https://github.com/acme/api")

	app := mustConfigure(t, map[string]any{"method": "app", "app_id": "1", "installation_id": "42", "api_url": srv.URL,
		"credentials": map[string]string{"app_private_key": keyPEM}})
	_, err3 := app.GitCredential(context.Background(), "https://github.com/acme/api")
	if errs.CodeOf(err3) != errs.AdapterFailed || !strings.Contains(err3.Error(), "App ID or the private key") {
		t.Fatalf("app error = %v", err3)
	}

	for _, e := range []error{err, err2, err3} {
		if e == nil {
			t.Fatal("expected an error")
		}
		s := e.Error()
		if ae := errs.As(e); ae != nil {
			s += ae.Remedy
		}
		for _, leaked := range []string{"ghp_supersecret", "PRIVATE KEY", "Bearer"} {
			if strings.Contains(s, leaked) {
				t.Errorf("error %q contains %q", s, leaked)
			}
		}
	}
}

func TestEnterpriseServerAPIAddress(t *testing.T) {
	a := mustConfigure(t, map[string]any{"method": "token", "host": "https://github.acme.internal",
		"credentials": map[string]string{"token": "x"}})
	if a.apiURL != "https://github.acme.internal/api/v3" || a.webURL != "https://github.acme.internal" {
		t.Fatalf("api = %s, web = %s", a.apiURL, a.webURL)
	}
	if got := a.Covers("https://github.acme.internal/acme/api"); got != 1 {
		t.Fatalf("Covers = %d", got)
	}
	if got := a.Covers("https://github.com/acme/api"); got != 0 {
		t.Fatalf("a GHES connection covers github.com: %d", got)
	}
	dotcom := mustConfigure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": "x"}})
	if dotcom.apiURL != "https://api.github.com" {
		t.Fatalf("api = %s", dotcom.apiURL)
	}
}

func TestRefreshRenewsOnlyExpiredOAuthTokens(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	var refreshes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		switch {
		case r.URL.Path == "/login/oauth/access_token" && r.Form.Get("refresh_token") == "ghr_old":
			refreshes.Add(1)
			writeJSON(w, map[string]any{"access_token": "gho_new", "refresh_token": "ghr_new", "expires_in": 3600})
		case r.URL.Path == "/user/repos" || r.URL.Path == "/repos/acme/api/branches":
			t.Errorf("an expired token was used to call %s", r.URL.Path)
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	expired := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client", "api_url": srv.URL,
		"credentials": map[string]string{
			"access_token":     "gho_old",
			"refresh_token":    "ghr_old",
			"token_expires_at": now.Add(-time.Minute).Format(time.RFC3339),
		}})
	expired.webURL = srv.URL
	expired.now = func() time.Time { return now }

	// Listing uses the stored token as is and refuses an expired one.
	_, err := expired.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if errs.CodeOf(err) != errs.StateInvalid || !strings.Contains(errs.As(err).Remedy, "Authorize") {
		t.Fatalf("listing with an expired token: %v", err)
	}
	if _, err := expired.ListBranches(context.Background(), "https://github.com/acme/api"); errs.CodeOf(err) != errs.StateInvalid {
		t.Fatalf("branches with an expired token: %v", err)
	}
	if refreshes.Load() != 0 {
		t.Fatal("listing refreshed the token itself")
	}

	got, err := expired.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got[forgekit.FieldAccessToken].Reveal() != "gho_new" || got[forgekit.FieldRefreshToken].Reveal() != "ghr_new" ||
		got[forgekit.FieldTokenExpiresAt].Reveal() != now.Add(time.Hour).Format(time.RFC3339) {
		t.Fatalf("refreshed = %v", got)
	}

	live := mustConfigure(t, map[string]any{"method": "oauth", "client_id": "Iv1.client",
		"credentials": map[string]string{"access_token": "gho_live"}})
	if got, err := live.Refresh(context.Background()); got != nil || err != nil {
		t.Fatalf("a good token refreshed: %v, %v", got, err)
	}

	_, keyPEM := rsaKey(t)
	for name, cfg := range map[string]map[string]any{
		"token": {"method": "token", "credentials": map[string]string{"token": "x"}},
		"ssh":   {"method": "ssh", "credentials": map[string]string{"ssh_private_key": sshKey(t)}},
		"app":   {"method": "app", "app_id": "1", "installation_id": "42", "credentials": map[string]string{"app_private_key": keyPEM}},
	} {
		if got, err := mustConfigure(t, cfg).Refresh(context.Background()); got != nil || err != nil {
			t.Errorf("%s: Refresh = %v, %v", name, got, err)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refreshes = %d, want 1", refreshes.Load())
	}
}
