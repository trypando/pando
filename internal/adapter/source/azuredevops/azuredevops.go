// Package azuredevops is the source adapter for Azure DevOps Services and Azure
// DevOps Server (R-091). It signs in with a personal access token, with
// Microsoft Entra ID (a person's OAuth authorization, or a service principal's
// client credentials), or with an SSH key, and reads every form Azure DevOps
// writes a repository address in — including the legacy
// {organization}.visualstudio.com one — so core never has to (R-251).
package azuredevops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "azuredevops"

const (
	provider = "Azure DevOps"

	// devOpsScope asks Microsoft Entra ID for a token for Azure DevOps, whose
	// resource ID this is.
	devOpsScope = "499b84ac-1321-427f-aa17-267ca6975798/.default"

	defaultAuthority = "https://login.microsoftonline.com"
	defaultTenant    = "organizations"

	// The REST API versions asked for: the current one on Services, and one
	// Azure DevOps Server 2020 and later answer.
	servicesAPIVersion = "7.1"
	serverAPIVersion   = "6.0"

	// patUsername goes beside a personal access token. Azure DevOps ignores
	// it but git wants one.
	patUsername = "pat"
)

var methods = []string{forgekit.MethodToken, forgekit.MethodOAuth, forgekit.MethodServicePrincipal, forgekit.MethodSSH}

type settings struct {
	forgekit.Settings

	// TenantID is the Microsoft Entra tenant, for oauth and
	// service_principal.
	TenantID string `json:"tenant_id"`
}

// Adapter is one connection to an Azure DevOps organization or server.
type Adapter struct {
	cfg        settings
	services   bool
	configured bool
	http       *http.Client

	// authority is Microsoft Entra ID's address and now the clock; tests
	// replace them.
	authority string
	now       func() time.Time
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{authority: defaultAuthority, now: time.Now} }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategorySource }

// Configure reads the connection's settings and credentials.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg settings
	if err := forgekit.Decode(raw, &cfg); err != nil {
		return err
	}
	if err := cfg.Validate(provider, methods, servicesHost); err != nil {
		return err
	}
	services := isServicesHost(cfg.Host)
	if services {
		if org, ok := legacyOrg(cfg.Host); ok && len(forgekit.ScopeSegments(cfg.Scope)) == 0 {
			cfg.Scope = org
		}
		cfg.Host = servicesHost
	}
	scope := forgekit.ScopeSegments(cfg.Scope)
	if len(scope) > 2 {
		return errs.Newf(errs.ValidInvalid,
			"An Azure DevOps connection is limited to an organization or one project in it, and %q names more than that.", cfg.Scope).
			WithRemedy("Enter the organization, such as acme, or the organization and project, such as acme/Payments. On Azure DevOps Server, use the collection in place of the organization.")
	}
	if services && len(scope) == 0 {
		return errs.New(errs.ValidInvalid,
			"An Azure DevOps Services connection needs the organization it is for.").
			WithRemedy("Enter the organization as it appears in https://dev.azure.com/{organization}, such as acme, or acme/Payments to limit the connection to one project.")
	}
	cfg.Scope = strings.Join(scope, "/")
	cfg.TenantID = strings.TrimSpace(cfg.TenantID)
	if cfg.TenantID == "" {
		cfg.TenantID = defaultTenant
	}
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)

	switch cfg.Method {
	case forgekit.MethodOAuth, forgekit.MethodServicePrincipal:
		if !services {
			return errs.Newf(errs.ValidInvalid,
				"Azure DevOps Server does not accept Microsoft Entra ID sign-in, so the %s method works only with Azure DevOps Services (dev.azure.com).", cfg.Method).
				WithRemedy("Connect to the server with a personal access token or an SSH key instead.")
		}
		if cfg.ClientID == "" {
			return errs.New(errs.ValidInvalid,
				"A Microsoft Entra ID connection to Azure DevOps needs the application (client) ID of an app registration.").
				WithRemedy("Register an application in Microsoft Entra ID and enter its application (client) ID.")
		}
	}
	if cfg.Method == forgekit.MethodServicePrincipal {
		switch strings.ToLower(cfg.TenantID) {
		case "organizations", "common", "consumers":
			return errs.Newf(errs.ValidInvalid,
				"A service principal signs in to its own Microsoft Entra tenant, and %q is not one.", cfg.TenantID).
				WithRemedy("Enter the directory (tenant) ID shown on the app registration's overview page.")
		}
		if cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return errs.New(errs.ValidInvalid,
				"A service principal connection to Azure DevOps needs the app registration's client secret.").
				WithRemedy("Create a client secret under the app registration's Certificates & secrets and paste its value.")
		}
	}

	a.cfg, a.services, a.configured = cfg, services, true
	a.http = forgekit.HTTPClient(cfg.CA())
	if a.authority == "" {
		a.authority = defaultAuthority
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
		return errs.New(errs.AdapterUnavailable, "This Azure DevOps connection is not configured.")
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
		Method:              a.cfg.Method,
		Host:                a.cfg.Host,
		Scope:               a.cfg.Scope,
		ListRepositories:    a.configured && a.cfg.Method != forgekit.MethodSSH,
		DeviceAuthorization: oauth,
		WebAuthorization:    oauth && !a.cfg.Credential(forgekit.FieldClientSecret).IsZero(),
		Authorized:          authorized,
	}
}

