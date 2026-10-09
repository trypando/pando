package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/applimit"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// App limits (R-244) and an app's idle settings (R-397), issue #131. Each a
// sub-resource of its own, like a role, rather than a field on PATCH: "how
// many apps may this person own" and "what is this person called" are
// different acts, and null has to mean "not set here" without a PATCH body
// needing to tell an absent field from a null one.

// msgNoAppLimits is the answer when this server was started without the app
// limit service, as a test server may be.
const msgNoAppLimits = "App limits are not set up on this installation."

// limitsReady answers for a server started without the app limit services,
// as a test server may be, rather than letting a handler reach a nil one.
func (s *Server) limitsReady(w http.ResponseWriter, r *http.Request) bool {
	if s.AppLimits == nil || s.AppLimitStore == nil {
		Error(w, r, errs.New(errs.Internal, msgNoAppLimits))
		return false
	}
	return true
}

// idleReady is limitsReady for an app's idle settings.
func (s *Server) idleReady(w http.ResponseWriter, r *http.Request) bool {
	if s.IdleSettings == nil {
		Error(w, r, errs.New(errs.Internal, "Idle settings are not set up on this installation."))
		return false
	}
	return true
}

// appLimit is the limit in force for userID. No limit service, or no user —
// a token acting for nobody — is no limit.
func (s *Server) appLimit(ctx context.Context, userID string) (applimit.Limit, error) {
	if s.AppLimits == nil || userID == "" {
		return applimit.Limit{}, nil
	}
	return s.AppLimits.For(ctx, userID)
}

// refuseOverLimit refuses a create past the limit, audited like every other
// refused create.
func (s *Server) refuseOverLimit(w http.ResponseWriter, r *http.Request, limit applimit.Limit, owned int) {
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "app.create.denied",
		Detail: map[string]any{
			"reason": "app_limit", "limit": limit.Limit, "owned": owned,
			"source": limit.Source, "group_id": limit.GroupID,
		},
	})
	Error(w, r, applimit.Refusal(limit, owned))
}

// maxAppsBody is the body of PUT …/app-limit. Null clears the value.
type maxAppsBody struct {
	MaxApps *int `json:"max_apps"`
}

func decodeMaxApps(r *http.Request) (*int, error) {
	var req maxAppsBody
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		return nil, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"max_apps": 5}, {"max_apps": 0} for unlimited, or {"max_apps": null} to clear it.`)
	}
	if req.MaxApps != nil && *req.MaxApps < 0 {
		return nil, errs.Newf(errs.ValidInvalid,
			"max_apps is %d; it is how many apps a person may own, so use 1 or more, 0 for unlimited, or null to clear it.", *req.MaxApps)
	}
	return req.MaxApps, nil
}

// handleGetUserAppLimit is the limit in force for a user, where it comes from,
// and how many apps they own. A person may read their own.
func (s *Server) handleGetUserAppLimit(w http.ResponseWriter, r *http.Request) {
	userID := chi.URLParam(r, "userID")
	if _, ok := s.requireSelfOrInstall(w, r, userID, authz.InstallUsersManage); !ok {
		return
	}
	if !s.limitsReady(w, r) {
		return
	}
	limit, err := s.AppLimits.For(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, limit)
}

// handlePutUserAppLimit sets a user's own limit. An administrator's act, on
// anybody's account including their own.
func (s *Server) handlePutUserAppLimit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok {
		return
	}
	if !s.limitsReady(w, r) {
		return
	}
	maxApps, err := decodeMaxApps(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	userID := chi.URLParam(r, "userID")
	if err := s.AppLimitStore.SetUserMaxApps(r.Context(), userID, maxApps); err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "user.update", TargetKind: "user", TargetID: userID,
		Detail: map[string]any{"max_apps": maxApps},
	})
	limit, err := s.AppLimits.For(r.Context(), userID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, limit)
}

// handleGetGroupAppLimit is a group's own limit.
func (s *Server) handleGetGroupAppLimit(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallUsersManage); !ok {
		return
	}
	if !s.limitsReady(w, r) {
		return
	}
	maxApps, err := s.AppLimitStore.GroupMaxApps(r.Context(), chi.URLParam(r, "groupID"))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, maxAppsBody{MaxApps: maxApps})
}

// handlePutGroupAppLimit sets a group's own limit.
func (s *Server) handlePutGroupAppLimit(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.InstallUsersManage)
	if !ok {
		return
	}
	if !s.limitsReady(w, r) {
		return
	}
	maxApps, err := decodeMaxApps(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	groupID := chi.URLParam(r, "groupID")
	if err := s.AppLimitStore.SetGroupMaxApps(r.Context(), groupID, maxApps); err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "group.update", TargetKind: "group", TargetID: groupID,
		Detail: map[string]any{"max_apps": maxApps},
	})
	JSON(w, http.StatusOK, maxAppsBody{MaxApps: maxApps})
}

// handleGetAppIdle is an app's idle settings and when Pando would stop or
// delete it.
func (s *Server) handleGetAppIdle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	if !s.idleReady(w, r) {
		return
	}
	report, err := s.IdleSettings.Report(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, report)
}

// handlePutAppIdle replaces an app's own idle settings. app.spec.edit, the
// verb that renames an app: these are its settings, not its spec, so no
// revision is written and nothing is deployed (R-397).
func (s *Server) handlePutAppIdle(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	if !s.idleReady(w, r) {
		return
	}
	var req state.IdleSettings
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read.").
			WithRemedy(`Send {"stop_days": 30, "delete_days": 90}. Use 0 to turn either off, or null for the installation's setting.`))
		return
	}
	report, err := s.IdleSettings.Set(r.Context(), app.ID, req)
	if err != nil {
		Error(w, r, err)
		return
	}
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.update", AppID: app.ID, TargetKind: "app", TargetID: app.ID,
		Detail: map[string]any{"idle_stop_days": req.StopDays, "idle_delete_days": req.DeleteDays},
	})
	JSON(w, http.StatusOK, report)
}
