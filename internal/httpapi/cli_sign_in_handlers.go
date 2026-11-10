package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Signing the CLI in through the browser (R-406, design 06 §3.5). The logic is
// clisignin's; these check who is asking, and audit.

// handleApproveCLISignIn is the console's Approve button: the signed-in person
// lets the CLI that opened the page sign in as them.
//
// A session only. A token approving would be a token minting a token, which
// POST /tokens refuses for the reason it gives there. Cross-site posts are
// already refused by sameOriginWrites, and a session that must set up its
// second factor by twoFactorGate, so neither a page elsewhere nor a half
// signed-in browser can approve.
func (s *Server) handleApproveCLISignIn(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous || p.UserID == "" {
		Error(w, r, errs.New(errs.AuthRequired, "Sign in to Pando in this browser to sign in the CLI."))
		return
	}
	if p.Kind == authz.KindToken {
		Error(w, r, errs.New(errs.PermDenied, "A token cannot sign in the CLI.").
			WithRemedy("Open the address pando login printed in a browser signed in to Pando."))
		return
	}
	if s.CLISignIn == nil {
		Error(w, r, errs.New(errs.NotFound, "This installation does not sign the CLI in through the browser.").
			WithRemedy("Run pando login --username <your username> to sign in with a password."))
		return
	}

	var req clisignin.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	approved, err := s.CLISignIn.Approve(r.Context(), p.UserID, p.ID, req)
	if err != nil {
		Error(w, r, err)
		return
	}

	delivery := "paste"
	if approved.RedirectURL != "" {
		delivery = "loopback"
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "cli_sign_in.approve", TargetKind: "user", TargetID: p.UserID,
		Detail: map[string]any{"name": req.Name, "delivery": delivery, "kind": kindOr(req.Kind)},
	})

	// The code, once. Revealed deliberately, as a token's secret is: it is
	// worth nothing without the CLI's verifier, but it is still not for a
	// cache to keep.
	w.Header().Set("Cache-Control", "no-store")
	JSON(w, http.StatusCreated, map[string]any{
		"code":         approved.Code.Reveal(),
		"redirect_url": approved.RedirectURL,
		"expires_at":   approved.ExpiresAt,
	})
}

// handleExchangeCLISignIn is the CLI trading its code and verifier for a
// token. Public: the CLI has no credential yet, and the verifier is what
// proves it is the one that asked.
func (s *Server) handleExchangeCLISignIn(w http.ResponseWriter, r *http.Request) {
	if s.CLISignIn == nil {
		Error(w, r, errs.New(errs.NotFound, "This installation does not sign the CLI in through the browser.").
			WithRemedy("Run pando login --username <your username> to sign in with a password."))
		return
	}
	var req struct {
		Code         string `json:"code"`
		CodeVerifier string `json:"code_verifier"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	got, err := s.CLISignIn.Exchange(r.Context(), req.Code, req.CodeVerifier)
	if err != nil {
		// Only a code that named an approval is recorded, against the account
		// that approved it. A guess at a code is refused without an audit
		// row: this route is public, and each row would otherwise be one
		// anybody can add.
		if e := errs.As(err); e != nil && e.Code == errs.AuthInvalid && got.UserID != "" {
			s.audit(r, audit.Event{
				PrincipalKind: audit.KindAnonymous, Action: "cli_sign_in.denied",
				TargetKind: "user", TargetID: got.UserID,
				Detail: map[string]any{"name": got.Name, "approved_by": got.ApprovedBy, "kind": got.Kind},
			})
		}
		Error(w, r, err)
		return
	}

	// Made by the person who approved it, as POST /tokens and POST
	// /tokens/service record it, from the address the CLI connected from.
	detail := map[string]any{"name": got.Name, "via": "cli_sign_in", "approved_by": got.ApprovedBy}
	if got.Kind == clisignin.KindService {
		detail["kind"] = state.TokenAccount
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: got.UserID, OnBehalfOf: got.UserID,
		Action: "token.create", TargetKind: "token", TargetID: got.Issued.Token.ID,
		Detail: detail,
	})

	w.Header().Set("Cache-Control", "no-store")
	JSON(w, http.StatusCreated, map[string]any{
		"token":    got.Issued.Token,
		"secret":   got.Issued.Secret.Reveal(),
		"username": got.Username,
	})
}

// kindOr is the kind of token an approval asks for, as the audit log names it.
func kindOr(kind string) string {
	if kind == "" {
		return clisignin.KindDelegated
	}
	return kind
}
