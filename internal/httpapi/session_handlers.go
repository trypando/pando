package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/secret"
)

// actionSessionDenied is the audit action for a refused sign-in, whatever
// refused it: a wrong password, a limit (R-404), or a policy.
const actionSessionDenied = "session.denied"

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`

	// Code is the second factor, for an account with two-factor
	// authentication (R-405): an authenticator code or a recovery code.
	Code string `json:"code,omitempty"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if refuseFormBody(w, r) {
		return
	}
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	// Host policy may turn password sign-in off once people sign in through
	// an identity provider. Refused before the password is checked, so the
	// answer says nothing about whether it was right.
	if s.IDP != nil {
		if err := s.IDP.PasswordSignInAllowed(r.Context()); err != nil {
			s.denySignIn(r, req.Username, "password_sign_in_disabled")
			Error(w, r, err)
			return
		}
	}

	// Over either limit, refused before the password is checked: a right
	// password answered differently from a wrong one would let guessing carry
	// on past the limit (R-404).
	account, addr := signInLimits(req.Username, limitAddr(r))
	failures := s.failedAttempts()
	if s.signInLimited(w, r, req.Username, failures, account, addr) {
		return
	}

	subject, err := s.Identity.Authenticate(r.Context(), api.Credential{
		Username: req.Username,
		Password: secret.New(req.Password),
	})
	if err != nil {
		// Counted only when the answer was "wrong": a database that could not
		// be read is not a guess.
		if errs.CodeOf(err) == errs.AuthInvalid {
			if err := recordFailures(r.Context(), failures, account, addr); err != nil {
				Error(w, r, err)
				return
			}
		}
		// Audited as a denial: a pattern of failed logins is the signal that
		// matters, and it is the thing most commonly left out.
		s.denySignIn(r, req.Username, "")
		Error(w, r, err)
		return
	}

	user, found, err := s.Users.ByExternalID(r.Context(), state.LocalAdapterID, subject.ExternalID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.AuthInvalid, "That username and password do not match."))
		return
	}

	// The right password forgets the username's failures. Not the address's:
	// signing in to an account of your own would otherwise reset the count
	// for guessing at everyone else's.
	_ = failures.Clear(r.Context(), account.key)

	if !s.secondFactor(w, r, user, req.Code, addr) {
		return
	}

	s.startSession(w, r, user, http.StatusOK)
}

// Wrong passwords allowed before sign-in waits (R-404) [P]. Per username, so
// guessing at one account is slow wherever it comes from, and per address,
// higher, so one address spraying many usernames is slow too and a shared
// office address is not locked out by one person's typos.
const (
	signInAccountAttempts = 10
	signInAddrAttempts    = 50
	signInWindow          = 15 * time.Minute
)

// signInLimits are the two counts a password sign-in is held to (R-404). The
// username is counted whether or not the account exists, so the limit is not
// a way to learn which ones do.
func signInLimits(username, addr string) (account, address attemptLimit) {
	return attemptLimit{key: state.SignInUsernameKey(username), max: signInAccountAttempts, window: signInWindow},
		attemptLimit{key: state.SignInAddrKey(addr), max: signInAddrAttempts, window: signInWindow}
}

// denySignIn audits a refused sign-in, with why when it was not simply a
// wrong password.
func (s *Server) denySignIn(r *http.Request, username, reason string) {
	detail := map[string]any{"username": username}
	if reason != "" {
		detail["reason"] = reason
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindAnonymous,
		Action:        actionSessionDenied,
		TargetKind:    "user",
		Detail:        detail,
	})
}

// signInLimited answers a sign-in over the username's or the address's limit
// (R-404), audited, and reports whether it did. An unreadable count answers
// too: it is an error, never a free attempt.
func (s *Server) signInLimited(w http.ResponseWriter, r *http.Request, username string, failures FailedAttempts, account, addr attemptLimit) bool {
	wait, what, err := overLimit(r.Context(), failures, account, addr)
	if err != nil {
		Error(w, r, err)
		return true
	}
	if wait > 0 {
		s.denySignIn(r, username, "rate_limited")
		tooManyAttempts(w, r, wait, what)
		return true
	}
	return false
}

