package idp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Redirect sign-in (design 06 §3.2).
//
//  1. Start, on whatever hostname the browser is at — Pando's own, or an app's
//     (R-172). A flow is stored under a random state, and the browser gets a
//     cookie whose digest the flow keeps.
//  2. The provider sends the browser to the one callback registered with it,
//     on the external URL. The callback verifies the response through the
//     adapter, decides which account it is, and issues a one-time code.
//  3. The browser is sent back to the hostname it started on, where Complete
//     spends the code — in the same browser, proven by the cookie — and the
//     session cookie is set there.
//
// Step 3 is why a sign-in can start on an app's own hostname at all: the
// session cookie belongs to the hostname that sets it, and the callback is on
// a different one. The cookie check is what stops somebody from finishing, in
// your browser, a sign-in they started in theirs (login CSRF).

// FlowLifetime is how long a person has at the provider.
const FlowLifetime = 10 * time.Minute

// Paths on every hostname Pando serves (R-172).
const (
	LoginPath    = "/.pando/login"
	CompletePath = "/.pando/api/v1/auth/complete"
)

// PublicProvider is what the sign-in page shows: a name to press.
type PublicProvider struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// SignInOptions is how people can sign in here.
type SignInOptions struct {
	PasswordSignIn bool             `json:"password_sign_in"`
	Providers      []PublicProvider `json:"providers"`
}

// Options answers the sign-in page.
func (s *Service) Options(ctx context.Context) (SignInOptions, error) {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return SignInOptions{}, err
	}
	providers, err := s.Providers.List(ctx)
	if err != nil {
		return SignInOptions{}, err
	}
	out := SignInOptions{PasswordSignIn: !doc.DisablePasswordSignIn, Providers: []PublicProvider{}}
	for _, p := range providers {
		if p.Kind != "local" && p.Enabled {
			out.Providers = append(out.Providers, PublicProvider{ID: p.ID, Name: p.Name, Kind: p.Kind})
		}
	}
	return out, nil
}

// PasswordSignInAllowed is the gate on POST /sessions.
func (s *Service) PasswordSignInAllowed(ctx context.Context) error {
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return err
	}
	if doc.DisablePasswordSignIn {
		return errs.New(errs.AuthInvalid,
			"Password sign-in is turned off on this installation. Sign in with your organization's identity provider instead.").
			WithRemedy("Choose your identity provider on the sign-in page. If none works, whoever runs this installation " +
				"can turn password sign-in back on with `pando admin enable-password-sign-in`.")
	}
	return nil
}

// ValidatePolicy refuses turning password sign-in off while no external
// provider is on: the sign-in page would have nothing on it.
func (s *Service) ValidatePolicy(ctx context.Context, before, after bool) error {
	if !after || before {
		return nil
	}
	any, err := s.Providers.AnyExternalEnabled(ctx)
	if err != nil {
		return err
	}
	if !any {
		return errs.New(errs.ValidInvalid,
			"Password sign-in cannot be turned off while no identity provider is turned on: nobody could sign in.").
			WithRemedy("Add an identity provider, test it, and turn it on first.")
	}
	return nil
}

// StartRequest begins a redirect sign-in.
type StartRequest struct {
	ProviderID string
	Origin     Origin
	Next       string

	// Test is an administrator's test sign-in: nobody is signed in, and the
	// report says what would have happened.
	Test        bool
	InitiatedBy authz.Principal
}

// Started is where to send the browser, and the value of the cookie that
// binds the flow to it.
type Started struct {
	RedirectURL string
	Bind        string
}

