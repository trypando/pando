package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/twofactor"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Two-factor authentication for local accounts (R-405). The decisions are in
// internal/core/twofactor; these handlers check who is asking, hold guesses
// to R-404's counts, and audit.

// Wrong codes allowed before sign-in waits [P]. Per account, lower than the
// password's because the password was already right: what is being guessed is
// six digits.
const signInCodeAttempts = 5

// recentSignIn is how long after signing in a session may change its own
// second factor without giving its password again: long enough to set one up
// straight after signing in, which is when Pando asks.
const recentSignIn = 10 * time.Minute

// errUnreadableBody answers a request body that is not the JSON a route reads.
func errUnreadableBody() error {
	return errs.New(errs.ValidInvalid, "The request body could not be read.")
}

// codeLimit is the count wrong codes for an account are held to.
func codeLimit(userID string) attemptLimit {
	return attemptLimit{key: state.SignInCodeKey(userID), max: signInCodeAttempts, window: signInWindow}
}

// secondFactor is the second step of a password sign-in (R-405), for an
// account with two-factor authentication on. It reports whether sign-in may go
// on; when not, it has answered.
func (s *Server) secondFactor(w http.ResponseWriter, r *http.Request, user state.User, presented string, addr attemptLimit) bool {
	if !user.TwoFactorEnabled || s.TwoFactor == nil {
		return true
	}
	if strings.TrimSpace(presented) == "" {
		// Not a failure: the password was right and the client did not know
		// to send a code yet. It asks for one and sends both again.
		Error(w, r, errs.New(errs.AuthTwoFactorRequired,
			"This account has two-factor authentication. Enter the 6-digit code from your authenticator app, "+
				"or one of your recovery codes.").
			WithRemedy("Send the same username and password again, with the code as `code`."))
		return false
	}

	failures := s.failedAttempts()
	account := codeLimit(user.ID)
	wait, err := account.exceeded(r.Context(), failures)
	if err == nil && wait == 0 {
		wait, err = addr.exceeded(r.Context(), failures)
	}
	if err != nil {
		Error(w, r, err)
		return false
	}
	if wait > 0 {
		s.denySignIn(r, user.ExternalID, "rate_limited")
		tooManyAttempts(w, r, wait, "Too many wrong codes for this account.")
		return false
	}

	method, err := s.TwoFactor.Verify(r.Context(), user.ID, presented)
	if err != nil {
		if errs.CodeOf(err) == errs.AuthInvalid {
			if err := recordFailures(r.Context(), failures, account, addr); err != nil {
				Error(w, r, err)
				return false
			}
			s.denySignIn(r, user.ExternalID, "wrong_code")
		}
		Error(w, r, err)
		return false
	}
	_ = failures.Clear(r.Context(), account.key)

	if method == twofactor.MethodRecoveryCode {
		left := 0
		if summary, err := s.TwoFactor.Summarize(r.Context(), user.ID); err == nil {
			left = summary.RecoveryCodesRemaining
		}
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindUser,
			PrincipalID:   user.ID,
			Action:        "user.two_factor.recovery_code_used",
			TargetKind:    "user",
			TargetID:      user.ID,
			Detail:        map[string]any{"recovery_codes_remaining": left},
		})
	}
	return true
}

// twoFactorSetupRoutes are what a session that must set up its second factor
// may reach (R-405): who it is, signing out, the first-run password change
// that comes before it, and setting the second factor up. Everything else
// answers POLICY_TWO_FACTOR_REQUIRED. Listed rather than derived, like
// signInRoutes, so adding one is a decision a reviewer sees.
var twoFactorSetupRoutes = []struct{ method, pattern string }{
	{http.MethodPost, sessionsPath},
	{http.MethodDelete, sessionsPath},
	{http.MethodGet, "/api/v1/me"},
	{http.MethodPost, "/api/v1/me/password"},
	{http.MethodPost, "/api/v1/me/two-factor/totp"},
	{http.MethodPost, "/api/v1/me/two-factor/totp/confirm"},
	{http.MethodGet, "/api/v1/auth/options"},
	{http.MethodGet, "/api/v1/setup"},
}

