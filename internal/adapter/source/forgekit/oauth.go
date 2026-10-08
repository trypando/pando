package forgekit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The credential fields an oauth connection keeps, written by core from an
// AuthorizationResult or a GitCredential.Rotated and handed back in Configure.
const (
	FieldAccessToken    = "access_token"
	FieldRefreshToken   = "refresh_token"
	FieldTokenExpiresAt = "token_expires_at"
)

// OAuth is one provider's OAuth 2.0 endpoints and client.
type OAuth struct {
	// Provider names the provider in messages: "GitHub", "GitLab".
	Provider string

	ClientID     string
	ClientSecret secret.Value

	// DeviceURL is the device authorization endpoint (RFC 8628). Empty when
	// the provider has none.
	DeviceURL string

	// AuthorizeURL and TokenURL are the authorization code endpoints.
	AuthorizeURL string
	TokenURL     string

	Scopes []string

	// BasicClientAuth sends the client ID and secret as HTTP basic
	// authentication to the token endpoint rather than in the form, for a
	// provider that accepts only that (Bitbucket Cloud).
	BasicClientAuth bool

	// HTTP is the client to call the provider with.
	HTTP *http.Client
}

// Tokens is what an oauth connection was configured with.
type Tokens struct {
	Access    secret.Value
	Refresh   secret.Value
	ExpiresAt time.Time
}

// TokensFrom reads the stored token fields out of a connection's credentials.
func TokensFrom(creds map[string]secret.Value) Tokens {
	t := Tokens{Access: creds[FieldAccessToken], Refresh: creds[FieldRefreshToken]}
	if v := creds[FieldTokenExpiresAt]; !v.IsZero() {
		if at, err := time.Parse(time.RFC3339, v.Reveal()); err == nil {
			t.ExpiresAt = at
		}
	}
	return t
}

// Fields is the tokens as credential fields, to store.
func (t Tokens) Fields() map[string]secret.Value {
	out := map[string]secret.Value{FieldAccessToken: t.Access}
	if !t.Refresh.IsZero() {
		out[FieldRefreshToken] = t.Refresh
	}
	if !t.ExpiresAt.IsZero() {
		out[FieldTokenExpiresAt] = secret.New(t.ExpiresAt.UTC().Format(time.RFC3339))
	} else {
		out[FieldTokenExpiresAt] = secret.Value{}
	}
	return out
}

