package reconciler

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// GCInterval is how often garbage collection runs. [P], design 05 §6.
const GCInterval = time.Hour

// GC reclaims what retention says should be gone.
//
// A separate job from the reconciler, on a much slower clock, because nothing
// here is urgent and all of it is destructive. The reconciler's rule — never
// destroy anything a human may have wanted — applies here too, and is the
// reason each of these deletions has an explicit exemption attached.
type GC struct {
	Apps   *state.Apps
	Logger *zap.Logger

	// Registry resolves the adapters that hold a deleted app's bundle. Nil
	// disables teardown rather than failing: an install with no runtime
	// configured has nothing to tear down.
	Registry Registry

	// Auditor records teardowns. Destroying a bundle is destruction, and
	// R-227's rule does not have an exception for the janitor.
	Auditor Auditor

	// Backups, Backup and BundleSource drive R-211's rolling backups and their
	// expiry. Nil disables both rather than failing: an install with no backup
	// destination configured has nowhere to put one.
	Backups      *state.Backups
	Backup       BackupRunner
	BundleSource *state.BundleSource

	// The security pass (R-315, R-316). All four are nil on an installation
	// with no scanner, where nothing is scored and the pass does nothing.
	Security      SecurityScores
	SecurityState SecurityState
	PolicyStore   PolicyStore
	Desired       Desired
	Notifier      OwnerNotifier

	// Clock is here so retention is testable without waiting a day.
	Clock clock.Clock

	// Interval overrides GCInterval. Zero means the default.
	//
	// Configurable for the same reason the retry backoff is: the acceptance
	// test for bundle teardown would otherwise have to wait an hour to watch a
	// janitor run. Nothing here is urgent, so a short interval is wasteful
	// rather than dangerous — which is why this one gets no startup warning.
	Interval time.Duration

	// TeardownNow starts a teardown pass between ticks. Deleting an app sends
	// on it, so the app's containers stop when it is deleted rather than at the
	// next pass — which was up to an hour later, while a force-deleted app kept
	// running and holding its share of the host (issue #55). Nil means ticks
	// only.
	TeardownNow <-chan struct{}

	// BuildCaches and DiscardUpload remove a deleted app's build cache and
	// uploaded source at teardown. Nil skips each.
	BuildCaches   BuildCaches
	DiscardUpload func(appID string) error

	// DiscardCredential removes a deleted app's registry credential (issue
	// #41). Apps soft-delete, so the row's cascade never fires; a credential
	// for an app nobody can deploy is a key kept for no one. Nil skips it.
	DiscardCredential func(ctx context.Context, appID string) error
}

// BuildCaches has the builder that built an app forget its cache.
type BuildCaches interface {
	Forget(ctx context.Context, builderRef, appID string) error
}

// TeardownBatch bounds how many bundles one pass destroys.
//
// Bounded because this is destructive and runs unattended. An install upgrading
// with two hundred long-deleted apps should reclaim them over a few hours
// rather than issue two hundred destroys in a burst against a runtime that is
// also serving live traffic.
const TeardownBatch = 20

// Run collects until the context is canceled.
func (g *GC) Run(ctx context.Context) {
	interval := g.Interval
	if interval <= 0 {
		interval = GCInterval
	}

	// A pass at startup, before the first tick.
	//
	// Without it, an install that upgrades and restarts waits a full interval
	// before reclaiming anything — and the apps waiting longest are exactly the
	// ones deleted by a version that never tore anything down.
	g.Collect(ctx)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.Collect(ctx)
		case <-g.TeardownNow:
			// Teardown only. The rest of a pass is on the slow clock for a
			// reason, and a delete is not one.
			g.tearDownDeletedBundles(ctx)
		}
	}
}

