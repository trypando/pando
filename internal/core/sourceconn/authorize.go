package sourceconn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The fields an authorization in progress is kept under
// (state.NewSourceAuthorizations).
const (
	pendingFlow    = "flow"
	pendingMode    = "mode"
	pendingState   = "state_hash"
	pendingExpires = "expires_at"
	pendingRedir   = "redirect_url"
)

// Authorization is an OAuth authorization started, as a person is shown it.
type Authorization struct {
	Mode            api.AuthorizationMode `json:"mode"`
	UserCode        string                `json:"user_code,omitempty"`
	VerificationURL string                `json:"verification_url,omitempty"`
	AuthorizeURL    string                `json:"authorize_url,omitempty"`
	IntervalSeconds int                   `json:"interval_seconds,omitempty"`
	ExpiresAt       time.Time             `json:"expires_at,omitzero"`

	// Form is what the browser posts to the provider instead of opening
	// AuthorizeURL: a GitHub App's manifest. Only a browser can send it.
	Form *api.AuthorizeForm `json:"form,omitempty"`
}

// Addresses are where Pando is reached, for a browser authorization: the
// callback a provider returns the browser to, and for a connection that
// creates a GitHub App, the setup address an installation returns to, the
// connection's webhook, and Pando's own address.
type Addresses struct {
	Callback string
	Setup    string
	Webhook  string
	Homepage string
}

// AuthorizationStatus is how an authorization stands.
type AuthorizationStatus struct {
	// Status is "pending" or "authorized".
	Status string `json:"status"`

	// SlowDown asks the caller to poll less often.
	SlowDown bool `json:"slow_down,omitempty"`
}

