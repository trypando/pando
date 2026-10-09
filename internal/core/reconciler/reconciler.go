// Package reconciler runs the loop that converges observed state toward pinned
// specs.
//
// The dividing line: the reconciler may create and start things; it may not
// destroy anything a human may have wanted (R-148, R-028). A failed app stays
// failed — R-151 is true because there is no code path here that touches the
// failed state, not because a flag is checked. See design 05.
package reconciler

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/telemetry"
)

// Tunables, all [P] from design 05 §2.
const (
	// Interval between ticks. Short enough that a killed container comes back
	// while someone is still looking at the page, long enough that a hundred
	// apps is not a hundred Observe calls a second.
	Interval = 15 * time.Second

	// Concurrency is how many apps are reconciled at once.
	Concurrency = 8

	// DefaultFailureThreshold and DefaultFailureWindow are R-150's give-up rule.
	DefaultFailureThreshold = 10
	DefaultFailureWindow    = 30 * time.Minute

	// DefaultLeaseDuration is how long a claim on an app lasts unless the
	// pass holding it extends it, which it does every third of this. It is
	// how long an app held by a replica that died waits to be visited again.
	DefaultLeaseDuration = 2 * time.Minute

	// DefaultMinRevisit is how recently visited an app must be for a pass to
	// leave it for the next one. Half an interval: with several replicas
	// ticking out of phase, an app one of them has just looked at is not
	// looked at again by the next a few seconds later, and on one replica a
	// pass that finishes within half an interval still visits every app on
	// every tick.
	DefaultMinRevisit = Interval / 2
)

// DefaultBackoff is R-149, capped at five minutes.
//
// Indexed by consecutive failures, so the first correction is immediate: a
// container killed once should come back now, not in five seconds. The cap
// matters more than the curve — an app that cannot start must not be retried
// forever at speed, and must still be retried.
var DefaultBackoff = []time.Duration{0, 5 * time.Second, 15 * time.Second, 60 * time.Second, 5 * time.Minute}

// MinProductionCap is the smallest last-step backoff that is not obviously a
// test setting.
//
// Not enforced — a floor would make the schedule untestable end to end, which
// is the problem this configurability exists to solve. It is the threshold for
// saying so loudly at startup instead: an install retrying a broken app every
// two seconds forever is a real way to melt a host, and it should not be
// something an operator can do without being told.
const MinProductionCap = 30 * time.Second

// Registry resolves adapters by reference.
type Registry interface {
	Runtime(ref string) (api.RuntimeAdapter, bool)
	Routing(ref string) (api.RoutingAdapter, bool)
}

// Notifier tells someone an app needs attention.
type Notifier interface {
	Notify(ctx context.Context, n api.Notification) error
}

// Reconciler converges running apps toward their pinned specs.
type Reconciler struct {
	Apps       *state.Apps
	Reconciles *state.Reconciles
	Secrets    *state.Secrets

	// Services adds provisioned services to what should be running (R-131).
	// Nil on an install with no provisioner, where every app's shape is already
	// complete.
	Services ServiceShapes

	// Environments supplies what each workload runs with when a correction
	// is applied (R-148). Nil applies the shape as it is, which is complete
	// only for an app whose workloads take no environment.
	Environments Environments

	Volumes  *state.Volumes
	Registry Registry
	Auditor  Auditor
	Notifier Notifier
	Logger   *zap.Logger
	Clock    clock.Clock

	// BuiltImageAuth is the install registry's credential for an image Pando
	// pushed there, nil for any other (issue #72, PR 5). A workload restored
	// on a runtime that pulls needs it for the pull, as the deploy did. Nil
	// means no install registry.
	BuiltImageAuth func(ctx context.Context, ref string) *api.RegistryAuth

	// Backoff, FailureThreshold and FailureWindow override R-149 and R-150's
	// defaults. Zero values mean the defaults, so a caller that does not care
	// sets nothing.
	//
	// Configurable because the acceptance test for R-151 — a crash-looping app
	// reaches failed and stays there — has to wait out the real schedule, and
	// at the production numbers that is forty minutes of a forty-three minute
	// suite. The test is asserting the state machine, not the durations, and it
	// was paying for the durations.
	//
	// The state machine is what stays under test either way: compressing the
	// schedule changes how long each step waits and nothing about which step
	// comes next.
	Backoff          []time.Duration
	FailureThreshold int
	FailureWindow    time.Duration

	// MinRevisit is how recently visited an app may be and still be left
	// for a later pass (DefaultMinRevisit in production). Zero visits every
	// app on every pass, which is what a test calling Tick in a row wants.
	MinRevisit time.Duration

	// LeaseDuration overrides DefaultLeaseDuration. Zero means the default.
	LeaseDuration time.Duration

	// SettledRevisit is how long an app found as it should be is left before
	// it is looked at again, when its runtime reports events (O-52, events.go).
	// Zero means DefaultSettledRevisit.
	SettledRevisit time.Duration

	// Runtimes names the runtime adapters whose events Run follows (O-52).
	// Asked again on every tick, so a runtime added in the console is
	// followed without a restart. Nil follows none, and every app is visited
	// on every pass, as before events.
	Runtimes func() []string

	// events is what Run's event watchers share with Tick (events.go).
	eventsOnce sync.Once
	ev         *eventState

	// ProxyUpstream is where routes point. Every route points at Pando's proxy
	// and never at a workload (R-023) — the reconciler re-ensuring a route must
	// not be the one place that forgets.
	ProxyUpstream string
}