// Start begins a sign-in through a provider.
func (s *Service) Start(ctx context.Context, req StartRequest) (Started, error) {
	a, p, err := s.Adapter(ctx, req.ProviderID)
	if err != nil {
		return Started{}, err
	}
	if p.Kind == "local" {
		return Started{}, errs.New(errs.ValidInvalid, "Local accounts sign in with a username and password, not a redirect.")
	}
	if !p.Enabled && !req.Test {
		return Started{}, errs.Newf(errs.ValidInvalid, "Sign-in through %s is turned off on this installation.", p.Name).
			WithRemedy("Choose another way to sign in, or ask an administrator to turn it on.")
	}
	st := randomToken(32)
	bind := randomToken(32)
	endpoints := s.Endpoints(req.Origin, p.ID)
	redirect, err := a.Begin(ctx, api.BeginRequest{Endpoints: endpoints, State: st})
	if err != nil {
		return Started{}, err
	}
	if redirect == nil || redirect.URL == "" {
		return Started{}, errs.New(errs.Internal, "The identity provider gave Pando nowhere to send you.")
	}
	purpose, initiated := state.FlowSignIn, ""
	if req.Test {
		purpose, initiated = state.FlowTest, req.InitiatedBy.ID
	}
	if err := s.Flows.Create(ctx, state.SSOFlow{
		ID: st, AdapterID: p.ID, Purpose: purpose, BindHash: Digest(bind),
		ReturnOrigin: req.Origin.String(), NextPath: SafeNext(req.Next),
		CallbackURL: endpoints.CallbackURL, EntityID: endpoints.EntityID, Flow: redirect.Flow,
		InitiatedBy: initiated, ExpiresAt: s.now().Add(FlowLifetime),
	}); err != nil {
		return Started{}, err
	}
	return Started{RedirectURL: redirect.URL, Bind: bind}, nil
}

// SafeNext keeps a post-sign-in destination on the same host: a path, not a
// URL. "//evil.example" and "/\evil.example" are URLs to a browser.
func SafeNext(next string) string {
	if next == "" || !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") ||
		strings.HasPrefix(next, "/\\") || strings.ContainsAny(next, "\r\n") {
		return "/"
	}
	return next
}

// Callback handles the provider's response and returns where to send the
// browser next. It never returns an error for the browser to see raw: a
// failure is recorded on the flow and the browser is sent to a page that
// explains it.
func (s *Service) Callback(ctx context.Context, providerID string, params url.Values, o Origin) string {
	stateParam := params.Get("state")
	if stateParam == "" {
		stateParam = params.Get("RelayState")
	}
	if stateParam == "" && params.Get("SAMLResponse") != "" {
		return s.unsolicited(ctx, providerID, params, o)
	}

	f, ok, err := s.Flows.TakeForCallback(ctx, stateParam, providerID)
	if err != nil || !ok {
		// Some providers put a URL of their own in RelayState on a sign-in
		// they started. That is still unsolicited, and the adapter decides
		// whether this provider may send one.
		if err == nil && params.Get("SAMLResponse") != "" {
			return s.unsolicited(ctx, providerID, params, o)
		}
		return s.base(o) + LoginPath + "?sso_error=expired"
	}
	a, p, err := s.Adapter(ctx, providerID)
	if err != nil {
		return s.fail(ctx, f, err)
	}
	subject, err := a.Authenticate(ctx, api.Credential{
		Callback: params, Flow: f.Flow,
		Endpoints: api.Endpoints{CallbackURL: f.CallbackURL, EntityID: f.EntityID},
	})
	if err != nil {
		return s.fail(ctx, f, err)
	}
	if err := s.useOnce(ctx, p.ID, subject); err != nil {
		return s.fail(ctx, f, err)
	}

	if f.Purpose == state.FlowTest {
		outcome := s.decide(ctx, p, subject, true)
		report := TestReport{OK: outcome.Kind != OutcomeRefused, Subject: view(subject), Attributes: subject.Attributes, Outcome: &outcome}
		body, _ := json.Marshal(report)
		_ = s.Flows.Authenticated(ctx, f.ID, "", Digest(randomToken(32)), body)
		s.audit(ctx, audit.Event{
			PrincipalKind: audit.KindUser, PrincipalID: f.InitiatedBy, Action: "identity_provider.test",
			TargetKind: "identity_provider", TargetID: p.ID,
			Detail: map[string]any{"external_id": subject.ExternalID, "outcome": outcome.Kind},
		})
		return f.ReturnOrigin + testPage(p.ID, f.ID)
	}

	outcome := s.decide(ctx, p, subject, false)
	if outcome.Kind == OutcomeRefused {
		return s.fail(ctx, f, outcome.err)
	}
	code := randomToken(32)
	body, _ := json.Marshal(outcome)
	if err := s.Flows.Authenticated(ctx, f.ID, outcome.UserID, Digest(code), body); err != nil {
		return s.fail(ctx, f, err)
	}
	return f.ReturnOrigin + CompletePath + "?code=" + url.QueryEscape(code)
}

