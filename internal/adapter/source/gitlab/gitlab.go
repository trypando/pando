// Package gitlab is the source adapter for GitLab, on gitlab.com or
// self-managed (R-091). A connection signs in with an OAuth application (device
// or browser authorization), an access token or deploy token, or a deploy key,
// and may be limited to one group, nested or not.
package gitlab

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
const Kind = "gitlab"

// DefaultHost is the host a connection is for when its settings name none.
const DefaultHost = "gitlab.com"

// The token types a token connection takes.
const (
	TokenTypeAccess = "access_token"
	TokenTypeDeploy = "deploy_token"
)

// gitlabKnownHosts is GitLab.com's published SSH host key, so a deploy key
// connection to gitlab.com needs none pasted. Only the ed25519 key is pinned;
// a client offers ed25519 first, and the key is GitLab's documented one.
const gitlabKnownHosts = "gitlab.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAfuCHKVTjquxvt6CM6tdG4SLp1Btn/nOeHHE5UOzRdf"

const provider = "GitLab"

var methods = []string{forgekit.MethodOAuth, forgekit.MethodToken, forgekit.MethodSSH}

var scopes = []string{"read_repository", "read_api"}

type settings struct {
	forgekit.Settings
	TokenType string `json:"token_type"`
}

// Adapter is one connection to a GitLab host.
type Adapter struct {
	cfg        settings
	configured bool

	// web is where GitLab's own pages and OAuth endpoints are, and api where
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
	var cfg settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	if cfg.Method == forgekit.MethodSSH && strings.TrimSpace(cfg.KnownHosts) == "" &&
		forgekit.HostOf(firstNonEmpty(cfg.Host, DefaultHost)) == DefaultHost {
		cfg.KnownHosts = gitlabKnownHosts
	}
	if err := cfg.Validate(provider, methods, DefaultHost); err != nil {
		return err
	}
	switch cfg.Method {
	case forgekit.MethodToken:
		if cfg.TokenType == "" {
			cfg.TokenType = TokenTypeAccess
		}
		switch cfg.TokenType {
		case TokenTypeAccess:
		case TokenTypeDeploy:
			if strings.TrimSpace(cfg.Username) == "" {
				return errs.New(errs.ValidInvalid,
					"A GitLab deploy token connection needs the deploy token's username, which GitLab shows beside the token when it is created, such as gitlab+deploy-token-12.").
					WithRemedy("Enter the deploy token's username in the connection's settings.")
			}
		default:
			return errs.Newf(errs.ValidInvalid,
				"%q is not a GitLab token type Pando knows. Choose access_token for a personal, project or group access token, or deploy_token for a deploy token.",
				cfg.TokenType)
		}
	case forgekit.MethodOAuth:
		if strings.TrimSpace(cfg.ClientID) == "" {
			return errs.New(errs.ValidInvalid, "A GitLab OAuth connection needs the OAuth application's client ID.").
				WithRemedy("Create an application in GitLab under Preferences → Applications (or the group's or instance's Applications page) with the read_repository and read_api scopes, and enter its Application ID as the client ID.")
		}
	}
	if cfg.APIURL != "" {
		u, err := url.Parse(strings.TrimSpace(cfg.APIURL))
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			return errs.Newf(errs.ValidInvalid,
				"The API address %q is not a URL Pando can call. Enter it in full, such as https://%s/api/v4.", cfg.APIURL, cfg.Host)
		}
	}
	a.cfg, a.configured = cfg, true
	a.web = "https://" + cfg.Host
	a.api = strings.TrimRight(firstNonEmpty(strings.TrimSpace(cfg.APIURL), a.web+"/api/v4"), "/")
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
		return errs.New(errs.AdapterUnavailable, "This GitLab connection is not configured.")
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
		c.DeviceAuthorization = a.configured
		c.WebAuthorization = a.configured && !a.cfg.Credential(forgekit.FieldClientSecret).IsZero()
		c.ListRepositories = c.Authorized
	case forgekit.MethodToken:
		c.ListRepositories = a.configured && a.cfg.TokenType == TokenTypeAccess
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