// Auditor writes the events a reconciliation produces.
type Auditor interface {
	Write(ctx context.Context, e AuditEvent) error
}

// AuditEvent is what the reconciler records.
type AuditEvent struct {
	Action string
	AppID  string
	Detail map[string]any
}

// Run ticks until the context is canceled.
//
// Every replica runs this, and every replica follows every runtime's events
// (O-52, events.go): the lease gives no app an owning replica, so the replica
// that hears about an app is not necessarily the one that will visit it. An
// event makes the app due in the database, where any replica's next claim
// finds it, and wakes this replica's loop so it does not wait for the tick.
func (r *Reconciler) Run(ctx context.Context) {
	ticker := time.NewTicker(Interval)
	defer ticker.Stop()

	ev := r.events()
	go r.nudgeLoop(ctx)
	r.followRuntimes(ctx)

	// One immediately, so that starting Pando converges rather than waiting a
	// quarter of a minute to notice anything.
	r.Tick(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.followRuntimes(ctx)
			r.Tick(ctx)
		case <-ev.wake:
			r.Tick(ctx)
		}
	}
}

// Tick reconciles every app that is due.
//
// Apps are claimed in small batches under a lease (state.Lease), least
// recently visited first, until none is left that was last visited before the
// pass began. The batches keep the number held but not yet started small,
// and the lease is what keeps two passes — on this replica or another — off
// one app. Nothing is held on a database connection while an app is
// reconciled, however long its Apply takes.
func (r *Reconciler) Tick(ctx context.Context) {
	cutoff, err := r.Reconciles.Cutoff(ctx, r.MinRevisit)
	if err != nil {
		r.Logger.Warn("could not list apps to reconcile", zap.Error(err))
		return
	}

	lease := r.Reconciles.Lease()
	keeper := newLeaseKeeper(lease, r.leaseDuration(), r.Logger)
	keeperCtx, stopKeeper := context.WithCancel(ctx)
	keeperDone := make(chan struct{})
	go func() { defer close(keeperDone); keeper.run(keeperCtx) }()
	defer func() { stopKeeper(); <-keeperDone }()

	sem := make(chan struct{}, Concurrency)
	var wg sync.WaitGroup
	defer wg.Wait()

	for {
		batch, err := keeper.claim(func() ([]state.Reconcilable, error) {
			return lease.Claim(ctx, r.now(), cutoff, Concurrency*2, r.leaseDuration())
		})
		if err != nil {
			r.Logger.Warn("could not list apps to reconcile", zap.Error(err))
			return
		}
		if len(batch) == 0 {
			return
		}

		for i, app := range batch {
			select {
			case <-ctx.Done():
				// Claimed and never started: back to the front of the queue
				// now rather than when the lease runs out.
				for _, left := range batch[i:] {
					_ = lease.Release(ctx, left.ID, nil)
				}
				return
			case sem <- struct{}{}:
			}

			appCtx, ok := keeper.start(ctx, app.ID)
			if !ok {
				// The lease ran out while the app waited its turn, and
				// another pass may have it now.
				<-sem
				continue
			}
			wg.Add(1)
			go func(a state.Reconcilable) {
				defer wg.Done()
				defer func() { <-sem }()
				var seen visit
				started, done := time.Now(), false
				defer func() {
					telemetry.Reconcile(appCtx, reconcileOutcome(appCtx, done, seen), time.Since(started))
					keeper.finish(a.ID)
					if err := r.release(ctx, lease, a, seen); err != nil {
						r.Logger.Warn("could not release an app after reconciling it",
							zap.String("app_id", a.ID), zap.Error(err))
					}
				}()
				defer r.recoverPanic(a)
				seen = r.reconcileOne(appCtx, a)
				done = true
			}(app)
		}
	}
}

