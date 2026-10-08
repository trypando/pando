// Package bitbucket is the source adapter for Bitbucket Cloud and Bitbucket
// Data Center (R-091). The host decides which: bitbucket.org is Cloud, any
// other host is a Data Center server. It signs in with an access token or an
// API token, with an OAuth consumer (Cloud), or with an SSH access key.
package bitbucket

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
const Kind = "bitbucket"

const (
	cloudHost      = "bitbucket.org"
	cloudAPI       = "https://api.bitbucket.org/2.0"
	cloudOAuth     = "https://bitbucket.org/site/oauth2"
	defaultSSHPort = "7999"

	// The kinds of token a Bitbucket Cloud connection takes.
	tokenTypeAccess = "access_token" // a workspace, project or repository access token
	tokenTypeAPI    = "api_token"    // an Atlassian account's API token

	// tokenUsername goes beside an access token or an OAuth token.
	tokenUsername = "x-token-auth"

	// pageSize is how many results each listing request asks for, and
	// maxPages how many pages a listing follows at most.
	pageSize = 100
	maxPages = 50
)

var methods = []string{forgekit.MethodToken, forgekit.MethodOAuth, forgekit.MethodSSH}

type settings struct {
	forgekit.Settings

	// TokenType is which kind of Bitbucket Cloud token the token is.
	TokenType string `json:"token_type"`

	// Email is the Atlassian account email an API token signs in to the API
	// with. The username when empty.
	Email string `json:"email"`

	// SSHPort is Bitbucket Data Center's SSH port.
	SSHPort string `json:"ssh_port"`
}

// Adapter is one connection to Bitbucket Cloud or a Bitbucket Data Center.
type Adapter struct {
	cfg        settings
	cloud      bool
	configured bool
	http       *http.Client

	// oauthBase is where Bitbucket Cloud's OAuth endpoints are and now the
	// clock; tests replace them.
	oauthBase string
	now       func() time.Time
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{oauthBase: cloudOAuth, now: time.Now} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySource }

// Configure reads the connection's settings and credentials.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate("Bitbucket", methods, cloudHost); err != nil {
		return err
	}
	cloud := cfg.Host == cloudHost
	scope := forgekit.ScopeSegments(cfg.Scope)
	if len(scope) > 1 {
		if cloud {
			return errs.Newf(errs.ValidInvalid,
				"A Bitbucket Cloud connection is limited to one workspace, and %q names more than that.", cfg.Scope).
				WithRemedy("Enter the workspace ID as it appears in https://bitbucket.org/{workspace}, such as acme.")
		}
		return errs.Newf(errs.ValidInvalid,
			"A Bitbucket Data Center connection is limited to one project, and %q names more than that.", cfg.Scope).
			WithRemedy("Enter the project key, such as PAY.")
	}
	cfg.Scope = strings.Join(scope, "/")
	cfg.Username = strings.TrimSpace(cfg.Username)
	cfg.Email = strings.TrimSpace(cfg.Email)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)

	switch cfg.Method {
	case forgekit.MethodToken:
		if !cloud {
			cfg.TokenType = ""
			break
		}
		if cfg.TokenType == "" {
			cfg.TokenType = tokenTypeAccess
		}
		switch cfg.TokenType {
		case tokenTypeAccess:
		case tokenTypeAPI:
			if cfg.Username == "" {
				return errs.New(errs.ValidInvalid,
					"A Bitbucket Cloud API token needs the Bitbucket username it belongs to, which git signs in with beside it.").
					WithRemedy("Enter your Bitbucket username, shown under Personal settings → Account settings.")
			}
		default:
			return errs.Newf(errs.ValidInvalid,
				"%q is not a kind of Bitbucket Cloud token Pando knows. Choose access_token or api_token.", cfg.TokenType)
		}
	case forgekit.MethodOAuth:
		if !cloud {
			return errs.New(errs.ValidInvalid,
				"Pando signs in to Bitbucket Data Center with an HTTP access token or an SSH key, not OAuth.").
				WithRemedy("Create an HTTP access token with repository read permission and connect with the token method.")
		}
		if cfg.ClientID == "" || cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return errs.New(errs.ValidInvalid,
				"A Bitbucket Cloud OAuth connection needs an OAuth consumer's key and secret.").
				WithRemedy("Add an OAuth consumer under the workspace's settings → OAuth consumers, with Pando's callback address and the Repositories: Read permission, and enter its key and secret.")
		}
	case forgekit.MethodSSH:
		cfg.SSHPort = strings.TrimSpace(cfg.SSHPort)
		if cfg.SSHPort == "" {
			cfg.SSHPort = defaultSSHPort
		}
		if n, err := strconv.Atoi(cfg.SSHPort); err != nil || n < 1 || n > 65535 {
			return errs.Newf(errs.ValidInvalid, "%q is not a port number. Bitbucket Data Center's SSH port is usually 7999.", cfg.SSHPort)
		}
	}

	a.cfg, a.cloud, a.configured = cfg, cloud, true
	a.http = forgekit.HTTPClient(cfg.CA())
	if a.oauthBase == "" {
		a.oauthBase = cloudOAuth
	}
	if a.now == nil {
		a.now = time.Now
	}
	return nil
}