// Fresh returns tokens that work now: these, or refreshed ones when the access
// token has expired or will within a minute and there is a refresh token. The
// bool says they were refreshed, and so must be stored (GitCredential.Rotated).
func (o OAuth) Fresh(ctx context.Context, t Tokens, now time.Time) (Tokens, bool, error) {
	if t.Access.IsZero() {
		return t, false, errs.Newf(errs.StateInvalid,
			"This %s connection has not been authorized yet.", o.Provider).
			WithRemedy("Authorize it under Sources in the console, or with pando source authorize.")
	}
	if t.ExpiresAt.IsZero() || now.Add(time.Minute).Before(t.ExpiresAt) {
		return t, false, nil
	}
	if t.Refresh.IsZero() {
		return t, false, errs.Newf(errs.StateInvalid,
			"This %s connection's authorization expired at %s and cannot be renewed without you.",
			o.Provider, t.ExpiresAt.UTC().Format(time.RFC3339)).
			WithRemedy("Authorize it again under Sources in the console.")
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {t.Refresh.Reveal()},
		"client_id":     {o.ClientID},
	}
	if !o.ClientSecret.IsZero() {
		form.Set("client_secret", o.ClientSecret.Reveal())
	}
	if len(o.Scopes) > 0 {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	tok, err := o.token(ctx, form)
	if err != nil {
		return t, false, err
	}
	if tok.Error != "" {
		return t, false, errs.Newf(errs.StateInvalid,
			"%s refused to renew this connection's authorization (%s). It may have been revoked.",
			o.Provider, tok.Error).
			WithRemedy("Authorize it again under Sources in the console.")
	}
	out := tok.tokens(now)
	if out.Refresh.IsZero() {
		// Providers that do not rotate refresh tokens keep the old one valid.
		out.Refresh = t.Refresh
	}
	return out, true, nil
}

// BeginDevice starts a device authorization.
func (o OAuth) BeginDevice(ctx context.Context, now time.Time) (api.Authorization, error) {
	if o.DeviceURL == "" {
		return api.Authorization{}, errs.Newf(errs.ValidInvalid,
			"%s does not offer device authorization for this connection.", o.Provider).
			WithRemedy("Authorize it in the browser instead.")
	}
	if o.ClientID == "" {
		return api.Authorization{}, errs.Newf(errs.ValidInvalid,
			"This %s connection has no OAuth client ID.", o.Provider).
			WithRemedy("Register an OAuth application with the provider and enter its client ID in the connection's settings.")
	}
	form := url.Values{"client_id": {o.ClientID}}
	if len(o.Scopes) > 0 {
		form.Set("scope", strings.Join(o.Scopes, " "))
	}
	var resp struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		VerificationURL         string `json:"verification_url"` // older Microsoft endpoints
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Error                   string `json:"error"`
		ErrorDescription        string `json:"error_description"`
	}
	if err := o.post(ctx, o.DeviceURL, form, &resp); err != nil {
		return api.Authorization{}, err
	}
	if resp.Error != "" || resp.DeviceCode == "" {
		return api.Authorization{}, errs.Newf(errs.AdapterFailed,
			"%s refused to start device authorization: %s.", o.Provider, firstNonEmpty(resp.ErrorDescription, resp.Error, "no device code was returned")).
			WithRemedy("Check that the OAuth application has device flow enabled and that the client ID is right.")
	}
	verify := firstNonEmpty(resp.VerificationURI, resp.VerificationURL)
	interval := time.Duration(resp.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	out := api.Authorization{
		Mode:            api.AuthorizationDevice,
		UserCode:        resp.UserCode,
		VerificationURL: verify,
		Interval:        interval,
		Flow:            secret.New(resp.DeviceCode),
	}
	if resp.ExpiresIn > 0 {
		out.ExpiresAt = now.Add(time.Duration(resp.ExpiresIn) * time.Second)
	}
	return out, nil
}

// PollDevice asks once whether a device authorization was approved.
func (o OAuth) PollDevice(ctx context.Context, flow secret.Value, now time.Time) (api.AuthorizationResult, error) {
	form := url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {flow.Reveal()},
		"client_id":   {o.ClientID},
	}
	if !o.ClientSecret.IsZero() {
		form.Set("client_secret", o.ClientSecret.Reveal())
	}
	tok, err := o.token(ctx, form)
	if err != nil {
		return api.AuthorizationResult{}, err
	}
	switch tok.Error {
	case "":
	case "authorization_pending":
		return api.AuthorizationResult{Pending: true}, nil
	case "slow_down":
		return api.AuthorizationResult{Pending: true, SlowDown: true}, nil
	case "expired_token":
		return api.AuthorizationResult{}, errs.Newf(errs.StateInvalid,
			"The %s code expired before it was entered.", o.Provider).
			WithRemedy("Start the authorization again and enter the new code.")
	case "access_denied":
		return api.AuthorizationResult{}, errs.Newf(errs.StateInvalid,
			"The authorization was declined on %s.", o.Provider).
			WithRemedy("Start it again and approve it, or ask the organization's owner to allow the application.")
	default:
		return api.AuthorizationResult{}, errs.Newf(errs.AdapterFailed,
			"%s refused the authorization: %s.", o.Provider, firstNonEmpty(tok.ErrorDescription, tok.Error))
	}
	t := tok.tokens(now)
	return api.AuthorizationResult{Credentials: t.Fields(), ExpiresAt: t.ExpiresAt}, nil
}

// BeginWeb makes the URL to send the person's browser to, with a PKCE
// challenge whose verifier is the flow core keeps.
func (o OAuth) BeginWeb(req api.AuthorizationRequest, extra url.Values) (api.Authorization, error) {
	if o.AuthorizeURL == "" || o.ClientID == "" {
		return api.Authorization{}, errs.Newf(errs.ValidInvalid,
			"This %s connection has no OAuth client ID.", o.Provider).
			WithRemedy("Register an OAuth application with the provider and enter its client ID in the connection's settings.")
	}
	if req.RedirectURL == "" {
		return api.Authorization{}, errs.New(errs.ValidInvalid,
			"Browser authorization needs an address the provider can send you back to, and this installation has none.").
			WithRemedy("Set the installation's public URL, or authorize with a device code instead.")
	}
	verifier := randomString(48)
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {o.ClientID},
		"redirect_uri":          {req.RedirectURL},
		"state":                 {req.State},
		"code_challenge":        {base64.RawURLEncoding.EncodeToString(sum[:])},
		"code_challenge_method": {"S256"},
	}
	if len(o.Scopes) > 0 {
		q.Set("scope", strings.Join(o.Scopes, " "))
	}
	for k, v := range extra {
		q[k] = v
	}
	sep := "?"
	if strings.Contains(o.AuthorizeURL, "?") {
		sep = "&"
	}
	return api.Authorization{
		Mode:         api.AuthorizationWeb,
		AuthorizeURL: o.AuthorizeURL + sep + q.Encode(),
		Flow:         secret.New(verifier),
	}, nil
}