// leaseDuration is how long a claim lasts without being extended.
func (r *Reconciler) leaseDuration() time.Duration {
	if r.LeaseDuration > 0 {
		return r.LeaseDuration
	}
	return DefaultLeaseDuration
}

// leaseKeeper extends a pass's leases while its apps are reconciled, and stops
// the work on any app whose lease it could not keep.
//
// Stopping is what makes a lease as good as the lock it replaced. A lease that
// runs out is another pass's to claim; an Apply still running here past that
// point is two reconciliations of one app, which is the thing being prevented.
type leaseKeeper struct {
	lease    *state.Lease
	duration time.Duration
	logger   *zap.Logger

	// claiming is held across a claim and across an extension, so an app
	// claimed while an extension is in flight is never mistaken for one the
	// extension found already gone.
	claiming sync.Mutex

	mu      sync.Mutex
	waiting map[string]bool
	running map[string]context.CancelFunc
	lost    map[string]bool
	// heldUntil is when the leases last extended run out, by this process's
	// clock and conservatively: measured from before the extension was asked.
	heldUntil time.Time
}

func newLeaseKeeper(lease *state.Lease, duration time.Duration, logger *zap.Logger) *leaseKeeper {
	return &leaseKeeper{
		lease: lease, duration: duration, logger: logger,
		waiting:   map[string]bool{},
		running:   map[string]context.CancelFunc{},
		lost:      map[string]bool{},
		heldUntil: time.Now().Add(duration),
	}
}

// claim runs one claim and records what it returned as held and waiting.
func (k *leaseKeeper) claim(claim func() ([]state.Reconcilable, error)) ([]state.Reconcilable, error) {
	k.claiming.Lock()
	defer k.claiming.Unlock()
	batch, err := claim()
	if err != nil {
		return nil, err
	}
	k.mu.Lock()
	for _, a := range batch {
		k.waiting[a.ID] = true
	}
	k.mu.Unlock()
	return batch, nil
}

// start returns the context an app's reconciliation runs under, or false if
// its lease has already been lost.
func (k *leaseKeeper) start(ctx context.Context, appID string) (context.Context, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	delete(k.waiting, appID)
	if k.lost[appID] || time.Now().After(k.heldUntil) {
		return nil, false
	}
	appCtx, cancel := context.WithCancel(ctx)
	k.running[appID] = cancel
	return appCtx, true
}

func (k *leaseKeeper) finish(appID string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if cancel, ok := k.running[appID]; ok {
		cancel()
		delete(k.running, appID)
	}
}

// run extends every lease the pass holds at a third of their length, so two
// extensions can fail before one runs out.
func (k *leaseKeeper) run(ctx context.Context) {
	ticker := time.NewTicker(k.duration / 3)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		k.claiming.Lock()
		asked := time.Now()
		held, err := k.lease.Extend(ctx, k.duration)

		k.mu.Lock()
		if err != nil {
			if ctx.Err() == nil && time.Now().After(k.heldUntil) {
				// Not extended, and the last extension has run out: none of
				// this pass's apps can be assumed to be its own any more.
				k.logger.Warn("lost the reconciliation lease; stopping this pass's work", zap.Error(err))
				for appID, cancel := range k.running {
					cancel()
					k.lost[appID] = true
				}
			}
			k.mu.Unlock()
			k.claiming.Unlock()
			continue
		}
		k.heldUntil = asked.Add(k.duration)
		for appID, cancel := range k.running {
			if !held[appID] {
				k.logger.Warn("lost the reconciliation lease on an app; stopping its work",
					zap.String("app_id", appID))
				cancel()
				k.lost[appID] = true
			}
		}
		for appID := range k.waiting {
			if !held[appID] {
				k.lost[appID] = true
			}
		}
		k.mu.Unlock()
		k.claiming.Unlock()
	}
}

// recover stops one app's reconciliation from taking the process down.
//
// This loop runs forever and every tick calls into adapter code, which is
// ordinary Go that can panic. A panic in a goroutine is not recoverable by its
// caller — it kills the process — so a single adapter with a nil map would stop
// Pando entirely, including for every app that was working. This was not a
// hypothetical: the first version of the failure path dereferenced the nil that
// errs.As returns for an unenveloped error, and one adapter returning a plain
// error would have done exactly that.
//
// The app is left alone. The next tick tries again, which is the right
// behavior for something that might be transient, and the log line is what
// makes it visible if it is not.
func (r *Reconciler) recoverPanic(app state.Reconcilable) {
	if v := recover(); v != nil {
		r.Logger.Error("reconciliation panicked",
			zap.String("app_id", app.ID),
			zap.Any("panic", v),
			zap.Stack("stack"))
	}
}

