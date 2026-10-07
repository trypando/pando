//go:build integration

package reconciler_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// --- harness ----------------------------------------------------------------

// connected is a fresh, migrated database of the test's own (statetest).
func connected(t *testing.T) *state.DB {
	t.Helper()
	db, _ := statetest.Connect(t)
	return db
}

// seedOwner creates a user for apps to belong to: apps.owner_user_id is a
// foreign key, and an app with no owner is a different test.
func seedOwner(t *testing.T, db *state.DB) string {
	t.Helper()
	ctx := context.Background()

	users := state.NewUsers(db)
	require.NoError(t, users.EnsureLocalAdapter(ctx))

	digest, err := hash.New(secret.New("correct-password"))
	require.NoError(t, err)

	u, err := users.Create(ctx, state.LocalAdapterID, "reconciler-owner",
		"reconciler-owner@example.test", "Reconciler Owner", digest, false)
	require.NoError(t, err)
	return u.ID
}

// fakeRuntime records what it was asked to do and reports what it is told to.
type fakeRuntime struct {
	mu sync.Mutex

	observed   api.ObservedBundle
	observeErr error

	applies  int
	stops    int
	observes int

	// applyErr fails Apply, which is how a repeatedly-unstartable app is
	// simulated without needing a real container that refuses to boot.
	applyErr error

	panicOnObserve bool
}

func (f *fakeRuntime) Kind() string                                     { return "fake" }
func (f *fakeRuntime) Category() api.Category                           { return api.CategoryRuntime }
func (f *fakeRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeRuntime) HealthCheck(context.Context) error                { return nil }
func (f *fakeRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsPrivateNetwork: true}, nil
}
func (f *fakeRuntime) Capacity(context.Context) (api.Capacity, error) { return api.Capacity{}, nil }
func (f *fakeRuntime) LargestFitFor(context.Context, string) (*api.Fit, error) {
	return nil, nil
}
func (f *fakeRuntime) InUse(context.Context) (api.InUse, error) { return api.InUse{}, nil }

func (f *fakeRuntime) Usage(context.Context, api.BundleRef) (api.BundleUsage, error) {
	return api.BundleUsage{}, nil
}

func (f *fakeRuntime) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observes++
	if f.panicOnObserve {
		panic("an adapter that panics is ordinary Go, not a hypothetical")
	}
	return f.observed, f.observeErr
}

func (f *fakeRuntime) Apply(context.Context, api.BundlePlan) (api.BundleHandle, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applies++
	if f.applyErr != nil {
		return api.BundleHandle{}, f.applyErr
	}
	return api.BundleHandle{}, nil
}

func (f *fakeRuntime) Stop(context.Context, api.BundleRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops++
	return nil
}

func (f *fakeRuntime) counts() (observes, applies, stops int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.observes, f.applies, f.stops
}

func (f *fakeRuntime) setObserved(o api.ObservedBundle) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.observed = o
}

func (f *fakeRuntime) Destroy(context.Context, api.BundleRef, api.DestroyOptions) error { return nil }
func (f *fakeRuntime) CreateVolume(context.Context, api.VolumeRequest) (api.VolumeHandle, error) {
	return api.VolumeHandle{}, nil
}
func (f *fakeRuntime) DestroyVolume(context.Context, api.VolumeHandle) error { return nil }
func (f *fakeRuntime) SnapshotVolume(context.Context, api.VolumeHandle, io.Writer) error {
	return nil
}
func (f *fakeRuntime) RestoreVolume(context.Context, api.VolumeHandle, io.Reader) error { return nil }
func (f *fakeRuntime) ImportImage(context.Context, io.Reader) (string, error)           { return "", nil }
func (f *fakeRuntime) Logs(context.Context, api.WorkloadRef, api.LogOptions) (io.ReadCloser, error) {
	return nil, nil
}
func (f *fakeRuntime) Exec(context.Context, api.WorkloadRef, api.ExecRequest) (api.ExecSession, error) {
	return nil, nil
}
func (f *fakeRuntime) Upstream(context.Context, api.WorkloadRef, int) (api.Upstream, error) {
	return api.Upstream{}, nil
}
func (f *fakeRuntime) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{}, nil
}

