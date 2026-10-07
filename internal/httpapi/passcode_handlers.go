package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"time"

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

// MinPasscodeLength is the shortest passcode accepted. Short on purpose — a
// passcode is typed by people it was read out to — and made safe by the
// attempt limit below rather than by length.
const MinPasscodeLength = 4

// Wrong passcodes a visitor may try per app before waiting, and how long.
const (
	passcodeAttempts = 10
	passcodeWindow   = 15 * time.Minute
)

// PasscodeFailures counts wrong passcodes per app and client address.
//
// Shared by every replica in production (state.PasscodeFailures): a count kept
// in one process's memory gave an attacker one allowance per replica behind
// the load balancer, and a fresh one whenever a replica restarted (issue #72).
type PasscodeFailures interface {
	Recent(ctx context.Context, key string, window time.Duration) (int, error)
	Record(ctx context.Context, key string, window time.Duration) error
	Clear(ctx context.Context, key string) error
}

// memoryPasscodeFailures is the count when no store is wired, as in a test
// that builds a Server by hand. Correct for one process and only one.
type memoryPasscodeFailures struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

var passcodeTries = &memoryPasscodeFailures{seen: map[string][]time.Time{}}

func (l *memoryPasscodeFailures) Recent(_ context.Context, key string, window time.Duration) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	recent := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	l.seen[key] = recent
	return len(recent), nil
}

func (l *memoryPasscodeFailures) Record(_ context.Context, key string, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[key] = append(l.seen[key], time.Now())
	return nil
}

func (l *memoryPasscodeFailures) Clear(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, key)
	return nil
}

func (s *Server) passcodeFailures() PasscodeFailures {
	if s.PasscodeFailures != nil {
		return s.PasscodeFailures
	}
	return passcodeTries
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

	key := app.ID + "|" + clientIP(r)
	failures := s.passcodeFailures()
	recent, err := failures.Recent(r.Context(), key, passcodeWindow)
	if err != nil {
		Error(w, r, err)
		return
	}
	if recent >= passcodeAttempts {
		Error(w, r, errs.New(errs.RateLimited, "Too many wrong passcodes. Wait a few minutes and try again."))
		return
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
		if err := failures.Record(r.Context(), key, passcodeWindow); err != nil {
			Error(w, r, err)
			return
		}
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "app.passcode.denied", AppID: app.ID,
		})
		Error(w, r, errs.New(errs.AuthInvalid, "That passcode isn't right. Check it with whoever gave it to you."))
		return
	}
	_ = failures.Clear(r.Context(), key)

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
	if len(passcode) < MinPasscodeLength {
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