// Covers reads the address into organization (or collection), project and
// repository, and scores it against the connection's scope.
func (a *Adapter) Covers(repoURL string) int {
	if !a.SourceCapabilities().Authorized {
		return 0
	}
	ref, ok := parse(repoURL)
	if !ok || ref.services != a.services {
		return 0
	}
	return forgekit.Covers([]string{a.cfg.Host}, forgekit.ScopeSegments(a.cfg.Scope), ref.host, ref.segments())
}

// GitCredential is the personal access token, an Entra ID token (refreshed or
// minted here), or the SSH key.
func (a *Adapter) GitCredential(ctx context.Context, repoURL string) (api.GitCredential, error) {
	ref, ok := parse(repoURL)
	if !ok {
		return api.GitCredential{}, notAddress(repoURL)
	}
	if a.cfg.Method == forgekit.MethodSSH {
		return a.sshCredential(ref), nil
	}
	if ref.ssh {
		return api.GitCredential{}, errs.Newf(errs.ValidInvalid,
			"The repository %s is an SSH address, and this Azure DevOps connection signs in over HTTPS.", repoURL).
			WithRemedy("Enter the repository's HTTPS address instead, such as https://dev.azure.com/acme/Payments/_git/api.")
	}
	ca := a.cfg.CA()
	switch a.cfg.Method {
	case forgekit.MethodToken:
		return forgekit.TokenCredential(patUsername, a.cfg.Credential(forgekit.FieldToken), ca), nil
	case forgekit.MethodOAuth:
		tokens, rotated, err := a.oauth().Fresh(ctx, forgekit.TokensFrom(a.cfg.Credentials), a.now())
		if err != nil {
			return api.GitCredential{}, err
		}
		cred := api.GitCredential{BearerToken: tokens.Access, CABundle: ca, ExpiresAt: tokens.ExpiresAt}
		if rotated {
			cred.Rotated = tokens.Fields()
		}
		return cred, nil
	case forgekit.MethodServicePrincipal:
		tok, expires, err := a.mint(ctx)
		if err != nil {
			return api.GitCredential{}, err
		}
		return api.GitCredential{BearerToken: tok, CABundle: ca, ExpiresAt: expires}, nil
	}
	return api.GitCredential{}, errs.Newf(errs.Internal, "Azure DevOps connection method %q has no credential.", a.cfg.Method)
}

// sshCredential is the key, with an HTTPS address turned into the SSH one for
// the same repository.
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
	if ref.services {
		cred.URL = "git@" + servicesSSHHost + ":v3/" + ref.org + "/" + ref.project + "/" + ref.repo
		return cred
	}
	path := ref.org + "/" + ref.project + "/_git/" + ref.repo
	if ref.prefix != "" {
		path = ref.prefix + "/" + path
	}
	cred.URL = "ssh://git@" + ref.host + ":22/" + path
	return cred
}

// Refresh renews an expired OAuth access token and returns the tokens to
// store. Nil for every other method, and for a token still good.
func (a *Adapter) Refresh(ctx context.Context) (map[string]secret.Value, error) {
	if !a.configured {
		return nil, errs.New(errs.AdapterUnavailable, "This Azure DevOps connection is not configured.")
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
		return secret.Value{}, errs.New(errs.StateInvalid, "This Azure DevOps connection has not been authorized yet.").
			WithRemedy("Authorize it under Sources in the console, or with pando source authorize.")
	}
	if !t.ExpiresAt.IsZero() && !a.now().Before(t.ExpiresAt) {
		return secret.Value{}, errs.Newf(errs.StateInvalid,
			"This Azure DevOps connection's authorization expired at %s, so its repositories cannot be listed until it is renewed.",
			t.ExpiresAt.UTC().Format(time.RFC3339)).
			WithRemedy("Authorize it again under Sources in the console.")
	}
	return t.Access, nil
}