// twoFactorGate holds back a session that must set up its second factor.
// Authentication, not authorization: the session's holder has given only
// their password, which policy says is not enough, so they are not yet
// signed in to anything but the setup (R-405).
func (s *Server) twoFactorGate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !PrincipalFrom(r.Context()).TwoFactorSetupPending {
			next.ServeHTTP(w, r)
			return
		}
		method := r.Method
		if method == http.MethodHead {
			method = http.MethodGet
		}
		for _, route := range twoFactorSetupRoutes {
			if route.method == method && matchRoute(route.pattern, r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
		}
		Error(w, r, errs.New(errs.PolicyTwoFactorRequired,
			"This installation requires two-factor authentication on local accounts. "+
				"Set it up before doing anything else.").
			WithRemedy("Open the console and follow the steps to set up an authenticator app."))
	})
}

// meTwoFactorJSON is GET /me's two_factor: a local account's second factor as
// its holder sees it, and whether changing it needs the password again.
type meTwoFactorJSON struct {
	twofactor.Summary

	// PasswordRequired says changing it needs current_password: the session
	// is more than a few minutes old, or this is a token.
	PasswordRequired bool `json:"password_required"`

	// SetupHere says it can be set up at this address. Not on an app's own
	// hostname or port, where the app's script could read the key and the
	// recovery codes (issue #78); the page sends the person to ConsoleURL,
	// Pando's own address, where one is configured.
	SetupHere  bool   `json:"setup_here"`
	ConsoleURL string `json:"console_url,omitempty"`
}

func (s *Server) meTwoFactor(r *http.Request, user state.User) (*meTwoFactorJSON, error) {
	summary, err := s.TwoFactor.Summarize(r.Context(), user.ID)
	if err != nil {
		return nil, err
	}
	out := &meTwoFactorJSON{Summary: summary, PasswordRequired: !s.signedInRecently(r), SetupHere: !s.onAppAddress(r)}
	if s.ExternalURL != nil {
		out.ConsoleURL = s.ExternalURL.String()
	}
	return out, nil
}

// signedInRecently says the request's session began within recentSignIn. A
// token never did: it is not a sign-in.
func (s *Server) signedInRecently(r *http.Request) bool {
	sess, ok, err := s.currentSession(r)
	return err == nil && ok && time.Since(sess.IssuedAt) < recentSignIn
}

// selfLocal is the caller's own local account, for the routes that change its
// second factor. It answers and reports false when there is none.
func (s *Server) selfLocal(w http.ResponseWriter, r *http.Request) (authz.Principal, state.User, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return p, state.User{}, false
	}
	if p.UserID == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "A token has no account to set up two-factor authentication on."))
		return p, state.User{}, false
	}
	if s.TwoFactor == nil {
		Error(w, r, errs.New(errs.StateInvalid, "Two-factor authentication is not set up on this server."))
		return p, state.User{}, false
	}
	user, found, err := s.Users.ByID(r.Context(), p.UserID)
	if err != nil {
		Error(w, r, err)
		return p, state.User{}, false
	}
	if !found {
		Error(w, r, errs.New(errs.AuthInvalid, "This account no longer exists."))
		return p, state.User{}, false
	}
	if user.AdapterID != state.LocalAdapterID {
		Error(w, r, errs.New(errs.ValidInvalid,
			"This account signs in through an identity provider, which asks for its second factor. "+
				"Pando's two-factor authentication is for local accounts.").
			WithRemedy("Set up two-factor authentication at your organization's identity provider."))
		return p, state.User{}, false
	}
	return p, user, true
}

type reauthRequest struct {
	// Not needed within a few minutes of signing in (recentSignIn).
	CurrentPassword string `json:"current_password,omitempty"`
}

