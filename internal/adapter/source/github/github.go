// Package github is the source adapter for GitHub (R-091): github.com and
// GitHub Enterprise Server, reached with a GitHub App installation, an OAuth
// application, a personal access token or a deploy key.
//
// A GitHub App is not entered by hand: authorizing the connection creates one
// from a manifest, owned by the person's account or organization, and then
// installs it (manifest.go). Each installation of Pando has its own App, so no
// private key is shared between installations.
//
// Stateless like every adapter (R-027): an App installation token is minted
// on every call rather than cached, and an OAuth token refreshed here goes
// back to core in GitCredential.Rotated for core to store.
package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "github"

const (
	provider    = "GitHub"
	defaultHost = "github.com"

	// The credential fields a GitHub App connection holds, all written by
	// Pando when it creates and installs the App: its private key, the
	// secret its webhook is signed with, and its ID, URL name and
	// installation, which are not secret but arrive with the key.
	FieldAppPrivateKey  = "app_private_key"
	FieldWebhookSecret  = api.CredentialWebhookSecret
	FieldAppID          = "app_id"
	FieldAppSlug        = "app_slug"
	FieldAppOwner       = "app_owner"
	FieldAppOwnerType   = "app_owner_type"
	FieldInstallationID = "installation_id"

	// tokenUser is the username GitHub expects beside a token over HTTPS.
	tokenUser = "x-access-token"

	apiVersion = "2022-11-28"
)

var methods = []string{forgekit.MethodApp, forgekit.MethodOAuth, forgekit.MethodToken, forgekit.MethodSSH}

// githubKnownHosts are the SSH host keys GitHub publishes for github.com at
// https://api.github.com/meta, used when an SSH connection to github.com
// names none.
const githubKnownHosts = `github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl
github.com ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBEmKSENjQEezOmxkZMy7opKgwFB9nkt5YRrYMjNuG5N87uRgg6CLrbo5wAdT/y6v0mKV0U2w0WZ2YB/++Tpockg=
github.com ssh-rsa AAAAB3NzaC1yc2EAAAADAQABAAABgQCj7ndNxQowgcQnjshcLrqPEiiphnt+VTTvDP6mHBL9j1aNUkY4Ue1gvwnGLVlOhGeYrnZaMgRK6+PKCUXaDbC7qtbW8gIkhL7aGCsOr/C56SJMy/BCZfxd1nWzAOxSDPgVsmerOBYfNqltV9/hWCqBywINIR+5dIg6JTJ72pcEpEjcYgXkE2YEFXV1JHnsKgbLWNlhScqb2UmyRkQyytRLtL+38TGxkxCflmO+5Z8CSSNY7GidjMIZ7Q4zMjA2n1nGrlTDkzwDCsw+wqFPGQA179cnfGWOWRVruj16z6XyvxvjJwbz0wQZ75XK5tKSb7FNyeIEs4TT4jk+S4dhPeAUC5y+bDYirYgM4GC7uEnztnZyaVWQ7B381AK4Qdrwt51ZqExKbQpTUNn+EjqoTwvqNj4kqx5QUCI0ThS/YkOxJCXmPUWZbhjpCg56i+2aB6CmK2JGhn57K5mj0MNdBXA4/WnwH6XoPWJzK5Nyu2zB3nAZp+S5hpQs+p1vN1/wsjk=
`

// settings are a GitHub connection's settings.
type settings struct {
	forgekit.Settings

	// AppID is the GitHub App's ID, and InstallationID the installation of
	// it to mint tokens for, for a connection whose App was entered by hand
	// before Pando created Apps itself. An empty installation is looked up
	// by owner. A connection Pando created the App for keeps both among its
	// credentials instead.
	AppID          string `json:"app_id"`
	InstallationID string `json:"installation_id"`

	// PushWebhook gives the App Pando creates a webhook, so a push starts a
	// check at once (R-142). Nil is on.
	PushWebhook *bool `json:"push_webhook"`
}

// pushWebhook says whether the App is created with its webhook.
func (s settings) pushWebhook() bool { return s.PushWebhook == nil || *s.PushWebhook }

