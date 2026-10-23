package sourceconn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
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
	if !s.stored(id) {
		// What an authorization produces is stored as the connection's
		// credentials, and a declared connection's are the file's. Refused
		// before the provider is asked: a GitHub App would be created there
		// with nowhere to keep its key.
		return Authorization{}, errs.Newf(errs.StateInvalid,
			"The source connection %q is declared in the configuration file, so Pando cannot store what authorizing it returns.", c.Name).
			WithRemedy("Put the connection's credential in the configuration file, or add the connection under Adapters in the console instead.")
	}
	if err := canAuthorize(c, mode); err != nil {
		return Authorization{}, err
	}

	state := randomState()
	req := request(mode, id+"."+state, at)
	redirectURL := req.RedirectURL
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

// canAuthorize refuses a mode the connection cannot be authorized in.
func canAuthorize(c Connection, mode api.AuthorizationMode) error {
	switch mode {
	case api.AuthorizationDevice:
		if !c.Capabilities.DeviceAuthorization {
			return errs.Newf(errs.ValidInvalid,
				"The source connection %q cannot be authorized with a device code.", c.Name).
				WithRemedy(remedyFor(c.Capabilities, mode))
		}
	case api.AuthorizationWeb:
		if !c.Capabilities.WebAuthorization {
			return errs.Newf(errs.ValidInvalid,
				"The source connection %q cannot be authorized in the browser.", c.Name).
				WithRemedy(remedyFor(c.Capabilities, mode))
		}
	default:
		return errs.Newf(errs.ValidInvalid,
			"%q is not a way to authorize. Valid answers: device, web.", mode)
	}
	return nil
}

// request is what the adapter is asked to begin: in web mode, with the
// addresses the provider returns the browser and sends webhooks to.
func request(mode api.AuthorizationMode, state string, at Addresses) api.AuthorizationRequest {
	req := api.AuthorizationRequest{Mode: mode, State: state}
	if mode != api.AuthorizationWeb {
		return req
	}
	req.RedirectURL = withoutQuery(at.Callback)
	req.SetupURL = at.Setup
	req.HomepageURL = at.Homepage
	// A provider refuses a webhook it cannot reach, and would refuse the
	// whole App with it; without one, auto-deploy polls (R-142).
	if Public(at.Webhook) {
		req.WebhookURL = at.Webhook
	}
	return req
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
	id, c, pending, err := s.matchState(ctx, state,
		"This authorization link is not one Pando started. Start the authorization again from Adapters in the console.",
		"This authorization link is not the one Pando is waiting for. Start the authorization again from Adapters in the console.")
	if err != nil {
		return "", "", err
	}
	res, err := c.adapter.CompleteAuthorization(ctx, api.AuthorizationCompletion{
		Mode: api.AuthorizationWeb, Flow: pending[pendingFlow], Code: code,
		RedirectURL: pending[pendingRedir].Reveal(),
	})
	if err != nil {
		return "", "", err
	}
	next = onHost(res.ContinueURL, c.Capabilities.Host)
	if next == "" {
		return id, "", s.finish(ctx, id, res)
	}
	if len(res.Credentials) == 0 {
		return "", "", errs.New(errs.AdapterFailed, "The provider finished the authorization without a token.")
	}
	if err := s.store(ctx, id, res.Credentials); err != nil {
		return "", "", err
	}
	next, err = s.continueAt(ctx, id, next)
	if err != nil {
		return "", "", err
	}
	return id, next, nil
}

// matchState resolves the state a provider sent the browser back with to the
// connection and the browser authorization it was started for, refusing one
// Pando did not hand out: malformed is said when it is not a state at all,
// mismatch when it is not this authorization's.
func (s *Service) matchState(ctx context.Context, state, malformed, mismatch string) (string, Connection, map[string]secret.Value, error) {
	id, raw, ok := strings.Cut(state, ".")
	if !ok || id == "" || raw == "" {
		return "", Connection{}, nil, errs.New(errs.ValidInvalid, malformed)
	}
	c, pending, err := s.pending(ctx, id)
	if err != nil {
		return "", Connection{}, nil, err
	}
	sum := sha256.Sum256([]byte(raw))
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(pending[pendingState].Reveal())) != 1 ||
		pending[pendingMode].Reveal() != string(api.AuthorizationWeb) {
		return "", Connection{}, nil, errs.New(errs.ValidInvalid, mismatch)
	}
	return id, c, pending, nil
}

