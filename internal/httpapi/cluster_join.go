package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
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
	r.Post(hosts.RenewPath, s.handleClusterRenew)
	return r
}

// verifiedClient is the certificate the client presented, verified against
// the cluster authority by the listener, or nil.
func verifiedClient(r *http.Request) []byte {
	if r.TLS != nil && len(r.TLS.VerifiedChains) > 0 && len(r.TLS.VerifiedChains[0]) > 0 {
		return r.TLS.VerifiedChains[0][0].Raw
	}
	return nil
}

// errNoClientCert refuses a renewal made without a certificate from the
// cluster authority: it is no host's, and is not audited, so a caller with
// nothing cannot fill the audit log (R-347).
var errNoClientCert = errs.New(errs.AuthInvalid, "A renewal must present the certificate the host holds, issued by the cluster's authority.")

// errRevokedCert refuses a renewal presented with a certificate of a host
// removed from the cluster (R-415).
var errRevokedCert = errs.New(errs.AuthInvalid, "This certificate belonged to a host removed from the cluster, so it renews nothing.").
	WithRemedy("Join the host to the cluster again with a new join token.")

// Refused renewals allowed per certificate and per client address before
// renewing waits [P] (R-404 applied to R-421). A host renews about every 60
// days and tries again hourly when an answer is lost, so a handful is
// plenty; every refusal within them is audited, and none past them but one
// cluster.host.renew.limited per window (R-347, R-348).
const (
	renewCertAttempts = 5
	renewAddrAttempts = 20
	renewWindow       = 15 * time.Minute
)

// handleClusterRenew issues a joined host new certificates, to the host
// presenting the one it holds (R-421). Audited, with the host's ID. A
// removed host's certificate, which still chains to the authority, is refused
// before anything else, and audited once per window however often it is
// presented; other refusals are limited per certificate and per address
// (R-347, R-404).
func (s *Server) handleClusterRenew(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	der := verifiedClient(r)
	if der == nil {
		Error(w, r, errNoClientCert)
		return
	}
	pin := hostagent.Fingerprint256(der)
	revoked, err := s.Hosts.CertRevoked(r.Context(), der)
	if err != nil {
		Error(w, r, err)
		return
	}
	if revoked {
		s.auditOnce(r, "cluster-renew|revoked|"+pin, renewWindow, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.host.renew.denied", TargetKind: "host",
			Detail: map[string]any{"code": string(errs.AuthInvalid), "reason": "revoked", "cert_sha256": pin},
		})
		Error(w, r, errRevokedCert)
		return
	}
	const what = "Too many refused renewals with this certificate or from this network address."
	reservation, ok := s.reserveLimited(w, r, func(over state.AttemptLimit) {
		s.auditOnce(r, over.Key+"|limited", renewWindow, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.host.renew.limited", TargetKind: "host",
			Detail: map[string]any{"cert_sha256": pin, "window_seconds": int(renewWindow.Seconds())},
		})
	},
		limited{state.AttemptLimit{Key: "cluster-renew|cert|" + pin, Max: renewCertAttempts, Window: renewWindow}, what},
		limited{state.AttemptLimit{Key: "cluster-renew|addr|" + limitAddr(r), Max: renewAddrAttempts, Window: renewWindow}, what},
	)
	if !ok {
		return
	}
	var req hosts.WireRenewRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJoinBody)).Decode(&req); err != nil {
		err = errs.New(errs.ValidInvalid, "The renewal request could not be read.")
		s.settleRefusal(r, reservation, err)
		Error(w, r, err)
		return
	}
	res, err := s.Hosts.Renew(r.Context(), der, []byte(req.CSR))
	s.settleRefusal(r, reservation, err)
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.host.renew.denied", TargetKind: "host",
			Detail: map[string]any{"code": string(errs.CodeOf(err))},
		})
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: res.Host.ID,
		Action: "cluster.host.renew", TargetKind: "host", TargetID: res.Host.ID,
		Detail: map[string]any{"name": res.Host.Name, "cert_sha256": res.Host.CertSHA256},
	})
	// The probe certificate's key, only over this connection (R-194).
	JSON(w, http.StatusCreated, res.Wire())
}

