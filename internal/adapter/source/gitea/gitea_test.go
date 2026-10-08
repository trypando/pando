package gitea

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
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

const (
	tokenSecret   = "gitea-s3cret-token"
	clientSecret  = "gto-client-s3cret"
	accessSecret  = "access-s3cret"
	refreshSecret = "refresh-s3cret"
)

var secrets = []string{tokenSecret, clientSecret, accessSecret, refreshSecret}

func TestInfoValidates(t *testing.T) {
	info := Info()
	if err := info.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(info.Presets) != 1 || info.Presets[0].Values["host"] != "codeberg.org" {
		t.Fatalf("presets = %+v", info.Presets)
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

func point(a *Adapter, srv *httptest.Server) {
	a.web = srv.URL
	a.api = srv.URL + "/api/v1"
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

func testKey(t *testing.T) (string, string) {
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
	return string(pem.EncodeToMemory(block)), "codeberg.org " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestHostIsRequired(t *testing.T) {
	_, err := configure(t, map[string]any{"method": "token", "credentials": map[string]string{"token": tokenSecret}})
	if err == nil {
		t.Fatal("a Gitea connection was configured without a host")
	}
	noSecret(t, err)
}

func TestScopeIsOneOwner(t *testing.T) {
	if _, err := configure(t, map[string]any{
		"method": "token", "host": "codeberg.org", "scope": "acme/api",
		"credentials": map[string]string{"token": tokenSecret},
	}); err == nil {
		t.Fatal("a two-segment scope was accepted")
	}
	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "https://codeberg.org/", "scope": "acme",
		"credentials": map[string]string{"token": tokenSecret},
	})
	for u, want := range map[string]int{
		"https://codeberg.org/acme/api.git":   2,
		"https://Codeberg.org/ACME/api":       2,
		"git@codeberg.org:acme/api.git":       2,
		"https://codeberg.org/other/api.git":  0,
		"https://codeberg.org/acme":           0,
		"https://gitea.example.com/acme/api":  0,
		"ftp://codeberg.org/acme/api.git":     0,
		"https://codeberg.org/acme/api/extra": 2,
	} {
		if got := a.Covers(u); got != want {
			t.Errorf("Covers(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestTokenCredential(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "gitea.example.com", "credentials": map[string]string{"token": tokenSecret},
	})
	cred, err := a.GitCredential(context.Background(), "https://gitea.example.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "oauth2" || cred.Password.Reveal() != tokenSecret || cred.URL != "" {
		t.Fatalf("credential = %+v", cred)
	}
	b := mustConfigure(t, map[string]any{
		"method": "token", "host": "gitea.example.com", "username": "ben",
		"credentials": map[string]string{"token": tokenSecret},
	})
	cred, err = b.GitCredential(context.Background(), "https://gitea.example.com/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "ben" {
		t.Fatalf("username = %q", cred.Username)
	}
	if c := a.SourceCapabilities(); !c.ListRepositories || !c.Authorized || c.DeviceAuthorization || c.WebAuthorization {
		t.Fatalf("capabilities = %+v", c)
	}
	_, err = a.GitCredential(context.Background(), "git@gitea.example.com:acme/api.git")
	if err == nil {
		t.Fatal("a token connection handed out a credential for an SSH address")
	}
	noSecret(t, err)
}

func TestSSHNeedsKnownHosts(t *testing.T) {
	key, known := testKey(t)
	if _, err := configure(t, map[string]any{
		"method": "ssh", "host": "codeberg.org", "credentials": map[string]string{"ssh_private_key": key},
	}); err == nil {
		t.Fatal("an SSH connection was configured without known hosts")
	}
	a := mustConfigure(t, map[string]any{
		"method": "ssh", "host": "codeberg.org", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key},
	})
	cred, err := a.GitCredential(context.Background(), "https://codeberg.org/acme/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "git@codeberg.org:acme/api.git" || cred.KnownHosts != known || cred.SSHPrivateKey.Reveal() != key {
		t.Fatalf("credential = %+v", cred)
	}
	if a.SourceCapabilities().ListRepositories {
		t.Fatal("a deploy key connection claims it can list repositories")
	}
	if _, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err == nil {
		t.Fatal("listing on a deploy key connection succeeded")
	}
}

func TestOAuthWebOnly(t *testing.T) {
	if _, err := configure(t, map[string]any{"method": "oauth", "host": "codeberg.org"}); err == nil {
		t.Fatal("an OAuth connection was configured without a client ID")
	}
	noSecretApp := mustConfigure(t, map[string]any{"method": "oauth", "host": "codeberg.org", "client_id": "cid"})
	if c := noSecretApp.SourceCapabilities(); c.WebAuthorization || c.DeviceAuthorization || c.Authorized || c.ListRepositories {
		t.Fatalf("capabilities = %+v", c)
	}
	if noSecretApp.Covers("https://codeberg.org/acme/api") != 0 {
		t.Fatal("an unauthorized connection covers a repository")
	}
	if _, err := noSecretApp.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "s",
	}); err == nil {
		t.Fatal("web authorization began without a client secret")
	}

	a := mustConfigure(t, map[string]any{
		"method": "oauth", "host": "codeberg.org", "client_id": "cid",
		"credentials": map[string]string{"client_secret": clientSecret},
	})
	if c := a.SourceCapabilities(); !c.WebAuthorization || c.DeviceAuthorization {
		t.Fatalf("capabilities = %+v", c)
	}
	_, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice})
	if err == nil {
		t.Fatal("device authorization began on Gitea")
	}
	noSecret(t, err)
}