// Adapter is one connection to GitHub.
type Adapter struct {
	cfg        settings
	configured bool

	apiURL string // https://api.github.com, or https://HOST/api/v3
	webURL string // https://github.com, or https://HOST; where OAuth lives
	http   *http.Client
	appKey *rsa.PrivateKey // the App's parsed private key, once it is created
	now    func() time.Time
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{now: time.Now} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySource }

// Configure reads the connection's settings and credentials.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	// The shared validation requires known hosts for ssh; github.com's are
	// published, so fill them before it looks.
	if cfg.Method == forgekit.MethodSSH && strings.TrimSpace(cfg.KnownHosts) == "" &&
		forgekit.HostOf(firstNonEmpty(cfg.Host, defaultHost)) == defaultHost {
		cfg.KnownHosts = githubKnownHosts
	}
	if err := cfg.Validate(provider, methods, defaultHost); err != nil {
		return err
	}
	cfg.Scope = strings.Trim(strings.TrimSpace(cfg.Scope), "/")
	if strings.Contains(cfg.Scope, "/") {
		return errs.Newf(errs.ValidInvalid,
			"%q is not a GitHub owner. A GitHub connection is limited to one organization or user, such as acme.", cfg.Scope).
			WithRemedy("Enter only the organization or user name, or leave the owner empty for every repository the credential can read.")
	}

	a.webURL = "https://" + cfg.Host
	a.apiURL = "https://" + cfg.Host + "/api/v3"
	if cfg.Host == defaultHost {
		a.apiURL = "https://api.github.com"
	}
	if cfg.APIURL != "" {
		u, err := url.Parse(strings.TrimSpace(cfg.APIURL))
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return errs.Newf(errs.ValidInvalid,
				"%q is not an address Pando can reach GitHub's API at. Use a full address, such as https://github.acme.internal/api/v3.", cfg.APIURL)
		}
		a.apiURL = strings.TrimRight(u.String(), "/")
	}

	switch cfg.Method {
	case forgekit.MethodApp:
		key, err := checkApp(&cfg)
		if err != nil {
			return err
		}
		a.appKey = key
	case forgekit.MethodOAuth:
		if strings.TrimSpace(cfg.ClientID) == "" {
			return errs.New(errs.ValidInvalid, "A GitHub OAuth connection needs the OAuth application's client ID.").
				WithRemedy("Register an OAuth application in GitHub under Settings → Developer settings → OAuth Apps, enable device flow, and enter its client ID.")
		}
	}

	a.cfg, a.configured = cfg, true
	a.http = forgekit.HTTPClient(cfg.CA())
	if a.now == nil {
		a.now = time.Now
	}
	return nil
}

// HealthCheck succeeds once configured. Whether the credential still works is
// learned at the next clone or listing, which reports it (R-146).
func (a *Adapter) HealthCheck(context.Context) error {
	if !a.configured {
		return errs.New(errs.AdapterUnavailable, "This GitHub connection is not configured.")
	}
	return nil
}

func (a *Adapter) SourceCapabilities() api.SourceCapabilities {
	c := api.SourceCapabilities{
		Method:           a.cfg.Method,
		Host:             a.cfg.Host,
		Scope:            a.cfg.Scope,
		ListRepositories: a.cfg.Method != forgekit.MethodSSH,
		Authorized:       a.authorized(),
	}
	switch a.cfg.Method {
	case forgekit.MethodOAuth:
		c.DeviceAuthorization = true
		c.WebAuthorization = !a.cfg.Credential(forgekit.FieldClientSecret).IsZero()
	case forgekit.MethodApp:
		// Creating the App, and installing it, happen in the browser. One
		// entered by hand has no URL name to install it from.
		c.WebAuthorization = a.appKey == nil || a.cfg.Credential(FieldAppSlug).Reveal() != ""
		// Turning the webhook off stops Pando acting on it at once, though the
		// App on GitHub keeps sending until it is changed there.
		c.Webhook = a.appKey != nil && a.cfg.pushWebhook() && !a.cfg.Credential(FieldWebhookSecret).IsZero()
		c.ManageURL = a.appSettingsURL()
	}
	return c
}

func (a *Adapter) authorized() bool {
	if !a.configured {
		return false
	}
	switch a.cfg.Method {
	case forgekit.MethodOAuth:
		return !a.cfg.Credential(forgekit.FieldAccessToken).IsZero()
	case forgekit.MethodApp:
		return a.appKey != nil && (a.cfg.installationID() != "" || a.cfg.byOwner())
	}
	return true
}

