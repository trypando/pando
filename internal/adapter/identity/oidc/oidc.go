// Package oidc is the generic OpenID Connect identity adapter (R-043).
//
// One adapter for every provider that speaks OpenID Connect — Okta, Microsoft
// Entra ID, Google Workspace, Keycloak, Authentik — configured by its issuer.
// What differs between them is which claim carries what, and that is
// configuration with presets, not a kind per vendor.
//
// Authentication only (R-044). The adapter turns a callback into a Subject and
// nothing else: it does not know what an account is, what a group may do, or
// whether this person has signed in before. Core decides all of that.
package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is the adapter's kind string.
const Kind = "oidc"

// DefaultSessionLifetime is how long a session lasts when the configuration
// does not say. It is also the revocation window when no SCIM client pushes
// changes (R-050), which is why it is not longer.
const DefaultSessionLifetime = 12 * time.Hour

// Config is what an administrator sets.
type Config struct {
	Issuer   string `json:"issuer"`
	ClientID string `json:"client_id"`

	// Scopes, space- or comma-separated. openid is always requested.
	Scopes string `json:"scopes,omitempty"`

	// Which claim carries what. Empty is the standard claim.
	SubjectClaim  string `json:"subject_claim,omitempty"`
	UsernameClaim string `json:"username_claim,omitempty"`
	EmailClaim    string `json:"email_claim,omitempty"`
	NameClaim     string `json:"name_claim,omitempty"`
	GroupsClaim   string `json:"groups_claim,omitempty"`

	// HostedDomain restricts sign-in to one Google Workspace domain, by the
	// hd claim Google sets. Without it, a Google provider lets in anyone with
	// a Google account.
	HostedDomain string `json:"hosted_domain,omitempty"`

	// DisableUserinfo skips the userinfo endpoint. By default its claims fill
	// in what the ID token left out: several providers put groups or email
	// only there.
	DisableUserinfo bool `json:"disable_userinfo,omitempty"`

	// Prompt is sent as the prompt parameter: "login" to always ask for
	// credentials, "select_account" to always offer a choice.
	Prompt string `json:"prompt,omitempty"`

	SessionMaxLifetime string `json:"session_max_lifetime,omitempty"`

	Credentials struct {
		ClientSecret string `json:"client_secret,omitempty"`
	} `json:"credentials,omitempty"`
}

// Adapter is the OIDC identity adapter.
type Adapter struct {
	cfg      Config
	secret   secret.Value
	lifetime time.Duration
	client   *http.Client
	graphURL string

	mu       sync.Mutex
	provider *gooidc.Provider
	fetched  time.Time
}

// New builds an unconfigured adapter.
func New() *Adapter {
	return &Adapter{client: &http.Client{Timeout: 15 * time.Second}, lifetime: DefaultSessionLifetime}
}

// WithHTTPClient replaces the client used to reach the provider, for tests.
func (a *Adapter) WithHTTPClient(c *http.Client) *Adapter { a.client = c; return a }

// WithGraphURL replaces the Microsoft Graph address used to read the groups an
// Entra ID token left out, for tests.
func (a *Adapter) WithGraphURL(u string) *Adapter { a.graphURL = u; return a }

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryIdentity }

// Configure validates settings without contacting the provider, so a
// configuration can be saved while the provider is unreachable. HealthCheck
// and the first sign-in find out whether it answers.
func (a *Adapter) Configure(_ context.Context, raw json.RawMessage) error {
	var cfg Config
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The OpenID Connect provider's settings could not be read.", err)
		}
	}
	// Not trimmed of a trailing slash: the issuer is compared to the
	// discovery document's to the character, and Authentik's ends in one.
	cfg.Issuer = strings.TrimSpace(cfg.Issuer)
	cfg.ClientID = strings.TrimSpace(cfg.ClientID)
	if cfg.Issuer == "" {
		return errs.New(errs.ValidInvalid, "An OpenID Connect provider needs its issuer URL.").
			WithRemedy("Use the issuer your provider publishes, such as https://example.okta.com. Pando reads " +
				"<issuer>/.well-known/openid-configuration to find everything else.")
	}
	u, err := url.Parse(cfg.Issuer)
	if err != nil || u.Host == "" || (u.Scheme != "https" && !isLoopback(u.Hostname())) {
		return errs.Newf(errs.ValidInvalid, "%q is not a usable issuer URL. An issuer is an https:// URL.", cfg.Issuer).
			WithRemedy("Copy the issuer from your provider's application settings, such as https://example.okta.com.")
	}
	if strings.Contains(cfg.Issuer, "{") {
		return errs.Newf(errs.ValidInvalid, "The issuer URL %q still has a placeholder in it.", cfg.Issuer).
			WithRemedy("Replace the part in braces with your own value, such as your tenant ID or domain.")
	}
	if cfg.ClientID == "" {
		return errs.New(errs.ValidInvalid, "An OpenID Connect provider needs the client ID of the application you registered for Pando.").
			WithRemedy("Register Pando as a web application in your provider, then copy its client ID here.")
	}
	lifetime := DefaultSessionLifetime
	if cfg.SessionMaxLifetime != "" {
		d, err := time.ParseDuration(cfg.SessionMaxLifetime)
		if err != nil || d <= 0 {
			return errs.Newf(errs.ValidInvalid, "session_max_lifetime %q is not a duration such as \"12h\".", cfg.SessionMaxLifetime)
		}
		lifetime = d
	}
	switch cfg.Prompt {
	case "", "login", "select_account", "consent", "none":
	default:
		return errs.Newf(errs.ValidInvalid, "prompt %q is not one OpenID Connect defines. Use login, select_account or leave it empty.", cfg.Prompt)
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	a.secret = secret.New(cfg.Credentials.ClientSecret)
	cfg.Credentials.ClientSecret = ""
	a.cfg = cfg
	a.lifetime = lifetime
	a.provider = nil
	return nil
}

// HealthCheck fetches the provider's discovery document.
func (a *Adapter) HealthCheck(ctx context.Context) error {
	_, err := a.discover(ctx, true)
	return err
}

// discover reads the discovery document, and keeps it for an hour. Keys are
// fetched by go-oidc as tokens need them, so a provider rotating its signing
// key is followed without this cache expiring.
func (a *Adapter) discover(ctx context.Context, fresh bool) (*gooidc.Provider, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.provider != nil && !fresh && time.Since(a.fetched) < time.Hour {
		return a.provider, nil
	}
	issuer := a.cfg.Issuer
	p, err := gooidc.NewProvider(gooidc.ClientContext(ctx, a.client), issuer)
	if err != nil {
		if a.provider != nil && !fresh {
			// A provider that stops answering its discovery document has not
			// stopped signing tokens with the keys it had an hour ago.
			return a.provider, nil
		}
		return nil, errs.Wrap(errs.AdapterUnavailable, fmt.Sprintf(
			"Pando could not read the OpenID Connect discovery document at %s/.well-known/openid-configuration.", issuer), err).
			WithRemedy("Check the issuer URL, and that Pando can reach it. For Microsoft Entra ID the issuer " +
				"includes your tenant ID: https://login.microsoftonline.com/<tenant-id>/v2.0.")
	}
	if err := checkSigningAlgs(p); err != nil {
		return nil, err
	}
	a.provider = p
	a.fetched = time.Now()
	return p, nil
}

// asymmetric is the signing algorithms go-oidc verifies against the
// provider's published keys.
var asymmetric = map[string]bool{
	"RS256": true, "RS384": true, "RS512": true, "ES256": true, "ES384": true, "ES512": true,
	"PS256": true, "PS384": true, "PS512": true, "EdDSA": true,
}

// checkSigningAlgs refuses a provider whose discovery document says it signs
// ID tokens only with an algorithm Pando cannot check against a published key
// — Authentik with no signing key chosen signs with HS256, keyed by the client
// secret, and advertises exactly that.
func checkSigningAlgs(p *gooidc.Provider) error {
	var d struct {
		Algs []string `json:"id_token_signing_alg_values_supported"`
	}
	// A discovery document that does not say, or cannot be read for it, is
	// left to the token check at sign-in, which refuses an HS-signed token.
	if err := p.Claims(&d); err != nil {
		return nil //nolint:nilerr // unreadable here is checked at sign-in, as above
	}
	if len(d.Algs) == 0 {
		return nil
	}
	for _, alg := range d.Algs {
		if asymmetric[alg] {
			return nil
		}
	}
	return symmetricSigning(errs.ValidInvalid, strings.Join(d.Algs, ", "))
}