// unsolicited handles a SAML response the provider sent without Pando asking
// — an IdP-initiated sign-in. The adapter refuses it unless the provider is
// set to allow it; when it is, there is no browser binding to check, which is
// the risk an administrator accepted by turning it on.
func (s *Service) unsolicited(ctx context.Context, providerID string, params url.Values, o Origin) string {
	loginErr := s.base(o) + LoginPath + "?sso_error="
	a, p, err := s.Adapter(ctx, providerID)
	if err != nil || !p.Enabled {
		return loginErr + "expired"
	}
	endpoints := s.Endpoints(o, p.ID)
	f := state.SSOFlow{
		ID: randomToken(32), AdapterID: p.ID, ReturnOrigin: s.base(o), NextPath: "/",
		CallbackURL: endpoints.CallbackURL, EntityID: endpoints.EntityID, ExpiresAt: s.now().Add(FlowLifetime),
	}
	if err := s.Flows.CreateUnsolicited(ctx, f); err != nil {
		return loginErr + "expired"
	}
	subject, err := a.Authenticate(ctx, api.Credential{Callback: params, Endpoints: endpoints})
	if err != nil {
		return s.fail(ctx, f, err)
	}
	if err := s.useOnce(ctx, p.ID, subject); err != nil {
		return s.fail(ctx, f, err)
	}
	outcome := s.decide(ctx, p, subject, false)
	if outcome.Kind == OutcomeRefused {
		return s.fail(ctx, f, outcome.err)
	}
	code := randomToken(32)
	body, _ := json.Marshal(outcome)
	if err := s.Flows.Authenticated(ctx, f.ID, outcome.UserID, Digest(code), body); err != nil {
		return s.fail(ctx, f, err)
	}
	return f.ReturnOrigin + CompletePath + "?code=" + url.QueryEscape(code)
}

func (s *Service) useOnce(ctx context.Context, providerID string, subject api.Subject) error {
	if subject.OneTimeID == "" {
		return nil
	}
	fresh, err := s.Flows.UseOnce(ctx, providerID, subject.OneTimeID, subject.OneTimeUntil)
	if err != nil {
		return err
	}
	if !fresh {
		return errs.New(errs.AuthInvalid, "This sign-in response was already used once, so Pando refused it.").
			WithRemedy("Start again from Pando's sign-in page.")
	}
	return nil
}

// fail records why a flow failed and returns where the browser goes to be
// told: the sign-in page, or for a test, the provider's page in the console.
func (s *Service) fail(ctx context.Context, f state.SSOFlow, err error) string {
	e := errs.As(err)
	if e == nil {
		e = errs.Wrap(errs.Internal, "The sign-in could not be completed.", err)
	}
	report := TestReport{Error: &Problem{Code: string(e.Code), Message: e.Message, Remedy: e.Remedy}}
	body, _ := json.Marshal(report)
	_ = s.Flows.Fail(ctx, f.ID, body)
	s.audit(ctx, audit.Event{
		PrincipalKind: audit.KindAnonymous, Action: "session.denied", TargetKind: "identity_provider", TargetID: f.AdapterID,
		Detail: map[string]any{"reason": e.Message, "purpose": f.Purpose},
	})
	if f.Purpose == state.FlowTest {
		return f.ReturnOrigin + testPage(f.AdapterID, f.ID)
	}
	return f.ReturnOrigin + LoginPath + "?sso_error=" + url.QueryEscape(f.ID) + "&next=" + url.QueryEscape(f.NextPath)
}

func testPage(providerID, flowID string) string {
	return "/admin/sign-in?provider=" + url.QueryEscape(providerID) + "&test=" + url.QueryEscape(flowID)
}

// Completed is a finished sign-in: the session to set, and where to go.
type Completed struct {
	Session state.Started
	User    state.User
	Next    string
}