// visit is what one reconciliation found, for deciding when the app is next
// due (release).
type visit struct {
	// runtime is the adapter the app runs on.
	runtime string
	// settled: the app was already as it should be and nothing was done or
	// changed — running and healthy with no drift, or stopped and staying
	// stopped. Anything else keeps the fast cadence.
	settled bool
	// failed: Pando could not look at the app or work out what it should
	// be, so nothing was compared. Exported as the reconcile's outcome
	// (R-399); a correction that fails is counted against the app instead.
	failed bool
}

// reconcileOutcome is how one reconcile ended, for R-399's metric. done is
// false after a panic.
func reconcileOutcome(ctx context.Context, done bool, seen visit) telemetry.Outcome {
	switch {
	case ctx.Err() != nil:
		return telemetry.Canceled
	case !done || seen.failed:
		return telemetry.Failed
	default:
		return telemetry.Succeeded
	}
}

// reconcileOne converges a single app.
func (r *Reconciler) reconcileOne(ctx context.Context, app state.Reconcilable) visit {
	// No lock taken here: the app is this pass's under its lease (Tick), and
	// ctx is canceled if the lease is lost.
	rev, found, err := r.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil || !found {
		return visit{failed: true}
	}
	s := rev.Body
	seen := visit{runtime: s.Runtime.AdapterRef}

	runtime, ok := r.Registry.Runtime(s.Runtime.AdapterRef)
	if !ok {
		// The adapter this app runs on is not configured. That is an install
		// problem and not the app's fault, so it is reported the same way an
		// unreachable adapter is rather than counted against the app.
		r.unobservable(ctx, app, "the runtime adapter "+s.Runtime.AdapterRef+" is not configured")
		seen.failed = true
		return seen
	}

	observed, err := runtime.Observe(ctx, api.BundleRef{BundleID: app.ID})
	if err != nil {
		r.unobservable(ctx, app, reason(err))
		seen.failed = true
		return seen
	}
	if app.UnobservableSince != nil {
		_ = r.Reconciles.ClearUnobservable(ctx, app.ID)
	}
	// Settled only from a visit that changed nothing about the app, so one
	// that has just recovered — from a failure, from being unobservable, from
	// any other state — is looked at again on the fast cadence first (O-52).
	quiet := app.ConsecutiveFailures == 0 && app.UnobservableSince == nil

	// R-140: desired_state is what a person asked for, and it outranks
	// everything below. An app someone stopped stays stopped.
	if app.DesiredState == "stopped" {
		seen.settled = r.holdStopped(ctx, app, runtime, observed) && quiet && app.State == state.StateStopped
		return seen
	}

	want, inputs, err := r.desired(ctx, app, s, observed)
	if err != nil {
		r.Logger.Warn("could not work out what should be running",
			zap.String("app_id", app.ID), zap.Error(err))
		seen.failed = true
		return seen
	}

	drift := Classify(want, observed, inputs)
	healthy := observedHealthy(observed, r.now())

	switch {
	case drift.None() && healthy:
		r.settle(ctx, app, state.StateRunning)
		seen.settled = quiet && app.State == state.StateRunning

	case drift.None():
		// Matches the spec and is not healthy. Nothing to converge — the app
		// itself is unwell, which is degraded and counts toward the threshold.
		r.degrade(ctx, app, runtime, "the app is running but not healthy")

	case len(drift.ReportOnly) > 0:
		// R-148: reconcile when possible, report when not. A single
		// unreconcilable difference stops the whole app being touched — the
		// reconcilable half might be the half that destroys the evidence.
		r.report(ctx, app, drift)

	case drift.Actionable():
		r.correct(ctx, app, runtime, want, drift, s)
	}
	return seen
}