// continueAt keeps the authorization going into the provider's next step —
// installing a GitHub App just created — under a new state, added to next,
// which the provider sends back to the setup address. The setup address is
// then reached only by the browser this authorization was handed to.
func (s *Service) continueAt(ctx context.Context, id, next string) (string, error) {
	u, err := url.Parse(next)
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "The provider's next step is not an address Pando can read.", err)
	}
	state := randomState()
	sum := sha256.Sum256([]byte(state))
	for field, v := range map[string]secret.Value{
		pendingFlow:    secret.New(flowContinued),
		pendingMode:    secret.New(string(api.AuthorizationWeb)),
		pendingState:   secret.New(hex.EncodeToString(sum[:])),
		pendingExpires: secret.New(s.now().Add(time.Hour).UTC().Format(time.RFC3339)),
	} {
		if err := s.Authorizations.Put(ctx, id, field, v); err != nil {
			return "", err
		}
	}
	_ = s.Authorizations.Delete(ctx, id, pendingRedir)
	q := u.Query()
	q.Set("state", id+"."+state)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// flowContinued marks an authorization carried into the provider's next step
// by continueAt, whose own flow ended with the step before.
const flowContinued = "continued"

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
// further than that check. state must be the one the installation page was
// opened with: the setup address changes the connection, and only the browser
// Pando sent there may reach it, not a link somebody else sent an
// administrator.
func (s *Service) CompleteInstallation(ctx context.Context, id, installationID, state string) error {
	if !s.stored(id) {
		return errs.Newf(errs.StateInvalid,
			"The source connection %q is declared in the configuration file, so Pando cannot store its installation.", id)
	}
	const again = "Install the App from the connection under Adapters in the console."
	named, c, _, err := s.matchState(ctx, state,
		"This installation link is not one Pando started. "+again,
		"This installation link is not the one Pando is waiting for. "+again)
	if err != nil {
		return err
	}
	if named != id {
		return errs.New(errs.ValidInvalid, "This installation link is for another source connection. "+again)
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
	if err := s.store(ctx, id, res.Credentials); err != nil {
		return err
	}
	s.clear(ctx, id)
	return nil
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

// store writes credentials an authorization returned, all or none: a
// connection left with some of a GitHub App's — its key and not its URL name —
// could neither be used nor authorized again. The store writes one field at a
// time, so on a failure the fields already written are put back as they were.
func (s *Service) store(ctx context.Context, id string, creds map[string]secret.Value) error {
	before, err := s.Credentials.Resolve(ctx, id)
	if err != nil {
		return err
	}
	var written []string
	for field, v := range creds {
		if err := s.Credentials.Put(ctx, id, field, v); err != nil {
			if !s.restore(ctx, id, before, written) {
				return errs.Wrap(errs.Internal, fmt.Sprintf(
					"Pando could not store the source connection %q's credentials, nor put back the ones it had. The connection may not work.", id), err).
					WithRemedy("Disconnect the connection under Adapters in the console and add it again.")
			}
			return err
		}
		written = append(written, field)
	}
	return nil
}

// restore puts back fields as they were before a failed store, on a context
// of its own: the request's may be what failed. It says whether every one was.
func (s *Service) restore(ctx context.Context, id string, before map[string]secret.Value, fields []string) bool {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	ok := true
	for _, f := range fields {
		var err error
		if old, had := before[f]; had {
			err = s.Credentials.Put(ctx, id, f, old)
		} else {
			err = s.Credentials.Delete(ctx, id, f)
		}
		ok = ok && err == nil
	}
	return ok
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