// Complete spends a one-time code in the browser that started the flow.
func (s *Service) Complete(ctx context.Context, code, bind string, o Origin, userAgent, ip string) (Completed, error) {
	expired := errs.New(errs.AuthInvalid, "That sign-in has expired or was already used.").
		WithRemedy("Start again from the sign-in page.")
	if code == "" {
		return Completed{}, expired
	}
	f, ok, err := s.Flows.Complete(ctx, Digest(code))
	if err != nil {
		return Completed{}, err
	}
	if !ok || f.Purpose != state.FlowSignIn || f.UserID == "" {
		return Completed{}, expired
	}
	if f.ReturnOrigin != o.String() {
		return Completed{}, expired
	}
	if f.BindHash != "" && (bind == "" || Digest(bind) != f.BindHash) {
		s.audit(ctx, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "session.denied", TargetKind: "user", TargetID: f.UserID,
			Detail: map[string]any{"reason": "browser_mismatch", "adapter_id": f.AdapterID},
		})
		return Completed{}, errs.New(errs.AuthInvalid,
			"This sign-in was started in a different browser, so Pando did not finish it here.").
			WithRemedy("Start again from the sign-in page, in the browser you want to be signed in on.")
	}
	user, found, err := s.Users.ByID(ctx, f.UserID)
	if err != nil {
		return Completed{}, err
	}
	if !found || user.Status != "active" {
		return Completed{}, errs.New(errs.AuthInvalid, "This account cannot sign in.")
	}
	lifetime, err := s.SessionLifetime(ctx, f.AdapterID)
	if err != nil {
		return Completed{}, err
	}
	sess, err := s.Sessions.Create(ctx, user.ID, f.AdapterID, lifetime, userAgent, ip)
	if err != nil {
		return Completed{}, err
	}
	s.audit(ctx, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: user.ID, Action: "session.create",
		TargetKind: "session", TargetID: sess.Session.ID,
		Detail: map[string]any{"adapter_id": f.AdapterID, "via": "sso"},
	})
	return Completed{Session: sess, User: user, Next: f.NextPath}, nil
}

// FailureMessage is what the sign-in page shows for a failed flow. Only a
// sign-in's failure is shown, never a test's, and never anything but the
// message and remedy recorded when it failed.
func (s *Service) FailureMessage(ctx context.Context, flowID string) (Problem, bool, error) {
	if p, ok := fixedFailures[flowID]; ok {
		return p, true, nil
	}
	f, ok, err := s.Flows.ByID(ctx, flowID)
	if err != nil {
		return Problem{}, false, err
	}
	if !ok || !f.Failed || f.Purpose != state.FlowSignIn {
		return Problem{}, false, nil
	}
	// A result that does not read as a report is not a failure this page can
	// explain; the page says the generic thing instead.
	var report TestReport
	if uerr := json.Unmarshal(f.Result, &report); uerr != nil || report.Error == nil {
		return Problem{}, false, nil //nolint:nilerr // an unreadable report is "nothing to explain", not a failure.
	}
	return *report.Error, true, nil
}

// fixedFailures are the failures that have no flow to carry their message:
// named by a fixed key, so the sign-in page can only ever show one of these
// sentences for them, never text from the query string.
var fixedFailures = map[string]Problem{
	"expired": {Code: string(errs.AuthInvalid), Message: "That sign-in has expired or was already used.",
		Remedy: "Start again below."},
	"browser": {Code: string(errs.AuthInvalid),
		Message: "That sign-in was started in a different browser, so Pando did not finish it here.",
		Remedy:  "Start again below, in the browser you want to be signed in on."},
	"unavailable": {Code: string(errs.AdapterUnavailable),
		Message: "Pando could not start signing you in with that provider. It may be turned off, or unreachable from Pando.",
		Remedy:  "Try again in a moment, or choose another way to sign in. If it keeps happening, tell whoever runs this installation."},
}

// Problem is an error as a page shows it.
type Problem struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Remedy  string `json:"remedy,omitempty"`
}

// SubjectView is a Subject as an administrator's test sign-in shows it.
type SubjectView struct {
	ExternalID    string   `json:"external_id"`
	Username      string   `json:"username,omitempty"`
	Email         string   `json:"email,omitempty"`
	EmailVerified bool     `json:"email_verified"`
	DisplayName   string   `json:"display_name,omitempty"`
	Groups        []string `json:"groups"`
}

func view(s api.Subject) *SubjectView {
	g := s.Groups
	if g == nil {
		g = []string{}
	}
	return &SubjectView{ExternalID: s.ExternalID, Username: s.Username, Email: s.Email,
		EmailVerified: s.EmailVerified, DisplayName: s.DisplayName, Groups: g}
}

// TestReport is what a test sign-in found: what the provider sent, and what
// Pando would have done with it.
type TestReport struct {
	OK         bool                `json:"ok"`
	Error      *Problem            `json:"error,omitempty"`
	Subject    *SubjectView        `json:"subject,omitempty"`
	Attributes map[string][]string `json:"attributes,omitempty"`
	Outcome    *SignInOutcome      `json:"outcome,omitempty"`
}

