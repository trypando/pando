package github

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// A GitHub App connection is authorized in two trips through the browser.
// The first posts a manifest to GitHub, which creates the App under the
// person's account or organization and returns a code; Pando exchanges the
// code for the App's ID, private key and webhook secret. The second opens the
// App's installation page, where the person picks repositories; GitHub sends
// the browser to the setup address with the installation's ID, which Pando
// checks with the App's own token before keeping it.
//
// The adapter keeps nothing between the trips (R-027): which one is next is
// read from the connection's credentials, and the flow core holds says which
// trip a callback finishes.

// The flows core keeps between BeginAuthorization and the callback.
const (
	flowManifest = "manifest"
	flowInstall  = "install"
)

// maxAppName is the longest name GitHub takes for an App.
const maxAppName = 34

// manifest is a GitHub App manifest: the settings GitHub fills its
// registration form with. Read access to contents is all Pando asks for.
type manifest struct {
	Name               string            `json:"name"`
	URL                string            `json:"url"`
	Description        string            `json:"description"`
	RedirectURL        string            `json:"redirect_url"`
	SetupURL           string            `json:"setup_url,omitempty"`
	SetupOnUpdate      bool              `json:"setup_on_update"`
	Public             bool              `json:"public"`
	DefaultPermissions map[string]string `json:"default_permissions"`
	DefaultEvents      []string          `json:"default_events,omitempty"`
	HookAttributes     *hookAttributes   `json:"hook_attributes,omitempty"`
}

type hookAttributes struct {
	URL    string `json:"url"`
	Active bool   `json:"active"`
}

func (a *Adapter) beginApp(ctx context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	if req.Mode != api.AuthorizationWeb {
		return api.Authorization{}, errs.New(errs.ValidInvalid,
			"A GitHub App connection is authorized in the browser, not with a code.").
			WithRemedy("Authorize it under Adapters in the console, or run pando source authorize with --web.")
	}
	if a.appKey == nil {
		return a.manifestForm(ctx, req)
	}
	slug := a.cfg.Credential(FieldAppSlug).Reveal()
	if slug == "" {
		return api.Authorization{}, errs.New(errs.ValidInvalid,
			"This GitHub App was entered by hand, so Pando cannot open its installation page.").
			WithRemedy("Install it from the App's page in GitHub, or disconnect the connection and add it again so Pando creates the App.")
	}
	return api.Authorization{
		Mode: api.AuthorizationWeb,
		// GitHub hands state on to the setup address, which keeps the
		// installation only for the browser it was given to.
		AuthorizeURL: a.installURL(slug) + "?state=" + url.QueryEscape(req.State),
		Flow:         secret.New(flowInstall),
	}, nil
}

// manifestForm is the form that creates the App: posted to the organization's
// registration page when the connection names one, the person's own
// otherwise.
func (a *Adapter) manifestForm(ctx context.Context, req api.AuthorizationRequest) (api.Authorization, error) {
	m := manifest{
		Name:          appName(req.HomepageURL),
		URL:           firstNonEmpty(req.HomepageURL, a.webURL),
		Description:   "Reads repositories for Pando.",
		RedirectURL:   req.RedirectURL,
		SetupURL:      req.SetupURL,
		SetupOnUpdate: true,
		DefaultPermissions: map[string]string{
			"contents": "read",
			"metadata": "read",
		},
	}
	if a.cfg.pushWebhook() && req.WebhookURL != "" {
		m.HookAttributes = &hookAttributes{URL: req.WebhookURL, Active: true}
		m.DefaultEvents = []string{"push", "create", "release"}
	}
	body, err := json.Marshal(m)
	if err != nil {
		return api.Authorization{}, errs.Wrap(errs.Internal, "Pando could not write the GitHub App's manifest.", err)
	}

	action := a.webURL + "/settings/apps/new"
	if a.cfg.Scope != "" && a.isOrganization(ctx, a.cfg.Scope) {
		action = a.webURL + "/organizations/" + url.PathEscape(a.cfg.Scope) + "/settings/apps/new"
	}
	return api.Authorization{
		Mode: api.AuthorizationWeb,
		Form: &api.AuthorizeForm{
			Action: action + "?state=" + url.QueryEscape(req.State),
			Fields: map[string]string{"manifest": string(body)},
		},
		Flow: secret.New(flowManifest),
	}, nil
}

