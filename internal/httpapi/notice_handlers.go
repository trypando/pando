package httpapi

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/notice"
	"github.com/trypando/pando/internal/errs"
)

// Notices (R-412): what an app's Overview asks of somebody, and each person's
// choice to ignore one until it changes.
//
// All three are app.view. A notice is a fact about an app somebody can already
// see, and ignoring one is that person's own and changes nothing about the
// app — it is not a write to the app any more than a favorite is.

func (s *Server) handleListNotices(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok || !s.noticesConfigured(w, r) {
		return
	}
	notices, err := s.Notices.List(r.Context(), PrincipalFrom(r.Context()).UserID, app)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, noticeList{Notices: notices})
}

func (s *Server) handleIgnoreNotice(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok || !s.noticesConfigured(w, r) {
		return
	}
	key := chi.URLParam(r, "key")
	n, err := s.Notices.Ignore(r.Context(), PrincipalFrom(r.Context()).UserID, app, key)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.auditNotice(r, app.ID, "app.notice.ignore", key)
	JSON(w, http.StatusOK, n)
}

func (s *Server) handleRestoreNotice(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok || !s.noticesConfigured(w, r) {
		return
	}
	key := chi.URLParam(r, "key")
	if err := s.Notices.Restore(r.Context(), PrincipalFrom(r.Context()).UserID, app, key); err != nil {
		Error(w, r, err)
		return
	}
	s.auditNotice(r, app.ID, "app.notice.restore", key)
	JSON(w, http.StatusNoContent, nil)
}

// noticeList is GET /apps/{appID}/notices. An object rather than a bare
// array, so it can grow without breaking a client.
type noticeList struct {
	Notices []notice.Notice `json:"notices"`
}

func (s *Server) noticesConfigured(w http.ResponseWriter, r *http.Request) bool {
	if s.Notices != nil {
		return true
	}
	Error(w, r, errs.New(errs.AdapterUnavailable, "This installation does not list app notices."))
	return false
}

func (s *Server) auditNotice(r *http.Request, appID, action, key string) {
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		AppID:         appID,
		TargetKind:    "app",
		TargetID:      appID,
		Detail:        map[string]any{"notice": key},
	})
}