// desired builds what should be running, and the facts drift classification
// needs that the plan does not carry.
//
// No secrets are fetched. Environment drift is detected by comparing
// fingerprints, and a fingerprint is built from secret *versions* — so the loop
// that runs every fifteen seconds for every app never decrypts anything.
func (r *Reconciler) desired(ctx context.Context, app state.Reconcilable, s *spec.AppSpec, observed api.ObservedBundle) (api.BundlePlan, Inputs, error) {
	want := PlanShape(s, app.ImageRef, app.WorkloadImages, app.EgressRules)

	// A provisioned database is part of what should be running (R-131).
	//
	// Left out, it is neither restored when it is killed nor recognized when it
	// is running — the app is told every fifteen seconds that its own database
	// is a workload the spec does not declare.
	//
	// A failure here is not a reason to reconcile against half a bundle: an
	// incomplete "want" makes the service look like a stranger, and acting on
	// that is worse than waiting for the next tick.
	if r.Services != nil {
		services, err := r.Services.ServiceShapes(ctx, s)
		if err != nil {
			return api.BundlePlan{}, Inputs{}, err
		}
		want.Workloads = append(want.Workloads, services.Workloads...)
		want.Volumes = append(want.Volumes, services.Volumes...)
	}

	versions, err := r.Secrets.Versions(ctx, app.ID)
	if err != nil {
		return api.BundlePlan{}, Inputs{}, err
	}

	handles, err := r.Volumes.Handles(ctx, app.ID)
	if err != nil {
		return api.BundlePlan{}, Inputs{}, err
	}
	handles = r.recordObservedVolumes(ctx, app, s, want, observed, handles)
	held := make(map[string]bool, len(handles))
	for volumeID, handle := range handles {
		held[volumeID] = handle != ""
	}

	// Per workload where the deployment recorded it. The app-level digest
	// belongs to the primary workload and to nothing else: comparing every
	// workload against it made a compose app's second service permanently
	// "running the wrong image", and the correction replaced it with the
	// primary's image.
	expected := map[string]string{}
	for name, ran := range app.WorkloadImages {
		if ran.Digest != "" {
			expected[name] = ran.Digest
		}
	}
	if len(expected) == 0 && app.ImageDigest != "" {
		if primary, ok := s.PrimaryWorkload(); ok {
			expected[primary.Name] = app.ImageDigest
		}
	}

	return want, Inputs{
		ExpectedDigests:     expected,
		ExpectedDigest:      app.ImageDigest,
		AppliedEnvHash:      app.AppliedEnvHash,
		CurrentEnvHash:      EnvHash(s, versions),
		VolumesThatHeldData: held,
	}, nil
}

// recordObservedVolumes brings Pando's volume rows in line with what the
// runtime reports holding for this app, and returns the handles as recorded.
//
// Only a deploy used to write these rows, and before migration 000036 a deploy
// could not write one for a volume whose ID another app already used: the
// second app got no row and the first app's row was pointed at the second
// app's volume (issue #87). A redeploy would fix each app, but nothing asks
// for one — so the loop that already observes every app fixes it instead.
//
// Only volumes the app should have, only ones the runtime says exist, and only
// where the record disagrees, so a settled app costs no write. This changes
// Pando's record, never the runtime: the volume is already there.
func (r *Reconciler) recordObservedVolumes(ctx context.Context, app state.Reconcilable, s *spec.AppSpec, want api.BundlePlan, observed api.ObservedBundle, handles map[string]string) map[string]string {
	present := make(map[string]string, len(observed.Volumes))
	for _, v := range observed.Volumes {
		if v.Present && v.Handle != "" {
			present[v.VolumeID] = v.Handle
		}
	}

	var records []state.VolumeRecord
	for _, v := range want.Volumes {
		handle, ok := present[v.VolumeID]
		if !ok || handles[v.VolumeID] == handle {
			continue
		}
		records = append(records, state.VolumeRecord{VolumeID: v.VolumeID, Name: v.Name, Handle: handle})
	}
	if len(records) == 0 {
		return handles
	}

	if err := r.Volumes.RecordFromRuntime(ctx, app.ID, s.Runtime.AdapterRef, records); err != nil {
		r.Logger.Warn("could not record the app's storage",
			zap.String("app_id", app.ID), zap.Error(err))
		return handles
	}
	for _, rec := range records {
		handles[rec.VolumeID] = rec.Handle
	}
	r.Logger.Info("recorded storage the runtime holds for this app",
		zap.String("app_id", app.ID), zap.Int("volumes", len(records)))
	return handles
}

// settle records that an app is as it should be.
func (r *Reconciler) settle(ctx context.Context, app state.Reconcilable, to string) {
	if app.State != to {
		_ = r.Apps.SetState(ctx, app.ID, to)
	}
	// Only here. A flapping app that recovers between failures still
	// accumulates toward the threshold, because flapping is a failure mode.
	if to == state.StateRunning {
		_ = r.Reconciles.ClearFailures(ctx, app.ID)
	}
}