// HealthCheck succeeds once configured. Whether the credential still works is
// learned at the next clone or listing, which reports it (R-146).
func (a *Adapter) HealthCheck(context.Context) error {
	if !a.configured {
		return errs.New(errs.AdapterUnavailable, "This Bitbucket connection is not configured.")
	}
	return nil
}

func (a *Adapter) SourceCapabilities() api.SourceCapabilities {
	oauth := a.cfg.Method == forgekit.MethodOAuth
	authorized := a.configured
	if oauth {
		authorized = authorized && !forgekit.TokensFrom(a.cfg.Credentials).Access.IsZero()
	}
	return api.SourceCapabilities{
		Method:           a.cfg.Method,
		Host:             a.cfg.Host,
		Scope:            a.cfg.Scope,
		ListRepositories: a.configured && a.cfg.Method != forgekit.MethodSSH,
		WebAuthorization: oauth,
		Authorized:       authorized,
	}
}

// Covers scores the address by workspace (Cloud) or project key (Data
// Center).
func (a *Adapter) Covers(repoURL string) int {
	if !a.SourceCapabilities().Authorized {
		return 0
	}
	ref, ok := parse(repoURL)
	if !ok {
		return 0
	}
	return forgekit.Covers([]string{a.cfg.Host}, forgekit.ScopeSegments(a.cfg.Scope), ref.host, []string{ref.owner, ref.slug})
}

// GitCredential is the token, the OAuth token (refreshed here), or the key.
func (a *Adapter) GitCredential(ctx context.Context, repoURL string) (api.GitCredential, error) {
	ref, ok := parse(repoURL)
	if !ok {
		return api.GitCredential{}, a.notAddress(repoURL)
	}
	if a.cfg.Method == forgekit.MethodSSH {
		return a.sshCredential(ref), nil
	}
	if ref.ssh {
		return api.GitCredential{}, errs.Newf(errs.ValidInvalid,
			"The repository %s is an SSH address, and this Bitbucket connection signs in over HTTPS.", repoURL).
			WithRemedy("Enter the repository's HTTPS clone address instead.")
	}
	ca := a.cfg.CA()
	switch a.cfg.Method {
	case forgekit.MethodToken:
		username := tokenUsername
		if a.cfg.TokenType == tokenTypeAPI || (!a.cloud && a.cfg.Username != "") {
			username = a.cfg.Username
		}
		return forgekit.TokenCredential(username, a.cfg.Credential(forgekit.FieldToken), ca), nil
	case forgekit.MethodOAuth:
		tokens, rotated, err := a.oauth().Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
		if err != nil {
			return api.GitCredential{}, err
		}
		cred := forgekit.TokenCredential(tokenUsername, tokens.Access, ca)
		cred.ExpiresAt = tokens.ExpiresAt
		if rotated {
			cred.Rotated = tokens.Fields()
		}
		return cred, nil
	}
	return api.GitCredential{}, errs.Newf(errs.Internal, "Bitbucket connection method %q has no credential.", a.cfg.Method)
}

