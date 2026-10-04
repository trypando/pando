package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/errs"
)

// VersionHeader carries the server's version to a signed-in caller (R-353).
const VersionHeader = "Pando-Version"

// versionHeader sends the version on every API response to someone signed
// in. Not to anyone else: which version an install runs says which
// advisories apply to it.
func (s *Server) versionHeader(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.Version != "" && PrincipalFrom(r.Context()).Kind != authz.KindAnonymous {
			w.Header().Set(VersionHeader, strings.TrimPrefix(s.Version, "v"))
		}
		next.ServeHTTP(w, r)
	})
}

// handleGetUpdates is GET /api/v1/updates (R-351).
func (s *Server) handleGetUpdates(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	if s.Updates == nil {
		JSON(w, http.StatusOK, update.Status{
			Current:  strings.TrimPrefix(s.Version, "v"),
			Releases: []update.Release{},
		})
		return
	}
	st, err := s.Updates.Status(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, st)
}

func (s *Server) upgradesReady(w http.ResponseWriter, r *http.Request) bool {
	if s.Upgrades == nil {
		Error(w, r, errs.New(errs.Internal, "In-place upgrades are not set up on this installation."))
		return false
	}
	return true
}

// handleGetUpgradePlan is GET /api/v1/upgrade?version= (R-355, R-360).
func (s *Server) handleGetUpgradePlan(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok || !s.upgradesReady(w, r) {
		return
	}
	version := r.URL.Query().Get("version")
	if version == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "Say which version to plan an upgrade to.").
			WithRemedy("Add ?version= with a version from GET /updates, such as 0.4.0."))
		return
	}
	plan, err := s.Upgrades.PlanFor(r.Context(), version)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, plan)
}

// handleGetLastUpgrade is GET /api/v1/upgrade/last (R-356).
func (s *Server) handleGetLastUpgrade(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok || !s.upgradesReady(w, r) {
		return
	}
	last, err := s.Upgrades.Last(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"upgrade": last})
}

// handleStartUpgrade is POST /api/v1/upgrade (R-356). Accepted rather than
// done: the helper stops this process moments after the answer is sent.
func (s *Server) handleStartUpgrade(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUpgrade)
	if !ok || !s.upgradesReady(w, r) {
		return
	}
	var req upgrade.Request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"version": "0.4.0", "passphrase": "…"} or {"version": "0.4.0", "skip_backup": true}.`))
		return
	}
	o, err := s.Upgrades.Start(r.Context(), p, req)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusAccepted, o)
}