// holdStopped keeps a stopped app down (design 05 §1.1).
//
// The one place the reconciler stops something, and it is not an exception to
// the rule about not destroying things: desired_state is what a person asked
// for, and stopping is exactly what they asked for. Nothing is removed —
// volumes, the bundle and the spec all stay.
//
// It reports whether nothing was running, so nothing needed stopping.
func (r *Reconciler) holdStopped(ctx context.Context, app state.Reconcilable, runtime api.RuntimeAdapter, observed api.ObservedBundle) bool {
	running := false
	for _, w := range observed.Workloads {
		if w.Running {
			running = true
			break
		}
	}
	if !running {
		r.settle(ctx, app, state.StateStopped)
		return true
	}

	if err := runtime.Stop(ctx, api.BundleRef{BundleID: app.ID}); err != nil {
		r.attempt(ctx, app, runtime, reason(err))
		return false
	}
	_ = r.Auditor.Write(ctx, AuditEvent{
		Action: "app.stopped_by_reconciler",
		AppID:  app.ID,
		Detail: map[string]any{"reason": "desired_state is stopped and something was running"},
	})
	r.settle(ctx, app, state.StateStopped)
	return false
}

// correct applies the plan, with backoff and a give-up threshold.
//
// Every correction counts as an attempt, whether or not Apply returns an error,
// and that is the important part. A crash-looping app is the ordinary failure
// mode: the workload exists, it has exited, Pando recreates it, it exits again.
// Apply succeeds every time — the container really is created — so counting
// only Apply errors would correct that app every fifteen seconds forever and
// never reach R-150's threshold. The counter measures attempts; reaching
// running with health passing is what clears it (design 05 §2.2).
func (r *Reconciler) correct(ctx context.Context, app state.Reconcilable, runtime api.RuntimeAdapter, want api.BundlePlan, drift Drift, s *spec.AppSpec) {
	r.Logger.Info("correcting drift",
		zap.String("app_id", app.ID),
		zap.String("drift", drift.Describe()),
		zap.Int("previous_attempts", app.ConsecutiveFailures))

	// Asked again, immediately before acting. A pass reads its apps once and
	// works through them, and an Apply can wait minutes for a dependency to
	// come up — long enough for an app further down the list to be deleted
	// meanwhile. Applying it then re-created storage for an app that no
	// longer existed, after its teardown had run (issue #55).
	if r.Apps != nil {
		if live, err := r.Apps.Live(ctx, app.ID); err == nil && !live {
			return
		}
	}

	// The credential is resolved here, for the one apply, rather than on every
	// tick: for ECR it is minted by a call to AWS.
	if r.BuiltImageAuth != nil {
		for i := range want.Workloads {
			if want.Workloads[i].PullAuth == nil {
				want.Workloads[i].PullAuth = r.BuiltImageAuth(ctx, want.Workloads[i].Image)
			}
		}
	}

	// The shape was compared without environment; what is started needs it,
	// or a re-created workload comes up with none (R-148).
	if r.Environments != nil {
		envs, err := r.Environments.Environments(ctx, s)
		if err != nil {
			r.attempt(ctx, app, runtime, "could not prepare the app's configuration: "+reason(err))
			return
		}
		for i := range want.Workloads {
			if env, ok := envs[want.Workloads[i].Name]; ok {
				want.Workloads[i].Env = env
			}
		}
	}

	if _, err := runtime.Apply(ctx, want); err != nil {
		r.attempt(ctx, app, runtime, "could not start the app: "+reason(err))
		return
	}

	// What it now runs with is the current environment, so a rotated value
	// that this correction applied is not drift on the next tick (R-193).
	if r.Environments != nil && r.Secrets != nil {
		if versions, err := r.Secrets.Versions(ctx, app.ID); err == nil {
			_ = r.Reconciles.SetAppliedEnvFingerprint(ctx, app.ID, EnvHash(s, versions))
		}
	}

	if err := r.ensureRoute(ctx, app, s); err != nil {
		r.attempt(ctx, app, runtime, "could not route traffic to the app: "+reason(err))
		return
	}

	_ = r.Auditor.Write(ctx, AuditEvent{
		Action: "app.reconciled",
		AppID:  app.ID,
		Detail: map[string]any{"drift": drift.Describe(), "attempt": app.ConsecutiveFailures + 1},
	})

	// Not settled to running here, and counted as an attempt rather than a
	// success. The next observation is the only thing entitled to say whether
	// the correction held — and if it did, that is where the counter clears.
	r.attempt(ctx, app, runtime, "corrected: "+drift.Describe())
}

