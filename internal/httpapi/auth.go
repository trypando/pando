package httpapi

import (
	"context"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

type ctxKeyPrincipal struct{}

// SessionCookie carries only the session ID (design 02 §2.7).
const SessionCookie = "pando_session"

// Authenticator resolves credentials to a principal. Steps 1–3 of the
// evaluation order; steps 4–7 belong to the authorizer.
type Authenticator struct {
	Sessions *state.Sessions
	Tokens   *state.Tokens
	Users    *state.Users
	Groups   interface {
		GroupsForUser(ctx context.Context, userID string) ([]string, error)
	}
}

// Authenticate resolves a request to a principal, or to anonymous.
//
// Anonymous is a principal, not an absence: R-075's anonymous grant is checked
// on the same path as any other, and there is no branch that skips the check.
func (a *Authenticator) Authenticate(r *http.Request) (authz.Principal, error) {
	// Precedence: bearer token, then session cookie (design 04 §1).
	if header := r.Header.Get("Authorization"); header != "" {
		raw, ok := strings.CutPrefix(header, "Bearer ")
		if !ok {
			return authz.Anonymous(), errs.New(errs.AuthInvalid, "The Authorization header must be a bearer token.")
		}
		return a.fromToken(r.Context(), secret.New(raw))
	}

	if cookie, err := r.Cookie(SessionCookie); err == nil && cookie.Value != "" {
		return a.fromSession(r.Context(), cookie.Value)
	}

	return authz.Anonymous(), nil
}

func (a *Authenticator) fromToken(ctx context.Context, presented secret.Value) (authz.Principal, error) {
	tok, err := a.Tokens.Authenticate(ctx, presented)
	if err != nil {
		return authz.Anonymous(), err
	}

	p := authz.Principal{Kind: authz.KindToken, ID: tok.ID, TokenID: tok.ID}

	// A delegated token acts as its owner (R-058). Its groups are the owner's,
	// resolved live. An account token is its own principal and has none.
	if tok.OwnerUserID != "" {
		p.UserID = tok.OwnerUserID
		groups, err := a.Groups.GroupsForUser(ctx, tok.OwnerUserID)
		if err != nil {
			return authz.Anonymous(), err
		}
		p.Groups = groups
	}
	return p, nil
}

func (a *Authenticator) fromSession(ctx context.Context, sessionID string) (authz.Principal, error) {
	p, _, err := a.SessionPrincipal(ctx, sessionID)
	return p, err
}

// SessionPrincipal resolves a session cookie to its principal, and says when
// the session expires so a caller that keeps the answer keeps it no longer
// (the proxy's cache, issue #93). The session, its account and the account's
// groups are one query (Sessions.ActiveWithUser); groups are still read live
// on every call (R-079).
func (a *Authenticator) SessionPrincipal(ctx context.Context, sessionID string) (authz.Principal, time.Time, error) {
	sess, user, groups, ok, err := a.Sessions.ActiveWithUser(ctx, sessionID)
	if err != nil {
		return authz.Anonymous(), time.Time{}, err
	}
	if !ok {
		// An expired or revoked session, or one whose account is gone, is not
		// an error the caller must handle — it is simply not authenticated,
		// and the anonymous path decides whether that is enough.
		return authz.Anonymous(), time.Time{}, nil
	}

	// Email and name go into the assertion an app receives (R-054). Display
	// detail only: authorization reads ID and Status, never these.
	return authz.Principal{
		Kind:        authz.KindUser,
		ID:          user.ID,
		UserID:      user.ID,
		Groups:      groups,
		Email:       user.Email,
		DisplayName: user.DisplayName,
		AdapterID:   user.AdapterID,
		Status:      user.Status,
	}, sess.ExpiresAt, nil
}

// Authenticate is middleware that attaches a principal to every request.
//
// It never rejects: an unauthenticated request proceeds as anonymous and is
// rejected, or not, by the authorization check the handler performs. Rejecting
// here would create a second place where access is decided.
func Authenticate(a *Authenticator) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// SCIM presents a provider's own bearer token, which is not a
			// Pando credential and would be refused as one. Those handlers
			// authenticate it themselves, and it reaches nothing else.
			if strings.HasPrefix(r.URL.Path, scimPrefix+"/") {
				next.ServeHTTP(w, r)
				return
			}
			p, err := a.Authenticate(r)
			if err != nil {
				Error(w, r, err)
				return
			}

			ctx := context.WithValue(r.Context(), ctxKeyPrincipal{}, p)
			if p.ID != "" {
				ctx = log.With(ctx, zap.String("principal_id", p.ID))
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// PrincipalFrom returns the request's principal, or anonymous.
func PrincipalFrom(ctx context.Context) authz.Principal {
	if p, ok := ctx.Value(ctxKeyPrincipal{}).(authz.Principal); ok {
		return p
	}
	return authz.Anonymous()
}