func symmetricSigning(code errs.Code, alg string) error {
	return errs.Newf(code,
		"The identity provider signs ID tokens with %s, a key shared through the client secret rather than a published key, so Pando cannot check them.", alg).
		WithRemedy("In Authentik, edit the OAuth2/OpenID provider and choose a Signing Key, such as the self-signed " +
			"certificate Authentik creates. For another provider, set the application's ID token signing algorithm to RS256.")
}

// tokenAlg is the alg in a compact JWT's header, or "" if it cannot be read.
func tokenAlg(raw string) string {
	head, _, ok := strings.Cut(raw, ".")
	if !ok {
		return ""
	}
	b, err := base64.RawURLEncoding.DecodeString(head)
	if err != nil {
		return ""
	}
	var h struct {
		Alg string `json:"alg"`
	}
	if json.Unmarshal(b, &h) != nil {
		return ""
	}
	return h.Alg
}

func (a *Adapter) oauth(p *gooidc.Provider, callback string) *oauth2.Config {
	scopes := []string{gooidc.ScopeOpenID}
	for _, s := range strings.FieldsFunc(a.cfg.Scopes, func(r rune) bool { return r == ' ' || r == ',' }) {
		if s != gooidc.ScopeOpenID {
			scopes = append(scopes, s)
		}
	}
	if a.cfg.Scopes == "" {
		scopes = append(scopes, "email", "profile")
	}
	return &oauth2.Config{
		ClientID:     a.cfg.ClientID,
		ClientSecret: a.secret.Reveal(),
		Endpoint:     p.Endpoint(),
		RedirectURL:  callback,
		Scopes:       scopes,
	}
}

// flow is what Begin hands core to keep until the callback.
type flow struct {
	Verifier string `json:"v"`
	Nonce    string `json:"n"`
}

// Begin builds the authorization request: code flow, PKCE S256, a nonce, and
// core's state.
func (a *Adapter) Begin(ctx context.Context, req api.BeginRequest) (*api.Redirect, error) {
	if req.State == "" || req.CallbackURL == "" {
		return nil, errs.New(errs.Internal, "A sign-in was started without a state or a callback address.")
	}
	p, err := a.discover(ctx, false)
	if err != nil {
		return nil, err
	}
	f := flow{Verifier: oauth2.GenerateVerifier(), Nonce: random()}
	opts := []oauth2.AuthCodeOption{oauth2.S256ChallengeOption(f.Verifier), gooidc.Nonce(f.Nonce)}
	if a.cfg.Prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", a.cfg.Prompt))
	}
	if a.cfg.HostedDomain != "" {
		// A hint to Google's account chooser. The hd claim is what is checked.
		opts = append(opts, oauth2.SetAuthURLParam("hd", a.cfg.HostedDomain))
	}
	body, err := json.Marshal(f)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not start the sign-in.", err)
	}
	return &api.Redirect{URL: a.oauth(p, req.CallbackURL).AuthCodeURL(req.State, opts...), Flow: body}, nil
}