// BeginAuthorization starts an OAuth authorization of a connection, or the
// creation or installation of its GitHub App. Starting one replaces any other
// in progress for the connection.
func (s *Service) BeginAuthorization(ctx context.Context, id string, mode api.AuthorizationMode, at Addresses) (Authorization, error) {
	if s.Authorizations == nil {
		return Authorization{}, errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot keep an authorization while it is in progress.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	c, err := s.Get(ctx, id)
	if err != nil {
		return Authorization{}, err
	}
	if c.adapter == nil {
		return Authorization{}, errs.Newf(errs.StateInvalid,
			"The source connection %q cannot be authorized: %s", c.Name, c.Problem)
	}
	switch mode {
	case api.AuthorizationDevice:
		if !c.Capabilities.DeviceAuthorization {
			return Authorization{}, errs.Newf(errs.ValidInvalid,
				"The source connection %q cannot be authorized with a device code.", c.Name).
				WithRemedy(remedyFor(c.Capabilities, mode))
		}
	case api.AuthorizationWeb:
		if !c.Capabilities.WebAuthorization {
			return Authorization{}, errs.Newf(errs.ValidInvalid,
				"The source connection %q cannot be authorized in the browser.", c.Name).
				WithRemedy(remedyFor(c.Capabilities, mode))
		}
	default:
		return Authorization{}, errs.Newf(errs.ValidInvalid,
			"%q is not a way to authorize. Valid answers: device, web.", mode)
	}

	state := randomState()
	redirectURL := ""
	req := api.AuthorizationRequest{Mode: mode, State: id + "." + state}
	if mode == api.AuthorizationWeb {
		redirectURL = withoutQuery(at.Callback)
		req.RedirectURL = redirectURL
		req.SetupURL = at.Setup
		req.HomepageURL = at.Homepage
		// A provider refuses a webhook it cannot reach, and would refuse the
		// whole App with it; without one, auto-deploy polls (R-142).
		if Public(at.Webhook) {
			req.WebhookURL = at.Webhook
		}
	}
	a, err := c.adapter.BeginAuthorization(ctx, req)
	if err != nil {
		return Authorization{}, err
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = s.now().Add(15 * time.Minute)
	}
	sum := sha256.Sum256([]byte(state))
	for field, v := range map[string]secret.Value{
		pendingFlow:    a.Flow,
		pendingMode:    secret.New(string(mode)),
		pendingState:   secret.New(hex.EncodeToString(sum[:])),
		pendingExpires: secret.New(a.ExpiresAt.UTC().Format(time.RFC3339)),
		pendingRedir:   secret.New(redirectURL),
	} {
		if err := s.Authorizations.Put(ctx, id, field, v); err != nil {
			return Authorization{}, err
		}
	}
	out := Authorization{
		Mode:            a.Mode,
		UserCode:        a.UserCode,
		VerificationURL: a.VerificationURL,
		AuthorizeURL:    a.AuthorizeURL,
		ExpiresAt:       a.ExpiresAt,
		Form:            a.Form,
	}
	if a.Interval > 0 {
		out.IntervalSeconds = int(a.Interval / time.Second)
	}
	return out, nil
}

// PollAuthorization asks once whether a device authorization was approved,
// and stores the token when it was.
func (s *Service) PollAuthorization(ctx context.Context, id string) (AuthorizationStatus, error) {
	c, pending, err := s.pending(ctx, id)
	if err != nil {
		return AuthorizationStatus{}, err
	}
	if pending[pendingMode].Reveal() != string(api.AuthorizationDevice) {
		return AuthorizationStatus{}, errs.Newf(errs.StateInvalid,
			"The authorization in progress for %q is in the browser, not with a device code.", c.Name).
			WithRemedy("Finish it in the browser, or start a device authorization instead.")
	}
	res, err := c.adapter.CompleteAuthorization(ctx, api.AuthorizationCompletion{
		Mode: api.AuthorizationDevice, Flow: pending[pendingFlow],
	})
	if err != nil {
		return AuthorizationStatus{}, err
	}
	if res.Pending {
		return AuthorizationStatus{Status: "pending", SlowDown: res.SlowDown}, nil
	}
	if err := s.finish(ctx, id, res); err != nil {
		return AuthorizationStatus{}, err
	}
	return AuthorizationStatus{Status: "authorized"}, nil
}

// CompleteWebAuthorization finishes a browser authorization from the
// provider's callback. state is what the provider sent back; it names the
// connection and must match the one this authorization was started with. It
// returns the connection, and where the browser goes next when the provider
// has another step — a GitHub App just created is installed next — or "".
func (s *Service) CompleteWebAuthorization(ctx context.Context, state, code string) (id, next string, err error) {
	id, raw, ok := strings.Cut(state, ".")
	if !ok || id == "" || raw == "" {
		return "", "", errs.New(errs.ValidInvalid,
			"This authorization link is not one Pando started. Start the authorization again from Adapters in the console.")
	}
	c, pending, err := s.pending(ctx, id)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256([]byte(raw))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(pending[pendingState].Reveal())) != 1 ||
		pending[pendingMode].Reveal() != string(api.AuthorizationWeb) {
		return "", "", errs.New(errs.ValidInvalid,
			"This authorization link is not the one Pando is waiting for. Start the authorization again from Adapters in the console.")
	}
	res, err := c.adapter.CompleteAuthorization(ctx, api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: pending[pendingFlow], Code: code,
		RedirectURL: pending[pendingRedir].Reveal(),
	})
	if err != nil {
		return "", "", err
	}
	if err := s.finish(ctx, id, res); err != nil {
		return "", "", err
	}
	return id, onHost(res.ContinueURL, c.Capabilities.Host), nil
}

// onHost is a next step's address when it is on the connection's own host —
// the only place a provider's next step can be — and "" otherwise, so the
// callback never sends a browser anywhere else.
func onHost(next, host string) string {
	u, err := url.Parse(next)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || host == "" || !strings.EqualFold(u.Hostname(), host) {
		return ""
	}
	return u.String()
}

// CompleteInstallation keeps the installation a provider's setup address was
// given, once the connection's adapter has checked it is an installation of
// its own App. installationID came through the browser and is trusted no
// further than that check.
func (s *Service) CompleteInstallation(ctx context.Context, id, installationID string) error {
	if !s.stored(id) {
		return errs.Newf(errs.StateInvalid,
			"The source connection %q is declared in the configuration file, so Pando cannot store its installation.", id)
	}
	c, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if c.adapter == nil {
		return errs.Newf(errs.StateInvalid,
			"The source connection %q cannot be installed: %s", c.Name, c.Problem)
	}
	res, err := c.adapter.CompleteAuthorization(ctx, api.AuthorizationCompletion{
		Mode: api.AuthorizationInstall, Code: installationID,
	})
	if err != nil {
		return err
	}
	if len(res.Credentials) == 0 {
		return errs.New(errs.AdapterFailed, "The provider finished the installation without an ID.")
	}
	return s.store(ctx, id, res.Credentials)
}