// sshCredential is the key, with an HTTPS address turned into the SSH one:
// git@bitbucket.org:{workspace}/{repo}.git on Cloud, and
// ssh://git@HOST:7999/{project}/{repo}.git on Data Center.
func (a *Adapter) sshCredential(ref repoRef) api.GitCredential {
	cred := api.GitCredential{
		SSHUser:       "git",
		SSHPrivateKey: a.cfg.Credential(forgekit.FieldSSHPrivateKey),
		SSHPassphrase: a.cfg.Credential(forgekit.FieldSSHPassphrase),
		KnownHosts:    a.cfg.KnownHosts,
	}
	if ref.ssh {
		if ref.user != "" {
			cred.SSHUser = ref.user
		}
		return cred
	}
	port := ""
	if !a.cloud {
		port = a.cfg.SSHPort
	}
	cred.URL = forgekit.SCPURL("git", ref.host, port, ref.owner+"/"+ref.slug)
	return cred
}

// Refresh renews an expired OAuth access token and returns the tokens to
// store. Nil for every other method, and for a token still good.
func (a *Adapter) Refresh(ctx context.Context) (map[string]secret.Value, error) {
	if !a.configured {
		return nil, errs.New(errs.AdapterUnavailable, "This Bitbucket connection is not configured.")
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

// storedAccess is the stored OAuth access token, for the API. A listing
// never refreshes: core calls Refresh first and stores what it returns, so a
// token renewed here would be lost.
func (a *Adapter) storedAccess() (secret.Value, error) {
	t := forgekit.TokensFrom(a.cfg.Credentials)
	if t.Access.IsZero() {
		return secret.Value{}, errs.New(errs.StateInvalid, "This Bitbucket connection has not been authorized yet.").
			WithRemedy("Authorize it under Sources in the console, or with pando source authorize.")
	}
	if !t.ExpiresAt.IsZero() && !a.now().Before(t.ExpiresAt) {
		return secret.Value{}, errs.Newf(errs.StateInvalid,
			"This Bitbucket connection's authorization expired at %s, so its repositories cannot be listed until it is renewed.",
			t.ExpiresAt.UTC().Format(time.RFC3339)).
			WithRemedy("Authorize it again under Sources in the console.")
	}
	return t.Access, nil
}

// ListRepositories lists repositories the credential can read, in the
// workspace or project when the connection is limited to one.
func (a *Adapter) ListRepositories(ctx context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	limit := forgekit.Limit(req.Limit)
	if a.cloud {
		return a.cloudRepositories(ctx, client, req.Query, limit)
	}
	return a.dcRepositories(ctx, client, req.Query, limit)
}

type cloneLinks struct {
	Clone []struct {
		Name string `json:"name"`
		Href string `json:"href"`
	} `json:"clone"`
}

func (l cloneLinks) https() string {
	for _, c := range l.Clone {
		if c.Name == "https" || c.Name == "http" {
			return stripUser(c.Href)
		}
	}
	return ""
}

func (a *Adapter) cloudRepositories(ctx context.Context, client forgekit.Client, query string, limit int) ([]api.Repository, error) {
	base := a.apiBase()
	q := url.Values{"role": {"member"}, "pagelen": {strconv.Itoa(pageSize)}}
	// Bitbucket filters on the whole query as one substring; a query of
	// several words is matched here instead (forgekit.Matches).
	if w := strings.TrimSpace(query); w != "" && !strings.ContainsAny(w, " \t") {
		q.Set("q", `name~"`+strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w)+`"`)
	}
	next := base + "/repositories?" + q.Encode()
	if a.cfg.Scope != "" {
		next = base + "/repositories/" + url.PathEscape(a.cfg.Scope) + "?" + q.Encode()
	}
	out := []api.Repository{}
	for pages := 0; next != "" && len(out) < limit && pages < maxPages; pages++ {
		var page struct {
			Values []struct {
				FullName   string `json:"full_name"`
				IsPrivate  bool   `json:"is_private"`
				Mainbranch *struct {
					Name string `json:"name"`
				} `json:"mainbranch"`
				Links cloneLinks `json:"links"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if _, err := client.Get(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Values {
			if !forgekit.Matches(r.FullName, query) || len(out) >= limit {
				continue
			}
			repo := api.Repository{URL: r.Links.https(), FullName: r.FullName, Private: r.IsPrivate}
			if repo.URL == "" {
				repo.URL = "https://" + cloudHost + "/" + r.FullName + ".git"
			}
			if r.Mainbranch != nil {
				repo.DefaultBranch = r.Mainbranch.Name
			}
			out = append(out, repo)
		}
		next = page.Next
		if next != "" && !sameOrigin(next, base) {
			break
		}
	}
	return out, nil
}

// dcPage is a Bitbucket Data Center page.
type dcPage[T any] struct {
	Values        []T  `json:"values"`
	IsLastPage    bool `json:"isLastPage"`
	NextPageStart int  `json:"nextPageStart"`
}

// dcEach fetches every page of a Data Center listing, until each says it is
// the last or stop says to.
func dcEach[T any](ctx context.Context, client forgekit.Client, endpoint string, q url.Values, each func(T) bool) error {
	for pages := 0; pages < maxPages; pages++ {
		var page dcPage[T]
		if _, err := client.Get(ctx, endpoint+"?"+q.Encode(), &page); err != nil {
			return err
		}
		for _, v := range page.Values {
			if !each(v) {
				return nil
			}
		}
		if page.IsLastPage || len(page.Values) == 0 {
			return nil
		}
		q.Set("start", strconv.Itoa(page.NextPageStart))
	}
	return nil
}

func (a *Adapter) dcRepositories(ctx context.Context, client forgekit.Client, query string, limit int) ([]api.Repository, error) {
	base := a.apiBase()
	q := url.Values{"limit": {strconv.Itoa(pageSize)}}
	endpoint := base + "/repos"
	if a.cfg.Scope != "" {
		endpoint = base + "/projects/" + url.PathEscape(projectKey(a.cfg.Scope)) + "/repos"
	} else if w := strings.TrimSpace(query); w != "" && !strings.ContainsAny(w, " \t") {
		q.Set("name", w)
	}
	type dcRepo struct {
		Slug    string `json:"slug"`
		Public  bool   `json:"public"`
		Project struct {
			Key string `json:"key"`
		} `json:"project"`
		Links cloneLinks `json:"links"`
	}
	out := []api.Repository{}
	err := dcEach(ctx, client, endpoint, q, func(r dcRepo) bool {
		full := r.Project.Key + "/" + r.Slug
		if !forgekit.Matches(full, query) {
			return true
		}
		repo := api.Repository{URL: r.Links.https(), FullName: full, Private: !r.Public}
		if repo.URL == "" {
			repo.URL = "https://" + a.cfg.Host + "/scm/" + strings.ToLower(r.Project.Key) + "/" + r.Slug + ".git"
		}
		out = append(out, repo)
		return len(out) < limit
	})
	return out, err
}

// ListBranches lists a repository's branches.
func (a *Adapter) ListBranches(ctx context.Context, repoURL string) ([]string, error) {
	ref, ok := parse(repoURL)
	if !ok {
		return nil, a.notAddress(repoURL)
	}
	if a.Covers(repoURL) == 0 {
		return nil, errs.Newf(errs.ValidInvalid,
			"The repository %s is not one this Bitbucket connection is for.", repoURL).
			WithRemedy("Use the connection for the repository's workspace or project, or add one.")
	}
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	base := a.apiBase()
	out := []string{}
	if !a.cloud {
		endpoint := base + "/projects/" + url.PathEscape(projectKey(ref.owner)) + "/repos/" + url.PathEscape(ref.slug) + "/branches"
		err := dcEach(ctx, client, endpoint, url.Values{"limit": {strconv.Itoa(pageSize)}}, func(b struct {
			DisplayID string `json:"displayId"`
		}) bool {
			out = append(out, b.DisplayID)
			return true
		})
		return out, err
	}
	next := base + "/repositories/" + url.PathEscape(ref.owner) + "/" + url.PathEscape(ref.slug) + "/refs/branches?pagelen=" + strconv.Itoa(pageSize)
	for pages := 0; next != "" && pages < maxPages; pages++ {
		var page struct {
			Values []struct {
				Name string `json:"name"`
			} `json:"values"`
			Next string `json:"next"`
		}
		if _, err := client.Get(ctx, next, &page); err != nil {
			return nil, err
		}
		for _, b := range page.Values {
			out = append(out, b.Name)
		}
		next = page.Next
		if next != "" && !sameOrigin(next, base) {
			break
		}
	}
	return out, nil
}

// projectKey is a Data Center project key as its API takes it: upper case,
// as keys are, except a personal project ("~alice").
func projectKey(key string) string {
	if strings.HasPrefix(key, "~") {
		return key
	}
	return strings.ToUpper(key)
}

// BeginAuthorization sends the person's browser to Bitbucket Cloud.
func (a *Adapter) BeginAuthorization(_ context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if err := a.needsOAuth(); err != nil {
		return api.Authorization{}, err
	}
	if req.Mode != api.AuthorizationWeb {
		return api.Authorization{}, noDevice()
	}
	return a.oauth().BeginWeb(req, nil)
}

// CompleteAuthorization exchanges the code the callback received.
func (a *Adapter) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if err := a.needsOAuth(); err != nil {
		return api.AuthorizationResult{}, err
	}
	if req.Mode != api.AuthorizationWeb {
		return api.AuthorizationResult{}, noDevice()
	}
	return a.oauth().CompleteWeb(ctx, req, a.now())
}

func (a *Adapter) needsOAuth() error {
	if a.cfg.Method != forgekit.MethodOAuth {
		return errs.Newf(errs.ValidInvalid,
			"This Bitbucket connection signs in with the %s method, which has nothing to authorize.", a.cfg.Method).
			WithRemedy("Authorization is for connections whose method is oauth.")
	}
	return nil
}

func noDevice() error {
	return errs.New(errs.ValidInvalid,
		"Bitbucket Cloud does not offer device authorization.").
		WithRemedy("Authorize the connection in the browser instead.")
}

func (a *Adapter) oauth() forgekit.OAuth {
	base := strings.TrimRight(a.oauthBase, "/")
	return forgekit.OAuth{
		Provider:        "Bitbucket",
		ClientID:        a.cfg.ClientID,
		ClientSecret:    a.cfg.Credential(forgekit.FieldClientSecret),
		AuthorizeURL:    base + "/authorize",
		TokenURL:        base + "/access_token",
		BasicClientAuth: true,
		HTTP:            a.http,
	}
}

// client is an API client signed in the connection's way.
func (a *Adapter) client(ctx context.Context) (forgekit.Client, error) {
	c := forgekit.Client{HTTP: a.http, Provider: "Bitbucket"}
	switch a.cfg.Method {
	case forgekit.MethodToken:
		tok := a.cfg.Credential(forgekit.FieldToken)
		if a.cfg.TokenType == tokenTypeAPI {
			user := a.cfg.Email
			if user == "" {
				user = a.cfg.Username
			}
			c.Authorize = func(r *http.Request) { r.SetBasicAuth(user, tok.Reveal()) }
		} else {
			c.Authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok.Reveal()) }
		}
		return c, nil
	case forgekit.MethodOAuth:
		access, err := a.storedAccess()
		if err != nil {
			return c, err
		}
		c.Authorize = func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+access.Reveal()) }
		return c, nil
	}
	return c, errs.New(errs.ValidInvalid,
		"A Bitbucket connection that signs in with an SSH key cannot list repositories: SSH reaches git, not Bitbucket's API.").
		WithRemedy("Enter the repository's address instead, or connect with a token.")
}

func (a *Adapter) apiBase() string {
	if a.cfg.APIURL != "" {
		return strings.TrimRight(a.cfg.APIURL, "/")
	}
	if a.cloud {
		return cloudAPI
	}
	return "https://" + a.cfg.Host + "/rest/api/1.0"
}

func (a *Adapter) notAddress(repoURL string) error {
	example := "https://bitbucket.org/acme/api.git or git@bitbucket.org:acme/api.git"
	if !a.cloud && a.cfg.Host != "" {
		example = "https://" + a.cfg.Host + "/scm/PAY/api.git or ssh://git@" + a.cfg.Host + ":7999/PAY/api.git"
	}
	return errs.Newf(errs.ValidInvalid, "%q is not a Bitbucket repository address.", repoURL).
		WithRemedy("Use the clone address Bitbucket shows, such as " + example + ".")
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	token := &api.Condition{Key: "method", Values: []string{forgekit.MethodToken}}
	oauth := &api.Condition{Key: "method", Values: []string{forgekit.MethodOAuth}}
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodToken, Label: "Access token",
				Description: "Bitbucket Cloud: a workspace, project or repository access token, or an API token. Data Center: an HTTP access token."},
			api.Option{Value: forgekit.MethodOAuth, Label: "OAuth consumer",
				Description: "A person authorizes Pando in the browser. Bitbucket Cloud only."},
			api.Option{Value: forgekit.MethodSSH, Label: "SSH access key",
				Description: "A key added to a repository or project as an access key. Repositories are entered by address."},
		),
		forgekit.HostField(cloudHost),
		forgekit.ScopeField("Workspace or project",
			"Bitbucket Cloud: the workspace ID, such as acme. Data Center: the project key, such as PAY. Empty is every repository the credential can read.",
			"acme"),
		{Key: "token_type", Label: "Token type", Type: "select", ShownWhen: token, Default: tokenTypeAccess,
			Help: "Bitbucket Cloud only. Data Center takes an HTTP access token whichever is chosen.",
			Options: []api.Option{
				{Value: tokenTypeAccess, Label: "Access token", Description: "A workspace, project or repository access token."},
				{Value: tokenTypeAPI, Label: "API token", Description: "An Atlassian account's API token, with your Bitbucket username."},
			}},
		forgekit.UsernameField(
			"Bitbucket Cloud API token: your Bitbucket username. Data Center: the account the token belongs to, if git should sign in as it; x-token-auth otherwise.",
			"alice"),
		{Key: "email", Label: "Atlassian account email", Type: "string", ShownWhen: token, Placeholder: "alice@example.com",
			Help: "Bitbucket Cloud API token only: the email of the Atlassian account the token was created under, which Bitbucket's API signs in with. The username when empty."},
		forgekit.TokenField("Token", "Read access to repositories is enough."),
		{Key: "client_id", Label: "OAuth consumer key", Type: "string", ShownWhen: oauth,
			Help: "From the workspace's settings → OAuth consumers. Give the consumer Pando's callback address and the Repositories: Read permission."},
		{Key: forgekit.FieldClientSecret, Label: "OAuth consumer secret", Type: "string", Credential: true, ShownWhen: oauth,
			Help: "The consumer's secret, shown beside its key."},
	}
	fields = append(fields, forgekit.SSHFields(
		"The host's public key, as ssh-keyscan bitbucket.org prints it. For Data Center, ssh-keyscan -p 7999 HOST, whose lines begin [HOST]:7999.")...)
	fields = append(fields,
		api.Field{Key: "ssh_port", Label: "SSH port", Type: "string", Advanced: true, Default: defaultSSHPort,
			ShownWhen: &api.Condition{Key: "method", Values: []string{forgekit.MethodSSH}},
			Help:      "Bitbucket Data Center's SSH port, for turning an HTTPS address into an SSH one. Not used on Bitbucket Cloud."},
		forgekit.APIURLField("https://api.bitbucket.org/2.0 on Bitbucket Cloud, https://HOST/rest/api/1.0 on Data Center"),
		forgekit.CAField(),
	)
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "Bitbucket",
		Description: "Clone from Bitbucket Cloud or Bitbucket Data Center with an access token, an OAuth consumer or an SSH access key, and pick repositories from a list.",
		IDPrefix:    "src_",
		Fields:      fields,
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