// isOrganization asks GitHub whether an owner is an organization. A user's
// App is created from the person's own settings, and an owner GitHub will not
// describe is taken for an organization, whose page then says what is wrong.
func (a *Adapter) isOrganization(ctx context.Context, owner string) bool {
	var acct struct {
		Type string `json:"type"`
	}
	if _, err := a.anonymous().Get(ctx, a.apiURL+"/users/"+url.PathEscape(owner), &acct); err != nil {
		return true
	}
	return acct.Type != "User"
}

// appName names the App after the address Pando is reached at, which GitHub
// shows on the installation page. GitHub's rules: at most 34 characters,
// unique across GitHub, and not an existing account's name. Its docs name no
// characters beyond letters, digits, spaces and hyphens, so a host's dots
// become hyphens. An installation with no name of its own — localhost — gets
// a random suffix, since "Pando" alone is taken. One already taken is changed
// on GitHub's form before creating it.
func appName(homepage string) string {
	host := ""
	if u, err := url.Parse(homepage); err == nil {
		host = u.Hostname()
	}
	if host == "" || isLoopback(host) || net.ParseIP(host) != nil {
		return "Pando " + randomSuffix()
	}
	name := "Pando " + strings.ReplaceAll(host, ".", "-")
	if len(name) > maxAppName {
		name = name[:maxAppName]
	}
	return strings.TrimRight(name, "-")
}

// randomSuffix is six hex digits.
func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// appSettingsURL is the settings page of the App Pando created, where its
// logo is uploaded: GitHub has no way to set one from a manifest or its API.
// Under the organization that owns the App, or the person's own settings.
func (a *Adapter) appSettingsURL() string {
	slug := a.cfg.Credential(FieldAppSlug).Reveal()
	if a.appKey == nil || slug == "" {
		return ""
	}
	if owner := a.cfg.Credential(FieldAppOwner).Reveal(); owner != "" &&
		a.cfg.Credential(FieldAppOwnerType).Reveal() == "Organization" {
		return a.webURL + "/organizations/" + url.PathEscape(owner) + "/settings/apps/" + url.PathEscape(slug)
	}
	return a.webURL + "/settings/apps/" + url.PathEscape(slug)
}

func (a *Adapter) installURL(slug string) string {
	return a.webURL + "/apps/" + url.PathEscape(slug) + "/installations/new"
}

func (a *Adapter) completeApp(ctx context.Context, req api.AuthorizationCompletion) (api.AuthorizationResult, error) {
	switch {
	case req.Mode == api.AuthorizationInstall:
		return a.completeInstall(ctx, req.Code)
	case req.Mode == api.AuthorizationWeb && req.Flow.Reveal() == flowManifest:
		return a.convertManifest(ctx, req.Code)
	}
	return api.AuthorizationResult{}, errs.New(errs.ValidInvalid,
		"This is not a step of creating or installing the connection's GitHub App.").
		WithRemedy("Authorize the connection again under Adapters in the console.")
}