type fakeRegistry struct{ runtime *fakeRuntime }

func (r fakeRegistry) Runtime(string) (api.RuntimeAdapter, bool) { return r.runtime, true }
func (r fakeRegistry) Routing(string) (api.RoutingAdapter, bool) { return nil, false }

type recordingAuditor struct {
	mu     sync.Mutex
	events []reconciler.AuditEvent
}

func (a *recordingAuditor) Write(_ context.Context, e reconciler.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return nil
}

func (a *recordingAuditor) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

type harness struct {
	db      *state.DB
	apps    *state.Apps
	runtime *fakeRuntime
	auditor *recordingAuditor
	rec     *reconciler.Reconciler
	appID   string
}

func newHarness(t *testing.T, appState string) *harness {
	t.Helper()
	ctx := context.Background()

	db := connected(t)
	apps := state.NewApps(db)
	owner := seedOwner(t, db)

	app, err := apps.Create(ctx, "rec-"+id.New(id.App), id.New(id.App), owner, owner,
		spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"})
	require.NoError(t, err)

	s := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         app.ID,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{
			Name: "web", Image: "example/app:1", Primary: true, Exposed: true,
		}},
		Routing: spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime: spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:  spec.Deploy{Strategy: spec.DeployRecreate},
	}
	rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginDetected, "")
	require.NoError(t, err)
	require.NoError(t, apps.Pin(ctx, app.ID, rev.ID, appState, ""))

	// A successful deploy sets this (deploy.go), and apps default to stopped.
	// Without it every app here would be one the reconciler correctly keeps
	// down, and every test below would be testing that instead.
	if appState != state.StateStopped {
		require.NoError(t, apps.SetDesiredState(ctx, app.ID, "running"))
	}

	runtime := &fakeRuntime{}
	auditor := &recordingAuditor{}

	return &harness{
		db: db, apps: apps, runtime: runtime, auditor: auditor, appID: app.ID,
		rec: &reconciler.Reconciler{
			Apps:       apps,
			Reconciles: state.NewReconciles(db),
			Secrets:    state.NewSecrets(db, nil, ""),
			Volumes:    state.NewVolumes(db),
			Registry:   fakeRegistry{runtime: runtime},
			Auditor:    auditor,
			Logger:     zap.NewNop(),
		},
	}
}

func (h *harness) state(t *testing.T) string {
	t.Helper()
	app, found, err := h.apps.ByID(context.Background(), h.appID)
	require.NoError(t, err)
	require.True(t, found)
	return app.State
}

func healthy() api.ObservedBundle {
	ok := true
	return api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: true, Healthy: &ok},
	}}
}

// --- the requirements -------------------------------------------------------

// R-151: a failed app stays failed.
//
// The mechanism is the absence of a code path, so the test is an absence too:
// the reconciler must not so much as *look* at a failed app. Asserting on state
// alone would pass even if it were being observed, corrected and coincidentally
// left where it was.
func TestR151_AFailedAppIsNeverTouched(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateFailed)
	h.runtime.setObserved(api.ObservedBundle{Exists: false})

	for range 5 {
		h.rec.Tick(context.Background())
	}

	observes, applies, stops := h.runtime.counts()
	require.Zero(t, observes, "a failed app is not even observed")
	require.Zero(t, applies)
	require.Zero(t, stops)
	require.Equal(t, state.StateFailed, h.state(t))
	require.Empty(t, h.auditor.actions())
}

// An app that is running and matches its spec needs nothing done to it.
func TestARunningAppThatMatchesIsLeftAlone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.setObserved(healthy())

	h.rec.Tick(context.Background())

	_, applies, stops := h.runtime.counts()
	require.Zero(t, applies, "nothing to converge")
	require.Zero(t, stops)
	require.Equal(t, state.StateRunning, h.state(t))
}

