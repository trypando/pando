package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/sourceconn"
	"github.com/trypando/pando/internal/errs"
)

// Source connections (R-091, issue #127). A connection is configured like any
// adapter, with POST /adapters and category "source"; these routes are what
// is particular to one: listing them for whoever adds apps, picking a
// repository through one, authorizing one with OAuth, and disconnecting it.
//
// Reading connections and the repositories they reach is behind app.create:
// it is what adding an app needs, and a connection's list of repositories
// says nothing about any app on this installation (R-080). Changing them is
// install.adapters.manage, like every adapter.

// sourcesPage is the console screen a browser authorization returns to.
const sourcesPage = "/admin/sources"

func (s *Server) sourcesConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.SourceConnections == nil {
		Error(w, r, errs.New(errs.StateInvalid, "This installation cannot use source connections."))
		return false
	}
	return true
}

func (s *Server) handleListSources(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.AppCreate); !ok {
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	all, err := s.SourceConnections.List(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}
	if all == nil {
		all = []sourceconn.Connection{}
	}
	JSON(w, http.StatusOK, map[string]any{"sources": all})
}

func (s *Server) handleListSourceRepositories(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.AppCreate); !ok {
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	repos, err := s.SourceConnections.ListRepositories(r.Context(), chi.URLParam(r, "sourceID"),
		api.ListRepositoriesRequest{Query: r.URL.Query().Get("q"), Limit: limit})
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"repositories": repos})
}

func (s *Server) handleListSourceBranches(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.AppCreate); !ok {
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	branches, err := s.SourceConnections.ListBranches(r.Context(), chi.URLParam(r, "sourceID"), r.URL.Query().Get("url"))
	if err != nil {
		Error(w, r, err)
		return
	}
	if branches == nil {
		branches = []string{}
	}
	JSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func (s *Server) handleDeleteSource(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if s.Adapters == nil {
		Error(w, r, errs.New(errs.StateInvalid, "This installation cannot use source connections."))
		return
	}
	id := chi.URLParam(r, "sourceID")
	// Recorded before it happens, like every privileged action (R-228).
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "source.connection.delete", TargetKind: "source_connection", TargetID: id,
	})
	if err := s.Adapters.DeleteInCategory(r.Context(), id, string(api.CategorySource)); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleBeginSourceAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	var req struct {
		Mode api.AuthorizationMode `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"mode": "device"} or {"mode": "web"}.`))
		return
	}
	if req.Mode == "" {
		req.Mode = api.AuthorizationDevice
	}
	id := chi.URLParam(r, "sourceID")
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "source.connection.authorize", TargetKind: "source_connection", TargetID: id,
		Detail: map[string]any{"mode": string(req.Mode)},
	})
	a, err := s.SourceConnections.BeginAuthorization(r.Context(), id, req.Mode, s.sourceCallbackURL(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, a)
}

func (s *Server) handlePollSourceAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	id := chi.URLParam(r, "sourceID")
	st, err := s.SourceConnections.PollAuthorization(r.Context(), id)
	if err != nil {
		Error(w, r, err)
		return
	}
	if st.Status == "authorized" {
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "source.connection.authorized", TargetKind: "source_connection", TargetID: id,
			Detail: map[string]any{"mode": string(api.AuthorizationDevice)},
		})
	}
	JSON(w, http.StatusOK, st)
}

// handleSourceCallback is where a provider sends the browser back after a
// browser authorization. It answers with a redirect to the console either
// way, carrying the outcome, because a person is looking at it.
func (s *Server) handleSourceCallback(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallAdaptersManage)
	if !ok {
		return
	}
	q := r.URL.Query()
	back := url.Values{}
	if denied := q.Get("error"); denied != "" {
		back.Set("error", "The provider did not authorize the connection ("+denied+"). Start the authorization again to retry.")
		http.Redirect(w, r, sourcesPage+"?"+back.Encode(), http.StatusFound)
		return
	}
	if !s.sourcesConfigured(w, r) {
		return
	}
	id, err := s.SourceConnections.CompleteWebAuthorization(r.Context(), q.Get("state"), q.Get("code"))
	if err != nil {
		msg := "The authorization could not be finished."
		if e := errs.As(err); e != nil {
			msg = e.Message
		}
		back.Set("error", msg)
		http.Redirect(w, r, sourcesPage+"?"+back.Encode(), http.StatusFound)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "source.connection.authorized", TargetKind: "source_connection", TargetID: id,
		Detail: map[string]any{"mode": string(api.AuthorizationWeb)},
	})
	back.Set("authorized", id)
	http.Redirect(w, r, sourcesPage+"?"+back.Encode(), http.StatusFound)
}

// sourceCallbackURL is the address a provider returns a browser
// authorization to: the installation's external address when one is set,
// otherwise the one this request came in on.
func (s *Server) sourceCallbackURL(r *http.Request) string {
	if s.ExternalURL != nil && s.ExternalURL.Host != "" {
		u := *s.ExternalURL
		u.Path = "/api/v1/sources/callback"
		u.RawQuery = ""
		return u.String()
	}
	o := s.origin(r)
	return o.Scheme + "://" + o.Host + "/api/v1/sources/callback"
}
