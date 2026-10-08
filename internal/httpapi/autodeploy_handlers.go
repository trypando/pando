package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/autodeploy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Automatic deploys (R-141, R-142). Every decision is in autodeploy.Service;
// these read the request, check the verb, and write the answer.

// webhookBodyLimit bounds a webhook's body. A push of many commits is the
// largest delivery a git host sends, and well under this.
const webhookBodyLimit = 10 << 20

func (s *Server) handleGetAutoDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	v, err := s.AutoDeploy.Get(r.Context(), app)
	if err != nil {
		Error(w, r, err)
		return
	}
	v.WebhookURL = s.webhookURL(r, app.ID)
	JSON(w, http.StatusOK, v)
}

func (s *Server) handleSetAutoDeploy(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	var req spec.AutoDeploy
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The automatic deploy settings could not be read.").
			WithRemedy(`Send JSON such as {"enabled": true, "trigger": "branch_updated", "branch": "main"} or {"enabled": true, "trigger": "release_tagged", "tag_pattern": "v*"}.`))
		return
	}
	saved, err := s.AutoDeploy.Set(r.Context(), PrincipalFrom(r.Context()), app, req)
	if err != nil {
		Error(w, r, err)
		return
	}
	status := http.StatusOK
	if saved.Changed {
		status = http.StatusCreated
	}
	JSON(w, status, saved)
}

func (s *Server) handleRotateWebhookSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	key, err := s.AutoDeploy.RotateSecret(r.Context(), PrincipalFrom(r.Context()), app)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusCreated, map[string]any{
		"webhook_url":    s.webhookURL(r, app.ID),
		"webhook_secret": key,
	})
}

func (s *Server) handleRemoveWebhookSecret(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	if err := s.AutoDeploy.RemoveSecret(r.Context(), PrincipalFrom(r.Context()), app); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

// handleAutoDeployWebhook takes a webhook from a git host (R-142). Public: the
// git host has no Pando credential. The signature, against the app's own
// secret, is the credential, and the service checks it before anything else.
func (s *Server) handleAutoDeployWebhook(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, webhookBodyLimit))
	if err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "This webhook's body is larger than Pando accepts."))
		return
	}
	out, err := s.AutoDeploy.Deliver(r.Context(), chi.URLParam(r, "appID"), autodeploy.Delivery{
		Event:     r.Header.Get("X-GitHub-Event"),
		Signature: r.Header.Get("X-Hub-Signature-256"),
		Body:      body,
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	status := http.StatusOK
	if out.Checking {
		status = http.StatusAccepted
	}
	JSON(w, status, out)
}

// webhookURL is the address a git host sends an app's webhooks to: the
// configured external URL, or the one this request came in on.
func (s *Server) webhookURL(r *http.Request, appID string) string {
	base := s.origin(r).Scheme + "://" + r.Host
	if s.ExternalURL != nil && s.ExternalURL.Host != "" {
		base = s.ExternalURL.Scheme + "://" + s.ExternalURL.Host
	}
	return base + "/api/v1/apps/" + appID + "/auto-deploy/webhook"
}
