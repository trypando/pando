package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Joining a cluster (R-415, docs/design/notes-cluster.md). Two halves: the
// cluster listener a control host serves, which a joining host connects to
// with the authority pinned, and POST /setup/join on the fresh Pando that is
// joining.

// maxJoinBody bounds a join request: a name, a certificate request and a
// token are a few kilobytes.
const maxJoinBody = 64 << 10

// ClusterRoutes is the cluster listener (server.cluster_addr), served over
// TLS from the cluster authority. It is not under /api/v1 and needs no
// session: a joining host has nothing but its token.
func (s *Server) ClusterRoutes() http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(RequestID)
	r.Use(s.ClientAddr.Middleware)
	r.Use(Logger(s.Logger))
	r.Get(hosts.AuthorityPath, s.handleClusterAuthority)
	r.Post(hosts.JoinPath, s.handleClusterJoin)
	r.Get(hosts.PeersPath, s.handleClusterPeers)
	return r
}

// handleClusterPeers lists the cluster's hosts to a host that has joined it,
// which presents the certificate it joined with (R-421). The listener checks
// the certificate against the authority; Peers checks it is a host's.
func (s *Server) handleClusterPeers(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	var der []byte
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
		der = r.TLS.VerifiedChains[0][0].Raw
	}
	peers, err := s.Hosts.Peers(r.Context(), der)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"hosts": peers})
}

// handleClusterAuthority is the authority's certificate, public: every host
// checks the others' certificates against it.
func (s *Server) handleClusterAuthority(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	w.Header().Set("Content-Type", "application/x-pem-file")
	_, _ = w.Write(s.Hosts.Authority.CertPEM())
}

var errClusterOff = errs.New(errs.Internal, "This installation's cluster is not set up, so no host can join it.")

// handleClusterJoin admits a host with a join token. Every join and every
// refusal is audited (R-415).
func (s *Server) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	var req hosts.WireJoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJoinBody)).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The join request could not be read."))
		return
	}
	res, err := s.Hosts.Join(r.Context(), req.Request())
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.join.denied", TargetKind: "host",
			Detail: map[string]any{"name": req.Name, "version": req.Version, "code": string(errs.CodeOf(err))},
		})
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindToken, PrincipalID: res.Host.JoinedWith,
		Action: "cluster.host.join", TargetKind: "host", TargetID: res.Host.ID,
		Detail: map[string]any{"name": res.Host.Name, "roles": res.Host.Roles, "version": res.Host.Version, "address": res.Host.Address},
	})
	// The keys, for a control host, only here (R-415). Revealed deliberately:
	// secret.Value redacts itself in every marshaler (R-194).
	JSON(w, http.StatusCreated, res.Wire())
}

// handlePostSetupJoin joins a fresh Pando to a cluster (R-415). Public, like
// POST /setup, needing the same setup token (R-046), and refused the same way
// once this Pando has an account or an app. Answers before Pando restarts
// into the cluster.
func (s *Server) handlePostSetupJoin(w http.ResponseWriter, r *http.Request) {
	if refuseFormBody(w, r) {
		return
	}
	if s.Joiner == nil {
		Error(w, r, errs.New(errs.Internal, "This Pando cannot join a cluster: joining is not set up."))
		return
	}
	var req struct {
		SetupToken string `json:"setup_token"`
		Token      string `json:"token"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJoinBody)).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	joined, err := s.Joiner.SetupWithToken(r.Context(), secret.New(strings.TrimSpace(req.SetupToken)), strings.TrimSpace(req.Token), req.Name)
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.join.denied", TargetKind: "host",
			Detail: map[string]any{"name": req.Name, "code": string(errs.CodeOf(err))},
		})
		Error(w, r, err)
		return
	}
	// In this Pando's own log, which a control host leaves behind for the
	// cluster's when it restarts; the cluster records cluster.host.join.
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindAnonymous, Action: "cluster.join", TargetKind: "host", TargetID: joined.HostID,
		Detail: map[string]any{"name": joined.Name, "roles": joined.Roles, "cluster": joined.Cluster},
	})
	JSON(w, http.StatusAccepted, map[string]any{"roles": joined.Roles, "status": "joined", "host_id": joined.HostID, "name": joined.Name})
}