// pending is a connection and the authorization in progress for it.
func (s *Service) pending(ctx context.Context, id string) (Connection, map[string]secret.Value, error) {
	if s.Authorizations == nil {
		return Connection{}, nil, errs.New(errs.StateInvalid, "No authorization is in progress.")
	}
	c, err := s.Get(ctx, id)
	if err != nil {
		return Connection{}, nil, err
	}
	pending, err := s.Authorizations.Resolve(ctx, id)
	if err != nil {
		return Connection{}, nil, err
	}
	if len(pending) == 0 || pending[pendingFlow].IsZero() || c.adapter == nil {
		return Connection{}, nil, errs.Newf(errs.StateInvalid,
			"No authorization of the source connection %q is in progress.", c.Name).
			WithRemedy("Start one from Adapters in the console, or with pando source authorize.")
	}
	if at, err := time.Parse(time.RFC3339, pending[pendingExpires].Reveal()); err == nil && s.now().After(at) {
		s.clear(ctx, id)
		return Connection{}, nil, errs.Newf(errs.StateInvalid,
			"The authorization of %q expired before it was finished.", c.Name).
			WithRemedy("Start it again.")
	}
	return c, pending, nil
}

// finish stores the tokens an authorization produced and forgets the
// authorization.
func (s *Service) finish(ctx context.Context, id string, res api.AuthorizationResult) error {
	if len(res.Credentials) == 0 {
		return errs.New(errs.AdapterFailed, "The provider finished the authorization without a token.")
	}
	if err := s.store(ctx, id, res.Credentials); err != nil {
		return err
	}
	s.clear(ctx, id)
	return nil
}

func (s *Service) store(ctx context.Context, id string, creds map[string]secret.Value) error {
	for field, v := range creds {
		if err := s.Credentials.Put(ctx, id, field, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) clear(ctx context.Context, id string) {
	for _, f := range []string{pendingFlow, pendingMode, pendingState, pendingExpires, pendingRedir} {
		_ = s.Authorizations.Delete(ctx, id, f)
	}
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock.Now()
	}
	return time.Now().UTC()
}

func remedyFor(c api.SourceCapabilities, mode api.AuthorizationMode) string {
	switch {
	case c.Method != "oauth":
		return "This connection signs in with a " + strings.ReplaceAll(c.Method, "_", " ") + "; there is nothing to authorize."
	case mode == api.AuthorizationWeb && c.DeviceAuthorization:
		return "Authorize it with a device code instead, or add the OAuth application's client secret to the connection."
	case mode == api.AuthorizationDevice && c.WebAuthorization:
		return "Authorize it in the browser instead."
	default:
		return "Add the OAuth application's client ID and secret to the connection's settings."
	}
}

func randomState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// withoutQuery drops any query from a callback address; the provider adds the
// state and the code.
func withoutQuery(redirectURL string) string {
	u, err := url.Parse(redirectURL)
	if err != nil {
		return redirectURL
	}
	u.RawQuery = ""
	return u.String()
}

// refuseInlineCredential refuses a repository address with a password or
// token in it. The address is stored in the spec in the clear and exported
// with it, so a credential there would be a secret on disk (R-190).
func refuseInlineCredential(raw string) error {
	// An address that does not parse as a URL — scp-like git@host:path —
	// has no place for a password, and is checked by the probe that follows.
	u, err := url.Parse(raw)
	if err != nil {
		return nil //nolint:nilerr // not a URL, so it carries no URL credential
	}
	if u.User == nil {
		return nil
	}
	if _, hasPassword := u.User.Password(); hasPassword {
		return errs.New(errs.ValidInvalid,
			"The repository address has a password or token in it, and Pando would store it in the clear with the app.").
			WithRemedy("Enter the address without it, and connect the host under Adapters in the console so Pando reads the repository with a stored credential.")
	}
	return nil
}
