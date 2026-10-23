package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Tokens (design 04 §2.7, R-058–R-063).
//
// A token is how the CLI and an agent authenticate (R-262). These are
// self-service: a token acts as its owner and holds nothing the owner does not,
// so issuing one grants no new power — it creates a second credential for
// power already held. Requiring an administrator would make agents an
// administrative feature, which R-262 is explicitly not.
//
// What it *does* create is a credential that outlives a session and can be
// stolen separately, which is why they are listed, revocable, and expire.

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous || p.UserID == "" {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}

	tokens, err := s.Tokens.ListForUser(r.Context(), p.UserID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

// handleCreateToken mints a delegated token for the caller (R-058).
//
// Delegated only. An account token is its own principal with its own grants
// (R-060) — it belongs to whoever administers the install rather than to a
// person, so minting one is not self-service and does not belong on this route.
func (s *Server) handleCreateToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous || p.UserID == "" {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}

	// A token cannot mint a token. Otherwise a stolen one renews itself
	// indefinitely and revoking the original achieves nothing — the thief
	// simply holds a newer one that Pando has no reason to distrust.
	if p.Kind == authz.KindToken {
		Error(w, r, errs.New(errs.PermDenied, "A token cannot create another token.").
			WithRemedy("Sign in to create one."))
		return
	}

	var req struct {
		Name        string `json:"name"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	if req.Name == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "A token needs a name.").
			WithRemedy("Name it after where it will be used, for example \"laptop CLI\"."))
		return
	}

	expires, err := s.tokenExpiry(r, req.ExpiresDays)
	if err != nil {
		Error(w, r, err)
		return
	}

	issued, err := s.Tokens.Create(r.Context(), state.TokenDelegated, req.Name, p.UserID, p.ID, expires)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "token.create", TargetKind: "token", TargetID: issued.Token.ID,
		Detail: map[string]any{"name": req.Name},
	})

	// The secret, exactly once (R-063). Revealed deliberately: secret.Value
	// redacts itself in every marshaler, so this is the one place that has to
	// opt in, and there is no path where it leaks by someone forgetting.
	JSON(w, http.StatusCreated, map[string]any{
		"token":  issued.Token,
		"secret": issued.Secret.Reveal(),
		"note":   "This is the only time Pando will show this. Store it now.",
	})
}

// Service tokens: account-level, its own principal (R-060, R-061).
//
// Not self-service, and that asymmetry is the whole point. A delegated token
// holds what its owner holds and dies with them (R-058, R-059), so issuing one
// creates no new power. An account token is a principal in its own right: it
// appears in grants under its own ID, in the audit log under its own name, and
// it survives the person who created it. That is a new subject on the
// installation, which is administration.
//
// The verb is install.tokens.manage (R-080). It used to be install.users.manage,
// on the reasoning that a service token acts like an account; but issuing a
// credential for an automation and managing people are different trusts, and an
// installation may want whoever runs its CI to hold the first without the
// second. The Administrator holds both.

func (s *Server) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if err := s.Authz.CheckInstall(r.Context(), p, authz.InstallTokensManage); err != nil {
		Error(w, r, err)
		return
	}

	tokens, err := s.Tokens.ListAccounts(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) handleCreateServiceToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if err := s.Authz.CheckInstall(r.Context(), p, authz.InstallTokensManage); err != nil {
		Error(w, r, err)
		return
	}

	// The same rule as a delegated token, for the same reason: a stolen token
	// that can mint tokens renews itself forever, and revoking the original
	// achieves nothing.
	if p.Kind == authz.KindToken {
		Error(w, r, errs.New(errs.PermDenied, "A token cannot create another token.").
			WithRemedy("Sign in to create one."))
		return
	}

	var req struct {
		Name        string `json:"name"`
		ExpiresDays int    `json:"expires_days"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	if req.Name == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "A service token needs a name.").
			WithRemedy("Name it after what will use it, for example \"CI deploys\". The name is what appears in the audit log."))
		return
	}

	expires, err := s.tokenExpiry(r, req.ExpiresDays)
	if err != nil {
		Error(w, r, err)
		return
	}

	issued, err := s.Tokens.Create(r.Context(), state.TokenAccount, req.Name, "", p.ID, expires)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "token.create", TargetKind: "token", TargetID: issued.Token.ID,
		Detail: map[string]any{"name": req.Name, "kind": state.TokenAccount},
	})

	// R-063, and R-060's consequence stated where it is acted on: this token is
	// a principal that currently holds nothing. It reaches an app when somebody
	// shares one with it.
	JSON(w, http.StatusCreated, map[string]any{
		"token":  issued.Token,
		"secret": issued.Secret.Reveal(),
		"note":   "This is the only time Pando will show this. Store it now.",
	})
}

// handleRevokeToken revokes a token.
//
// Your own; a service token with install.tokens.manage; anyone else's with
// install.users.manage — the same self-or-verb shape as the account endpoints,
// and for the same reason: revoking someone else's credential is
// administration.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}

	tokenID := chi.URLParam(r, "tokenID")
	owner, found, err := s.Tokens.Owner(r.Context(), tokenID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no token with that ID."))
		return
	}

	// A service token has no owner: revoking one is managing service tokens.
	// Somebody else's own token is somebody else's credential: that is
	// managing accounts. Your own needs nothing.
	switch {
	case owner == "":
		if err := s.Authz.CheckInstall(r.Context(), p, authz.InstallTokensManage); err != nil {
			Error(w, r, err)
			return
		}
	case owner != p.UserID:
		if err := s.Authz.CheckInstall(r.Context(), p, authz.InstallUsersManage); err != nil {
			Error(w, r, err)
			return
		}
		// Somebody's credential, and with it whatever they hold (R-080a).
		if err := s.Authz.CheckMayChangeAccount(r.Context(), p, owner, "revoke this account's token"); err != nil {
			Error(w, r, err)
			return
		}
	}

	if err := s.Tokens.Revoke(r.Context(), tokenID); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "token.revoke", TargetKind: "token", TargetID: tokenID,
	})
	JSON(w, http.StatusNoContent, nil)
}

// tokenExpiry is when a token asked to live days days expires, 0 meaning no
// expiry asked for. Host policy may cap how long a token lives (R-061, R-272):
// a token asked for without an expiry, or with a longer one, gets the cap. The
// clamp is here rather than in the caller, because the caller is the one
// being limited; the token's expires_at in the answer says what it got.
func (s *Server) tokenExpiry(r *http.Request, days int) (*time.Time, error) {
	now := time.Now().UTC()
	var expires *time.Time
	if days > 0 {
		at := now.AddDate(0, 0, days)
		expires = &at
	}
	if s.PolicyStore == nil {
		return expires, nil
	}
	doc, err := s.PolicyStore.Load(r.Context())
	if err != nil {
		return nil, err
	}
	limit := doc.TokenExpiry(now)
	if limit != nil && (expires == nil || expires.After(*limit)) {
		return limit, nil
	}
	return expires, nil
}