// decodeOptional reads a JSON body that may be empty.
func decodeOptional(r *http.Request, v any) error {
	err := json.NewDecoder(r.Body).Decode(v)
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

// reauthenticated checks that whoever holds this session is its account's
// holder before their second factor changes: they signed in a few minutes ago,
// or they give the password now, held to R-404's counts as on POST
// /me/password. A borrowed session could otherwise swap in its own
// authenticator and lock the owner out, or take theirs away. It answers and
// reports false when not.
func (s *Server) reauthenticated(w http.ResponseWriter, r *http.Request, p authz.Principal, user state.User, password string) bool {
	if password == "" && p.Kind == authz.KindUser && s.signedInRecently(r) {
		return true
	}
	if password == "" {
		Error(w, r, errs.New(errs.AuthInvalid, "Enter your current password to change two-factor authentication.").
			WithRemedy("Send it as `current_password`. Within 10 minutes of signing in it is not needed."))
		return false
	}
	account, addr := signInLimits(user.ExternalID, limitAddr(r))
	failures := s.failedAttempts()
	if wait, what, err := overLimit(r.Context(), failures, account, addr); err != nil || wait > 0 {
		if err != nil {
			Error(w, r, err)
			return false
		}
		s.auditTwoFactor(r, p, "user.two_factor.denied", user.ID, map[string]any{"reason": "rate_limited"})
		tooManyAttempts(w, r, wait, what)
		return false
	}
	if _, err := s.Identity.Authenticate(r.Context(), api.Credential{
		Username: user.ExternalID,
		Password: secret.New(password),
	}); err != nil {
		if errs.CodeOf(err) == errs.AuthInvalid {
			if err := recordFailures(r.Context(), failures, account, addr); err != nil {
				Error(w, r, err)
				return false
			}
		}
		s.auditTwoFactor(r, p, "user.two_factor.denied", user.ID, nil)
		Error(w, r, errs.New(errs.AuthInvalid, "That current password is not right."))
		return false
	}
	_ = failures.Clear(r.Context(), account.key)
	return true
}

func (s *Server) auditTwoFactor(r *http.Request, p authz.Principal, action, userID string, detail map[string]any) {
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		TargetKind:    "user",
		TargetID:      userID,
		Detail:        detail,
	})
}

// handleBeginTOTP makes a new authenticator for the caller's own account, to
// be confirmed with a code from it. Also how an authenticator is replaced: the
// one in use stays in use until the new one is confirmed.
func (s *Server) handleBeginTOTP(w http.ResponseWriter, r *http.Request) {
	p, user, ok := s.selfLocal(w, r)
	if !ok {
		return
	}
	var req reauthRequest
	if err := decodeOptional(r, &req); err != nil {
		Error(w, r, errUnreadableBody())
		return
	}
	// Setting one up where policy requires it, on a session that can do
	// nothing else, needs no password: the session was made by giving it, and
	// there is no factor yet for a borrowed session to take away.
	if !p.TwoFactorSetupPending && !s.reauthenticated(w, r, p, user, req.CurrentPassword) {
		return
	}
	enrollment, err := s.TwoFactor.Begin(r.Context(), user.ID, user.ExternalID)
	if err != nil {
		Error(w, r, err)
		return
	}
	// The secret is shown to its holder, once, to put in their app. Revealed
	// here and nowhere else; it is never logged or audited.
	JSON(w, http.StatusOK, map[string]any{
		"secret": enrollment.Secret.Reveal(),
		"uri":    enrollment.URI.Reveal(),
		"qr_svg": enrollment.QRSVG,
	})
}

// handleConfirmTOTP puts the pending authenticator in use when the code
// given is from it, and answers with the account's recovery codes — shown
// this once.
func (s *Server) handleConfirmTOTP(w http.ResponseWriter, r *http.Request) {
	p, user, ok := s.selfLocal(w, r)
	if !ok {
		return
	}
	var req struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errUnreadableBody())
		return
	}

	// Six digits again, so the same count as at sign-in: a borrowed session
	// guessing here would gain nothing, but an unlimited guess is never free.
	failures := s.failedAttempts()
	limit := codeLimit(user.ID)
	if wait, err := limit.exceeded(r.Context(), failures); err != nil || wait > 0 {
		if err != nil {
			Error(w, r, err)
			return
		}
		tooManyAttempts(w, r, wait, "Too many wrong codes for this account.")
		return
	}
	replacing := user.TwoFactorEnabled
	codes, err := s.TwoFactor.Confirm(r.Context(), user.ID, req.Code)
	if err != nil {
		if errs.CodeOf(err) == errs.AuthInvalid {
			_ = recordFailures(r.Context(), failures, limit)
		}
		Error(w, r, err)
		return
	}
	_ = failures.Clear(r.Context(), limit.key)
	s.auditTwoFactor(r, p, "user.two_factor.enable", user.ID, map[string]any{
		"method": "authenticator", "replaced": replacing,
	})
	JSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// handleNewRecoveryCodes replaces the caller's recovery codes.