// ListRepositories lists the organization's (or the project's) repositories.
func (a *Adapter) ListRepositories(ctx context.Context, req api.ListRepositoriesRequest) ([]api.Repository, error) {
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	scope := forgekit.ScopeSegments(a.cfg.Scope)
	org, project := "", ""
	if len(scope) > 0 {
		org = scope[0]
	}
	if len(scope) > 1 {
		project = scope[1]
	}
	base, err := a.apiBase(org, "")
	if err != nil {
		return nil, err
	}
	endpoint := base
	if project != "" {
		endpoint += "/" + url.PathEscape(project)
	}
	endpoint += "/_apis/git/repositories?api-version=" + a.apiVersion()

	var body struct {
		Value []struct {
			Name          string `json:"name"`
			RemoteURL     string `json:"remoteUrl"`
			DefaultBranch string `json:"defaultBranch"`
			IsDisabled    bool   `json:"isDisabled"`
			Project       struct {
				Name       string `json:"name"`
				Visibility string `json:"visibility"`
			} `json:"project"`
		} `json:"value"`
	}
	if _, err := client.Get(ctx, endpoint, &body); err != nil {
		return nil, err
	}
	out := []api.Repository{}
	for _, r := range body.Value {
		if r.IsDisabled {
			continue
		}
		full := r.Project.Name + "/" + r.Name
		if org != "" {
			full = org + "/" + full
		}
		if !forgekit.Matches(full, req.Query) {
			continue
		}
		out = append(out, api.Repository{
			URL:           stripUser(r.RemoteURL),
			FullName:      full,
			DefaultBranch: strings.TrimPrefix(r.DefaultBranch, "refs/heads/"),
			Private:       !strings.EqualFold(r.Project.Visibility, "public"),
		})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].FullName) < strings.ToLower(out[j].FullName) })
	if limit := forgekit.Limit(req.Limit); len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListBranches lists a repository's branches.
func (a *Adapter) ListBranches(ctx context.Context, repoURL string) ([]string, error) {
	ref, ok := parse(repoURL)
	if !ok {
		return nil, notAddress(repoURL)
	}
	if a.Covers(repoURL) == 0 {
		return nil, errs.Newf(errs.ValidInvalid,
			"The repository %s is not one this Azure DevOps connection is for.", repoURL).
			WithRemedy("Use the connection for the repository's organization and project, or add one.")
	}
	client, err := a.client(ctx)
	if err != nil {
		return nil, err
	}
	base, err := a.apiBase(ref.org, ref.prefix)
	if err != nil {
		return nil, err
	}
	endpoint := base + "/" + url.PathEscape(ref.project) + "/_apis/git/repositories/" + url.PathEscape(ref.repo) +
		"/refs?filter=heads/&api-version=" + a.apiVersion()
	var body struct {
		Value []struct {
			Name string `json:"name"`
		} `json:"value"`
	}
	if _, err := client.Get(ctx, endpoint, &body); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(body.Value))
	for _, r := range body.Value {
		out = append(out, strings.TrimPrefix(r.Name, "refs/heads/"))
	}
	return out, nil
}

// BeginAuthorization starts a Microsoft Entra ID authorization.
func (a *Adapter) BeginAuthorization(ctx context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if err := a.needsOAuth(); err != nil {
		return api.Authorization{}, err
	}
	switch req.Mode {
	case api.AuthorizationDevice:
		return a.oauth().BeginDevice(ctx, a.now())
	case api.AuthorizationWeb:
		if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return api.Authorization{}, noWebFlow()
		}
		return a.oauth().BeginWeb(req, nil)
	}
	return api.Authorization{}, badMode(req.Mode)
}

// CompleteAuthorization polls the device code or exchanges the browser's.
func (a *Adapter) CompleteAuthorization(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	if err := a.needsOAuth(); err != nil {
		return api.AuthorizationResult{}, err
	}
	switch req.Mode {
	case api.AuthorizationDevice:
		return a.oauth().PollDevice(ctx, req.Flow, a.now())
	case api.AuthorizationWeb:
		if a.cfg.Credential(forgekit.FieldClientSecret).IsZero() {
			return api.AuthorizationResult{}, noWebFlow()
		}
		return a.oauth().CompleteWeb(ctx, req, a.now())
	}
	return api.AuthorizationResult{}, badMode(req.Mode)
}