// Covers scores the repository by host and group path.
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
		return api.GitCredential{}, errs.New(errs.AdapterUnavailable, "This GitLab connection is not configured.")
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
			"The repository %s is an SSH address, and this GitLab connection signs in over HTTPS.", repoURL).
			WithRemedy("Enter the repository's HTTPS address instead, such as https://" + a.cfg.Host + "/" + u.Path + ".git.")
	}
	if a.cfg.Method == forgekit.MethodToken {
		username := "oauth2"
		if a.cfg.TokenType == TokenTypeDeploy {
			username = a.cfg.Username
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

type project struct {
	PathWithNamespace string `json:"path_with_namespace"`
	HTTPURLToRepo     string `json:"http_url_to_repo"`
	DefaultBranch     string `json:"default_branch"`
	Visibility        string `json:"visibility"`
}

// ListRepositories lists the projects the connection is a member of, or those
// in its group and the group's subgroups.
func (a *Adapter) ListRepositories(ctx context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	limit := forgekit.Limit(req.Limit)
	// Not simple=true: the simple view leaves out visibility.
	q := url.Values{"per_page": {"100"}}
	if s := strings.TrimSpace(req.Query); s != "" {
		q.Set("search", s)
	}
	var endpoint string
	if scope := strings.Trim(strings.TrimSpace(a.cfg.Scope), "/"); scope != "" {
		q.Set("include_subgroups", "true")
		endpoint = a.api + "/groups/" + url.PathEscape(scope) + "/projects"
	} else {
		q.Set("membership", "true")
		q.Set("order_by", "last_activity_at")
		endpoint = a.api + "/projects"
	}
	out := []api.Repository{}
	err = a.pages(ctx, client, endpoint, q, func(raw json.RawMessage) (int, bool, error) {
		var page []project
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, false, errs.New(errs.AdapterFailed, "GitLab's API answered the project listing in a form Pando could not read.")
		}
		for _, p := range page {
			out = append(out, api.Repository{
				URL:           p.HTTPURLToRepo,
				FullName:      p.PathWithNamespace,
				DefaultBranch: p.DefaultBranch,
				Private:       p.Visibility != "public",
			})
			if len(out) >= limit {
				return len(page), true, nil
			}
		}
		return len(page), false, nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListBranches lists a project's branches.
func (a *Adapter) ListBranches(ctx context.Context, repoURL string) ([]string, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	u, err := forgekit.ParseRepoURL(repoURL)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(u.Host, a.cfg.Host) || len(u.Segments()) < 2 {
		return nil, errs.Newf(errs.ValidInvalid,
			"The repository %s is not a project on %s, which this GitLab connection is for.", repoURL, a.cfg.Host).
			WithRemedy("Choose a repository on " + a.cfg.Host + ", or a connection for its host.")
	}
	endpoint := a.api + "/projects/" + url.PathEscape(u.Path) + "/repository/branches"
	var names []string
	err = a.pages(ctx, client, endpoint, url.Values{"per_page": {"100"}}, func(raw json.RawMessage) (int, bool, error) {
		var page []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return 0, false, errs.New(errs.AdapterFailed, "GitLab's API answered the branch listing in a form Pando could not read.")
		}
		for _, b := range page {
			names = append(names, b.Name)
		}
		return len(page), len(names) >= 1000, nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// pages walks a listing. It follows the Link header's next page when it stays
// on the API's host — a credential is never sent anywhere else — and asks for
// the next page number when a full page came back without one. each returns
// how many items the page held and whether to stop.
func (a *Adapter) pages(ctx context.Context, client forgekit.Client, endpoint string, q url.Values,
	each func(json.RawMessage) (int, bool, error)) error {
	base, _ := url.Parse(a.api)
	page := 1
	next := endpoint + "?" + q.Encode()
	for i := 0; i < 100 && next != ""; i++ {
		var raw json.RawMessage
		link, err := client.Get(ctx, next, &raw)
		if err != nil {
			return err
		}
		n, stop, err := each(raw)
		if err != nil || stop || n == 0 {
			return err
		}
		page++
		next = ""
		if lu, err := url.Parse(link); link != "" && err == nil && base != nil && strings.EqualFold(lu.Host, base.Host) {
			next = link
		} else if n >= 100 {
			q.Set("page", strconv.Itoa(page))
			next = endpoint + "?" + q.Encode()
		}
	}
	return nil
}

func (a *Adapter) client(ctx context.Context) (forgekit.Client, error) {
	if !a.configured {
		return forgekit.Client{}, errs.New(errs.AdapterUnavailable, "This GitLab connection is not configured.")
	}
	c := forgekit.Client{HTTP: a.http, Provider: provider}
	switch {
	case a.cfg.Method == forgekit.MethodToken && a.cfg.TokenType == TokenTypeAccess:
		token := a.cfg.Credential(forgekit.FieldToken)
		c.Authorize = func(r *http.Request) { r.Header.Set("PRIVATE-TOKEN", token.Reveal()) }
	case a.cfg.Method == forgekit.MethodOAuth:
		// A listing uses the token as it stands. Core calls Refresh first
		// and reconfigures with what it stored; refreshing here would rotate
		// the refresh token with nowhere to keep the new one.
		tokens := forgekit.TokensFrom(a.cfg.Credentials)
		if tokens.Access.IsZero() {
			_, _, err := a.oauth().Fresh(ctx, tokens, a.now())
			return forgekit.Client{}, err
		}
		if !tokens.ExpiresAt.IsZero() && !a.now().Before(tokens.ExpiresAt) {
			return forgekit.Client{}, errs.Newf(errs.StateInvalid,
				"This GitLab connection's authorization expired at %s, so its repositories cannot be listed until it is renewed.",
				tokens.ExpiresAt.UTC().Format(time.RFC3339)).
				WithRemedy("Authorize the connection again under Adapters in the console, or with pando source authorize.")
		}
		c.Authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tokens.Access.Reveal()) }
	default:
		return forgekit.Client{}, noAPI(a.cfg.Method, a.cfg.TokenType)
	}
	return c, nil
}

func noAPI(method, tokenType string) error {
	what := "a deploy key"
	if method == forgekit.MethodToken && tokenType == TokenTypeDeploy {
		what = "a deploy token"
	}
	return errs.Newf(errs.ValidInvalid,
		"This GitLab connection signs in with %s, which can clone but cannot use GitLab's API to list repositories or branches.", what).
		WithRemedy("Enter the repository's address and branch instead, or connect GitLab with an access token or an OAuth application to pick from a list.")
}

func (a *Adapter) oauth() forgekit.OAuth {
	return forgekit.OAuth{
		Provider:     provider,
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.Credential(forgekit.FieldClientSecret),
		DeviceURL:    a.web + "/oauth/authorize_device",
		AuthorizeURL: a.web + "/oauth/authorize",
		TokenURL:     a.web + "/oauth/token",
		Scopes:       scopes,
		HTTP:         a.http,
	}
}

func (a *Adapter) checkOAuth(mode api.AuthorizationMode) error {
	if !a.configured {
		return errs.New(errs.AdapterUnavailable, "This GitLab connection is not configured.")
	}
	if a.cfg.Method != forgekit.MethodOAuth {
		return errs.Newf(errs.ValidInvalid,
			"This GitLab connection signs in with %s; only an OAuth connection is authorized.", methodName(a.cfg.Method))
	}
	switch mode {
	case api.AuthorizationDevice:
	case api.AuthorizationWeb:
		if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return errs.New(errs.ValidInvalid,
				"Browser authorization needs the GitLab OAuth application's secret, and this connection has none.").
				WithRemedy("Enter the application's secret in the connection's settings, or authorize with a device code instead (GitLab 17.2 or later).")
		}
	default:
		return errs.Newf(errs.ValidInvalid, "%q is not an authorization mode. Use device or web.", mode)
	}
	return nil
}

// BeginAuthorization starts device authorization (GitLab 17.2 and later) or
// browser authorization with PKCE.
func (a *Adapter) BeginAuthorization(ctx context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if err := a.checkOAuth(req.Mode); err != nil {
		return api.Authorization{}, err
	}
	if req.Mode == api.AuthorizationDevice {
		return a.oauth().BeginDevice(ctx, a.now())
	}
	return a.oauth().BeginWeb(req, nil)
}

// CompleteAuthorization polls the device code or exchanges the browser's code.
func (a *Adapter) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if err := a.checkOAuth(req.Mode); err != nil {
		return api.AuthorizationResult{}, err
	}
	if req.Mode == api.AuthorizationDevice {
		return a.oauth().PollDevice(ctx, req.Flow, a.now())
	}
	return a.oauth().CompleteWeb(ctx, req, a.now())
}

func methodName(m string) string {
	switch m {
	case forgekit.MethodSSH:
		return "a deploy key"
	case forgekit.MethodToken:
		return "a token"
	}
	return m
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	tokenWhen := &api.Condition{Key: "method", Values: []string{forgekit.MethodToken}}
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodOAuth, Label: "OAuth application",
				Description: "Authorize once in the browser or with a device code. Repositories can be picked from a list."},
			api.Option{Value: forgekit.MethodToken, Label: "Access token or deploy token",
				Description: "A personal, project or group access token, or a deploy token over HTTPS."},
			api.Option{Value: forgekit.MethodSSH, Label: "Deploy key",
				Description: "An SSH key added to the project as a deploy key. Repositories are entered by address."},
		),
		forgekit.HostField(DefaultHost),
		forgekit.ScopeField("Group",
			"Limit the connection to projects in this group and its subgroups, such as acme or acme/platform. Empty is every project the credential can read.",
			"acme/platform"),
	}
	fields = append(fields, forgekit.OAuthFields(
		"The Application ID of a GitLab OAuth application with the read_repository and read_api scopes. Create one under Preferences → Applications, or on a group's or the instance's Applications page.")...)
	fields = append(fields,
		api.Field{
			Key: "token_type", Label: "Token type", Type: "select", Default: TokenTypeAccess, ShownWhen: tokenWhen,
			Options: []api.Option{
				{Value: TokenTypeAccess, Label: "Access token",
					Description: "A personal, project or group access token with read_repository and read_api. Repositories can be picked from a list."},
				{Value: TokenTypeDeploy, Label: "Deploy token",
					Description: "A deploy token with read_repository. It cannot list repositories, so they are entered by address."},
			},
		},
		forgekit.UsernameField("For a deploy token, the username GitLab shows beside it, such as gitlab+deploy-token-12. Not used with an access token.", "gitlab+deploy-token-12"),
		forgekit.TokenField("Token", "Read access to repositories is enough: read_repository, and read_api to pick repositories from a list."),
	)
	fields = append(fields, forgekit.SSHFields(
		"The host's public key, as ssh-keyscan <host> prints it. Leave empty for gitlab.com; Pando knows its key.")...)
	fields = append(fields, forgekit.APIURLField("https://<host>/api/v4"), forgekit.CAField())
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "GitLab",
		Description: "Clone from gitlab.com or a self-managed GitLab with an OAuth application, an access or deploy token, or a deploy key, optionally limited to one group.",
		IDPrefix:    "src_",
		Fields:      fields,
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