func (s *Server) handleNewRecoveryCodes(w http.ResponseWriter, r *http.Request) {
	p, user, ok := s.selfLocal(w, r)
	if !ok {
		return
	}
	var req reauthRequest
	if err := decodeOptional(r, &req); err != nil {
		Error(w, r, errUnreadableBody())
		return
	}
	if !s.reauthenticated(w, r, p, user, req.CurrentPassword) {
		return
	}
	codes, err := s.TwoFactor.NewRecoveryCodes(r.Context(), user.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.auditTwoFactor(r, p, "user.two_factor.recovery_codes", user.ID, nil)
	JSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// handleTurnOffTwoFactor turns the caller's own second factor off, where
// policy allows.
func (s *Server) handleTurnOffTwoFactor(w http.ResponseWriter, r *http.Request) {
	p, user, ok := s.selfLocal(w, r)
	if !ok {
		return
	}
	var req reauthRequest
	if err := decodeOptional(r, &req); err != nil {
		Error(w, r, errUnreadableBody())
		return
	}
	if !s.reauthenticated(w, r, p, user, req.CurrentPassword) {
		return
	}
	had, err := s.TwoFactor.TurnOff(r.Context(), user.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if had {
		s.auditTwoFactor(r, p, "user.two_factor.disable", user.ID, nil)
	}
	JSON(w, http.StatusNoContent, nil)
}

// handleSkipTwoFactor is "Not now" when asked to set it up: not asked again.
func (s *Server) handleSkipTwoFactor(w http.ResponseWriter, r *http.Request) {
	p, user, ok := s.selfLocal(w, r)
	if !ok {
		return
	}
	if err := s.TwoFactor.Decline(r.Context(), user.ID); err != nil {
		Error(w, r, err)
		return
	}
	s.auditTwoFactor(r, p, "user.two_factor.decline", user.ID, nil)
	JSON(w, http.StatusNoContent, nil)
}

// handleResetTwoFactor is an administrator's reset of someone else's second
// factor (R-405), for a person who has lost their authenticator and their
// recovery codes: like a password reset, it signs them out everywhere, and
// they sign in with their password alone — and set it up again there, if
// policy requires it.
func (s *Server) handleResetTwoFactor(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok {
		return
	}
	userID := chi.URLParam(r, "userID")
	if p.UserID == userID {
		Error(w, r, errs.New(errs.ValidInvalid,
			"You cannot reset your own two-factor authentication here.").
			WithRemedy("Change it from your settings, which ask for your current password instead."))
		return
	}
	// Taking away someone's second factor weakens what stands between their
	// account and whoever has its password, and with it whatever it holds
	// (R-080a).
	if err := s.Authz.CheckMayChangeAccount(r.Context(), p, userID, "reset this account's two-factor authentication"); err != nil {
		Error(w, r, err)
		return
	}
	if s.TwoFactor == nil {
		Error(w, r, errs.New(errs.StateInvalid, "Two-factor authentication is not set up on this server."))
		return
	}
	user, found, err := s.Users.ByID(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found || user.Status == "deleted" {
		Error(w, r, errs.Newf(errs.NotFound, "There is no account %s.", userID))
		return
	}
	if user.AdapterID != state.LocalAdapterID {
		Error(w, r, errs.New(errs.ValidInvalid,
			"This account signs in through an identity provider, which holds its second factor. Pando has nothing to reset.").
			WithRemedy("Reset it at the identity provider."))
		return
	}
	had, err := s.TwoFactor.Reset(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if err := s.Sessions.RevokeAllForUser(r.Context(), userID); err != nil {
		Error(w, r, err)
		return
	}
	// The wrong codes counted against them go too: they start clean.
	_ = s.failedAttempts().Clear(r.Context(), state.SignInCodeKey(userID))
	s.auditTwoFactor(r, p, "user.two_factor.reset", userID, map[string]any{"had_two_factor": had})
	JSON(w, http.StatusNoContent, nil)
}
