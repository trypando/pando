package reconciler

import (
	"context"
	"fmt"

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
// counts toward nothing (R-151). That holds for every app on such a host,
// with a standby or without: an app's home is its standby row's, and for
// any app the host the runtime found it on (HostHeld).
type Holds interface {
	Held(ctx context.Context, appID string) (state.Hold, error)
	// HostHeld is what holds every app on host, a name the runtime gave
	// (api.ObservedBundle.Host): whether it is lost or fenced.
	HostHeld(ctx context.Context, host string) (state.Hold, error)
}

// held says why the app is left alone this visit, or "" when it is not.
func (r *Reconciler) held(ctx context.Context, appID string) string {
	if r.Holds == nil {
		return ""
	}
	h, err := r.Holds.Held(ctx, appID)
	return holdReason(h, err)
}

// heldOn says why an app the runtime found on host is left alone this visit,
// or "" when it is not. An app with no standby row is known to be on a lost
// or fenced host only this way.
func (r *Reconciler) heldOn(ctx context.Context, host string) string {
	if r.Holds == nil || host == "" {
		return ""
	}
	h, err := r.Holds.HostHeld(ctx, host)
	return holdReason(h, err)
}

func holdReason(h state.Hold, err error) string {
	switch {
	case err != nil:
		return "Pando could not read whether this app's host is lost or cut off, so it left the app as it is this time."
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