// overLimit says how long until a password may be tried again against the
// username's and the address's limits (R-404), and which one is holding it —
// zero when neither is.
func overLimit(ctx context.Context, failures FailedAttempts, account, addr attemptLimit) (time.Duration, string, error) {
	for _, limit := range []struct {
		attemptLimit
		what string
	}{
		{account, "Too many wrong passwords for this username."},
		{addr, "Too many wrong passwords from your network address."},
	} {
		wait, err := limit.exceeded(ctx, failures)
		if err != nil || wait > 0 {
			return wait, limit.what, err
		}
	}
	return 0, "", nil
}

// recordFailures counts one wrong guess against each limit.
func recordFailures(ctx context.Context, failures FailedAttempts, limits ...attemptLimit) error {
	for _, limit := range limits {
		if err := failures.Record(ctx, limit.key, limit.window); err != nil {
			return err
		}
	}
	return nil
}

// startSession signs a user in: a session, its cookie, and the audit event.
// Shared by sign-in and first-run setup, which signs the new administrator in
// so the next thing they see is the console rather than the same form again.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request, user state.User, status int) {
	policy := s.Identity.SessionPolicy()
	sess, err := s.Sessions.Create(r.Context(), user.ID, user.AdapterID, policy.MaxLifetime,
		r.UserAgent(), clientIP(r))
	if err != nil {
		Error(w, r, err)
		return
	}

	//nolint:gosec // G124: CookieScheme.sessionCookie sets Secure, HttpOnly and SameSite.
	cookie := s.cookies().sessionCookie(r, sess.Cookie.Reveal())
	cookie.Expires = sess.Session.ExpiresAt
	http.SetCookie(w, cookie)

	s.audit(r, audit.Event{
		PrincipalKind: audit.KindUser,
		PrincipalID:   user.ID,
		Action:        "session.create",
		TargetKind:    "session",
		TargetID:      sess.Session.ID,
	})

	setupRequired := false
	if s.TwoFactor != nil {
		pending, err := s.TwoFactor.SetupPending(r.Context(), user)
		if err != nil {
			Error(w, r, err)
			return
		}
		setupRequired = pending
	}

	JSON(w, status, map[string]any{
		"user_id":              user.ID,
		"must_change_password": user.MustChangePassword,

		// R-405: this session can do nothing but set up two-factor
		// authentication until it has.
		"two_factor_setup_required": setupRequired,
		"expires_at":                sess.Session.ExpiresAt.Format(time.RFC3339),

		// The revocation window is stated rather than implied (design 06 §3.1).
		// Implying revocation is instant is the failure mode here.
		"revocation_window_seconds": 120,
	})
}

// handleGetSetup says whether the installation is waiting for its first
// administrator (R-046). Public: the sign-in page asks before anyone can sign
// in, and the answer says nothing about who has an account.
func (s *Server) handleGetSetup(w http.ResponseWriter, r *http.Request) {
	needed, err := s.Users.NeedsSetup(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"needed": needed})
}