// ensureRoute re-points routing at Pando's proxy.
//
// R-023: routes put traffic in front of Pando's proxy and never at a workload.
// The reconciler re-ensuring a route is the easiest place in the system to get
// that wrong, because the workload's address is right there.
func (r *Reconciler) ensureRoute(ctx context.Context, app state.Reconcilable, s *spec.AppSpec) error {
	routing, ok := r.Registry.Routing(s.Routing.AdapterRef)
	if !ok {
		return nil
	}
	_, err := routing.Ensure(ctx, api.RouteRequest{
		AppID:         app.ID,
		Mode:          s.Routing.Mode,
		Hostname:      s.Routing.Hostname,
		PathPrefix:    s.Routing.PathPrefix,
		Port:          s.Routing.Port,
		ProxyUpstream: r.ProxyUpstream,
	})
	return err
}

// degrade records an unhealthy app and counts it toward the threshold.
//
// An app that matches its spec and is not healthy has nothing to converge —
// there is no drift — but it is not working either, and a permanently unhealthy
// app must reach `failed` rather than being reported as degraded forever.
func (r *Reconciler) degrade(ctx context.Context, app state.Reconcilable, runtime api.RuntimeAdapter, why string) {
	r.attempt(ctx, app, runtime, why)
}

// report records drift the reconciler must not act on (R-148).
//
// The app goes degraded and nothing is touched. No failure is counted: the app
// is not failing, Pando is declining to act, and counting that would march an
// app someone deliberately modified toward `failed`.
func (r *Reconciler) report(ctx context.Context, app state.Reconcilable, drift Drift) {
	if app.State != state.StateDegraded {
		_ = r.Apps.SetState(ctx, app.ID, state.StateDegraded)
		_ = r.Auditor.Write(ctx, AuditEvent{
			Action: "app.drift_unreconcilable",
			AppID:  app.ID,
			Detail: map[string]any{"drift": drift.Describe()},
		})
		r.notify(ctx, app, api.NotifyAppFailed,
			"This app has changed in a way Pando will not correct on its own",
			drift.Describe()+
				". Pando only creates and starts things — correcting this would mean removing or "+
				"overwriting something, and it cannot tell whether you put it there deliberately.")
	}
}

// attempt records one go at getting an app working, backs off, and gives up at
// the threshold (R-150).
//
// "Attempt" rather than "failure" because that is what is being counted. An app
// that needed correcting was not working; whether the correction returned an
// error is a detail of how it was not working. What resets the count is the app
// actually running with health passing, and nothing else.
func (r *Reconciler) attempt(ctx context.Context, app state.Reconcilable, runtime api.RuntimeAdapter, reason string) {
	count, err := r.Reconciles.RecordFailure(ctx, app.ID, reason, r.failureWindow(), r.nextAttempt(app.ConsecutiveFailures+1))
	if err != nil {
		r.Logger.Warn("could not record the attempt", zap.String("app_id", app.ID), zap.Error(err))
		return
	}

	if count < r.failureThreshold() {
		if app.State != state.StateDegraded {
			_ = r.Apps.SetState(ctx, app.ID, state.StateDegraded)
		}
		return
	}

	// R-150, and then stop. There is deliberately no path back from here that
	// does not involve a person (R-151).
	_ = r.Apps.SetState(ctx, app.ID, state.StateFailed)

	// And stop it, because "Pando has stopped trying to start this app" has to
	// be true. A crash-looping workload is restarted by the runtime, not by
	// Pando, so giving up on it left the app failed in the console and looping
	// on the host — the restart count climbing under a banner saying nothing
	// was trying any more.
	//
	// Not destructive and therefore not R-148's problem: the containers stay,
	// the storage stays, the app is one deploy away from running. What stops is
	// the churn.
	if runtime != nil {
		if err := runtime.Stop(ctx, api.BundleRef{BundleID: app.ID}); err != nil {
			r.Logger.Warn("could not stop an app Pando has given up on",
				zap.String("app_id", app.ID), zap.Error(err))
		}
	}

	_ = r.Auditor.Write(ctx, AuditEvent{
		Action: "app.failed",
		AppID:  app.ID,
		Detail: map[string]any{"failures": count, "window": r.failureWindow().String(), "reason": reason},
	})
	r.notify(ctx, app, api.NotifyAppFailed,
		"Pando has stopped trying to start this app",
		"It failed "+itoa(count)+" times in "+r.failureWindow().String()+". "+reason+
			" Pando will not try again on its own — fix what is wrong and deploy again.")
}

