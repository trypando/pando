package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/errs"
)

// The cluster: its hosts and the tokens they join with (R-414, R-415,
// docs/design/notes-cluster.md).

// keysHandedOver is why a control-role join token needs every install verb
// (R-080a).
const keysHandedOver = "a host joining with the control role is given every key this installation has"

func (s *Server) requireCluster(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p, ok := s.requireInstall(w, r, authz.InstallClusterManage)
	if !ok {
		return p, false
	}
	if s.Hosts == nil {
		Error(w, r, errs.New(errs.Internal, "This installation's cluster is not set up, so hosts cannot be listed or joined."))
		return p, false
	}
	return p, true
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCluster(w, r); !ok {
		return
	}
	list, err := s.Hosts.Hosts(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"hosts": list, "this_host": s.Hosts.ThisHost(list)})
}

func (s *Server) handleRemoveHost(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	removed, err := s.Hosts.Remove(r.Context(), chi.URLParam(r, "hostID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	h := removed.Host
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "cluster.host.remove", TargetKind: "host", TargetID: h.ID,
		Detail: map[string]any{"name": h.Name, "roles": h.Roles},
	})
	JSON(w, http.StatusOK, removed)
}

func (s *Server) handleListJoinTokens(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireCluster(w, r); !ok {
		return
	}
	list, err := s.Hosts.Tokens(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"tokens": list})
}

func (s *Server) handleCreateJoinToken(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	var req struct {
		Roles        []string `json:"roles"`
		MaxUses      int      `json:"max_uses"`
		ExpiresHours int      `json:"expires_hours"`
		Address      string   `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	roles := req.Roles
	if len(roles) == 0 {
		var err error
		if roles, err = s.Hosts.DefaultRoles(r.Context()); err != nil {
			Error(w, r, err)
			return
		}
	}
	roles, err := hosts.NormalizeRoles(roles)
	if err != nil {
		Error(w, r, err)
		return
	}
	if hosts.ControlRoleNeedsEveryVerb(roles) {
		if err := s.Authz.CheckHoldsEveryInstallVerb(r.Context(), p, "create a join token for the control role", keysHandedOver); err != nil {
			Error(w, r, err)
			return
		}
	}

	minted, err := s.Hosts.Mint(r.Context(), hosts.MintRequest{
		Roles: roles, MaxUses: req.MaxUses, ExpiresIn: time.Duration(req.ExpiresHours) * time.Hour,
		Address: req.Address, CreatedBy: p.ID,
	})
	if err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "cluster.join_token.create", TargetKind: "join_token", TargetID: minted.Token.ID,
		Detail: map[string]any{"roles": minted.Token.Roles, "max_uses": minted.Token.MaxUses, "expires_at": minted.Token.ExpiresAt},
	})

	// The token, exactly once. Revealed deliberately: secret.Value redacts
	// itself in every marshaler (R-194).
	JSON(w, http.StatusCreated, map[string]any{
		"token": minted.Token,
		"join":  minted.Join.Reveal(),
		"note":  "This is the only time Pando will show this. Use it on the host that is joining.",
	})
}

func (s *Server) handleRevokeJoinToken(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireCluster(w, r)
	if !ok {
		return
	}
	t, err := s.Hosts.Revoke(r.Context(), chi.URLParam(r, "tokenID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "cluster.join_token.revoke", TargetKind: "join_token", TargetID: t.ID,
	})
	w.WriteHeader(http.StatusNoContent)
}