func TestOAuthWebFlow(t *testing.T) {
	var verifier string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/login/oauth/access_token" {
			http.NotFound(w, r)
			return
		}
		if r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "the-code" ||
			r.Form.Get("code_verifier") != verifier || r.Form.Get("client_secret") != clientSecret {
			t.Errorf("exchange form = %v", r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"refresh_token":%q,"expires_in":3600,"token_type":"bearer"}`, accessSecret, refreshSecret)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "oauth", "host": "codeberg.org", "client_id": "cid",
		"credentials": map[string]string{"client_secret": clientSecret},
	})
	point(a, srv)
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	a.now = func() time.Time { return now }
	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "state-9",
	})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(auth.AuthorizeURL)
	q := u.Query()
	if u.Path != "/login/oauth/authorize" || q.Get("state") != "state-9" || q.Get("code_challenge") == "" ||
		q.Get("code_challenge_method") != "S256" || q.Get("scope") != "read:repository" {
		t.Fatalf("authorize URL = %s", auth.AuthorizeURL)
	}
	verifier = auth.Flow.Reveal()
	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: auth.Flow, Code: "the-code", RedirectURL: "https://pando.example/cb",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials["access_token"].Reveal() != accessSecret || res.Credentials["refresh_token"].Reveal() != refreshSecret ||
		!res.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatalf("result = %+v", res)
	}
}

func TestOAuthRefreshOnExpirySetsRotated(t *testing.T) {
	now := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.URL.Path != "/login/oauth/access_token" || r.Form.Get("grant_type") != "refresh_token" ||
			r.Form.Get("refresh_token") != refreshSecret {
			t.Errorf("refresh request = %s %v", r.URL.Path, r.Form)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "oauth", "host": "codeberg.org", "client_id": "cid",
		"credentials": map[string]string{
			"client_secret": clientSecret, "access_token": accessSecret, "refresh_token": refreshSecret,
			"token_expires_at": now.Add(-time.Second).Format(time.RFC3339),
		},
	})
	point(a, srv)
	a.now = func() time.Time { return now }
	cred, err := a.GitCredential(context.Background(), "https://codeberg.org/acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "oauth2" || cred.Password.Reveal() != "new-access" ||
		cred.Rotated["access_token"].Reveal() != "new-access" || cred.Rotated["refresh_token"].Reveal() != "new-refresh" {
		t.Fatalf("credential = %+v rotated = %v", cred, cred.Rotated)
	}
}

func repos(from, n int, owner string) string {
	var out []string
	for i := from; i < from+n; i++ {
		out = append(out, fmt.Sprintf(`{"full_name":"%s/r%d","clone_url":"https://codeberg.org/%s/r%d.git","default_branch":"main","private":%t,"owner":{"login":%q}}`,
			owner, i, owner, i, i%2 == 1, owner))
	}
	return "[" + strings.Join(out, ",") + "]"
}

func TestListRepositoriesPagesFiltersAndLimits(t *testing.T) {
	var pages []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token "+tokenSecret {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/api/v1/user/repos" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("request = %s", r.URL)
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		pages = append(pages, page)
		switch page {
		case 1:
			_, _ = fmt.Fprint(w, repos(0, 3, "acme")[:len(repos(0, 3, "acme"))-1]+","+repos(0, 2, "other")[1:])
		case 2:
			_, _ = fmt.Fprint(w, repos(3, 3, "ACME"))
		default:
			_, _ = fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "codeberg.org", "scope": "acme",
		"credentials": map[string]string{"token": tokenSecret},
	})
	point(a, srv)
	got, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || fmt.Sprint(pages) != "[1 2 3]" {
		t.Fatalf("got %d repositories from pages %v: %+v", len(got), pages, got)
	}
	want := api.Repository{URL: "https://codeberg.org/acme/r1.git", FullName: "acme/r1", DefaultBranch: "main", Private: true}
	if got[1] != want {
		t.Fatalf("repository = %+v", got[1])
	}

	pages = nil
	got, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Query: "R4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].FullName != "ACME/r4" {
		t.Fatalf("query result = %+v", got)
	}

	pages = nil
	got, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || fmt.Sprint(pages) != "[1]" {
		t.Fatalf("limited result = %+v from pages %v", got, pages)
	}
}

func TestListBranches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/repos/acme/api/branches" || r.URL.Query().Get("limit") != "50" {
			t.Errorf("request = %s", r.URL)
		}
		if r.URL.Query().Get("page") == "1" {
			_, _ = fmt.Fprint(w, `[{"name":"main"},{"name":"dev"}]`)
			return
		}
		_, _ = fmt.Fprint(w, `[]`)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "codeberg.org", "credentials": map[string]string{"token": tokenSecret},
	})
	point(a, srv)
	names, err := a.ListBranches(context.Background(), "git@codeberg.org:acme/api.git")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "main,dev" {
		t.Fatalf("branches = %v", names)
	}
	if _, err := a.ListBranches(context.Background(), "https://codeberg.org/acme/group/api"); err == nil {
		t.Fatal("listed branches for a three-segment path")
	}
}

func TestUnauthorizedIsActionableAndCarriesNoSecret(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "codeberg.org", "credentials": map[string]string{"token": tokenSecret},
	})
	point(a, srv)
	_, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	e := errs.As(err)
	if e == nil || e.Code != errs.AdapterFailed || !strings.Contains(e.Message, "did not accept") || e.Remedy == "" {
		t.Fatalf("error = %v", err)
	}
	noSecret(t, err)
	_, err = a.ListBranches(context.Background(), "https://codeberg.org/acme/api")
	if err == nil {
		t.Fatal("a 401 was not an error")
	}
	noSecret(t, err)
}
