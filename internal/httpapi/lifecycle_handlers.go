package httpapi

import (
	"context"
	"net/http"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// App lifecycle (design 04 §2.1).
//
// Start and stop set *desired* state, and the reconciler converges (design 05),
// which is what makes "stopped" survive a Pando restart and a container that
// comes back on its own.
//
// Each also does the one thing the loop cannot do for a `failed` app. The
// reconciler has no code path that touches one — that absence is how R-151 is
// enforced — so for an app it has given up on, desired state alone converges to
// nothing: stopping does not stop it and starting does not start it. R-151 says
// a failed app stays failed "until a human intervenes", and this is the human.
//
// Restart is not desired state — it is an act, and the reconciler has no way to
// express "the same state, again". So it goes through the runtime adapter, and
// always has.

func (s *Server) handleStartApp(w http.ResponseWriter, r *http.Request) {
	s.setDesired(w, r, state.StateRunning, "app.start", func(ctx context.Context, app state.App) {
		if app.State != state.StateFailed {
			return
		}
		// The intervention R-151 asks for. The count goes with it: leaving it
		// at the threshold would give up again on the first tick.
		if s.Reconciles != nil {
			_ = s.Reconciles.ClearFailures(ctx, app.ID)
		}
		_ = s.Apps.SetState(ctx, app.ID, state.StateDegraded)
	})
}

func (s *Server) handleStopApp(w http.ResponseWriter, r *http.Request) {
	s.setDesired(w, r, "stopped", "app.stop", func(ctx context.Context, app state.App) {
		// Directly, rather than waiting for the loop — and for an app in
		// `failed` the loop is never coming. Best effort: the desired state is
		// recorded either way, and an unreachable runtime is not a reason to
		// refuse the request.
		if runtime, ok := s.runtimeFor(ctx, app); ok {
			if err := runtime.Stop(ctx, apiBundleRef(app.ID)); err != nil {
				log.From(ctx).Warn("could not stop the app now; the reconciler will",
					zap.String("app_id", app.ID), zap.Error(err))
			}
		}
	})
}

// runtimeFor resolves the runtime an app's pinned spec names.
func (s *Server) runtimeFor(ctx context.Context, app state.App) (api.RuntimeAdapter, bool) {
	if app.PinnedSpecID == "" {
		return nil, false
	}
	rev, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil || !found {
		return nil, false
	}
	return s.Registry.Runtime(rev.Body.Runtime.AdapterRef)
}

func (s *Server) setDesired(w http.ResponseWriter, r *http.Request, desired, action string, after func(context.Context, state.App)) {
	// app.restart rather than app.spec.edit: starting and stopping an app is
	// operating it, not reconfiguring it, and R-080 separates those.
	app, ok := s.requireControl(w, r, authz.AppRestart)
	if !ok {
		return
	}

	if err := s.Apps.SetDesiredState(r.Context(), app.ID, desired); err != nil {
		Error(w, r, err)
		return
	}

	if after != nil {
		after(r.Context(), app)
	}
	// What this replica last saw of the app is now about to change. Other
	// replicas' copies age out within their TTL (issue #72).
	s.Observations.Forget(app.ID)

	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: action, AppID: app.ID, TargetKind: "app", TargetID: app.ID,
	})

	// 202: the desired state is recorded and the reconciler will converge. Not
	// 200, because the app is not there yet and saying so would be a lie the
	// console would have to unpick.
	JSON(w, http.StatusAccepted, map[string]any{
		"app_id": app.ID, "desired_state": desired,
	})
}

// handleRestartApp restarts an app's workloads in place.
func (s *Server) handleRestartApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppRestart)
	if !ok {
		return
	}

	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app has nothing pinned to restart.").
			WithRemedy("Deploy it first."))
		return
	}
	revision, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found || revision.Body == nil {
		Error(w, r, errs.New(errs.StateInvalid, "This app has nothing pinned to restart.").
			WithRemedy("Deploy it first."))
		return
	}

	runtime, configured := s.Registry.Runtime(revision.Body.Runtime.AdapterRef)
	if !configured {
		Error(w, r, errs.Newf(errs.PlanAdapterNotConfigured,
			"The runtime %q is not configured.", revision.Body.Runtime.AdapterRef))
		return
	}

	// Stop, and let the reconciler start it again.
	//
	// Rather than a restart call the interface does not have: RuntimeAdapter
	// has Stop and Apply, and "restart" as a primitive would be a third thing
	// every adapter had to implement to mean what these two already mean
	// together. The reconciler brings it back because desired state is still
	// running — which is also what makes this survive Pando dying mid-restart.
	if err := runtime.Stop(r.Context(), apiBundleRef(app.ID)); err != nil {
		Error(w, r, err)
		return
	}
	s.Observations.Forget(app.ID)

	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.restart", AppID: app.ID, TargetKind: "app", TargetID: app.ID,
	})
	JSON(w, http.StatusAccepted, map[string]any{"app_id": app.ID, "restarting": true})
}