// convertManifest exchanges the code GitHub returned for the App it created.
// The code is good for an hour and once.
func (a *Adapter) convertManifest(ctx context.Context, code string) (api.AuthorizationResult, error) {
	if code == "" || strings.ContainsAny(code, "/?#") {
		return api.AuthorizationResult{}, errs.New(errs.ValidInvalid,
			"GitHub returned without a code for the App it created.").
			WithRemedy("Authorize the connection again under Adapters in the console.")
	}
	var app struct {
		ID            int64  `json:"id"`
		Slug          string `json:"slug"`
		PEM           string `json:"pem"`
		WebhookSecret string `json:"webhook_secret"`
		Owner         struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"owner"`
	}
	if _, err := a.anonymous().Do(ctx, http.MethodPost,
		a.apiURL+"/app-manifests/"+url.PathEscape(code)+"/conversions", nil, &app); err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return api.AuthorizationResult{}, errs.New(errs.ValidInvalid,
				"GitHub no longer accepts the code for the App it created. A code is good for an hour, and once.").
				WithRemedy("Authorize the connection again under Adapters in the console. The App GitHub created can be deleted from its settings page.")
		}
		return api.AuthorizationResult{}, err
	}
	if app.ID == 0 || app.Slug == "" {
		return api.AuthorizationResult{}, errs.New(errs.AdapterFailed, "GitHub answered without the App it created.")
	}
	if _, err := parseAppKey(secret.New(app.PEM)); err != nil {
		return api.AuthorizationResult{}, errs.New(errs.AdapterFailed,
			"GitHub answered with a private key for the App that Pando could not read.")
	}
	creds := map[string]secret.Value{
		FieldAppID:         secret.New(strconv.FormatInt(app.ID, 10)),
		FieldAppSlug:       secret.New(app.Slug),
		FieldAppPrivateKey: secret.New(app.PEM),
	}
	if app.WebhookSecret != "" {
		creds[FieldWebhookSecret] = secret.New(app.WebhookSecret)
	}
	if app.Owner.Login != "" {
		creds[FieldAppOwner] = secret.New(app.Owner.Login)
		creds[FieldAppOwnerType] = secret.New(app.Owner.Type)
	}
	return api.AuthorizationResult{Credentials: creds, ContinueURL: a.installURL(app.Slug)}, nil
}

// completeInstall checks an installation GitHub's setup redirect named before
// keeping it. The ID came through the browser, so it is only kept if the App
// can see it as its own — and, when the connection names an owner, as
// installed there.
func (a *Adapter) completeInstall(ctx context.Context, id string) (api.AuthorizationResult, error) {
	if a.appKey == nil {
		return api.AuthorizationResult{}, notCreated()
	}
	id = strings.TrimSpace(id)
	if !digits(id) {
		return api.AuthorizationResult{}, errs.New(errs.ValidInvalid,
			"GitHub returned without the ID of the App's installation.").
			WithRemedy("Install the App again from the connection under Adapters in the console.")
	}
	appTok, err := a.appJWT()
	if err != nil {
		return api.AuthorizationResult{}, err
	}
	c, status := a.appClient(secret.New(appTok))
	var inst struct {
		Account struct {
			Login string `json:"login"`
		} `json:"account"`
	}
	if _, err := c.Get(ctx, a.apiURL+"/app/installations/"+url.PathEscape(id), &inst); err != nil {
		if errs.CodeOf(err) == errs.NotFound {
			return api.AuthorizationResult{}, errs.Newf(errs.ValidInvalid,
				"Installation %s is not an installation of this connection's GitHub App.", id).
				WithRemedy("Install the App again from the connection under Adapters in the console.")
		}
		return api.AuthorizationResult{}, appErr(err, *status)
	}
	if a.cfg.Scope != "" && !strings.EqualFold(inst.Account.Login, a.cfg.Scope) {
		return api.AuthorizationResult{}, errs.Newf(errs.ValidInvalid,
			"The App was installed on %s, and this connection is for %s.", inst.Account.Login, a.cfg.Scope).
			WithRemedy("Install the App again and choose " + a.cfg.Scope + ", or change the connection's owner.")
	}
	return api.AuthorizationResult{Credentials: map[string]secret.Value{
		FieldInstallationID: secret.New(id),
	}}, nil
}

// anonymous is the API client for calls that carry no credential: the
// manifest's exchange, whose code is the credential, and public lookups.
func (a *Adapter) anonymous() forgekit.Client {
	return forgekit.Client{
		HTTP:     a.http,
		Provider: provider,
		Authorize: func(r *http.Request) {
			r.Header.Set("Accept", "application/vnd.github+json")
			r.Header.Set("X-GitHub-Api-Version", apiVersion)
		},
	}
}

func notCreated() error {
	return errs.New(errs.StateInvalid, "This GitHub connection's App has not been created yet.").
		WithRemedy("Authorize the connection under Adapters in the console to create it.")
}

func notInstalled() error {
	return errs.New(errs.StateInvalid, "This GitHub connection's App is not installed yet.").
		WithRemedy("Authorize the connection under Adapters in the console to install it.")
}