// TestResult returns a test sign-in's report, to the administrator who ran it.
func (s *Service) TestResult(ctx context.Context, p authz.Principal, providerID, flowID string) (TestReport, error) {
	f, ok, err := s.Flows.ByID(ctx, flowID)
	if err != nil {
		return TestReport{}, err
	}
	if !ok || f.Purpose != state.FlowTest || f.AdapterID != providerID || f.InitiatedBy != p.ID {
		return TestReport{}, errs.New(errs.NotFound, "There is no test sign-in with that ID for you.")
	}
	if len(f.Result) == 0 || string(f.Result) == "{}" {
		return TestReport{}, errs.New(errs.NotFound, "That test sign-in has not come back from the provider yet.").
			WithRemedy("Finish signing in at the provider, or start the test again.")
	}
	var report TestReport
	if err := json.Unmarshal(f.Result, &report); err != nil {
		return TestReport{}, errs.Wrap(errs.Internal, "The test result could not be read.", err)
	}
	return report, nil
}

// What a sign-in resolves to.
const (
	OutcomeExisting = "existing"
	OutcomeLinked   = "linked_by_email"
	OutcomeCreated  = "created"
	OutcomeRefused  = "refused"
)

// SignInOutcome is which account a sign-in reaches, and how.
type SignInOutcome struct {
	Kind        string   `json:"kind"`
	UserID      string   `json:"user_id,omitempty"`
	Username    string   `json:"username,omitempty"`
	DisplayName string   `json:"display_name,omitempty"`
	Message     string   `json:"message,omitempty"`
	Remedy      string   `json:"remedy,omitempty"`
	Groups      []string `json:"groups,omitempty"`

	// GroupsFrom is where the account's groups at this provider come from:
	// "sign_in" (the groups claim, on every sign-in) or "scim".
	GroupsFrom string `json:"groups_from"`

	err error
}

func refused(e *errs.Error) SignInOutcome {
	return SignInOutcome{Kind: OutcomeRefused, Message: e.Message, Remedy: e.Remedy, err: e}
}

// decide resolves a verified subject to an account (O-1, R-045): the
// account its identity already reaches; or, where the provider allows, the one
// account with the email it vouches for; or, where the provider and host
// policy allow, a new one. Otherwise nobody. dryRun answers the question
// without changing anything, for a test sign-in.
func (s *Service) decide(ctx context.Context, p state.IdentityProvider, subject api.Subject, dryRun bool) SignInOutcome {
	groupsFrom := "sign_in"
	if p.SCIMEnabled {
		groupsFrom = "scim"
	}
	out := func(kind string, u state.User) SignInOutcome {
		return SignInOutcome{Kind: kind, UserID: u.ID, Username: u.ExternalID, DisplayName: u.DisplayName,
			Groups: subject.Groups, GroupsFrom: groupsFrom}
	}

	user, found, err := s.Identities.Resolve(ctx, p.ID, subject.ExternalID)
	if err != nil {
		return refused(errs.As(err))
	}
	if found {
		switch {
		case user.Status == "deleted":
			return refused(errs.New(errs.AuthInvalid, "The Pando account for this identity was deleted.").
				WithRemedy("Ask an administrator for access."))
		case user.AliasOf != "":
			return refused(errs.New(errs.AuthInvalid, "The Pando account for this identity was merged into another.").
				WithRemedy("Ask an administrator which account to use."))
		case user.Status != "active":
			return refused(errs.New(errs.AuthInvalid, "Your Pando account is suspended.").
				WithRemedy("Ask an administrator to reactivate it."))
		}
		if !dryRun {
			s.afterSignIn(ctx, p, subject, user)
		}
		return out(OutcomeExisting, user)
	}

	if p.LinkByEmail && subject.EmailVerified && subject.Email != "" {
		candidate, ok, err := s.Identities.ByVerifiedEmail(ctx, subject.Email)
		if err != nil {
			return refused(errs.As(err))
		}
		if ok && !s.hasIdentityAt(ctx, candidate.ID, p.ID) {
			// An account holding an installation role is not linked on the
			// provider's word: whoever could get that provider to vouch for its
			// email would sign in as an administrator (R-080a). An
			// administrator links it explicitly instead.
			if refusal := s.installRoleRefusal(ctx, p, subject, candidate); refusal != nil {
				return refused(refusal)
			}
			if !dryRun {
				if _, err := s.Identities.Link(ctx, candidate.ID, p.ID, subject.ExternalID, "system", false, false); err != nil {
					return refused(errs.As(err))
				}
				s.audit(ctx, audit.Event{
					PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "user.identity.link",
					TargetKind: "user", TargetID: candidate.ID,
					Detail: map[string]any{"adapter_id": p.ID, "external_id": subject.ExternalID, "by_email": true},
				})
				s.afterSignIn(ctx, p, subject, candidate)
			}
			return out(OutcomeLinked, candidate)
		}
	}

	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return refused(errs.As(err))
	}
	if !p.JITProvisioning || doc.DisableJITProvisioning {
		return refused(errs.Newf(errs.AuthInvalid,
			"%s signed you in, but you do not have a Pando account yet.", p.Name).
			WithRemedy(fmt.Sprintf("Ask an administrator to give you access. They will need your ID at %s: %s.",
				p.Name, subject.ExternalID)))
	}
	if dryRun {
		return SignInOutcome{Kind: OutcomeCreated, Username: subject.ExternalID, DisplayName: subject.DisplayName,
			Groups: subject.Groups, GroupsFrom: groupsFrom}
	}
	created, err := s.Identities.CreateExternal(ctx, state.NewExternal{
		AdapterID: p.ID, ExternalID: subject.ExternalID, Email: subject.Email,
		DisplayName: displayName(subject), CreatedBy: "system",
	})
	if err != nil {
		return refused(errs.As(err))
	}
	s.audit(ctx, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "user.create",
		TargetKind: "user", TargetID: created.ID,
		Detail: map[string]any{"adapter_id": p.ID, "external_id": subject.ExternalID, "via": "jit"},
	})
	s.afterSignIn(ctx, p, subject, created)
	return out(OutcomeCreated, created)
}