func (a *Adapter) needsOAuth() error {
	if a.cfg.Method != forgekit.MethodOAuth {
		return errs.Newf(errs.ValidInvalid,
			"This Azure DevOps connection signs in with the %s method, which has nothing to authorize.", a.cfg.Method).
			WithRemedy("Authorization is for connections whose method is oauth.")
	}
	return nil
}

func noWebFlow() error {
	return errs.New(errs.ValidInvalid,
		"Browser authorization with Microsoft Entra ID needs the app registration's client secret, and this connection has none.").
		WithRemedy("Add a client secret in the connection's settings, or authorize with a device code instead.")
}

func badMode(mode api.AuthorizationMode) error {
	return errs.Newf(errs.ValidInvalid, "%q is not an authorization mode. Use device or web.", string(mode))
}

func (a *Adapter) endpoint(name string) string {
	return strings.TrimRight(a.authority, "/") + "/" + url.PathEscape(a.cfg.TenantID) + "/oauth2/v2.0/" + name
}

func (a *Adapter) oauth() forgekit.OAuth {
	return forgekit.OAuth{
		Provider:     provider,
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.cfg.Credential(forgekit.FieldClientSecret),
		DeviceURL:    a.endpoint("devicecode"),
		AuthorizeURL: a.endpoint("authorize"),
		TokenURL:     a.endpoint("token"),
		Scopes:       []string{devOpsScope, "offline_access"},
		HTTP:         a.http,
	}
}

