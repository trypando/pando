package azuredevops

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

const (
	pat          = "pat-s3cret-value"
	clientSecret = "client-s3cret-value"
)

var fixedNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

func configure(t *testing.T, cfg map[string]any, authority string) (*Adapter, error) {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	a := New()
	a.now = func() time.Time { return fixedNow }
	if authority != "" {
		a.authority = authority
	}
	return a, a.Configure(context.Background(), raw)
}

func mustConfigure(t *testing.T, cfg map[string]any, authority string) *Adapter {
	t.Helper()
	a, err := configure(t, cfg, authority)
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
	for _, s := range []string{pat, clientSecret, "access-1", "refresh-1"} {
		if strings.Contains(err.Error(), s) {
			t.Fatalf("error %q contains a secret", err)
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
	return string(pem.EncodeToMemory(block)), "ssh.dev.azure.com " + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshPub)))
}

func TestInfoValidates(t *testing.T) {
	if err := Info().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestServicesCoversEveryAddressForm(t *testing.T) {
	org := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme", "credentials": map[string]string{"token": pat},
	}, "")
	project := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme/Payments", "credentials": map[string]string{"token": pat},
	}, "")
	for u, want := range map[string][2]int{
		"https://dev.azure.com/acme/Payments/_git/api":                      {2, 3},
		"https://acme@dev.azure.com/acme/Payments/_git/api":                 {2, 3},
		"https://dev.azure.com/ACME/payments/_git/api":                      {2, 3},
		"https://dev.azure.com/acme/Payments/_git/api/pullrequest/4":        {2, 3},
		"https://dev.azure.com/acme/_git/Payments":                          {2, 3},
		"https://acme.visualstudio.com/Payments/_git/api":                   {2, 3},
		"https://acme.visualstudio.com/DefaultCollection/Payments/_git/api": {2, 3},
		"git@ssh.dev.azure.com:v3/acme/Payments/api":                        {2, 3},
		"acme@vs-ssh.visualstudio.com:v3/acme/Payments/api":                 {2, 3},
		"https://dev.azure.com/acme/Billing/_git/api":                       {2, 0},
		"https://dev.azure.com/other/Payments/_git/api":                     {0, 0},
		"https://other.visualstudio.com/Payments/_git/api":                  {0, 0},
		"https://dev.azure.com/acme/Payments":                               {0, 0},
		"https://tfs.acme.internal/acme/Payments/_git/api":                  {0, 0},
		"https://github.com/acme/api":                                       {0, 0},
		"not an address":                                                    {0, 0},
	} {
		if got := org.Covers(u); got != want[0] {
			t.Errorf("organization connection: Covers(%q) = %d, want %d", u, got, want[0])
		}
		if got := project.Covers(u); got != want[1] {
			t.Errorf("project connection: Covers(%q) = %d, want %d", u, got, want[1])
		}
	}
}

func TestLegacyHostSetsTheOrganization(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "https://acme.visualstudio.com", "credentials": map[string]string{"token": pat},
	}, "")
	caps := a.SourceCapabilities()
	if caps.Host != "dev.azure.com" || caps.Scope != "acme" {
		t.Fatalf("capabilities = %+v", caps)
	}
	if a.Covers("https://dev.azure.com/acme/Payments/_git/api") != 2 {
		t.Fatal("a legacy-host connection does not cover its organization's dev.azure.com address")
	}
}

func TestServerCoversItsCollection(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "host": "tfs.acme.internal", "scope": "DefaultCollection",
		"credentials": map[string]string{"token": pat},
	}, "")
	for u, want := range map[string]int{
		"https://tfs.acme.internal/DefaultCollection/Payments/_git/api":      2,
		"https://tfs.acme.internal/tfs/DefaultCollection/Payments/_git/api":  2,
		"ssh://tfs.acme.internal:22/DefaultCollection/Payments/_git/api":     2,
		"ssh://tfs.acme.internal:22/tfs/DefaultCollection/Payments/_ssh/api": 2,
		"https://tfs.acme.internal/Other/Payments/_git/api":                  0,
		"https://dev.azure.com/DefaultCollection/Payments/_git/api":          0,
		"https://tfs.acme.internal/DefaultCollection/Payments":               0,
	} {
		if got := a.Covers(u); got != want {
			t.Errorf("Covers(%q) = %d, want %d", u, got, want)
		}
	}
}

