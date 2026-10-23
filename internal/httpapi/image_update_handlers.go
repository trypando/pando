package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
)

// handleCheckImageUpdate reports whether an image app's tag names a newer
// build than the one it runs (R-143a). It writes nothing.
func (s *Server) handleCheckImageUpdate(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	check, err := s.Deployer.CheckImage(r.Context(), app)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, check)
}

// handleUpgradeImage writes a revision running the build a check found, then
// deploys it through the ordinary path (R-143a), as rollback does.
func (s *Server) handleUpgradeImage(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	p := PrincipalFrom(r.Context())
	if err := s.Authz.CheckControl(r.Context(), p, app.ID, authz.AppDeploy); err != nil {
		Error(w, r, err)
		return
	}

	var req struct {
		Digest string `json:"digest"`
	}
	key, err := decodeWithKey(r, &req)
	if err != nil {
		Error(w, r, err)
		return
	}
	// A retry replays the deploy it started (R-262), before a second revision is written.
	if s.replayed(w, r, key, "POST /apps/{id}/deployments") {
		return
	}

	// Checked before the revision is written, so a refused deploy leaves none behind.
	inFlight, err := s.Deployments.InFlight(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if inFlight {
		Error(w, r, errs.New(errs.StateInvalid, "This app is already deploying.").
			WithRemedy("Wait for the current deploy to finish, then try again."))
		return
	}

	rev, err := s.Deployer.UpgradeImage(r.Context(), app, req.Digest, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.auditApp(r, app.ID, "spec.create")

	body, _ := json.Marshal(map[string]any{"spec_revision": rev.Revision, "idempotency_key": key})
	r.Body = io.NopCloser(bytes.NewReader(body))
	s.handleDeploy(w, r)
}
