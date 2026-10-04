package httpapi

import (
	"net/http"
	"strings"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/update"
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