// handlePostSetup claims an unclaimed installation: the first account, made an
// administrator, with the username and password its holder chose, and signs
// them in (R-046). Public, because nobody can sign in yet — and refused the
// moment any account exists, so it is a door that exists only until it is
// used once. It opens only for the setup token Pando printed to its log
// (issue #130), so being first to the URL is not enough.
func (s *Server) handlePostSetup(w http.ResponseWriter, r *http.Request) {
	if refuseFormBody(w, r) {
		return
	}
	var req struct {
		SetupToken  string `json:"setup_token"`
		Username    string `json:"username"`
		DisplayName string `json:"display_name"`
		Password    string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	user, err := bootstrap.Claim(r.Context(), s.Users, s.Auditor, secret.New(strings.TrimSpace(req.SetupToken)), strings.TrimSpace(req.Username),
		strings.TrimSpace(req.DisplayName), secret.New(req.Password))
	if err != nil {
		Error(w, r, err)
		return
	}
	s.startSession(w, r, user, http.StatusCreated)
}

// cookies is how this install sets and reads its session cookie (cookie.go).
func (s *Server) cookies() CookieScheme {
	return CookieScheme{ExternalURL: s.ExternalURL}
}

// secureCookie decides whether Pando's cookies are marked Secure (O-19,
// CookieScheme.Secure).
func (s *Server) secureCookie(r *http.Request) bool {
	return s.cookies().Secure(r)
}

// currentSession is the active session the request's cookie carries, read by
// its secret, or ok false for none. Callers name the session by sess.ID, the
// public ID, never by the cookie: the cookie is a credential and the ID is
// what the audit log records (R-063, R-194).
func (s *Server) currentSession(r *http.Request) (sess state.Session, ok bool, err error) {
	cookie, _ := s.cookies().SessionID(r)
	if cookie == "" || s.Sessions == nil {
		return state.Session{}, false, nil
	}
	return s.Sessions.Active(r.Context(), cookie)
}

// endOtherSessions ends every session userID holds except the one the
// request's cookie carries, when that one is theirs. It is matched by its
// secret first, so a cookie with the wrong secret keeps nothing alive.
func (s *Server) endOtherSessions(r *http.Request, userID string) error {
	current, ok, err := s.currentSession(r)
	if err != nil {
		return err
	}
	if ok && current.UserID == userID {
		return s.Sessions.RevokeOthersForUser(r.Context(), userID, current.ID)
	}
	return s.Sessions.RevokeAllForUser(r.Context(), userID)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	sess, ok, err := s.currentSession(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	if ok {
		if err := s.Sessions.Revoke(r.Context(), sess.ID); err != nil {
			Error(w, r, err)
			return
		}
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindUser,
			PrincipalID:   PrincipalFrom(r.Context()).UserID,
			Action:        "session.revoke",
			TargetKind:    "session",
			TargetID:      sess.ID,
		})
	}

	// The attributes have to match the cookie being cleared, or a browser may
	// treat this as a different cookie and leave the original one in place —
	// which is why it comes from the same place as the one set at sign-in.
	//nolint:gosec // G124: CookieScheme.sessionCookie sets Secure, HttpOnly and SameSite.
	cleared := s.cookies().sessionCookie(r, "")
	cleared.MaxAge = -1
	http.SetCookie(w, cleared)
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}

	// The installation-wide verbs the caller holds (R-265). The console reads
	// this to decide whether to show the administrative entry and what to put
	// in it — the server's answer, not the client's inference. An empty list is
	// the normal case: most accounts hold nothing install-wide, and their
	// console is the launcher.
	// Never nil: a missing key and an empty list are the same thing to a client
	// reading `verbs ?? []`, and only one of them is an answer.
	verbs, err := s.installVerbs(r, p)
	if err != nil {
		Error(w, r, err)
		return
	}

	body := map[string]any{
		"principal_kind": string(p.Kind),
		"id":             p.ID,
		"groups":         p.Groups,
		"verbs":          verbs,
	}
	if p.UserID != "" {
		user, found, err := s.Users.ByID(r.Context(), p.UserID)
		if err != nil {
			Error(w, r, err)
			return
		}
		if found {
			body["user_id"] = user.ID

			// The sign-in name. The console shows it on the first-run password
			// screen so a password manager has something to file the new
			// credential under, and so the person can see which account they
			// are changing.
			body["username"] = user.ExternalID
			body["email"] = user.Email
			body["display_name"] = user.DisplayName
			body["must_change_password"] = user.MustChangePassword

			// A local account's second factor (R-405). An identity
			// provider's account has none here: the provider asks for it.
			if s.TwoFactor != nil && user.AdapterID == state.LocalAdapterID {
				tf, err := s.meTwoFactor(r, user)
				if err != nil {
					Error(w, r, err)
					return
				}
				body["two_factor"] = tf
			}
		}
	}
	JSON(w, http.StatusOK, body)
}

// installVerbs is the install-wide verbs p holds, never nil.
func (s *Server) installVerbs(r *http.Request, p authz.Principal) ([]string, error) {
	if s.Verbs == nil {
		return []string{}, nil
	}
	held, err := s.Verbs.InstallVerbsFor(r.Context(), p)
	if err != nil || held == nil {
		return []string{}, err
	}
	return held, nil
}

func clientIP(r *http.Request) string {
	// Behind Pando's own proxy this is the immediate peer. Trusting a forwarded
	// header here would let a client choose what gets recorded in the audit log.
	host, _, found := cut(r.RemoteAddr, ':')
	if !found {
		return r.RemoteAddr
	}
	return host
}

func cut(s string, sep byte) (string, string, bool) {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == sep {
			return s[:i], s[i+1:], true
		}
	}
	return s, "", false
}

type changePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// handleChangePassword changes the caller's own password (R-046).
//
// Self only, and deliberately not a verb. Changing your own password is not
// administration, and changing somebody else's is a *reset* — a different
// action with different consequences, which does not exist yet and should not
// arrive by relaxing this route.
//
// The current password is required even though the caller is already
// authenticated. A session cookie is a bearer credential: someone holding a
// borrowed one could otherwise lock the owner out of their own account, which
// is the same escalation shape as O-17 on a smaller scale. Re-authentication
// goes through the identity adapter rather than comparing hashes here, so the
// adapter's own rules — including its failure timing — still apply (R-044).
//
// Without this endpoint R-046's "must be changed on first login" is a flag that
// nothing can clear, which is what it was until now.
// readChangePassword reads a password change's body and checks the new
// password's shape.
func readChangePassword(r *http.Request) (changePasswordRequest, error) {
	var req changePasswordRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return req, errs.New(errs.ValidInvalid, "The request body could not be read.")
	}
	if req.NewPassword == "" {
		return req, errs.New(errs.ValidInvalid, "A password cannot be empty.")
	}
	if len(req.NewPassword) < hash.MinPasswordLength {
		return req, errs.Newf(errs.ValidInvalid,
			"A password needs at least %d characters.", hash.MinPasswordLength).
			WithRemedy("A short phrase you will remember is a good password.")
	}
	return req, nil
}

func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	if p.UserID == "" {
		// An account token is its own principal and has no account to hold a
		// password (R-060).
		Error(w, r, errs.New(errs.ValidInvalid, "A token has no password to change."))
		return
	}

	req, err := readChangePassword(r)
	if err != nil {
		Error(w, r, err)
		return
	}

	user, found, err := s.Users.ByID(r.Context(), p.UserID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.AuthInvalid, "This account no longer exists."))
		return
	}
	// An account from an identity provider has no Pando password; asking
	// for its "current password" would only ever say it was wrong.
	if user.AdapterID != state.LocalAdapterID {
		Error(w, r, errs.New(errs.ValidInvalid,
			"This account signs in through an identity provider, so it has no Pando password to change.").
			WithRemedy("Change your password where you sign in, at your organization's identity provider."))
		return
	}

	// The current password is a guess like any sign-in's, and is held to the
	// same counts (R-404): the username's is shared with sign-in, so neither
	// door gives a second allowance. Without it a stolen session or token could
	// try passwords here as fast as it liked, then set a new one and end the
	// owner's sessions.
	denied := func(reason string) {
		detail := map[string]any{}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind),
			PrincipalID:   p.ID,
			OnBehalfOf:    p.UserID,
			Action:        "user.password.denied",
			TargetKind:    "user",
			TargetID:      p.UserID,
			Detail:        detail,
		})
	}
	account, addr := signInLimits(user.ExternalID, limitAddr(r))
	failures := s.failedAttempts()
	if wait, what, err := overLimit(r.Context(), failures, account, addr); err != nil || wait > 0 {
		if err != nil {
			Error(w, r, err)
			return
		}
		denied("rate_limited")
		tooManyAttempts(w, r, wait, what)
		return
	}

	if _, err := s.Identity.Authenticate(r.Context(), api.Credential{
		Username: user.ExternalID,
		Password: secret.New(req.CurrentPassword),
	}); err != nil {
		if errs.CodeOf(err) == errs.AuthInvalid {
			if err := recordFailures(r.Context(), failures, account, addr); err != nil {
				Error(w, r, err)
				return
			}
		}
		denied("")
		Error(w, r, errs.New(errs.AuthInvalid, "That current password is not right."))
		return
	}
	_ = failures.Clear(r.Context(), account.key)

	digest, err := hash.New(secret.New(req.NewPassword))
	if err != nil {
		Error(w, r, errs.Wrap(errs.Internal, "Could not secure the password.", err))
		return
	}
	if err := s.Users.SetPassword(r.Context(), p.UserID, digest); err != nil {
		Error(w, r, err)
		return
	}

	// Every other session ends. A password change is what someone does when
	// they think a credential has leaked, and leaving the leaked session alive
	// would make the act cosmetic. The current one survives so that changing a
	// password does not sign you out of the page you changed it on.
	if err := s.endOtherSessions(r, p.UserID); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "user.password.change",
		TargetKind:    "user",
		TargetID:      p.UserID,
	})
	JSON(w, http.StatusNoContent, nil)
}