// CompleteWeb exchanges the code the callback received.
func (o OAuth) CompleteWeb(ctx context.Context, req api.AuthorizationCompletion, now time.Time) (api.AuthorizationResult, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {req.Code},
		"redirect_uri":  {req.RedirectURL},
		"client_id":     {o.ClientID},
		"code_verifier": {req.Flow.Reveal()},
	}
	if !o.ClientSecret.IsZero() {
		form.Set("client_secret", o.ClientSecret.Reveal())
	}
	tok, err := o.token(ctx, form)
	if err != nil {
		return api.AuthorizationResult{}, err
	}
	if tok.Error != "" {
		return api.AuthorizationResult{}, errs.Newf(errs.StateInvalid,
			"%s refused the authorization: %s.", o.Provider, firstNonEmpty(tok.ErrorDescription, tok.Error)).
			WithRemedy("Start the authorization again.")
	}
	t := tok.tokens(now)
	return api.AuthorizationResult{Credentials: t.Fields(), ExpiresAt: t.ExpiresAt}, nil
}

type tokenResponse struct {
	AccessToken      string `json:"access_token"`
	RefreshToken     string `json:"refresh_token"`
	ExpiresIn        int64  `json:"expires_in"`
	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
}

func (t tokenResponse) tokens(now time.Time) Tokens {
	out := Tokens{Access: secret.New(t.AccessToken), Refresh: secret.New(t.RefreshToken)}
	if t.ExpiresIn > 0 {
		out.ExpiresAt = now.Add(time.Duration(t.ExpiresIn) * time.Second).UTC()
	}
	return out
}

func (o OAuth) token(ctx context.Context, form url.Values) (tokenResponse, error) {
	var tok tokenResponse
	if err := o.post(ctx, o.TokenURL, form, &tok); err != nil {
		return tok, err
	}
	if tok.Error == "" && tok.AccessToken == "" {
		return tok, errs.Newf(errs.AdapterFailed, "%s answered without a token.", o.Provider)
	}
	return tok, nil
}

// post sends a form and reads a JSON answer. OAuth errors arrive as 400 with
// a JSON body, so a 4xx body is read rather than treated as a failure.
func (o OAuth) post(ctx context.Context, endpoint string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not build the request to the provider.", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if o.BasicClientAuth && endpoint == o.TokenURL {
		req.SetBasicAuth(o.ClientID, o.ClientSecret.Reveal())
	}
	client := o.HTTP
	if client == nil {
		client = HTTPClient(nil)
	}
	resp, err := client.Do(req)
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not reach %s to authorize the connection.", o.Provider), err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 500 {
		return errs.Newf(errs.AdapterUnavailable,
			"%s answered %d while authorizing the connection. Try again shortly.", o.Provider, resp.StatusCode)
	}
	if err := json.Unmarshal(body, out); err != nil {
		// Some providers answer form-encoded unless told otherwise.
		if v, perr := url.ParseQuery(string(body)); perr == nil && (v.Get("access_token") != "" || v.Get("error") != "" || v.Get("device_code") != "") {
			b, _ := json.Marshal(flatten(v))
			return json.Unmarshal(b, out)
		}
		return errs.Newf(errs.AdapterFailed, "%s answered in a form Pando could not read.", o.Provider)
	}
	return nil
}

func flatten(v url.Values) map[string]any {
	out := map[string]any{}
	for k := range v {
		s := v.Get(k)
		if k == "expires_in" || k == "interval" {
			var n int64
			if _, err := fmt.Sscan(s, &n); err == nil {
				out[k] = n
				continue
			}
		}
		out[k] = s
	}
	return out
}

func randomString(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
