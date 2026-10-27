package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/errs"
)

// handleMarkHostLost is a person saying a host is gone (R-421): the host is
// marked lost and its apps' standbys started. In a cluster under three hosts
// it is the only way anything fails over. The service audits and decides;
// this handler only authorizes.
func (s *Server) handleMarkHostLost(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	if s.HostLoss == nil {
		Error(w, r, errs.New(errs.Internal, "This installation's cluster is not set up, so no host can be marked lost."))
		return
	}
	out, err := s.HostLoss.MarkLost(r.Context(), chi.URLParam(r, "hostID"), liveness.Actor{
		Kind: audit.PrincipalKind(p.Kind), ID: p.ID, OnBehalfOf: p.UserID, RequestID: RequestIDFrom(r.Context()),
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, out)
}