// Collect runs one pass.
//
// Spec revision pruning only, for now. Log retention (R-222–R-224) is not here
// because there is nothing for it to act on: Pando does not hold app logs, it
// streams them from the runtime, and the bytes live wherever that runtime put
// them. Enforcing a per-app cap means the log driver's own options at container
// creation; enforcing the aggregate would mean recreating every container when
// one app turns chatty, which is destruction on a schedule and not something
// the reconciler may do. Recorded as O-16 rather than guessed at.
//
// Backup expiry (R-211) arrives with backups, in phase 9.
func (g *GC) Collect(ctx context.Context) {
	pruned, err := g.Apps.PruneSpecRevisions(ctx)
	if err != nil {
		g.Logger.Warn("could not prune spec revisions", zap.Error(err))
	} else if pruned > 0 {
		g.Logger.Info("pruned spec revisions", zap.Int("count", pruned))
	}

	g.tearDownDeletedBundles(ctx)
	g.runBackups(ctx)
	g.reclaimOrphanedVolumes(ctx)
	g.enforceSecurity(ctx)
}

// tearDownDeletedBundles destroys the bundles of apps that have been deleted.
//
// Nothing used to do this. Deleting an app archived the row and left its
// containers running on a private network nobody reclaimed — and Pando takes
// one network per app against a Docker pool that holds about thirty, so an
// install that adds and removes apps eventually cannot start one. The failure
// arrives as a message about subnets, on an unrelated deploy, long after the
// deletion that caused it.
//
// Here rather than in the delete handler because convergence is the
// reconciler's job (design 05): a delete that destroyed inline would leave a
// bundle standing forever if the runtime happened to be unreachable at that
// moment, and nothing would ever come back for it.
//
// Volumes are destroyed only when the delete said so. R-204 has a delete
// settle the app's storage — keep a final backup or discard it — and a delete
// of an app with storage is refused until it does; both answers end with the
// volumes gone, the second after a backup holds the data. That decision is
// recorded on the app (TeardownTarget.DiscardStorage) and is the only thing
// that sets KeepVolumes false here: this is the one place a bug would
// silently destroy data, so nothing is inferred.
func (g *GC) tearDownDeletedBundles(ctx context.Context) {
	if g.Registry == nil {
		return
	}

	targets, err := g.Apps.AwaitingTeardown(ctx, TeardownBatch)
	if err != nil {
		g.Logger.Warn("could not list apps waiting to be torn down", zap.Error(err))
		return
	}

	for _, t := range targets {
		if err := g.tearDown(ctx, t); err != nil {
			// Left for the next pass rather than marked done. An unreachable
			// runtime is temporary; a bundle marked destroyed that is still
			// running is permanent, and invisible.
			g.Logger.Warn("could not tear down a deleted app's bundle",
				zap.String("app_id", t.AppID), zap.Error(err))
			continue
		}

		if err := g.Apps.MarkBundleDestroyed(ctx, t.AppID); err != nil {
			// The bundle is gone but the row still says otherwise, so the next
			// pass destroys it again. Destroy is idempotent, so that is noise
			// rather than damage.
			g.Logger.Warn("tore down a bundle but could not record it",
				zap.String("app_id", t.AppID), zap.Error(err))
			continue
		}

		g.Logger.Info("tore down a deleted app's bundle", zap.String("app_id", t.AppID))
		if g.Auditor != nil {
			// Destroying a bundle is destruction, and R-227 has no exception
			// for the janitor. kept_volumes is recorded explicitly so the log
			// answers the question somebody will actually ask afterwards.
			_ = g.Auditor.Write(ctx, AuditEvent{
				Action: "app.bundle.destroy",
				AppID:  t.AppID,
				Detail: map[string]any{"runtime": t.RuntimeRef, "kept_volumes": !t.DiscardStorage},
			})
		}
	}
}