// The phase's "done when", first half: killing a container by hand restores it.
func TestAKilledWorkloadIsRestored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.setObserved(api.ObservedBundle{Exists: true})

	h.rec.Tick(context.Background())

	_, applies, _ := h.runtime.counts()
	require.Equal(t, 1, applies, "the missing workload is recreated")
	require.Contains(t, h.auditor.actions(), "app.reconciled")

	// It does not claim success. The next tick observes whether it worked,
	// which is the only thing entitled to say so.
	require.Equal(t, state.StateDegraded, h.state(t))

	h.rec.Clock = &steppingClock{now: time.Now().UTC().Add(6 * time.Minute)}
	h.runtime.setObserved(healthy())
	h.rec.Tick(context.Background())
	require.Equal(t, state.StateRunning, h.state(t))
}

// The second half, and the one that matters: killing it repeatedly reaches
// failed and stays there (R-150, R-151).
func TestR150_RepeatedFailureReachesFailedAndStops(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.setObserved(api.ObservedBundle{Exists: true})
	h.runtime.applyErr = errApplyFailed

	// Backoff would otherwise spread ten failures over minutes.
	h.rec.Clock = &steppingClock{now: time.Now().UTC()}

	for range reconciler.DefaultFailureThreshold {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(10 * time.Minute)
	}

	require.Equal(t, state.StateFailed, h.state(t))
	require.Contains(t, h.auditor.actions(), "app.failed")

	// And stops. R-151 is the absence of a retry path, not a longer interval.
	_, appliesBefore, _ := h.runtime.counts()
	for range 5 {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(time.Hour)
	}
	_, appliesAfter, _ := h.runtime.counts()
	require.Equal(t, appliesBefore, appliesAfter,
		"nothing retries a failed app, however long you wait")
	require.Equal(t, state.StateFailed, h.state(t))
}

// TestR150_GivingUpStopsTheApp asserts R-150.
//
// "Pando has stopped trying to start this app" has to be true of the host as
// well as of the state machine. A crash-looping workload was being restarted by
// the container runtime rather than by Pando, so an app Pando had given up on
// kept looping underneath the message — the restart count climbing on a screen
// that said nothing was trying any more.
func TestR150_GivingUpStopsTheApp(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.setObserved(api.ObservedBundle{Exists: true})
	h.runtime.applyErr = errApplyFailed
	h.rec.Clock = &steppingClock{now: time.Now().UTC()}

	_, _, before := h.runtime.counts()

	for range reconciler.DefaultFailureThreshold {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(10 * time.Minute)
	}

	require.Equal(t, state.StateFailed, h.state(t))

	_, _, after := h.runtime.counts()
	require.Greater(t, after, before, "the app is stopped, not left looping")
}

// An adapter being down is a platform problem, not app failure. Otherwise
// restarting the Docker daemon marks every app on the host as failed.
func TestAnUnreachableAdapterDoesNotMoveAnAppTowardFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.observeErr = errAdapterDown
	h.rec.Clock = &steppingClock{now: time.Now().UTC()}

	for range reconciler.DefaultFailureThreshold * 2 {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(10 * time.Minute)
	}

	require.Equal(t, state.StateRunning, h.state(t),
		"the app has not changed; Pando has merely stopped being able to see it")
	require.NotContains(t, h.auditor.actions(), "app.failed")

	var since *time.Time
	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT unobservable_since FROM apps WHERE id = $1`, h.appID).Scan(&since))
	require.NotNil(t, since, "it is recorded, as a platform problem")

	// And clears on the first successful observation.
	h.runtime.observeErr = nil
	h.runtime.setObserved(healthy())
	h.rec.Tick(context.Background())

	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT unobservable_since FROM apps WHERE id = $1`, h.appID).Scan(&since))
	require.Nil(t, since)
}