func TestConfigureRefusals(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"services without an organization": {"method": "token", "credentials": map[string]string{"token": pat}},
		"scope too deep":                   {"method": "token", "scope": "a/b/c", "credentials": map[string]string{"token": pat}},
		"oauth without client":             {"method": "oauth", "scope": "acme"},
		"oauth on server":                  {"method": "oauth", "host": "tfs.acme.internal", "scope": "c", "client_id": "x"},
		"service principal on organizations": {"method": "service_principal", "scope": "acme", "client_id": "x",
			"credentials": map[string]string{"client_secret": clientSecret}},
		"service principal without secret": {"method": "service_principal", "scope": "acme", "client_id": "x", "tenant_id": "t1"},
		"unknown method":                   {"method": "app", "scope": "acme"},
	} {
		_, err := configure(t, cfg, "")
		if err == nil {
			t.Errorf("%s: configured", name)
		}
		noSecret(t, err)
	}
}

func TestTokenCredential(t *testing.T) {
	a := mustConfigure(t, map[string]any{
		"method": "token", "scope": "acme", "credentials": map[string]string{"token": pat},
	}, "")
	cred, err := a.GitCredential(context.Background(), "https://acme@dev.azure.com/acme/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.Username != "pat" || cred.Password.Reveal() != pat || !cred.BearerToken.IsZero() || cred.URL != "" {
		t.Fatalf("credential = %+v", cred)
	}
	_, err = a.GitCredential(context.Background(), "git@ssh.dev.azure.com:v3/acme/Payments/api")
	if err == nil {
		t.Fatal("a token connection handed out a credential for an SSH address")
	}
	noSecret(t, err)
}

func TestSSHCredentialConvertsAddresses(t *testing.T) {
	key, known := testKey(t)
	services := mustConfigure(t, map[string]any{
		"method": "ssh", "scope": "acme", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key},
	}, "")
	for in, want := range map[string]string{
		"https://dev.azure.com/acme/Payments/_git/api":    "git@ssh.dev.azure.com:v3/acme/Payments/api",
		"https://acme.visualstudio.com/Payments/_git/api": "git@ssh.dev.azure.com:v3/acme/Payments/api",
		"git@ssh.dev.azure.com:v3/acme/Payments/api":      "",
	} {
		cred, err := services.GitCredential(context.Background(), in)
		if err != nil {
			t.Fatal(err)
		}
		if cred.URL != want || cred.SSHUser != "git" || cred.KnownHosts != known || cred.SSHPrivateKey.Reveal() != key {
			t.Errorf("GitCredential(%q) = URL %q user %q", in, cred.URL, cred.SSHUser)
		}
	}
	if services.SourceCapabilities().ListRepositories {
		t.Fatal("an SSH connection claims it can list repositories")
	}
	if _, err := services.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err == nil {
		t.Fatal("an SSH connection listed repositories")
	}

	server := mustConfigure(t, map[string]any{
		"method": "ssh", "host": "tfs.acme.internal", "scope": "DefaultCollection", "known_hosts": known,
		"credentials": map[string]string{"ssh_private_key": key},
	}, "")
	cred, err := server.GitCredential(context.Background(), "https://tfs.acme.internal/tfs/DefaultCollection/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.URL != "ssh://git@tfs.acme.internal:22/tfs/DefaultCollection/Payments/_git/api" {
		t.Fatalf("server SSH URL = %q", cred.URL)
	}
}

// entra is a fake Microsoft Entra ID token service for tenant "t1".
type entra struct {
	mu    sync.Mutex
	polls int
	forms []url.Values
}