// unobservable records that Pando cannot see the app right now.
//
// Not a state, not a failure, and it does not touch the counter. An adapter
// being down is a platform problem: counting it would mark every app on the
// host as failed the next time the Docker daemon restarts.
func (r *Reconciler) unobservable(ctx context.Context, app state.Reconcilable, reason string) {
	if err := r.Reconciles.MarkUnobservable(ctx, app.ID, reason); err != nil {
		r.Logger.Warn("could not mark app unobservable",
			zap.String("app_id", app.ID), zap.Error(err))
	}
}

func (r *Reconciler) notify(ctx context.Context, app state.Reconcilable, kind api.NotificationKind, subject, body string) {
	if r.Notifier == nil {
		return
	}
	_ = r.Notifier.Notify(ctx, api.Notification{
		Kind:       kind,
		Subject:    subject,
		Body:       body,
		AppID:      app.ID,
		Recipients: []api.Recipient{{UserID: app.OwnerUserID}},
	})
}

// backoff is the configured schedule, or R-149's default.
func (r *Reconciler) backoff() []time.Duration {
	if len(r.Backoff) > 0 {
		return r.Backoff
	}
	return DefaultBackoff
}

func (r *Reconciler) failureThreshold() int {
	if r.FailureThreshold > 0 {
		return r.FailureThreshold
	}
	return DefaultFailureThreshold
}

func (r *Reconciler) failureWindow() time.Duration {
	if r.FailureWindow > 0 {
		return r.FailureWindow
	}
	return DefaultFailureWindow
}

// nextAttempt returns when this app may be tried again.
func (r *Reconciler) nextAttempt(failures int) time.Time {
	schedule := r.backoff()
	index := failures
	if index >= len(schedule) {
		index = len(schedule) - 1
	}
	return r.now().Add(schedule[index])
}

func (r *Reconciler) now() time.Time {
	if r.Clock == nil {
		return time.Now().UTC()
	}
	return r.Clock.Now()
}

// observedHealthy reports whether every workload is running and settled.
//
// A nil Healthy means no signal, which is not unhealthy (R-221). An app with no
// health check is running, not perpetually degraded — which is also why
// auto-rollback defaults off (R-147).
//
// Restarting counts as not healthy (design 05 §1.1: degraded is "health failing
// **or restarting**"), and getting that wrong is subtle. Pando sets a restart
// policy on its containers, so the runtime restarts a crashed one on its own —
// which is wanted, because it recovers faster than a fifteen-second tick and
// keeps working while Pando is away. The cost is that a crash-looping app is
// briefly `Running` between crashes, and a tick landing in that window would
// read it as recovered and clear the failure count. The app would then never
// reach `failed`: every glimpse of it up would undo the progress toward giving
// up on it.
//
// A workload that has restarted before and started again moments ago is in a
// loop, not recovered. Once it stays up past the settle window it is treated as
// healthy, which is exactly the distinction being drawn.
func observedHealthy(observed api.ObservedBundle, now time.Time) bool {
	for _, w := range observed.Workloads {
		if !w.Running {
			return false
		}
		if w.Healthy != nil && !*w.Healthy {
			return false
		}

		// The runtime saying so, first. Docker reports Running and Restarting
		// together, and a crash-looping container is both.
		if w.Restarting {
			return false
		}

		// For a runtime that cannot tell: a workload which has restarted before
		// and started again moments ago is looping, not recovered. Weaker than
		// the explicit signal — a restart loop whose backoff has stretched past
		// this window looks settled — which is exactly why the explicit signal
		// is checked first.
		if w.RestartCount > 0 && !w.StartedAt.IsZero() && now.Sub(w.StartedAt) < RestartSettleWindow {
			return false
		}
	}
	return true
}

// RestartSettleWindow is how long a workload that has restarted must stay up
// before it counts as recovered rather than looping. [P].
const RestartSettleWindow = 30 * time.Second

// reason renders an adapter's error for a person to read.
//
// errs.As returns nil for an error that carries no envelope, and an adapter is
// under no obligation to produce one — it is ordinary Go code that can return
// anything. Dereferencing that nil crashed the reconciler goroutine, and a
// panic in a goroutine takes the whole process with it: one adapter returning a
// plain error would stop Pando, including for every app that was fine.
func reason(err error) string {
	if err == nil {
		return ""
	}
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return err.Error()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