// R-148: report, do not guess. A volume that held data is never recreated, and
// the presence of one unreconcilable difference stops the whole app being
// touched — the reconcilable half might be what destroys the evidence.
func TestR148_UnreconcilableDriftIsReportedAndNothingIsApplied(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)

	// A volume that was materialized, and is now gone.
	volumes := state.NewVolumes(h.db)
	volumeID, err := volumes.Create(ctx, h.appID, "data", "rt_fake")
	require.NoError(t, err)
	_, err = h.db.Exec(ctx, `UPDATE volumes SET handle = 'v1' WHERE id = $1`, volumeID)
	require.NoError(t, err)

	rev, _, err := h.apps.RevisionByID(ctx, mustPinned(t, h))
	require.NoError(t, err)
	rev.Body.Volumes = []spec.Volume{{ID: volumeID, Name: "data", Declared: spec.VolumeFromUser}}
	next, err := h.apps.CreateRevision(ctx, h.appID, rev.Body, spec.OriginEdited, "")
	require.NoError(t, err)
	require.NoError(t, h.apps.Pin(ctx, h.appID, next.ID, state.StateRunning, ""))

	h.runtime.setObserved(healthy()) // workload fine, volume absent

	h.rec.Tick(ctx)

	_, applies, _ := h.runtime.counts()
	require.Zero(t, applies,
		"an empty replacement volume would look healthy while the data it held is lost")
	require.Equal(t, state.StateDegraded, h.state(t))
	require.Contains(t, h.auditor.actions(), "app.drift_unreconcilable")
}

// desired_state is what a person asked for, and it outranks everything.
func TestAStoppedAppIsKeptStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)
	require.NoError(t, h.apps.SetDesiredState(ctx, h.appID, "stopped"))
	h.runtime.setObserved(healthy())

	h.rec.Tick(ctx)

	_, applies, stops := h.runtime.counts()
	require.Equal(t, 1, stops, "something was running and desired_state is stopped")
	require.Zero(t, applies, "keeping it down is not the same as converging it")
	require.Equal(t, state.StateStopped, h.state(t))
}

var (
	errApplyFailed = errors.New("the container exited immediately")
	errAdapterDown = errors.New("cannot connect to the Docker daemon")
)

func mustPinned(t *testing.T, h *harness) string {
	t.Helper()
	app, found, err := h.apps.ByID(context.Background(), h.appID)
	require.NoError(t, err)
	require.True(t, found)
	return app.PinnedSpecID
}

// steppingClock lets a test walk through a thirty-minute failure window without
// spending thirty minutes in it.
type steppingClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *steppingClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *steppingClock) Since(t time.Time) time.Duration { return c.Now().Sub(t) }
func (c *steppingClock) After(time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	ch <- c.Now()
	return ch
}
func (c *steppingClock) Sleep(time.Duration) {}

func (c *steppingClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// A panic in one app's reconciliation must not take the process down.
//
// The loop runs forever and calls into adapter code on every tick, which is
// ordinary Go that can panic. A panic in a goroutine kills the process, so one
// adapter with a nil map would stop Pando for every app including the healthy
// ones. This is not hypothetical — the first version of the failure path
// dereferenced the nil that errs.As returns for an unenveloped error.
func TestAPanickingAdapterDoesNotTakeDownTheLoop(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.runtime.panicOnObserve = true

	require.NotPanics(t, func() { h.rec.Tick(context.Background()) })

	// And the next tick carries on, because the panic might have been
	// transient and the app is still there either way.
	h.runtime.panicOnObserve = false
	h.runtime.setObserved(healthy())
	require.NotPanics(t, func() { h.rec.Tick(context.Background()) })
	require.Equal(t, state.StateRunning, h.state(t))
}

// A crash-looping app is the ordinary failure mode, and the one that exposed
// the counter measuring the wrong thing.
//
// The workload exists, it has exited, Pando recreates it, it exits again. Apply
// succeeds every time — the container really is created — so counting only
// Apply errors corrects this app every fifteen seconds forever and never
// reaches R-150's threshold. What is counted is attempts; what clears them is
// the app actually running with health passing.
func TestR150_ACrashLoopingAppReachesFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.rec.Clock = &steppingClock{now: time.Now().UTC()}

	exited := 1
	crashed := api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: false, ExitCode: &exited},
	}}
	h.runtime.setObserved(crashed)
	// Apply succeeds every time. The container is created; it just dies again.

	for range reconciler.DefaultFailureThreshold {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(6 * time.Minute)
	}

	require.Equal(t, state.StateFailed, h.state(t),
		"an app that is recreated and dies again must eventually be given up on")
	require.Contains(t, h.auditor.actions(), "app.failed")
}