// installRoleRefusal is why an account may not be linked by email, or nil when
// it may: it holds no installation role, directly or through a group.
func (s *Service) installRoleRefusal(ctx context.Context, p state.IdentityProvider, subject api.Subject, u state.User) *errs.Error {
	if s.Grants == nil {
		return errs.New(errs.AuthInvalid,
			"Pando could not check this account's permissions, so it did not link it to your sign-in by email.").
			WithRemedy("Ask an administrator to link your identity to your Pando account.")
	}
	held, err := s.Grants.InstallRolesForUsers(ctx, []string{u.ID})
	if err != nil {
		if e := errs.As(err); e != nil {
			return e
		}
		return errs.Wrap(errs.Internal, "Pando could not check this account's permissions.", err)
	}
	if len(held[u.ID]) == 0 {
		return nil
	}
	return errs.Newf(errs.AuthInvalid,
		"%s signed you in with an email that belongs to a Pando account holding installation permissions. "+
			"Pando does not link such an account to a provider by email alone.", p.Name).
		WithRemedy(fmt.Sprintf("Ask an administrator to link your identity at %s to your account in Accounts. "+
			"They will need your ID at %s: %s.", p.Name, p.Name, subject.ExternalID))
}

func displayName(s api.Subject) string {
	switch {
	case s.DisplayName != "":
		return s.DisplayName
	case s.Username != "":
		return s.Username
	}
	return s.Email
}

func (s *Service) hasIdentityAt(ctx context.Context, userID, providerID string) bool {
	ids, err := s.Identities.ForUser(ctx, userID)
	if err != nil {
		return true
	}
	for _, i := range ids {
		if i.AdapterID == providerID {
			return true
		}
	}
	return false
}

// afterSignIn records the sign-in and, unless SCIM owns this provider's
// groups, sets the account's groups at this provider from the claims.
//
// Neither failure refuses the sign-in: the person is who the provider said.
// A group change refused because it would leave nobody who can manage
// accounts (R-088) is audited, and the membership stays as it was.
func (s *Service) afterSignIn(ctx context.Context, p state.IdentityProvider, subject api.Subject, u state.User) {
	_ = s.Identities.RecordSignIn(ctx, p.ID, subject.ExternalID, u.ID, subject.Email, subject.DisplayName)
	if p.SCIMEnabled {
		return
	}
	if err := s.Groups.SyncMemberships(ctx, p.ID, u.ID, subject.Groups); err != nil {
		s.audit(ctx, audit.Event{
			PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "group.sync.refused",
			TargetKind: "user", TargetID: u.ID,
			Detail: map[string]any{"adapter_id": p.ID, "reason": errs.As(err).Message},
		})
	}
}