// Covers scores the repository by host and owner.
func (a *Adapter) Covers(repoURL string) int {
	if !a.authorized() {
		return 0
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return 0
	}
	return forgekit.Covers([]string{a.cfg.Host}, forgekit.ScopeSegments(a.cfg.Scope), u.Host, u.Segments())
}

// GitCredential is the installation token, the OAuth token, the personal
// access token or the deploy key.
func (a *Adapter) GitCredential(ctx context.Context, repoURL string) (api.GitCredential, error) {
	if !a.configured {
		return api.GitCredential{}, notConfigured()
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return api.GitCredential{}, err
	}
	if a.cfg.Method == forgekit.MethodSSH {
		return forgekit.SSHCredential(u, "", a.cfg.Credential(forgekit.FieldSSHPrivateKey),
			a.cfg.Credential(forgekit.FieldSSHPassphrase), a.cfg.KnownHosts), nil
	}

	var cred api.GitCredential
	switch a.cfg.Method {
	case forgekit.MethodApp:
		tok, expires, err := a.installationToken(ctx)
		if err != nil {
			return api.GitCredential{}, err
		}
		cred = forgekit.TokenCredential(tokenUser, tok, a.cfg.CA())
		cred.ExpiresAt = expires
	case forgekit.MethodOAuth:
		o := a.oauth()
		tokens, rotated, err := o.Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
		if err != nil {
			return api.GitCredential{}, err
		}
		cred = forgekit.TokenCredential(tokenUser, tokens.Access, a.cfg.CA())
		cred.ExpiresAt = tokens.ExpiresAt
		if rotated {
			cred.Rotated = tokens.Fields()
		}
	default:
		cred = forgekit.TokenCredential(tokenUser, a.cfg.Credential(forgekit.FieldToken), a.cfg.CA())
	}
	if u.SSH {
		// A token works over HTTPS only; clone the same repository there. The
		// spec keeps the address the app names.
		cred.URL = forgekit.HTTPSURL(a.cfg.Host, "", u.Path)
	}
	return cred, nil
}

// Refresh renews an expired OAuth token and returns what to store. Nil for
// every other method, and for an OAuth token still good.
func (a *Adapter) Refresh(ctx context.Context) (map[string]secret.Value, error) {
	if !a.configured {
		return nil, notConfigured()
	}
	if a.cfg.Method != forgekit.MethodOAuth {
		return nil, nil
	}
	tokens, rotated, err := a.oauth().Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
	if err != nil || !rotated {
		return nil, err
	}
	return tokens.Fields(), nil
}

// BeginAuthorization starts a device or browser authorization, or the
// creation or installation of a GitHub App.
func (a *Adapter) BeginAuthorization(ctx context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if a.configured && a.cfg.Method == forgekit.MethodApp {
		return a.beginApp(ctx, req)
	}
	if err := a.requireOAuth(); err != nil {
		return api.Authorization{}, err
	}
	switch req.Mode {
	case api.AuthorizationDevice, "":
		return a.oauth().BeginDevice(ctx, a.now())
	case api.AuthorizationWeb:
		if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return api.Authorization{}, noClientSecret()
		}
		return a.oauth().BeginWeb(req, nil)
	default:
		return api.Authorization{}, unknownMode(req.Mode)
	}
}

// CompleteAuthorization polls the device code or exchanges the browser's
// code, or finishes a GitHub App's creation or installation.
func (a *Adapter) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if a.configured && a.cfg.Method == forgekit.MethodApp {
		return a.completeApp(ctx, req)
	}
	if err := a.requireOAuth(); err != nil {
		return api.AuthorizationResult{}, err
	}
	switch req.Mode {
	case api.AuthorizationDevice, "":
		return a.oauth().PollDevice(ctx, req.Flow, a.now())
	case api.AuthorizationWeb:
		if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return api.AuthorizationResult{}, noClientSecret()
		}
		return a.oauth().CompleteWeb(ctx, req, a.now())
	default:
		return api.AuthorizationResult{}, unknownMode(req.Mode)
	}
}