// The counter measures attempts, and only a working app clears it.
func TestACorrectionThatHoldsClearsTheCounter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)

	h.rec.Clock = &steppingClock{now: time.Now().UTC()}
	h.runtime.setObserved(api.ObservedBundle{Exists: true})
	h.rec.Tick(ctx)

	var failures int
	require.NoError(t, h.db.QueryRow(ctx,
		`SELECT consecutive_failures FROM apps WHERE id = $1`, h.appID).Scan(&failures))
	require.Equal(t, 1, failures, "the correction is an attempt until something confirms it")

	// Past the backoff, so the app is due again.
	h.rec.Clock.(*steppingClock).advance(6 * time.Minute)
	h.runtime.setObserved(healthy())
	h.rec.Tick(ctx)

	require.NoError(t, h.db.QueryRow(ctx,
		`SELECT consecutive_failures FROM apps WHERE id = $1`, h.appID).Scan(&failures))
	require.Zero(t, failures, "running with health passing is the only thing that clears it")
	require.Equal(t, state.StateRunning, h.state(t))
}

// An app that is unhealthy but matches its spec has nothing to converge, and
// must still not sit in degraded forever.
func TestAPermanentlyUnhealthyAppReachesFailed(t *testing.T) {
	t.Parallel()
	h := newHarness(t, state.StateRunning)
	h.rec.Clock = &steppingClock{now: time.Now().UTC()}

	sick := false
	h.runtime.setObserved(api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: true, Healthy: &sick},
	}})

	for range reconciler.DefaultFailureThreshold {
		h.rec.Tick(context.Background())
		h.rec.Clock.(*steppingClock).advance(6 * time.Minute)
	}

	require.Equal(t, state.StateFailed, h.state(t))

	_, applies, _ := h.runtime.counts()
	require.Zero(t, applies, "there was no drift to correct; the app itself is unwell")
}

