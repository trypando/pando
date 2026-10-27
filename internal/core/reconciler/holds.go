package reconciler

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
)

// Holds reads what keeps the reconciler off an app in a cluster (R-148,
// R-153, R-419, R-421). *state.Standbys is one.
//
// R-148 restores what diverges from the spec, and three things in a cluster
// look like divergence and are not:
//
//   - an app whose host is marked lost: its standby is started elsewhere,
//     and restarting the copy on the lost host would be a second copy
//     (R-153);
//   - an app whose host is fenced: the host stopped its apps because it is
//     cut off from most of the cluster, and starting them again would undo
//     the fence (R-421);
//   - an app whose standby is being promoted: the promotion is starting it,
//     and a second Apply beside it would start it twice.
//
// Each is reported as the app being unobservable, never as a failure, so it
// counts toward nothing (R-151). A hold that cannot be read is not a hold
// (heldCheck). An install of one host asks nothing (consultHolds). That holds for every app on such a host,
// with a standby or without: an app's home is its standby row's, and for
// any app the host the runtime found it on (HostHeld).
type Holds interface {
	Held(ctx context.Context, appID string) (state.Hold, error)
	// HostHeld is what holds every app on host, a name the runtime gave
	// (api.ObservedBundle.Host): whether it is lost or fenced.
	HostHeld(ctx context.Context, host string) (state.Hold, error)
}

// held says why the app is left alone this visit, or "" when it is not. An
// error is that whether it is held could not be read (heldCheck).
func (r *Reconciler) held(ctx context.Context, appID string) (string, error) {
	if !r.consultHolds() {
		return "", nil
	}
	h, err := r.Holds.Held(ctx, appID)
	return holdReason(h), err
}

// heldOn says why an app the runtime found on host is left alone this visit,
// or "" when it is not. An app with no standby row is known to be on a lost
// or fenced host only this way.
func (r *Reconciler) heldOn(ctx context.Context, host string) (string, error) {
	if !r.consultHolds() || host == "" {
		return "", nil
	}
	h, err := r.Holds.HostHeld(ctx, host)
	return holdReason(h), err
}

// consultHolds is whether this pass asks Holds at all: not without one, and
// not while the install is one host (Reconciler.Hosts, noteHosts), where
// nothing can be lost, fenced or promoted to, so a hold check that fails
// cannot stop a single host's reconciling.
func (r *Reconciler) consultHolds() bool {
	return r.Holds != nil && !r.singleHost.Load()
}

// noteHosts records, once a pass, whether the install is one host. A count
// that cannot be read is said, and holds are consulted: what they cannot
// read is said for each app too (heldCheck).
func (r *Reconciler) noteHosts(ctx context.Context) {
	if r.Hosts == nil || r.Holds == nil {
		return
	}
	n, err := r.Hosts(ctx)
	if err != nil {
		r.Logger.Error("could not count the cluster's hosts; asking what holds each app", zap.Error(err))
	}
	r.singleHost.Store(err == nil && n < 2)
}

// heldCheck acts on what held or heldOn answered, and reports whether the
// app is left alone this visit. A hold is reported as the app being
// unobservable, counting toward nothing (R-151). A check that could not be
// read is never taken for a hold (R-148): it is logged at error and counted
// as a failed reconcile (R-399), so a database that cannot answer is seen
// rather than quietly stopping every app's reconciling, and the app is left
// for the next pass, still counting toward nothing.
func (r *Reconciler) heldCheck(ctx context.Context, app state.Reconcilable, why string, err error, seen *visit) bool {
	if err != nil {
		r.Logger.Error("could not read whether this app's host is lost or cut off; left for the next pass",
			zap.String("app_id", app.ID), zap.Error(err))
		r.unobservable(ctx, app, holdUnread)
		seen.failed = true
		return true
	}
	if why != "" {
		r.unobservable(ctx, app, why)
		return true
	}
	return false
}

// holdUnread is what an app whose hold check failed is reported with.
const holdUnread = "Pando could not read whether this app's host is lost or cut off from the cluster, so it left the app as it is and will check again on its next pass. This is a database error, not a hold; Pando's log has it."

func holdReason(h state.Hold) string {
	switch {
	case h.Promoting:
		return fmt.Sprintf("This app's standby is being started on %s as the app, so Pando leaves the app alone until that finishes.", h.Home)
	case h.HomeLost:
		return fmt.Sprintf("The host this app runs on, %s, is marked lost, so Pando does not start the app there. Its standby, if it has one, is started on another host.", h.Home)
	case h.HomeFenced:
		return fmt.Sprintf("The host this app runs on, %s, is cut off from most of the cluster and stopped its apps, so Pando does not start the app there until the host reaches the cluster again.", h.Home)
	}
	return ""
}

// localFenced reports whether this replica's own host is fenced (R-421):
// cut off from most of the cluster, so this replica changes nothing.
func (r *Reconciler) localFenced() bool {
	return r.LocalFenced != nil && r.LocalFenced()
}