// Authenticate exchanges the code and verifies the ID token: signature,
// issuer, audience, expiry and nonce. The state was matched by core before
// this is called — that is how core found the flow.
func (a *Adapter) Authenticate(ctx context.Context, c api.Credential) (api.Subject, error) {
	if code := c.Callback.Get("error"); code != "" {
		msg := c.Callback.Get("error_description")
		if msg == "" {
			msg = code
		}
		return api.Subject{}, errs.Newf(errs.AuthInvalid, "The identity provider did not sign you in. It said: %s", msg).
			WithRemedy("If it says you are not assigned to the application, ask whoever manages the identity " +
				"provider to assign you to Pando.")
	}
	if len(c.Flow) == 0 {
		return api.Subject{}, errs.New(errs.AuthInvalid,
			"This sign-in did not start at Pando, and OpenID Connect sign-in has to.").
			WithRemedy("Start again from Pando's sign-in page.")
	}
	var f flow
	if err := json.Unmarshal(c.Flow, &f); err != nil || f.Verifier == "" || f.Nonce == "" {
		return api.Subject{}, errs.New(errs.AuthInvalid, "This sign-in could not be matched to the one Pando started.").
			WithRemedy("Start again from Pando's sign-in page.")
	}
	code := c.Callback.Get("code")
	if code == "" {
		return api.Subject{}, errs.New(errs.AuthInvalid, "The identity provider sent Pando back without a sign-in code.").
			WithRemedy("Start again from Pando's sign-in page.")
	}
	p, err := a.discover(ctx, false)
	if err != nil {
		return api.Subject{}, err
	}
	// RFC 9207: a provider that names itself in the response must be this one.
	if iss := c.Callback.Get("iss"); iss != "" && strings.TrimRight(iss, "/") != strings.TrimRight(a.cfg.Issuer, "/") {
		return api.Subject{}, errs.New(errs.AuthInvalid, "The sign-in response came from a different identity provider than the one it was sent to.")
	}

	httpCtx := gooidc.ClientContext(ctx, a.client)
	oc := a.oauth(p, c.CallbackURL)
	token, err := oc.Exchange(context.WithValue(httpCtx, oauth2.HTTPClient, a.client), code, oauth2.VerifierOption(f.Verifier))
	if err != nil {
		return api.Subject{}, exchangeError(err)
	}
	rawID, _ := token.Extra("id_token").(string)
	if rawID == "" {
		return api.Subject{}, errs.New(errs.AuthInvalid,
			"The identity provider returned no ID token, so it is not acting as an OpenID Connect provider.").
			WithRemedy("Check that the application in your provider is an OpenID Connect web application and that the openid scope is allowed.")
	}
	if alg := tokenAlg(rawID); strings.HasPrefix(alg, "HS") {
		return api.Subject{}, symmetricSigning(errs.AuthInvalid, alg)
	}
	idToken, err := p.Verifier(&gooidc.Config{ClientID: a.cfg.ClientID}).Verify(httpCtx, rawID)
	if err != nil {
		return api.Subject{}, errs.Wrap(errs.AuthInvalid, "The ID token the identity provider returned did not verify.", err).
			WithRemedy("Check the issuer and client ID, and that the clocks on Pando's host and the provider agree.")
	}
	if idToken.Nonce != f.Nonce {
		return api.Subject{}, errs.New(errs.AuthInvalid, "The ID token was not issued for this sign-in.").
			WithRemedy("Start again from Pando's sign-in page.")
	}

	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return api.Subject{}, errs.Wrap(errs.AuthInvalid, "The ID token's claims could not be read.", err)
	}
	if !a.cfg.DisableUserinfo && p.UserInfoEndpoint() != "" {
		info, err := p.UserInfo(httpCtx, oauth2.StaticTokenSource(token))
		if err == nil {
			// Userinfo is only trusted about the subject the ID token named.
			if info.Subject == idToken.Subject {
				extra := map[string]any{}
				if err := info.Claims(&extra); err == nil {
					for k, v := range extra {
						if _, ok := claims[k]; !ok {
							claims[k] = v
						}
					}
				}
			}
		}
	}
	s, err := a.subject(claims)
	if err != nil {
		return api.Subject{}, err
	}
	if over, endpoint := overage(claims, orDefault(a.cfg.GroupsClaim, "groups")); over {
		if s.Groups, err = a.overageGroups(ctx, endpoint, token.AccessToken); err != nil {
			return api.Subject{}, err
		}
	}
	return s, nil
}