// handleClusterPeers lists the cluster's hosts to a host that has joined it,
// which presents the certificate it joined with (R-421). The listener checks
// the certificate against the authority; Peers checks it is a host's.
func (s *Server) handleClusterPeers(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	list, err := s.Hosts.PeerList(r.Context(), verifiedClient(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, list)
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

// Refused joins allowed from one client address before joining waits, as
// wrong passwords are limited (R-404 applied to R-415) [P]. The listener
// takes no session, so this is what keeps a caller without a token from
// guessing at one: joinAddrAttempts counts refusals that tested a secret.
// joinAddrUnusable counts the rest, refusals that tested none (a name, an
// address or a version Pando cannot use), and is higher, so that a host
// fumbling its own request does not spend the address's guesses while a
// caller still cannot fill the audit log, which keeps every refusal for at
// least three months (R-347, R-348).
const (
	joinAddrAttempts = 20
	joinAddrUnusable = 200
	joinWindow       = 15 * time.Minute
)

// joinUnusableKey counts an address's joins refused without testing a secret.
func joinUnusableKey(addr string) string { return state.ClusterJoinAddrKey(addr) + "|unusable" }

// reserveJoin counts a join from this client address before it is tried,
// and refuses it over either limit (R-404, R-415). The address's first
// refusal for the limit in a window is audited as cluster.join.limited, and
// no other: a caller over the limit adds no row however often it asks
// (R-347, R-348).
func (s *Server) reserveJoin(w http.ResponseWriter, r *http.Request) (state.Reservation, bool) {
	const what = "Too many refused joins from this network address."
	addr := limitAddr(r)
	return s.reserveLimited(w, r, func(state.AttemptLimit) {
		s.auditOnce(r, state.ClusterJoinAddrKey(addr)+"|limited", joinWindow, audit.Event{
			PrincipalKind: audit.KindAnonymous, Action: "cluster.join.limited", TargetKind: "host",
			Detail: map[string]any{"window_seconds": int(joinWindow.Seconds())},
		})
	},
		limited{state.AttemptLimit{Key: state.ClusterJoinAddrKey(addr), Max: joinAddrAttempts, Window: joinWindow}, what},
		limited{state.AttemptLimit{Key: joinUnusableKey(addr), Max: joinAddrUnusable, Window: joinWindow}, what},
	)
}

// auditOnce writes e the first time it is asked under key in a window, and
// never again in it, counted in the store every replica shares: what a
// caller that is refused again and again adds to the audit log, which keeps
// every row for three months, is one row a window (R-347, R-348). A count
// that cannot be read writes nothing; the refusal it records is still
// answered.
func (s *Server) auditOnce(r *http.Request, key string, window time.Duration, e audit.Event) {
	res, err := s.failedAttempts().Reserve(r.Context(), state.AttemptLimit{Key: key, Max: 1, Window: window})
	if err != nil || res.Refused() {
		return
	}
	_ = s.failedAttempts().Fail(context.WithoutCancel(r.Context()), res)
	s.audit(r, e)
}

// settleRefusal ends a reservation that counts every refusal (R-404): any
// refusal Pando decided counts, and a success, or a refusal it could not
// decide, does not.
func (s *Server) settleRefusal(r *http.Request, res state.Reservation, err error) {
	ctx := context.WithoutCancel(r.Context())
	if err == nil || errs.CodeOf(err) == errs.Internal {
		_ = s.failedAttempts().Release(ctx, res)
		return
	}
	_ = s.failedAttempts().Fail(ctx, res)
}

// settleJoin ends a join's reservation (R-404). A refusal that tested a
// secret (a join token that is wrong, expired, used up or revoked,
// AUTH_TOKEN_INVALID, or a wrong setup token, AUTH_INVALID) counts against
// the address's guesses. Any other refusal, for the request's shape, the
// version or the cluster's readiness, guesses at nothing and counts only
// toward joinAddrUnusable. A join that succeeded, or one Pando could not
// decide, counts toward neither.
func (s *Server) settleJoin(r *http.Request, res state.Reservation, err error) {
	counted := ""
	switch code := errs.CodeOf(err); {
	case err == nil || code == errs.Internal:
	case code == errs.AuthTokenInvalid || code == errs.AuthInvalid:
		counted = state.ClusterJoinAddrKey(limitAddr(r))
	default:
		counted = joinUnusableKey(limitAddr(r))
	}
	var fail, release state.Reservation
	for _, h := range res.Held {
		if h.Key == counted {
			fail.Held = append(fail.Held, h)
		} else {
			release.Held = append(release.Held, h)
		}
	}
	ctx := context.WithoutCancel(r.Context())
	_ = s.failedAttempts().Fail(ctx, fail)
	_ = s.failedAttempts().Release(ctx, release)
}

// denyJoin audits a refused join, with what the caller sent kept only where
// it is bounded (hosts.JoinRequest.AuditDetail).
func (s *Server) denyJoin(r *http.Request, detail map[string]any) {
	s.audit(r, audit.Event{
		PrincipalKind: audit.KindAnonymous, Action: "cluster.join.denied", TargetKind: "host", Detail: detail,
	})
}

// handleClusterJoin admits a host with a join token. Every join and every
// refusal within the limit is audited (R-415), and refusals are limited per
// client address (reserveJoin).
func (s *Server) handleClusterJoin(w http.ResponseWriter, r *http.Request) {
	if s.Hosts == nil {
		Error(w, r, errClusterOff)
		return
	}
	reservation, ok := s.reserveJoin(w, r)
	if !ok {
		return
	}
	var req hosts.WireJoinRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJoinBody)).Decode(&req); err != nil {
		err = errs.New(errs.ValidInvalid, "The join request could not be read.")
		s.settleJoin(r, reservation, err)
		s.denyJoin(r, map[string]any{"code": string(errs.CodeOf(err))})
		Error(w, r, err)
		return
	}
	join := req.Request()
	res, err := s.Hosts.Join(r.Context(), join)
	s.settleJoin(r, reservation, err)
	if err != nil {
		detail := join.AuditDetail()
		detail["code"] = string(errs.CodeOf(err))
		s.denyJoin(r, detail)
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
	reservation, ok := s.reserveJoin(w, r)
	if !ok {
		return
	}
	var req struct {
		SetupToken string `json:"setup_token"`
		Token      string `json:"token"`
		Name       string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJoinBody)).Decode(&req); err != nil {
		err := errUnreadableBody()
		s.settleJoin(r, reservation, err)
		Error(w, r, err)
		return
	}
	joined, err := s.Joiner.SetupWithToken(r.Context(), secret.New(strings.TrimSpace(req.SetupToken)), strings.TrimSpace(req.Token), req.Name)
	s.settleJoin(r, reservation, err)
	if err != nil {
		detail := hosts.JoinRequest{Name: strings.TrimSpace(req.Name)}.AuditDetail()
		delete(detail, "version")
		detail["code"] = string(errs.CodeOf(err))
		s.denyJoin(r, detail)
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
