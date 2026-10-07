package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/assist"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
)

// handleListAIFunctions reports every AI function, which adapter handles it
// and on which model, whether it is on, and where it was assigned (R-259,
// R-271). install.view, like the adapter list it sits beside.
func (s *Server) handleListAIFunctions(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if s.AIFunctions == nil {
		Error(w, r, errs.New(errs.Internal, "AI function assignment is not set up on this installation."))
		return
	}
	functions, err := s.AIFunctions.List(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"functions": functions})
}

// handleAssignAIFunction gives one AI function to one adapter, optionally on
// a model of its own (R-259). Takes effect at once: adapters load at startup
// (R-253), but which running adapter a function goes to is a lookup, not a
// load.
//
// Refused when another adapter handles the function — each has one at a
// time, and the database says so — and when the startup configuration
// assigns it (R-271).
func (s *Server) handleAssignAIFunction(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if s.AIFunctions == nil {
		Error(w, r, errs.New(errs.Internal, "AI function assignment is not set up on this installation."))
		return
	}

	var req struct {
		AdapterID string `json:"adapter_id"`
		Model     string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send the adapter and, optionally, a model: {"adapter_id": "ai_anthropic", "model": "claude-haiku-4-5"}.`))
		return
	}

	fn := api.AIFunction(chi.URLParam(r, "function"))
	f, err := s.AIFunctions.Assign(r.Context(), fn, req.AdapterID, req.Model, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "ai.function.assign", TargetKind: "ai_function", TargetID: string(fn),
		Detail: map[string]any{"adapter": f.AdapterID, "model": f.Model},
	})
	JSON(w, http.StatusOK, f)
}

// handleUnassignAIFunction turns an AI function off. A function nobody
// handles degrades the way R-106 describes — to a question, or to Pando
// without AI — and is never an error.
func (s *Server) handleUnassignAIFunction(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if s.AIFunctions == nil {
		Error(w, r, errs.New(errs.Internal, "AI function assignment is not set up on this installation."))
		return
	}

	fn := api.AIFunction(chi.URLParam(r, "function"))
	f, err := s.AIFunctions.Unassign(r.Context(), fn)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "ai.function.unassign", TargetKind: "ai_function", TargetID: string(fn),
	})
	JSON(w, http.StatusOK, f)
}

// askBody reads {"<field>": "…"}.
func askBody(w http.ResponseWriter, r *http.Request, field, example string) (string, bool) {
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(fmt.Sprintf(`Send {"%s": "%s"}.`, field, example)))
		return "", false
	}
	return body[field], true
}

// assisted records that an AI function sent something to a provider: which
// function, adapter and model, never the content. The operator's record of
// what left the host, as R-337 is for screening.
func (s *Server) assisted(r *http.Request, p authz.Principal, fn api.AIFunction, ran assist.Ran, detail map[string]any) {
	if detail == nil {
		detail = map[string]any{}
	}
	detail["adapter"], detail["model"] = ran.AdapterID, ran.Model
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "ai." + string(fn), TargetKind: "ai_function", TargetID: string(fn), Detail: detail,
	})
}

func (s *Server) assistReady(w http.ResponseWriter, r *http.Request) bool {
	if s.Assist == nil {
		Error(w, r, errs.New(errs.AdapterUnavailable, "AI assistance is not set up on this installation."))
		return false
	}
	return true
}

// handleDraftAccess drafts a role and a group from a description (R-343).
// install.users.manage, the verb that creates them: a draft of something the
// caller could not create is not worth drafting. It proposes; POST /roles and
// POST /groups create.
func (s *Server) handleDraftAccess(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok || !s.assistReady(w, r) {
		return
	}
	// current is the draft so far, when refining one.
	var body struct {
		Description string           `json:"description"`
		Current     *api.AccessDraft `json:"current"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"description": "Release managers can deploy and restart any app"}, and "current" with the draft so far when refining one.`))
		return
	}
	out, err := s.Assist.DraftAccess(r.Context(), p, body.Description, body.Current)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.assisted(r, p, api.AIFunctionDraftAccess, out.Ran, nil)
	JSON(w, http.StatusOK, out)
}

// handleDraftPolicy proposes host policy from a description (R-344).
// install.policy.manage, the verb that saves it. A field the startup
// configuration fixes is declined with its source. It proposes; PUT /policy
// saves.
func (s *Server) handleDraftPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallPolicyManage)
	if !ok || !s.assistReady(w, r) {
		return
	}
	// proposed is the document so far, when refining a proposal.
	var body struct {
		Description string               `json:"description"`
		Proposed    *corepolicy.Document `json:"proposed"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"description": "Nobody may open a shell in an app"}, and "proposed" with the document so far when refining one.`))
		return
	}
	out, err := s.Assist.DraftPolicy(r.Context(), body.Description, body.Proposed)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.assisted(r, p, api.AIFunctionDraftPolicy, out.Ran, nil)
	JSON(w, http.StatusOK, out)
}

// handleSearchAudit turns a question into audit filters, runs them, and
// summarizes what they found (R-345). install.audit.read, the verb that reads
// the log: the adapter sees what the caller could see and nothing more.
func (s *Server) handleSearchAudit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAuditRead)
	if !ok || !s.assistReady(w, r) {
		return
	}
	text, ok := askBody(w, r, "question", "Which apps did Ben Meeker create or delete last month?")
	if !ok {
		return
	}
	out, err := s.Assist.SearchAudit(r.Context(), p, text)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.assisted(r, p, api.AIFunctionSearchAudit, out.Ran, map[string]any{"records_sent": min(out.Matched, 100)})
	JSON(w, http.StatusOK, out)
}

// handleAnswerReference answers "How can I…" from the generated reference
// (R-346). Anyone signed in, like the reference itself.
func (s *Server) handleAnswerReference(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	if !s.assistReady(w, r) {
		return
	}
	text, ok := askBody(w, r, "question", "How can I give someone access to one app?")
	if !ok {
		return
	}
	out, err := s.Assist.AnswerReference(r.Context(), text)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.assisted(r, p, api.AIFunctionAnswerReference, out.Ran, nil)
	JSON(w, http.StatusOK, out)
}