func (e *entra) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		e.mu.Lock()
		e.forms = append(e.forms, r.PostForm)
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/t1/oauth2/v2.0/devicecode", "/organizations/oauth2/v2.0/devicecode":
			if !strings.Contains(r.PostForm.Get("scope"), devOpsScope) || !strings.Contains(r.PostForm.Get("scope"), "offline_access") {
				t.Errorf("device scope = %q", r.PostForm.Get("scope"))
			}
			_, _ = w.Write([]byte(`{"device_code":"dev-code","user_code":"ABCD-EFGH","verification_uri":"https://microsoft.com/devicelogin","expires_in":900,"interval":5}`))
		case "/t1/oauth2/v2.0/token", "/organizations/oauth2/v2.0/token":
			switch r.PostForm.Get("grant_type") {
			case "urn:ietf:params:oauth:grant-type:device_code":
				e.mu.Lock()
				e.polls++
				first := e.polls == 1
				e.mu.Unlock()
				if first {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"access-1","refresh_token":"refresh-1","expires_in":3600}`))
			case "refresh_token":
				if r.PostForm.Get("refresh_token") != "refresh-0" {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"access-2","refresh_token":"refresh-2","expires_in":3600}`))
			case "client_credentials":
				if r.PostForm.Get("client_secret") != clientSecret {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error":"invalid_client","error_description":"AADSTS7000215: Invalid client secret provided. Trace ID: 1234\r\nCorrelation ID: 5678"}`))
					return
				}
				_, _ = w.Write([]byte(`{"token_type":"Bearer","access_token":"sp-token","expires_in":3599}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

func TestOAuthDeviceAuthorization(t *testing.T) {
	e := &entra{}
	srv := httptest.NewServer(e.handler(t))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1"}, srv.URL)
	caps := a.SourceCapabilities()
	if !caps.DeviceAuthorization || caps.WebAuthorization || caps.Authorized {
		t.Fatalf("capabilities = %+v", caps)
	}
	if a.Covers("https://dev.azure.com/acme/Payments/_git/api") != 0 {
		t.Fatal("an unauthorized oauth connection covers a repository")
	}
	if _, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb"}); err == nil {
		t.Fatal("web authorization began without a client secret")
	}

	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{Mode: api.AuthorizationDevice})
	if err != nil {
		t.Fatal(err)
	}
	if auth.UserCode != "ABCD-EFGH" || auth.VerificationURL == "" || auth.Flow.Reveal() != "dev-code" {
		t.Fatalf("authorization = %+v", auth)
	}
	res, err := a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil || !res.Pending {
		t.Fatalf("first poll = %+v, %v", res, err)
	}
	res, err = a.CompleteAuthorization(context.Background(), api.AuthorizationCompletion{Mode: api.AuthorizationDevice, Flow: auth.Flow})
	if err != nil {
		t.Fatal(err)
	}
	if res.Credentials["access_token"].Reveal() != "access-1" || res.Credentials["refresh_token"].Reveal() != "refresh-1" {
		t.Fatal("the device authorization did not return the tokens")
	}

	// Configured with what core stores, it clones with a bearer token.
	creds := map[string]string{}
	for k, v := range res.Credentials {
		creds[k] = v.Reveal()
	}
	a = mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1", "credentials": creds}, srv.URL)
	if a.Covers("https://dev.azure.com/acme/Payments/_git/api") != 2 {
		t.Fatal("an authorized oauth connection does not cover its organization")
	}
	cred, err := a.GitCredential(context.Background(), "https://dev.azure.com/acme/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.BearerToken.Reveal() != "access-1" || !cred.Password.IsZero() || cred.Rotated != nil {
		t.Fatalf("credential = %+v", cred)
	}
}

func TestOAuthWebAuthorizationWithSecret(t *testing.T) {
	a := mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1", "tenant_id": "t1",
		"credentials": map[string]string{"client_secret": clientSecret}}, "https://login.example")
	if !a.SourceCapabilities().WebAuthorization {
		t.Fatal("a connection with a client secret does not offer web authorization")
	}
	auth, err := a.BeginAuthorization(context.Background(), api.AuthorizationRequest{
		Mode: api.AuthorizationWeb, RedirectURL: "https://pando.example/cb", State: "st"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(auth.AuthorizeURL, "https://login.example/t1/oauth2/v2.0/authorize?") ||
		!strings.Contains(auth.AuthorizeURL, url.QueryEscape(devOpsScope)) {
		t.Fatalf("authorize URL = %s", auth.AuthorizeURL)
	}
}

func TestOAuthRefreshesAnExpiredToken(t *testing.T) {
	e := &entra{}
	srv := httptest.NewServer(e.handler(t))
	defer srv.Close()

	a := mustConfigure(t, map[string]any{"method": "oauth", "scope": "acme", "client_id": "app-1", "tenant_id": "t1",
		"credentials": map[string]string{
			"access_token": "access-0", "refresh_token": "refresh-0",
			"token_expires_at": fixedNow.Add(-time.Hour).Format(time.RFC3339),
		}}, srv.URL)
	cred, err := a.GitCredential(context.Background(), "https://dev.azure.com/acme/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.BearerToken.Reveal() != "access-2" || cred.ExpiresAt.IsZero() {
		t.Fatalf("credential = %+v", cred)
	}
	if cred.Rotated["access_token"].Reveal() != "access-2" || cred.Rotated["refresh_token"].Reveal() != "refresh-2" {
		t.Fatal("the refreshed tokens are not in Rotated")
	}
}

func TestServicePrincipalMintsATokenPerCredential(t *testing.T) {
	e := &entra{}
	srv := httptest.NewServer(e.handler(t))
	defer srv.Close()

	cfg := map[string]any{"method": "service_principal", "scope": "acme", "client_id": "app-1", "tenant_id": "t1",
		"credentials": map[string]string{"client_secret": clientSecret}}
	a := mustConfigure(t, cfg, srv.URL)
	if c := a.SourceCapabilities(); !c.Authorized || c.DeviceAuthorization || c.WebAuthorization {
		t.Fatalf("capabilities = %+v", c)
	}
	cred, err := a.GitCredential(context.Background(), "https://dev.azure.com/acme/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if cred.BearerToken.Reveal() != "sp-token" || !cred.ExpiresAt.Equal(fixedNow.Add(3599*time.Second)) || cred.Rotated != nil {
		t.Fatalf("credential = %+v", cred)
	}
	form := e.forms[len(e.forms)-1]
	if form.Get("grant_type") != "client_credentials" || form.Get("client_id") != "app-1" || form.Get("scope") != devOpsScope {
		t.Fatalf("token request = %v", form)
	}

	cfg["credentials"] = map[string]string{"client_secret": "wrong-" + clientSecret}
	a = mustConfigure(t, cfg, srv.URL)
	_, err = a.GitCredential(context.Background(), "https://dev.azure.com/acme/Payments/_git/api")
	if err == nil || !strings.Contains(err.Error(), "AADSTS7000215") || strings.Contains(err.Error(), "Trace ID") {
		t.Fatalf("err = %v", err)
	}
	noSecret(t, err)
}

func fakeAPI(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, tok, _ := r.BasicAuth()
		if tok != pat || r.Header.Get("X-TFS-FedAuthRedirect") != "Suppress" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("api-version") != "7.1" {
			t.Errorf("api-version = %q", r.URL.Query().Get("api-version"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/_apis/git/repositories", "/Payments/_apis/git/repositories":
			_, _ = w.Write([]byte(`{"count":3,"value":[
				{"name":"api","remoteUrl":"https://acme@dev.azure.com/acme/Payments/_git/api","defaultBranch":"refs/heads/main","project":{"name":"Payments","visibility":"private"}},
				{"name":"web","remoteUrl":"https://acme@dev.azure.com/acme/Payments/_git/web","defaultBranch":"refs/heads/trunk","project":{"name":"Payments","visibility":"public"}},
				{"name":"old","remoteUrl":"https://acme@dev.azure.com/acme/Payments/_git/old","isDisabled":true,"project":{"name":"Payments"}}]}`))
		case "/Payments/_apis/git/repositories/api/refs":
			if r.URL.Query().Get("filter") != "heads/" {
				t.Errorf("filter = %q", r.URL.Query().Get("filter"))
			}
			_, _ = w.Write([]byte(`{"value":[{"name":"refs/heads/main"},{"name":"refs/heads/feature/x"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestListRepositoriesAndBranches(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": pat}}, "")

	repos, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 2 {
		t.Fatalf("repositories = %+v", repos)
	}
	want := api.Repository{URL: "https://dev.azure.com/acme/Payments/_git/api", FullName: "acme/Payments/api", DefaultBranch: "main", Private: true}
	if repos[0] != want {
		t.Fatalf("repository = %+v, want %+v", repos[0], want)
	}
	if repos[1].Private || repos[1].DefaultBranch != "trunk" {
		t.Fatalf("repository = %+v", repos[1])
	}

	repos, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Query: "payments WEB"})
	if err != nil || len(repos) != 1 || repos[0].FullName != "acme/Payments/web" {
		t.Fatalf("query = %+v, %v", repos, err)
	}
	repos, err = a.ListRepositories(context.Background(), api.ListRepositoriesRequest{Limit: 1})
	if err != nil || len(repos) != 1 {
		t.Fatalf("limit = %+v, %v", repos, err)
	}

	project := mustConfigure(t, map[string]any{"method": "token", "scope": "acme/Payments", "api_url": srv.URL,
		"credentials": map[string]string{"token": pat}}, "")
	if repos, err := project.ListRepositories(context.Background(), api.ListRepositoriesRequest{}); err != nil || len(repos) != 2 {
		t.Fatalf("project listing = %+v, %v", repos, err)
	}

	branches, err := a.ListBranches(context.Background(), "https://dev.azure.com/acme/Payments/_git/api")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(branches, ",") != "main,feature/x" {
		t.Fatalf("branches = %v", branches)
	}
	if _, err := a.ListBranches(context.Background(), "https://dev.azure.com/other/Payments/_git/api"); err == nil {
		t.Fatal("listed branches of a repository the connection is not for")
	}
}

func TestRefusedTokenIsActionable(t *testing.T) {
	srv := fakeAPI(t)
	defer srv.Close()
	a := mustConfigure(t, map[string]any{"method": "token", "scope": "acme", "api_url": srv.URL,
		"credentials": map[string]string{"token": "wrong-" + pat}}, "")
	_, err := a.ListRepositories(context.Background(), api.ListRepositoriesRequest{})
	if err == nil {
		t.Fatal("a refused token listed repositories")
	}
	e := errs.As(err)
	if e == nil || !strings.Contains(e.Message, "did not accept") || e.Remedy == "" {
		t.Fatalf("err = %v", err)
	}
	noSecret(t, err)
	if strings.Contains(err.Error(), "wrong-") {
		t.Fatal("the token is in the error")
	}
}

func TestSecretsDoNotRender(t *testing.T) {
	cred := api.GitCredential{BearerToken: secret.New(pat)}
	if strings.Contains(cred.BearerToken.String(), pat) {
		t.Fatal("the bearer token rendered")
	}
}
