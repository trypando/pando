package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/proxy"
	"github.com/trypando/pando/internal/secret"
)

// Public with a passcode (R-075a).
//
// Two public endpoints, because the person using them has no account: one
// names the app for the passcode page, the other takes the passcode and, if it
// is right, sets the cookie the proxy checks. Everything about *setting* a
// passcode is on the grants endpoints, behind app.grants.manage.

// MinPasscodeLength is the shortest passcode accepted [P]. Short on purpose —
// a passcode is typed by people it was read out to — but not so short that
// the per-app limit below is all that stands in the way: four characters was
// within reach of anyone with enough addresses to spread the guesses over.
const MinPasscodeLength = 6

// Wrong passcodes allowed before waiting (R-075a) [P]: per visitor's address
// for each app, and per app from every address together, so guesses spread
// over many addresses still run out. The app's count is never cleared by a
// right passcode — whoever knows it is not the one guessing — so the most a
// guesser can do is make new visitors wait out the window; a browser already
// let in keeps its day.
const (
	passcodeAttempts    = 10
	passcodeAppAttempts = 100
	passcodeWindow      = 15 * time.Minute
)

// passcodeLimits are the two counts a passcode attempt is held to.
func passcodeLimits(appID, addr string) (visitor, app attemptLimit) {
	return attemptLimit{key: appID + "|" + addr, max: passcodeAttempts, window: passcodeWindow},
		attemptLimit{key: "passcode|app|" + appID, max: passcodeAppAttempts, window: passcodeWindow}
}

// handleGetPasscodeApp names an app for its passcode page. Answers only for an
// app that asks for a passcode — any other app is not-found here, so this is no
// way to learn what apps exist or what they are called.
func (s *Server) handleGetPasscodeApp(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, grant, ok := s.passcodeApp(w, r, appID)
	if !ok {
		return
	}
	_ = grant
	JSON(w, http.StatusOK, map[string]string{"app_id": app.ID, "name": app.Name})
}

// handleEnterPasscode takes a visitor's passcode and, when it is right, lets
// their browser in for state.UnlockLifetime (R-075a).
func (s *Server) handleEnterPasscode(w http.ResponseWriter, r *http.Request) {
	appID := chi.URLParam(r, "appID")
	app, grant, ok := s.passcodeApp(w, r, appID)
	if !ok {
		return
	}

	visitor, everyone := passcodeLimits(app.ID, limitAddr(r))
	failures := s.failedAttempts()
	for _, limit := range []struct {
		attemptLimit
		what string
	}{
		{visitor, "Too many wrong passcodes for this app from your network address."},
		{everyone, "Too many wrong passcodes for this app have been tried recently, from anywhere."},
	} {
		wait, err := limit.exceeded(r.Context(), failures)
		if err != nil {
			Error(w, r, err)
			return
		}
		if wait > 0 {
			tooManyAttempts(w, r, wait, limit.what)
			return
		}
	}

	var req struct {
		Passcode string `json:"passcode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	right, err := hash.Verify(secret.New(req.Passcode), grant.PasscodeHash)
	if err != nil || !right {
		if err := recordFailures(r.Context(), failures, visitor, everyone); err != nil {
			Error(w, r, err)
			return
		}
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "app.passcode.denied", AppID: app.ID,
		})
		Error(w, r, errs.New(errs.AuthInvalid, "That passcode isn't right. Check it with whoever gave it to you."))
		return
	}
	_ = failures.Clear(r.Context(), visitor.key)

	token, expires, err := s.Grants.Unlock(r.Context(), app.ID, grant.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	//nolint:gosec // G124: Secure is decided by secureCookie, not left unset.
	http.SetCookie(w, &http.Cookie{
		Name:     proxy.PasscodeCookiePrefix + app.ID,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		Secure:   s.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
		Expires:  expires,
	})
	s.audit(r, audit.Event{PrincipalKind: audit.KindAnonymous, Action: "app.passcode.unlock", AppID: app.ID})
	JSON(w, http.StatusNoContent, nil)
}

// passcodeApp finds an app that asks for a passcode, or answers not-found.
func (s *Server) passcodeApp(w http.ResponseWriter, r *http.Request, appID string) (appRef, passcodeGrant, bool) {
	notFound := func() (appRef, passcodeGrant, bool) {
		Error(w, r, errs.New(errs.NotFound, "There is no app asking for a passcode here."))
		return appRef{}, passcodeGrant{}, false
	}
	if !id.Is(id.App, appID) {
		return notFound()
	}
	app, found, err := s.Apps.ByID(r.Context(), appID)
	if err != nil {
		Error(w, r, err)
		return appRef{}, passcodeGrant{}, false
	}
	if !found {
		return notFound()
	}
	grant, has, err := s.Grants.AnonymousGrantFor(r.Context(), appID)
	if err != nil {
		Error(w, r, err)
		return appRef{}, passcodeGrant{}, false
	}
	if !has || grant.PasscodeHash == "" {
		return notFound()
	}
	return appRef{ID: app.ID, Name: app.Name}, passcodeGrant{ID: grant.ID, PasscodeHash: grant.PasscodeHash}, true
}

type appRef struct{ ID, Name string }
type passcodeGrant struct{ ID, PasscodeHash string }

// passcodeDigest checks and hashes a passcode an administrator set.
func passcodeDigest(passcode string) (string, error) {
	passcode = strings.TrimSpace(passcode)
	// Characters, not bytes: "ééé" is three, as the console counts it.
	if utf8.RuneCountInString(passcode) < MinPasscodeLength {
		return "", errs.Newf(errs.ValidInvalid, "A passcode needs at least %d characters.", MinPasscodeLength)
	}
	digest, err := hash.New(secret.New(passcode))
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not secure the passcode.", err)
	}
	return digest, nil
}

// handleSharePrincipals finds people and groups to share an app with: the
// share dialog's picker. For whoever may share the app (app.grants.manage), who
// may not otherwise be able to list accounts at all — capped, and searched,
// so it answers "who is Dana" rather than handing over the directory.
func (s *Server) handleSharePrincipals(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireControl(w, r, authz.AppGrantsManage); !ok {
		return
	}
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	users, err := s.Users.Search(r.Context(), q, 20)
	if err != nil {
		Error(w, r, err)
		return
	}
	groups, err := s.Groups.Search(r.Context(), q, 20)
	if err != nil {
		Error(w, r, err)
		return
	}
	type person struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Name     string `json:"name,omitempty"`
	}
	type group struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	outU := make([]person, 0, len(users))
	for _, u := range users {
		outU = append(outU, person{ID: u.ID, Username: u.ExternalID, Name: u.DisplayName})
	}
	outG := make([]group, 0, len(groups))
	for _, g := range groups {
		outG = append(outG, group{ID: g.ID, Name: g.Name})
	}
	JSON(w, http.StatusOK, map[string]any{"users": outU, "groups": outG})
}