func (a *Adapter) oauth() forgekit.OAuth {
	return forgekit.OAuth{
		Provider:     provider,
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.Credential(forgekit.FieldClientSecret),
		DeviceURL:    a.webURL + "/login/device/code",
		AuthorizeURL: a.webURL + "/login/oauth/authorize",
		TokenURL:     a.webURL + "/login/oauth/access_token",
		Scopes:       []string{"repo"},
		HTTP:         a.http,
	}
}

func (a *Adapter) requireOAuth() error {
	if !a.configured {
		return notConfigured()
	}
	if a.cfg.Method != forgekit.MethodOAuth {
		return errs.Newf(errs.ValidInvalid,
			"This GitHub connection signs in with %s; there is nothing to authorize. Only a connection whose method is app or oauth is authorized.", methodName(a.cfg.Method))
	}
	return nil
}

func methodName(m string) string {
	switch m {
	case forgekit.MethodApp:
		return "a GitHub App"
	case forgekit.MethodToken:
		return "a personal access token"
	case forgekit.MethodSSH:
		return "a deploy key"
	}
	return m
}

func notConfigured() error {
	return errs.New(errs.AdapterUnavailable, "This GitHub connection is not configured.")
}

func noClientSecret() error {
	return errs.New(errs.ValidInvalid,
		"Browser authorization needs the GitHub OAuth application's client secret, and this connection has none.").
		WithRemedy("Enter the client secret in the connection's settings, or authorize with a device code instead.")
}

func unknownMode(m api.AuthorizationMode) error {
	return errs.Newf(errs.ValidInvalid,
		"%q is not a way to authorize a GitHub connection. Use device or web.", string(m))
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	appWhen := &api.Condition{Key: "method", Values: []string{forgekit.MethodApp}}
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodApp, Label: "GitHub App", Recommended: true,
				Description: "Pando creates an App on GitHub, and you pick the repositories it reads. Tokens are short-lived."},
			api.Option{Value: forgekit.MethodOAuth, Label: "OAuth application",
				Description: "Authorize as a GitHub user, with a code entered on GitHub or in the browser."},
			api.Option{Value: forgekit.MethodToken, Label: "Personal access token",
				Description: "A fine-grained or classic token with read access to repository contents."},
			api.Option{Value: forgekit.MethodSSH, Label: "Deploy key",
				Description: "An SSH key added to one repository. Repositories are entered by address."},
		),
		forgekit.HostField(defaultHost),
		forgekit.ScopeField("Owner",
			"The organization or user on GitHub. Empty is every repository the credential reaches. A GitHub App is created and installed here only; empty is your own account.",
			"acme"),
		{Key: "push_webhook", Label: "Deploy on push", Type: "bool", Default: "true", ShownWhen: appWhen,
			Help: "Needs a public Pando address. Without one, Pando checks for pushes on a schedule."},
	}
	fields = append(fields, forgekit.OAuthFields(
		"From the OAuth application registered in GitHub under Settings → Developer settings → OAuth Apps, with device flow enabled.")...)
	fields = append(fields, forgekit.TokenField("Personal access token",
		"A fine-grained token with read access to contents and metadata, or a classic token with the repo scope."))
	fields = append(fields, forgekit.SSHFields(
		"The host's public key, as ssh-keyscan <host> prints it. For github.com it may be left empty: Pando uses the keys GitHub publishes.")...)
	fields = append(fields,
		forgekit.APIURLField("https://api.github.com for github.com, https://<host>/api/v3 for GitHub Enterprise Server"),
		forgekit.CAField(),
	)
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "GitHub",
		Description: "Clone private repositories from github.com or GitHub Enterprise Server, and pick them from a list, with a GitHub App, an OAuth application, a personal access token or a deploy key.",
		IDPrefix:    "src_",
		Fields:      fields,
		Presets: []api.Preset{
			{ID: "github.com", Label: "GitHub.com",
				Help:   "Pando creates a GitHub App for you when the connection is saved.",
				Values: map[string]string{"host": defaultHost}},
			{ID: "ghes", Label: "GitHub Enterprise Server",
				Help: "Enter the server's host. Its API is at https://<host>/api/v3 unless it was moved."},
		},
		Steps: steps(),
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
