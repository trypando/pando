// Package gitea is the source adapter for Gitea and Forgejo, Codeberg among
// them (R-091). A connection signs in with an access token, an OAuth2
// application authorized in the browser, or a deploy key, and may be limited to
// one owner — a user or an organization.
package gitea

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "gitea"

const provider = "Gitea"

// pageSize is what a listing asks for per page. Gitea's own default maximum
// is 50; a server configured lower answers with fewer, which is why a listing
// stops on an empty page rather than a short one.
const pageSize = 50

// maxPages bounds a listing, so a server that never answers an empty page
// cannot keep Pando paging forever.
const maxPages = 100

var methods = []string{forgekit.MethodToken, forgekit.MethodOAuth, forgekit.MethodSSH}

var scopes = []string{"read:repository"}

// Adapter is one connection to a Gitea or Forgejo host.
type Adapter struct {
	cfg        forgekit.Settings
	configured bool

	// web is where the host's pages and OAuth endpoints are, and api where
	// its REST API is. Both follow from the host; tests point them elsewhere.
	web string
	api string

	http *http.Client
	now  func() time.Time
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{now: time.Now} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySource }

// Configure reads the connection's settings and credentials.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg forgekit.Settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate("Gitea or Forgejo", methods, ""); err != nil {
		return err
	}
	if s := forgekit.ScopeSegments(cfg.Scope); len(s) > 1 {
		return errs.Newf(errs.ValidInvalid,
			"%q is not an owner. A Gitea or Forgejo connection can be limited to one user or organization, such as acme, and not to a path below it.", cfg.Scope).
			WithRemedy("Enter only the owner's name, or leave the setting empty for every repository the credential can read.")
	}
	if cfg.Method == forgekit.MethodOAuth && strings.TrimSpace(cfg.ClientID) == "" {
		return errs.New(errs.ValidInvalid, "A Gitea or Forgejo OAuth connection needs the OAuth2 application's client ID.").
			WithRemedy("Create an OAuth2 application under Settings → Applications on " + cfg.Host + ", with Pando's callback as its redirect URI, and enter its client ID and secret.")
	}
	if cfg.APIURL != "" {
		u, err := url.Parse(strings.TrimSpace(cfg.APIURL))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errs.Newf(errs.ValidInvalid,
				"The API address %q is not a URL Pando can call. Enter it in full, such as https://%s/api/v1.", cfg.APIURL, cfg.Host)
		}
	}
	a.cfg, a.configured = cfg, true
	a.web = "https://" + cfg.Host
	apiURL := strings.TrimSpace(cfg.APIURL)
	if apiURL == "" {
		apiURL = a.web + "/api/v1"
	}
	a.api = strings.TrimRight(apiURL, "/")
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
		return errs.New(errs.AdapterUnavailable, "This Gitea or Forgejo connection is not configured.")
	}
	return nil
}

func (a *Adapter) SourceCapabilities() api.SourceCapabilities {
	c := api.SourceCapabilities{
		Method:     a.cfg.Method,
		Host:       a.cfg.Host,
		Scope:      strings.Trim(a.cfg.Scope, "/"),
		Authorized: a.authorized(),
	}
	switch a.cfg.Method {
	case forgekit.MethodOAuth:
		c.WebAuthorization = a.configured && a.cfg.ClientID != "" && !a.cfg.Credential(forgekit.FieldClientSecret).IsZero()
		c.ListRepositories = c.Authorized
	case forgekit.MethodToken:
		c.ListRepositories = a.configured
	}
	return c
}