// reclaimOrphanedVolumes destroys the runtime volumes of deleted apps whose
// data is already safe in a backup.
//
// The gap this closes: deleting an app with backup=true copies its data into a
// bundle and removes the volume *rows*, and the volume itself stayed on disk
// forever with nothing referencing it. Leaving data was the safe direction
// while there was any doubt; once a backup holds it there is none.
//
// Deliberately narrow. A volume is only destroyed when the app that owned it is
// deleted AND a backup of that app exists — R-204 says volumes outlive apps,
// and this does not weaken that, it just stops the storage outliving the last
// thing that could ever want it.
func (g *GC) reclaimOrphanedVolumes(ctx context.Context) {
	if g.Backups == nil || g.Registry == nil {
		return
	}

	orphans, err := g.Apps.OrphanedVolumes(ctx, TeardownBatch)
	if err != nil {
		g.Logger.Warn("could not list orphaned storage", zap.Error(err))
		return
	}

	for _, o := range orphans {
		rt, ok := g.Registry.Runtime(o.AdapterRef)
		if !ok {
			continue
		}
		if err := rt.DestroyVolume(ctx, api.VolumeHandle{VolumeID: o.VolumeID, Handle: o.Handle}); err != nil {
			g.Logger.Warn("could not remove orphaned storage",
				zap.String("volume_id", o.VolumeID), zap.Error(err))
			continue
		}
		if err := g.Apps.ForgetVolume(ctx, o.AppID, o.VolumeID); err != nil {
			g.Logger.Warn("removed orphaned storage but could not record it",
				zap.String("volume_id", o.VolumeID), zap.Error(err))
			continue
		}

		g.Logger.Info("removed storage whose app was deleted and backed up",
			zap.String("volume_id", o.VolumeID), zap.String("app_id", o.AppID))
		if g.Auditor != nil {
			_ = g.Auditor.Write(ctx, AuditEvent{
				Action: "volume.destroy", AppID: o.AppID,
				Detail: map[string]any{"volume_id": o.VolumeID, "backed_up": true},
			})
		}
	}
}

func (g *GC) tearDown(ctx context.Context, t state.TeardownTarget) error {
	// The route first. A route outliving its app points at a Pando that will
	// answer 404 for it, and on a file-provider edge it is a file that
	// accumulates one per deleted app.
	if t.RoutingRef != "" {
		if routing, ok := g.Registry.Routing(t.RoutingRef); ok {
			if err := routing.Remove(ctx, api.RouteHandle{AppID: t.AppID}); err != nil {
				return err
			}
		}
	}

	if t.RuntimeRef != "" {
		runtime, ok := g.Registry.Runtime(t.RuntimeRef)
		if !ok {
			// The adapter that held it is no longer configured. Reported rather
			// than marked done: something is still running and Pando can no
			// longer reach it, which an operator needs to know.
			return errs.Newf(errs.AdapterFailed,
				"The runtime %q is not configured, so this app's containers cannot be removed.", t.RuntimeRef)
		}
		// Volumes are kept unless the delete settled them — discarded with
		// force, or backed up first (R-204). A delete of an app with storage
		// is refused until it says which, so an app whose storage was never
		// decided on is one that had none Pando knew of, and that is kept.
		keep := !t.DiscardStorage
		if err := runtime.Destroy(ctx, api.BundleRef{BundleID: t.AppID}, api.DestroyOptions{KeepVolumes: keep}); err != nil {
			return err
		}
	}

	return g.forgetFiles(ctx, t)
}

// forgetFiles removes what a deleted app kept on disk outside its bundle: its
// build cache and its uploaded source. Both stayed forever, and an install
// that adds and removes apps filled its disk with them (issue #55, R-224).
// Neither is the app's data — volumes are, and teardown keeps those (R-204).
//
// It runs for an app that was never deployed too: an upload is stored before
// the first deploy.
func (g *GC) forgetFiles(ctx context.Context, t state.TeardownTarget) error {
	if t.BuilderRef != "" && g.BuildCaches != nil {
		if err := g.BuildCaches.Forget(ctx, t.BuilderRef, t.AppID); err != nil {
			return err
		}
	}
	if g.DiscardUpload != nil {
		if err := g.DiscardUpload(t.AppID); err != nil {
			return err
		}
	}
	if g.DiscardCredential != nil {
		if err := g.DiscardCredential(ctx, t.AppID); err != nil {
			return err
		}
	}
	return nil
}