// mint signs the service principal in with its client credentials. Nothing is
// kept: a token is minted for each use (R-027).
func (a *Adapter) mint(ctx context.Context) (secret.Value, time.Time, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.cfg.ClientID},
		"client_secret": {a.cfg.Credential(forgekit.FieldClientSecret).Reveal()},
		"scope":         {devOpsScope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint("token"), strings.NewReader(form.Encode()))
	if err != nil {
		return secret.Value{}, time.Time{}, errs.Wrap(errs.Internal, "Could not build the request to Microsoft Entra ID.", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := a.http.Do(req)
	if err != nil {
		return secret.Value{}, time.Time{}, errs.Wrap(errs.AdapterUnavailable,
			"Pando could not reach Microsoft Entra ID to sign the service principal in.", err).
			WithRemedy("Check that this installation can reach login.microsoftonline.com.")
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return secret.Value{}, time.Time{}, errs.Newf(errs.AdapterUnavailable,
			"Microsoft Entra ID answered %d while signing the service principal in. Try again shortly.", resp.StatusCode)
	}
	var body struct {
		AccessToken      string `json:"access_token"`
		ExpiresIn        int64  `json:"expires_in"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return secret.Value{}, time.Time{}, errs.Newf(errs.AdapterFailed,
			"Microsoft Entra ID answered %d in a form Pando could not read while signing the service principal in.", resp.StatusCode)
	}
	if body.Error != "" || body.AccessToken == "" {
		reason := firstLine(body.ErrorDescription)
		if reason == "" {
			reason = body.Error
		}
		if reason == "" {
			reason = "no token was returned"
		}
		return secret.Value{}, time.Time{}, errs.Newf(errs.AdapterFailed,
			"Microsoft Entra ID refused to sign this connection's service principal in: %s.", reason).
			WithRemedy("Check the tenant ID, the client ID and the client secret, and that the secret has not expired. Replace them in the connection's settings.")
	}
	expires := time.Time{}
	if body.ExpiresIn > 0 {
		expires = a.now().Add(time.Duration(body.ExpiresIn) * time.Second).UTC()
	}
	return secret.New(body.AccessToken), expires, nil
}

// firstLine is the useful part of an Entra ID error description, which runs
// on into trace and correlation IDs.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	if i := strings.Index(s, " Trace ID:"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimRight(strings.TrimSpace(s), ".")
}

// client is an API client signed in the connection's way.
func (a *Adapter) client(ctx context.Context) (forgekit.Client, error) {
	c := forgekit.Client{HTTP: a.http, Provider: provider}
	var bearer secret.Value
	switch a.cfg.Method {
	case forgekit.MethodToken:
		tok := a.cfg.Credential(forgekit.FieldToken)
		c.Authorize = func(r *http.Request) {
			r.SetBasicAuth("", tok.Reveal())
			suppressRedirect(r)
		}
		return c, nil
	case forgekit.MethodOAuth:
		access, err := a.storedAccess()
		if err != nil {
			return c, err
		}
		bearer = access
	case forgekit.MethodServicePrincipal:
		tok, _, err := a.mint(ctx)
		if err != nil {
			return c, err
		}
		bearer = tok
	default:
		return c, errs.New(errs.ValidInvalid,
			"An Azure DevOps connection that signs in with an SSH key cannot list repositories: SSH reaches git, not Azure DevOps's API.").
			WithRemedy("Enter the repository's address instead, or connect with a personal access token or Microsoft Entra ID.")
	}
	c.Authorize = func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+bearer.Reveal())
		suppressRedirect(r)
	}
	return c, nil
}

// suppressRedirect asks Azure DevOps to answer 401 to a credential it does not
// accept, rather than a sign-in page with 203.
func suppressRedirect(r *http.Request) {
	r.Header.Set("X-TFS-FedAuthRedirect", "Suppress")
}

// apiBase is where the organization's (or collection's) API is.
func (a *Adapter) apiBase(org, prefix string) (string, error) {
	if a.cfg.APIURL != "" {
		return strings.TrimRight(a.cfg.APIURL, "/"), nil
	}
	if org == "" {
		return "", errs.New(errs.ValidInvalid,
			"This Azure DevOps Server connection names no collection, so Pando does not know where its API is.").
			WithRemedy("Set the connection's scope to the collection, such as DefaultCollection, or set its API address.")
	}
	if a.services {
		return "https://" + servicesHost + "/" + url.PathEscape(org), nil
	}
	base := "https://" + a.cfg.Host
	if prefix != "" {
		base += "/" + prefix
	}
	return base + "/" + url.PathEscape(org), nil
}

func (a *Adapter) apiVersion() string {
	if a.services {
		return servicesAPIVersion
	}
	return serverAPIVersion
}

func notAddress(repoURL string) error {
	return errs.Newf(errs.ValidInvalid,
		"%q is not an Azure DevOps repository address.", repoURL).
		WithRemedy("Use the clone address Azure DevOps shows, such as https://dev.azure.com/acme/Payments/_git/api or git@ssh.dev.azure.com:v3/acme/Payments/api.")
}

// Info describes the kind for the console's and the CLI's forms.
func Info() api.KindInfo {
	entra := &api.Condition{Key: "method", Values: []string{forgekit.MethodOAuth, forgekit.MethodServicePrincipal}}
	fields := []api.Field{
		forgekit.MethodField(
			api.Option{Value: forgekit.MethodToken, Label: "Personal access token",
				Description: "A token with Code (Read) scope. Works with Azure DevOps Services and Server."},
			api.Option{Value: forgekit.MethodOAuth, Label: "Microsoft Entra ID sign-in",
				Description: "A person authorizes Pando with their Microsoft account, by device code or in the browser. Azure DevOps Services only."},
			api.Option{Value: forgekit.MethodServicePrincipal, Label: "Service principal",
				Description: "An app registration signs in with its client secret. It must be added to the organization. Azure DevOps Services only."},
			api.Option{Value: forgekit.MethodSSH, Label: "SSH key",
				Description: "A key added to a user's SSH public keys. Repositories are entered by address."},
		),
		forgekit.HostField(servicesHost),
		forgekit.ScopeField("Organization",
			"The organization, or organization/project to limit the connection to one project. On Azure DevOps Server, the collection, or collection/project.",
			"acme or acme/Payments"),
		forgekit.TokenField("Personal access token", "A personal access token with the Code (Read) scope."),
		{Key: "tenant_id", Label: "Directory (tenant) ID", Type: "string", Default: defaultTenant, ShownWhen: entra,
			Help: "The Microsoft Entra tenant. A service principal needs its own tenant's ID; a person's sign-in can use organizations."},
		{Key: "client_id", Label: "Application (client) ID", Type: "string", ShownWhen: entra,
			Help: "The app registration's application (client) ID, from Microsoft Entra ID."},
		{Key: forgekit.FieldClientSecret, Label: "Client secret", Type: "string", Credential: true, ShownWhen: entra,
			Help: "The app registration's client secret. Required for a service principal and for browser authorization; device authorization works without one."},
	}
	fields = append(fields, forgekit.SSHFields(
		"The host's public key, as ssh-keyscan ssh.dev.azure.com (or your server's host) prints it.")...)
	fields = append(fields,
		forgekit.APIURLField("https://dev.azure.com/{organization}, or https://HOST/{collection} on Azure DevOps Server"),
		forgekit.CAField(),
	)
	return api.KindInfo{
		Category:    api.CategorySource,
		Kind:        Kind,
		Name:        "Azure DevOps",
		Description: "Clone from Azure DevOps Services or Azure DevOps Server with a personal access token, Microsoft Entra ID or an SSH key, and pick repositories from a list.",
		IDPrefix:    "src_",
		Fields:      fields,
	}
}

var _ api.SourceAdapter = (*Adapter)(nil)