func (a *Adapter) authorized() bool {
	if !a.configured {
		return false
	}
	if a.cfg.Method == forgekit.MethodOAuth {
		return !a.cfg.Credential(forgekit.FieldAccessToken).IsZero()
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

// GitCredential is the key, the token, or the OAuth access token, refreshed
// when it has expired.
func (a *Adapter) GitCredential(ctx context.Context, repoURL string) (api.GitCredential, error) {
	if !a.configured {
		return api.GitCredential{}, errs.New(errs.AdapterUnavailable, "This Gitea or Forgejo connection is not configured.")
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return api.GitCredential{}, err
	}
	if a.cfg.Method == forgekit.MethodSSH {
		return forgekit.SSHCredential(u, "", a.cfg.Credential(forgekit.FieldSSHPrivateKey),
			a.cfg.Credential(forgekit.FieldSSHPassphrase), a.cfg.KnownHosts), nil
	}
	if u.SSH {
		return api.GitCredential{}, errs.Newf(errs.ValidInvalid,
			"The repository %s is an SSH address, and this connection signs in over HTTPS.", repoURL).
			WithRemedy("Enter the repository's HTTPS address instead, such as https://" + a.cfg.Host + "/" + u.Path + ".git.")
	}
	if a.cfg.Method == forgekit.MethodToken {
		username := strings.TrimSpace(a.cfg.Username)
		if username == "" {
			username = "oauth2"
		}
		return forgekit.TokenCredential(username, a.cfg.Credential(forgekit.FieldToken), a.cfg.CA()), nil
	}
	tokens, rotated, err := a.oauth().Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
	if err != nil {
		return api.GitCredential{}, err
	}
	cred := forgekit.TokenCredential("oauth2", tokens.Access, a.cfg.CA())
	cred.ExpiresAt = tokens.ExpiresAt
	if rotated {
		cred.Rotated = tokens.Fields()
	}
	return cred, nil
}

// Refresh renews an expired OAuth access token, and returns the tokens to
// store only when it did. Every other method has nothing to renew.
func (a *Adapter) Refresh(ctx context.Context) (map[string]secret.Value, error) {
	if !a.configured || a.cfg.Method != forgekit.MethodOAuth {
		return nil, nil
	}
	tokens, rotated, err := a.oauth().Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
	if err != nil || !rotated {
		return nil, err
	}
	return tokens.Fields(), nil
}

type repository struct {
	FullName      string `json:"full_name"`
	CloneURL      string `json:"clone_url"`
	DefaultBranch string `json:"default_branch"`
	Private       bool   `json:"private"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// ListRepositories lists the repositories the credential can read, narrowed
// to the connection's owner and the query.
func (a *Adapter) ListRepositories(ctx context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	limit := forgekit.Limit(req.Limit)
	owner := strings.Trim(strings.TrimSpace(a.cfg.Scope), "/")
	out := []api.Repository{}
	for page := 1; page <= maxPages; page++ {
		var repos []repository
		endpoint := a.api + "/user/repos?" + url.Values{
			"limit": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)},
		}.Encode()
		if _, err := client.Get(ctx, endpoint, &repos); err != nil {
			return nil, err
		}
		if len(repos) == 0 {
			break
		}
		for _, r := range repos {
			if owner != "" && !strings.EqualFold(ownerOf(r), owner) {
				continue
			}
			if !forgekit.Matches(r.FullName, req.Query) {
				continue
			}
			out = append(out, api.Repository{
				URL: r.CloneURL, FullName: r.FullName, DefaultBranch: r.DefaultBranch, Private: r.Private,
			})
			if len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

func ownerOf(r repository) string {
	if r.Owner.Login != "" {
		return r.Owner.Login
	}
	owner, _, _ := strings.Cut(r.FullName, "/")
	return owner
}

// ListBranches lists a repository's branches.
func (a *Adapter) ListBranches(ctx context.Context, repoURL string) ([]string, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	seg := u.Segments()
	if !strings.EqualFold(u.Host, a.cfg.Host) || len(seg) != 2 {
		return nil, errs.Newf(errs.ValidInvalid,
			"The repository %s is not a repository on %s, which this connection is for. A repository's address there has the form https://%s/owner/name.", repoURL, a.cfg.Host, a.cfg.Host).
			WithRemedy("Choose a repository on " + a.cfg.Host + ", or a connection for its host.")
	}
	base := a.api + "/repos/" + url.PathEscape(seg[0]) + "/" + url.PathEscape(seg[1]) + "/branches"
	var names []string
	for page := 1; page <= maxPages; page++ {
		var branches []struct {
			Name string `json:"name"`
		}
		endpoint := base + "?" + url.Values{"limit": {strconv.Itoa(pageSize)}, "page": {strconv.Itoa(page)}}.Encode()
		if _, err := client.Get(ctx, endpoint, &branches); err != nil {
			return nil, err
		}
		if len(branches) == 0 {
			break
		}
		for _, b := range branches {
			names = append(names, b.Name)
		}
	}
	return names, nil
}

func (a *Adapter) client(ctx context.Context) (forgekit.Client, error) {
	if !a.configured {
		return forgekit.Client{}, errs.New(errs.AdapterUnavailable, "This Gitea or Forgejo connection is not configured.")
	}
	c := forgekit.Client{HTTP: a.http, Provider: provider}
	switch a.cfg.Method {
	case forgekit.MethodToken:
		token := a.cfg.Credential(forgekit.FieldToken)
		c.Authorize = func(r *http.Request) { r.Header.Set("Authorization", "token "+token.Reveal()) }
	case forgekit.MethodOAuth:
		// A listing uses the token as it stands. Core calls Refresh first
		// and reconfigures with what it stored; refreshing here would
		// replace the refresh token with nowhere to keep the new one.
		tokens := forgekit.TokensFrom(a.cfg.Credentials)
		if tokens.Access.IsZero() {
			_, _, err := a.oauth().Fresh(ctx, tokens, a.now())
			return forgekit.Client{}, err
		}
		if !tokens.ExpiresAt.IsZero() && !a.now().Before(tokens.ExpiresAt) {
			return forgekit.Client{}, errs.Newf(errs.StateInvalid,
				"This connection's authorization expired at %s, so its repositories cannot be listed until it is renewed.",
				tokens.ExpiresAt.UTC().Format(time.RFC3339)).
				WithRemedy("Authorize the connection again under Adapters in the console, or with pando source authorize.")
		}
		c.Authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tokens.Access.Reveal()) }
	default:
		return forgekit.Client{}, errs.New(errs.ValidInvalid,
			"This connection signs in with a deploy key, which can clone but cannot use the host's API to list repositories or branches.").
			WithRemedy("Enter the repository's address and branch instead, or connect with an access token to pick from a list.")
	}
	return c, nil
}

func (a *Adapter) oauth() forgekit.OAuth {
	return forgekit.OAuth{
		Provider:     provider,
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.Credential(forgekit.FieldClientSecret),
		AuthorizeURL: a.web + "/login/oauth/authorize",
		TokenURL:     a.web + "/login/oauth/access_token",
		Scopes:       scopes,
		HTTP:         a.http,
	}
}

func (a *Adapter) checkOAuth(mode api.AuthorizationMode) error {
	if !a.configured {
		return errs.New(errs.AdapterUnavailable, "This Gitea or Forgejo connection is not configured.")
	}
	if a.cfg.Method != forgekit.MethodOAuth {
		return errs.New(errs.ValidInvalid,
			"This connection signs in with a token or a deploy key; only an OAuth connection is authorized.")
	}
	switch mode {
	case api.AuthorizationWeb:
	case api.AuthorizationDevice:
		return errs.New(errs.ValidInvalid,
			"Gitea and Forgejo do not offer device authorization.").
			WithRemedy("Authorize the connection in the browser instead, or connect with an access token.")
	default:
		return errs.Newf(errs.ValidInvalid, "%q is not an authorization mode. Use web.", mode)
	}
	if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
		return errs.New(errs.ValidInvalid,
			"Browser authorization needs the OAuth2 application's client secret, and this connection has none.").
			WithRemedy("Enter the application's client secret in the connection's settings.")
	}
	return nil
}

// BeginAuthorization starts browser authorization with PKCE.
func (a *Adapter) BeginAuthorization(_ context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if err := a.checkOAuth(req.Mode); err != nil {
		return api.Authorization{}, err
	}
	return a.oauth().BeginWeb(req, nil)
}

// CompleteAuthorization exchanges the code the browser came back with.
func (a *Adapter) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if err := a.checkOAuth(req.Mode); err != nil {
		return api.AuthorizationResult{}, err
	}
	return a.oauth().CompleteWeb(ctx, req, a.now())
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	oauthWhen := &api.Condition{Key: "method", Values: []string{forgekit.MethodOAuth}}
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodToken, Label: "Access token",
				Description: "An access token with read permission on repositories. Repositories can be picked from a list."},
			api.Option{Value: forgekit.MethodOAuth, Label: "OAuth2 application",
				Description: "Authorize once in the browser. Needs an address the host can send you back to."},
			api.Option{Value: forgekit.MethodSSH, Label: "Deploy key",
				Description: "An SSH key added to the repository as a deploy key. Repositories are entered by address."},
		),
		forgekit.HostField(""),
		forgekit.ScopeField("Owner",
			"Limit the connection to one user's or organization's repositories, such as acme. Empty is every repository the credential can read.",
			"acme"),
		forgekit.UsernameField("The account the token belongs to. Defaults to oauth2, which Gitea and Forgejo accept beside a token.", "oauth2"),
		forgekit.TokenField("Access token", "Create one under Settings → Applications with read permission on repositories."),
		{Key: "client_id", Label: "OAuth2 client ID", Type: "string", ShownWhen: oauthWhen,
			Help: "The client ID of an OAuth2 application created under Settings → Applications, with Pando's callback as its redirect URI."},
		{Key: forgekit.FieldClientSecret, Label: "OAuth2 client secret", Type: "string", Credential: true, ShownWhen: oauthWhen,
			Help: "The application's client secret. Gitea and Forgejo authorize only in the browser, so it is required."},
	}
	fields = append(fields, forgekit.SSHFields("The host's public key, as ssh-keyscan <host> prints it.")...)
	fields = append(fields, forgekit.APIURLField("https://<host>/api/v1"), forgekit.CAField())
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "Gitea or Forgejo",
		Description: "Clone from a Gitea or Forgejo server, Codeberg included, with an access token, an OAuth2 application, or a deploy key, optionally limited to one owner.",
		IDPrefix:    "src_",
		Fields:      fields,
		Presets: []api.Preset{
			{ID: "codeberg", Label: "Codeberg",
				Help:   "Create an access token on Codeberg under Settings → Applications, with read permission on repositories.",
				Values: map[string]string{"host": "codeberg.org"}},
		},
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
