package httpapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/trypando/pando/internal/core/authz"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// hostPolicy is the host policy document in force, or the shipped default on
// a server with no policy store.
func (s *Server) hostPolicy(ctx context.Context) (corepolicy.Document, error) {
	if s.PolicyStore == nil {
		return corepolicy.Default(), nil
	}
	return s.PolicyStore.Load(ctx)
}

// pinnedSpec is the app's pinned spec, nil when it has none.
func (s *Server) pinnedSpec(ctx context.Context, app state.App) (*spec.AppSpec, error) {
	if app.PinnedSpecID == "" {
		return nil, nil
	}
	rev, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	return rev.Body, nil
}

// gateSpec asks specgate whether the caller may write next over the app's
// pinned spec (R-158, R-182 – R-184, R-241). Every handler that writes a
// revision from content the caller supplied calls it, after validation.
func (s *Server) gateSpec(r *http.Request, app state.App, pinned, next *spec.AppSpec) (specgate.Result, error) {
	return s.gateSpecWith(r, s.Authz, app, pinned, next)
}

// gateSpecWith is gateSpec asking a given authorizer: specgate.Quiet for a
// dry run, so a refusal is reported and not audited.
func (s *Server) gateSpecWith(r *http.Request, az specgate.Authorizer, app state.App, pinned, next *spec.AppSpec) (specgate.Result, error) {
	doc, err := s.hostPolicy(r.Context())
	if err != nil {
		return specgate.Result{}, err
	}
	latest, err := s.latestRevisionSpec(r, app)
	if err != nil {
		return specgate.Result{}, err
	}
	var host spec.Resources
	if s.Defaults != nil {
		host = s.Defaults.Defaults(r.Context()).Resources
	}
	return specgate.Check(r.Context(), az, PrincipalFrom(r.Context()), specgate.Change{
		AppID: app.ID, Policy: doc, Pinned: pinned, Next: next, Latest: latest, HostResources: host,
	})
}

// handleGetEgress is an app's egress rules and the installation's they are
// edited against (R-182, R-188). app.view, so an owner without install.view
// can see the rules their app starts from.
//
// The pinned spec by default. ?revision=N, or ?revision=latest for the newest,
// reads another: what a saved change that is not yet deployed would run with,
// and what it loosens.
func (s *Server) handleGetEgress(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	doc, err := s.hostPolicy(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	rev, found, err := s.egressRevision(r, app)
	if err != nil {
		Error(w, r, err)
		return
	}

	body := map[string]any{
		"install":   doc.InstallEgress(),
		"revision":  nil,
		"effective": nil,
		"spec":      nil,
	}
	if found && rev.Body != nil {
		// Read in today's terms, so a spec written before issue #79 is shown
		// the way it is enforced.
		own := rev.Body.Egress
		own.Normalize()
		body["revision"] = rev.Revision
		body["effective"] = doc.EgressFor(rev.Body.Egress)
		body["spec"] = own
	}
	JSON(w, http.StatusOK, body)
}

// egressRevision is the revision GET /egress reads: the pinned one, or the
// one ?revision names. An app with nothing pinned is an empty answer; a
// revision somebody asked for that does not exist is a refusal.
func (s *Server) egressRevision(r *http.Request, app state.App) (state.Revision, bool, error) {
	raw := r.URL.Query().Get("revision")
	switch raw {
	case "":
		if app.PinnedSpecID == "" {
			return state.Revision{}, false, nil
		}
		return s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	case "latest":
		revs, err := s.Apps.ListRevisions(r.Context(), app.ID)
		if err != nil || len(revs) == 0 {
			return state.Revision{}, false, err
		}
		// Newest first, and listed without bodies.
		return s.Apps.RevisionByID(r.Context(), revs[0].ID)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return state.Revision{}, false, errs.Newf(errs.ValidInvalid,
			"%q is not a revision of this app's spec.", raw).
			WithRemedy("Use a revision number such as 3, or latest for the newest. Leave revision out for the pinned spec.")
	}
	rev, found, err := s.Apps.RevisionByNumber(r.Context(), app.ID, n)
	if err == nil && !found {
		err = errs.Newf(errs.NotFound, "This app's spec has no revision %d.", n).
			WithRemedy("List the app's revisions with GET /apps/{id}/specs, or leave revision out for the pinned spec.")
	}
	return rev, found, err
}

// writeDryRun is the answer to POST /apps/{id}/specs?dry_run=true for a spec
// a save would accept: the egress rules it would run with, what it newly
// loosens, and whether deploying it would need approval and why (R-154,
// R-188). Measured as a deploy would measure it, against what the app last
// ran, so the console's preview is the server's decision.
func (s *Server) writeDryRun(w http.ResponseWriter, r *http.Request, app state.App, next *spec.AppSpec, gated specgate.Result) {
	doc, err := s.hostPolicy(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}

	reasons := []state.ApprovalReason{}
	if s.Approvals != nil {
		needed, err := s.Approvals.NeededFor(r.Context(), app, next)
		if err != nil {
			Error(w, r, err)
			return
		}
		for _, reason := range needed {
			reasons = append(reasons, state.ApprovalReason{Reason: string(reason), Message: reason.Message()})
		}
	}

	loosenings := gated.Loosenings
	if loosenings == nil {
		loosenings = []corepolicy.Loosening{}
	}
	JSON(w, http.StatusOK, map[string]any{
		"dry_run":        true,
		"egress":         doc.EgressFor(next.Egress),
		"egress_changed": gated.EgressChanged,
		"new_loosenings": loosenings,
		"approval":       map[string]any{"required": len(reasons) > 0, "reasons": reasons},
	})
}