// A crash-looping app is briefly up between crashes, and a tick landing in that
// window must not read it as recovered.
//
// Pando sets a restart policy on its containers — wanted, because it recovers
// faster than a fifteen-second tick and keeps working while Pando is away — so
// this window genuinely exists on every crash-looping app. Treating it as
// recovery clears the failure count, and the app then never reaches `failed`:
// every glimpse of it up undoes the progress toward giving up on it.
func TestAnAppSeenBrieflyUpBetweenCrashesIsNotCountedAsRecovered(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	h := newHarness(t, state.StateRunning)
	h.rec.Clock = &steppingClock{now: now}

	// Up, healthy as far as any health check knows, and started a second ago
	// having already restarted eleven times.
	h.runtime.setObserved(api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: true, RestartCount: 11, StartedAt: now.Add(-time.Second)},
	}})

	h.rec.Tick(context.Background())

	var failures int
	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT consecutive_failures FROM apps WHERE id = $1`, h.appID).Scan(&failures))
	require.Equal(t, 1, failures, "a container that just restarted for the eleventh time is looping")
	require.Equal(t, state.StateDegraded, h.state(t))
}

// And once it stays up, it is recovered.
func TestAnAppThatStaysUpAfterRestartingIsRecovered(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	h := newHarness(t, state.StateRunning)
	h.rec.Clock = &steppingClock{now: now}

	h.runtime.setObserved(api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: true, RestartCount: 11,
			StartedAt: now.Add(-10 * time.Minute)},
	}})

	h.rec.Tick(context.Background())
	require.Equal(t, state.StateRunning, h.state(t),
		"a history of restarts is not the same as restarting now")
}

// The runtime saying "restarting" outranks every heuristic.
//
// Docker reports Running=true and Restarting=true together, and once the
// restart backoff stretches past the settle window the heuristic alone reads a
// looping container as settled. That happened on a real crash-looping app at
// twenty restarts: Running=true, Restarting=true, StartedAt 40 seconds ago, and
// the failure count reset instead of climbing.
func TestARuntimeReportingRestartingOutranksTheSettleWindow(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	h := newHarness(t, state.StateRunning)
	h.rec.Clock = &steppingClock{now: now}

	h.runtime.setObserved(api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{
		{
			Name: "web", Present: true,
			Running: true, Restarting: true, RestartCount: 20,
			// Past the settle window, which is how this got through before.
			StartedAt: now.Add(-40 * time.Second),
		},
	}})

	h.rec.Tick(context.Background())

	var failures int
	require.NoError(t, h.db.QueryRow(context.Background(),
		`SELECT consecutive_failures FROM apps WHERE id = $1`, h.appID).Scan(&failures))
	require.Equal(t, 1, failures)
	require.Equal(t, state.StateDegraded, h.state(t))
}

func (f *fakeRuntime) ApplyEdge(context.Context, api.EdgePlan) error { return nil }
func (f *fakeRuntime) ObserveEdge(context.Context, string) (api.EdgeState, error) {
	return api.EdgeState{}, nil
}
func (f *fakeRuntime) RemoveEdge(context.Context, string) error { return nil }
func (f *fakeRuntime) Edges(context.Context) ([]string, error)  { return nil, nil }
func (f *fakeRuntime) EdgeVolumes(context.Context) ([]api.VolumeHandle, error) {
	return nil, nil
}

// TestR203_TheReconcilerRecordsStorageTheRuntimeHolds asserts that Pando's
// record of an app's storage is brought back in line with the runtime without
// waiting for a deploy (issue #87).
//
// Before volume rows were keyed by app, a second app with a volume called
// "data" got no row and pointed the first app's row at its own volume. With no
// row, R-203 did not know the volume had ever held data — so a lost volume
// would be recreated empty — and rolling backups (R-211) found nothing to copy.
// Nothing asks for a redeploy, so the loop that already observes every app
// puts the record right.
func TestR203_TheReconcilerRecordsStorageTheRuntimeHolds(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)

	// The harness's app, keeping its data in a volume called "data".
	s := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		AppID:         h.appID,
		Source:        spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Ref: "main"},
		Build:         spec.Build{Strategy: spec.BuildPrebuilt},
		Workloads: []spec.Workload{{Name: "web", Image: "example/app:1", Primary: true, Exposed: true,
			Mounts: []spec.Mount{{VolumeID: "data", Path: "/data"}}}},
		Volumes: []spec.Volume{{ID: "data", Name: "data"}},
		Routing: spec.Routing{AdapterRef: "rte_fake", Mode: spec.RoutingPort, Port: 9000},
		Runtime: spec.RuntimeRef{AdapterRef: "rt_fake", IsolationFloor: spec.IsolationContainer},
		Deploy:  spec.Deploy{Strategy: spec.DeployRecreate},
	}
	rev, err := h.apps.CreateRevision(ctx, h.appID, s, spec.OriginEdited, "")
	require.NoError(t, err)
	require.NoError(t, h.apps.Pin(ctx, h.appID, rev.ID, state.StateRunning, ""))

	volumes := state.NewVolumes(h.db)
	// A row pointing at somebody else's volume, as the collision left it.
	require.NoError(t, volumes.RecordFromRuntime(ctx, h.appID, "rt_fake",
		[]state.VolumeRecord{{VolumeID: "data", Name: "data", Handle: "pando-app_other-data"}}))

	observed := healthy()
	observed.Volumes = []api.ObservedVolume{{VolumeID: "data", Present: true, Handle: "pando-" + h.appID + "-data"}}
	h.runtime.setObserved(observed)

	h.rec.Tick(ctx)

	handles, err := volumes.Handles(ctx, h.appID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"data": "pando-" + h.appID + "-data"}, handles,
		"the record names the volume the runtime actually holds for this app")
}
