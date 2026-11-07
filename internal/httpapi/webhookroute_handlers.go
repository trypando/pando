package httpapi

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Webhook routes (R-425, R-432). Every decision is in webhookroute.Service;
// these read the request, check the verb, and write the answer.

func (s *Server) handleListWebhookRoutes(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	view, err := s.WebhookRoutes.List(r.Context(), app)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, view)
}

func (s *Server) handleAddWebhookRoute(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	var req struct {
		spec.WebhookRoute
		Secret string `json:"secret,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The webhook route could not be read.").
			WithRemedy(`Send JSON such as {"name": "noodle", "method": "POST", "path": "/api/v1/webhook/noodle", "verification": {"kind": "hmac_sha256", "header": "X-Hub-Signature-256", "prefix": "sha256="}}.`))
		return
	}
	saved, err := s.WebhookRoutes.Add(r.Context(), PrincipalFrom(r.Context()), app, req.WebhookRoute, req.Secret)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusCreated, saved)
}

func (s *Server) handleRemoveWebhookRoute(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	revision, err := s.WebhookRoutes.Remove(r.Context(), PrincipalFrom(r.Context()), app, chi.URLParam(r, "name"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"revision": revision})
}

func (s *Server) handleSetWebhookRouteSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSecretsWrite)
	if !ok {
		return
	}
	var req struct {
		Secret string `json:"secret,omitempty"`
	}
	// An empty body generates one.
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			Error(w, r, errs.New(errs.ValidInvalid, "The webhook route's secret could not be read.").
				WithRemedy(`Send {"secret": "…"} with the secret the provider issued, or no body for Pando to generate one.`))
			return
		}
	}
	key, err := s.WebhookRoutes.SetSecret(r.Context(), PrincipalFrom(r.Context()), app, chi.URLParam(r, "name"), req.Secret)
	if err != nil {
		Error(w, r, err)
		return
	}
	if key == nil {
		JSON(w, http.StatusNoContent, nil)
		return
	}
	JSON(w, http.StatusCreated, map[string]any{"secret": key})
}
