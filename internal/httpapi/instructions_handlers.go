package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// The instructions for agents (R-424): Pando's built-in guidance and the
// company's own, which MCP sends every agent when it connects.
//
// Reading needs only a signed-in caller. Every agent is meant to follow them,
// and an agent acts for a person who may hold no install verb at all, so
// gating the read would keep the rules from exactly the agents they are for.
// Writing is install.policy.manage: these are rules agents work under, like
// host policy's, and the same people set them.

// handleGetInstructions answers GET /instructions for anyone signed in.
func (s *Server) handleGetInstructions(w http.ResponseWriter, r *http.Request) {
	if PrincipalFrom(r.Context()).Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	if !s.instructionsConfigured(w, r) {
		return
	}
	d, err := s.Instructions.Get(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, d)
}

// handlePutInstructions replaces the company's part, and audits it.
func (s *Server) handlePutInstructions(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallPolicyManage)
	if !ok || !s.instructionsConfigured(w, r) {
		return
	}
	var body struct {
		Company *string `json:"company"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Company == nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The instructions could not be read.").
			WithRemedy(`Send a JSON object with the company's instructions as text, such as {"company": "Use Postgres for every app's data."}. An empty string clears them.`))
		return
	}

	before, err := s.Instructions.Get(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	d, err := s.Instructions.Set(r.Context(), *body.Company, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	// Lengths rather than the text: the text is readable from the API, and
	// up to 32 KB in every event would crowd the log.
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "instructions.update",
		TargetKind:    "instructions",
		TargetID:      "company",
		Detail:        map[string]any{"bytes_before": len(before.Company), "bytes_after": len(d.Company)},
	})
	JSON(w, http.StatusOK, d)
}

// instructionsConfigured refuses when the installation keeps no
// instructions, as a server built without the store does.
func (s *Server) instructionsConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.Instructions != nil {
		return true
	}
	Error(w, r, errs.New(errs.AdapterUnavailable, "This installation does not keep instructions for agents."))
	return false
}