// subject maps claims to a Subject by the configured claim names.
func (a *Adapter) subject(claims map[string]any) (api.Subject, error) {
	s := api.Subject{Attributes: attributes(claims)}
	subClaim := orDefault(a.cfg.SubjectClaim, "sub")
	s.ExternalID = claimString(claims, subClaim)
	if s.ExternalID == "" {
		return api.Subject{}, errs.Newf(errs.AuthInvalid,
			"The identity provider's token has no %q claim, which Pando uses to tell people apart.", subClaim).
			WithRemedy("Set the subject claim in this provider's settings to one your provider sends, or leave it empty for \"sub\".")
	}
	if a.cfg.HostedDomain != "" && !strings.EqualFold(claimString(claims, "hd"), a.cfg.HostedDomain) {
		return api.Subject{}, errs.Newf(errs.AuthInvalid,
			"This provider only accepts accounts from %s, and you signed in with a different one.", a.cfg.HostedDomain).
			WithRemedy("Sign in with your " + a.cfg.HostedDomain + " account.")
	}
	s.Username = claimString(claims, orDefault(a.cfg.UsernameClaim, "preferred_username"))
	s.Email = claimString(claims, orDefault(a.cfg.EmailClaim, "email"))
	s.EmailVerified = s.Email != "" && claimBool(claims, "email_verified")
	s.DisplayName = claimString(claims, orDefault(a.cfg.NameClaim, "name"))
	if s.DisplayName == "" {
		s.DisplayName = strings.TrimSpace(claimString(claims, "given_name") + " " + claimString(claims, "family_name"))
	}
	s.Groups = claimStrings(claims, orDefault(a.cfg.GroupsClaim, "groups"))
	return s, nil
}

// ServiceMetadata returns nil: an OpenID Connect provider is given a redirect
// URI, which core shows, and nothing else.
func (a *Adapter) ServiceMetadata(context.Context, api.Endpoints) (*api.Metadata, error) {
	return nil, nil
}

// SessionPolicy declares this adapter's session behavior (R-047).
//
// Expiry-only on its own: Pando does not keep the provider's tokens, so there
// is nothing to refresh and nothing that would learn of a revocation. When the
// installation turns SCIM on for this provider, core reports push instead,
// because a deprovisioning then ends sessions immediately (R-048, R-050).
func (a *Adapter) SessionPolicy() api.SessionPolicy {
	return api.SessionPolicy{MaxLifetime: a.lifetime, RevocationMode: api.RevocationExpiryOnly}
}

// SupportsPush is true: every provider this adapter targets can provision
// through SCIM, which core accepts on its behalf.
func (a *Adapter) SupportsPush() bool { return true }

func exchangeError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		desc := re.ErrorDescription
		if desc == "" {
			desc = re.ErrorCode
		}
		if re.ErrorCode == "invalid_client" || re.ErrorCode == "unauthorized_client" {
			return errs.Wrap(errs.AuthInvalid, "The identity provider refused Pando's client credentials: "+desc, err).
				WithRemedy("Check the client ID and client secret in this provider's settings.")
		}
		return errs.Wrap(errs.AuthInvalid, "The identity provider refused to complete the sign-in: "+desc, err).
			WithRemedy("Check that the redirect URI registered with the provider is exactly the one Pando shows for it.")
	}
	return errs.Wrap(errs.AdapterUnavailable, "Pando could not reach the identity provider to complete the sign-in.", err)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func claimString(claims map[string]any, name string) string {
	switch v := claims[name].(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		return fmt.Sprintf("%.0f", v)
	}
	return ""
}

func claimBool(claims map[string]any, name string) bool {
	switch v := claims[name].(type) {
	case bool:
		return v
	case string:
		// Some providers send "true" as a string.
		return strings.EqualFold(v, "true")
	}
	return false
}

// claimStrings reads a claim that should be a list of strings, and accepts a
// single string or a comma-separated one, which some providers send when a
// person is in exactly one group.
func claimStrings(claims map[string]any, name string) []string {
	var out []string
	switch v := claims[name].(type) {
	case []any:
		for _, item := range v {
			if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
				out = append(out, strings.TrimSpace(s))
			}
		}
	case string:
		for _, s := range strings.Split(v, ",") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
	}
	return out
}

// attributes flattens claims for a test sign-in to display.
func attributes(claims map[string]any) map[string][]string {
	out := make(map[string][]string, len(claims))
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch v := claims[k].(type) {
		case []any:
			for _, item := range v {
				out[k] = append(out[k], fmt.Sprint(item))
			}
		case string:
			out[k] = []string{v}
		case float64:
			out[k] = []string{fmt.Sprintf("%.0f", v)}
		default:
			b, _ := json.Marshal(v)
			out[k] = []string{string(b)}
		}
	}
	return out
}

func random() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	sum := sha256.Sum256(b)
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func isLoopback(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

var _ api.IdentityAdapter = (*Adapter)(nil)
