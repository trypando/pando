package standby

import (
	"cmp"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type fakeStore struct {
	apps    []state.StandbyApp
	rows    map[string]state.Standby
	deleted []string
	states  map[string]string
	getErr  error
	listErr error
	// lost counts writes refused because the row changed since it was read.
	lost int
	// stateRead runs after each AppState, for what happens meanwhile.
	stateRead func()
	// discard is each deleted app's choice to discard its storage (R-204).
	discard map[string]bool
}

func (f *fakeStore) Candidates(context.Context) ([]state.StandbyApp, error) {
	out := make([]state.StandbyApp, 0, len(f.apps))
	for _, a := range f.apps {
		if sb, ok := f.rows[a.AppID]; ok {
			sb := sb
			a.Standby = &sb
		}
		out = append(out, a)
	}
	return out, nil
}

func (f *fakeStore) Get(_ context.Context, appID string) (state.Standby, bool, error) {
	if f.getErr != nil {
		return state.Standby{}, false, f.getErr
	}
	sb, ok := f.rows[appID]
	return sb, ok, nil
}

func (f *fakeStore) SaveIf(_ context.Context, sb state.Standby, prev *state.Standby) (bool, error) {
	if f.rows == nil {
		f.rows = map[string]state.Standby{}
	}
	have, ok := f.rows[sb.AppID]
	if (prev == nil && ok) || (prev != nil && (!ok || have.Version != prev.Version)) {
		f.lost++
		return false, nil
	}
	// Every write changes the row's version, as migration 000094's trigger
	// does.
	sb.Version = have.Version + 1
	f.rows[sb.AppID] = sb
	return true, nil
}

func (f *fakeStore) Fate(_ context.Context, appID string) (state.AppFate, error) {
	for _, a := range f.apps {
		if a.AppID == appID && a.Deleted {
			return state.AppFate{Deleted: true, DiscardStorage: f.discard[appID]}, nil
		}
	}
	return state.AppFate{}, nil
}

func (f *fakeStore) Delete(_ context.Context, appID string) error {
	if sb, ok := f.rows[appID]; ok && sb.PromotingAt != nil {
		return nil
	}
	delete(f.rows, appID)
	f.deleted = append(f.deleted, appID)
	return nil
}

func (f *fakeStore) AppState(_ context.Context, appID string) (string, bool, error) {
	st, ok := f.states[appID]
	if f.stateRead != nil {
		f.stateRead()
	}
	return st, ok, nil
}

// SaveIfLive is SaveIf that also refuses a failed or deleted app, as
// state.Standbys does.
func (f *fakeStore) SaveIfLive(ctx context.Context, sb state.Standby, prev *state.Standby) (bool, error) {
	if st, ok := f.states[sb.AppID]; !ok || st == state.StateFailed {
		f.lost++
		return false, nil
	}
	return f.SaveIf(ctx, sb, prev)
}

type fakePlans struct {
	specs     map[string]*spec.AppSpec
	completed int
	shaped    int
	err       error
}

func (f *fakePlans) Shape(_ context.Context, appID string) (api.BundlePlan, *spec.AppSpec, error) {
	f.shaped++
	s, ok := f.specs[appID]
	if !ok {
		return api.BundlePlan{}, nil, errs.New(errs.NotFound, "no app")
	}
	p := api.BundlePlan{BundleID: appID}
	for _, v := range s.Volumes {
		p.Volumes = append(p.Volumes, api.VolumePlan{VolumeID: v.ID, Name: v.Name})
	}
	return p, s, nil
}

func (f *fakePlans) Complete(context.Context, *api.BundlePlan, *spec.AppSpec) error {
	f.completed++
	return f.err
}

// fakeRuntime is a runtime that keeps standbys on "app-2".
type fakeRuntime struct {
	api.RuntimeAdapter
	supports  bool
	applied   []api.StandbyRequest
	shipped   []api.ShipRequest
	promoted  []api.PromoteRequest
	removed   []string
	stale     []string
	applyErr  error
	shipErr   error
	promoteOK api.PromoteResult
	// onApply and onPromote run inside ApplyStandby and PromoteStandby, for
	// what happens meanwhile; promoteFail is returned with promoteOK.
	onApply     func()
	onPromote   func()
	promoteFail error
	// homeName is where ApplyStandby says the app runs; empty is app-1.
	homeName string
	// promoteErr refuses the promotion of an app; staleErr every removal of
	// a stale copy.
	promoteErr map[string]error
	staleErr   error
	// removeErr is what RemoveStandby reports, after removing what it
	// reached; staleKept and staleDeleted are each stale removal's options.
	removeErr    error
	staleKept    []bool
	staleDeleted []bool
}

func (f *fakeRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsStandby: f.supports}, nil
}

func (f *fakeRuntime) ApplyStandby(_ context.Context, req api.StandbyRequest) (api.StandbyHandle, error) {
	f.applied = append(f.applied, req)
	if f.onApply != nil {
		f.onApply()
	}
	if f.applyErr != nil {
		return api.StandbyHandle{}, f.applyErr
	}
	return api.StandbyHandle{Host: "app-2", Home: cmp.Or(f.homeName, "app-1")}, nil
}

func (f *fakeRuntime) ShipVolumes(_ context.Context, req api.ShipRequest) (api.ShipResult, error) {
	f.shipped = append(f.shipped, req)
	if f.shipErr != nil {
		return api.ShipResult{}, f.shipErr
	}
	return api.ShipResult{Copies: min(len(f.shipped), 3)}, nil
}

func (f *fakeRuntime) PromoteStandby(_ context.Context, req api.PromoteRequest) (api.PromoteResult, error) {
	f.promoted = append(f.promoted, req)
	if f.onPromote != nil {
		f.onPromote()
	}
	if err := f.promoteErr[req.Bundle.BundleID]; err != nil {
		return api.PromoteResult{}, err
	}
	return f.promoteOK, f.promoteFail
}

func (f *fakeRuntime) RemoveStandby(_ context.Context, ref api.BundleRef) error {
	f.removed = append(f.removed, ref.BundleID)
	return f.removeErr
}

func (f *fakeRuntime) RemoveStaleCopy(_ context.Context, ref api.BundleRef, host string, opts api.StaleCopyOptions) error {
	if f.staleErr != nil {
		return f.staleErr
	}
	f.stale = append(f.stale, ref.BundleID+"@"+host)
	f.staleKept = append(f.staleKept, opts.KeepVolumes)
	f.staleDeleted = append(f.staleDeleted, opts.AppDeleted)
	return nil
}

type fakeRuntimes map[string]api.RuntimeAdapter

func (f fakeRuntimes) Runtime(ref string) (api.RuntimeAdapter, bool) {
	rt, ok := f[ref]
	return rt, ok
}

type staticPolicy struct{ doc policy.Document }

func (p staticPolicy) Load(context.Context) (policy.Document, error) { return p.doc, nil }

type fakeAuditor struct {
	actions []string
	// fail refuses every write, recording nothing.
	fail bool
}

func (f *fakeAuditor) Write(_ context.Context, e audit.Event) error {
	if f.fail {
		return errors.New("audit log away")
	}
	f.actions = append(f.actions, e.Action)
	return nil
}

type fakeServices struct{ api.ServicesAdapter }

func (fakeServices) Capabilities() api.ServicesCapabilities {
	return api.ServicesCapabilities{ReplicaTypes: []spec.SlotType{spec.SlotPostgres}}
}

type servicesFor struct{}

func (servicesFor) ServicesFor(spec.SlotType) (api.ServicesAdapter, string, bool) {
	return fakeServices{}, "svc", true
}

func appSpec(standby spec.StandbyMode, volumes int) *spec.AppSpec {
	s := &spec.AppSpec{Runtime: spec.RuntimeRef{AdapterRef: "rt_hosts"}, Availability: spec.Availability{Standby: standby}}
	for i := range volumes {
		s.Volumes = append(s.Volumes, spec.Volume{ID: "vol_" + string(rune('a'+i)), Name: "data"})
	}
	return s
}

type harness struct {
	store   *fakeStore
	plans   *fakePlans
	rt      *fakeRuntime
	clock   *clock.Fake
	auditor *fakeAuditor
	svc     *Service
	policy  policy.Document
}

func newHarness(s *spec.AppSpec) *harness {
	h := &harness{
		store: &fakeStore{
			apps:   []state.StandbyApp{{AppID: "app_a", State: state.StateRunning, DesiredState: "running", PinnedSpecID: "spec_1"}},
			rows:   map[string]state.Standby{},
			states: map[string]string{"app_a": state.StateRunning},
		},
		plans:   &fakePlans{specs: map[string]*spec.AppSpec{"app_a": s}},
		rt:      &fakeRuntime{supports: true},
		clock:   clock.NewFake(time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)),
		auditor: &fakeAuditor{},
		policy:  policy.Default(),
	}
	h.svc = &Service{
		Store: h.store, Plans: h.plans, Runtimes: fakeRuntimes{"rt_hosts": h.rt},
		Policy: staticPolicy{h.policy}, Apps: h.store, Auditor: h.auditor, Clock: h.clock,
	}
	return h
}

// TestR419_AnAppGetsAStandbyAndItsVolumesAreShippedOnAnInterval asserts the
// pass makes a standby once per pinned spec and ships a copy every five
// minutes [P].
func TestR419_AnAppGetsAStandbyAndItsVolumesAreShippedOnAnInterval(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.svc.Once(ctx)
	require.Len(t, h.rt.applied, 1)
	require.Len(t, h.rt.shipped, 1)
	require.Equal(t, []string{"vol_a"}, h.rt.shipped[0].VolumeIDs)
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Host)
	require.Equal(t, "spec_1", sb.SpecID)
	require.Equal(t, "rt_hosts", sb.RuntimeRef)
	require.Equal(t, 1, sb.Copies)
	require.Equal(t, []string{"app.standby.create"}, h.auditor.actions)

	// A minute later nothing is due.
	h.clock.Advance(time.Minute)
	h.svc.Once(ctx)
	require.Len(t, h.rt.applied, 1)
	require.Len(t, h.rt.shipped, 1)

	// Five minutes after the copy, the next.
	h.clock.Advance(4 * time.Minute)
	h.svc.Once(ctx)
	require.Len(t, h.rt.shipped, 2)

	// A new pinned spec makes the standby again, where it was.
	h.store.apps[0].PinnedSpecID = "spec_2"
	h.svc.Once(ctx)
	require.Len(t, h.rt.applied, 2)
	require.Equal(t, "app-2", h.rt.applied[1].Host)
	require.Equal(t, 2, h.plans.completed, "the environment is resolved only to create")
}

// TestR419_TheStandbyIsTurnedOffBySpecOverride asserts the spec outranks
// host policy both ways (R-020).
func TestR419_TheStandbyIsTurnedOffBySpecOverride(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 0))
	h.svc.Once(ctx)
	require.Contains(t, h.store.rows, "app_a")
	require.Empty(t, h.rt.shipped, "no volumes, nothing to ship")

	h.plans.specs["app_a"] = appSpec(spec.StandbyOff, 0)
	h.svc.Once(ctx)
	require.Equal(t, []string{"app_a"}, h.rt.removed)
	require.NotContains(t, h.store.rows, "app_a")
	require.Contains(t, h.auditor.actions, "app.standby.remove")

	// Host policy off, the app on: it has one.
	off := false
	h.svc.Policy = staticPolicy{policy.Document{Standby: &off}}
	h.plans.specs["app_a"] = appSpec(spec.StandbyOn, 0)
	h.svc.Once(ctx)
	require.Contains(t, h.store.rows, "app_a")
	h.plans.specs["app_a"] = appSpec(spec.StandbyDefault, 0)
	h.svc.Once(ctx)
	require.NotContains(t, h.store.rows, "app_a")
}

// TestR419_AFailedAppIsNotFailedOver asserts R-151 holds for standbys: a
// failed app's standby is neither refreshed nor started.
func TestR419_AFailedAppIsNotFailedOver(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.svc.Once(ctx)
	h.store.apps[0].State = state.StateFailed
	h.store.states["app_a"] = state.StateFailed
	h.store.apps[0].PinnedSpecID = "spec_2"
	h.svc.Once(ctx)
	require.Len(t, h.rt.applied, 1, "not refreshed")

	_, err := h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "a failed app is not started on another host")
	require.Empty(t, h.rt.promoted)
}

// TestR419_PromoteMakesTheStandbysHostTheHomeAndTheOldCopyIsRemoved asserts a
// failover's bookkeeping: the host left is recorded as stale, removed through
// the runtime when it returns, and then given the next standby.
func TestR419_PromoteMakesTheStandbysHostTheHomeAndTheOldCopyIsRemoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.svc.Once(ctx)
	h.rt.promoteOK = api.PromoteResult{Host: "app-2", FormerHost: "app-1", Healthy: true}

	got, err := h.svc.Promote(ctx, "app_a", "usr_1")
	require.NoError(t, err)
	require.Equal(t, "app-2", got.Host)
	require.Equal(t, "app-2", h.rt.promoted[0].Host)
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-1", sb.StaleHost)
	require.Empty(t, sb.Host, "the next pass gives the app a new standby")
	require.NotNil(t, sb.PromotedAt)
	require.Contains(t, h.auditor.actions, "app.standby.promote")

	require.NoError(t, h.svc.RemoveStale(ctx, "app_a", ""))
	require.Equal(t, []string{"app_a@app-1"}, h.rt.stale)
	sb = h.store.rows["app_a"]
	require.Empty(t, sb.StaleHost)
	require.Equal(t, "app-1", sb.Host, "the host that came back is the next standby's")

	h.svc.Once(ctx)
	require.Equal(t, "app-1", h.rt.applied[len(h.rt.applied)-1].Host)

	// Nothing stale: nothing to do.
	require.NoError(t, h.svc.RemoveStale(ctx, "app_a", ""))
	require.Len(t, h.rt.stale, 1)
}

func TestPromoteNeedsAStandby(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	_, err := h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	_, err = h.svc.Promote(ctx, "app_gone", "")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	h.store.rows["app_a"] = state.Standby{AppID: "app_a", Host: "app-2", RuntimeRef: "rt_other"}
	_, err = h.svc.Promote(ctx, "app_a", "")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))

	h.store.rows["app_a"] = state.Standby{AppID: "app_a", StaleHost: "app-1", RuntimeRef: "rt_other"}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(h.svc.RemoveStale(ctx, "app_a", "")))
}

func TestARuntimeWithOnePlaceKeepsNoStandby(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.store.rows["app_a"] = state.Standby{AppID: "app_a", Host: "old"}
	h.rt.supports = false
	h.svc.Once(ctx)
	require.Empty(t, h.rt.applied)
	require.NotContains(t, h.store.rows, "app_a", "a row from before is forgotten")
}

func TestADeletedAppsStandbyIsRemoved(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.store.apps[0].Deleted = true
	h.store.rows["app_a"] = state.Standby{AppID: "app_a", Host: "app-2", RuntimeRef: "rt_hosts"}
	h.svc.Once(ctx)
	require.Equal(t, []string{"app_a"}, h.rt.removed)
	require.Equal(t, []string{"app_a"}, h.store.deleted)
}

func TestWhatGoesWrongIsRecordedOnTheStandby(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.rt.applyErr = errs.New(errs.AdapterUnavailable, "No other host.")
	h.svc.Once(ctx)
	require.Equal(t, "No other host.", h.store.rows["app_a"].LastError)
	require.Empty(t, h.rt.shipped)

	h.rt.applyErr = nil
	h.rt.shipErr = errors.New("disk full")
	h.svc.Once(ctx)
	require.Equal(t, "app-2", h.store.rows["app_a"].Host)
	require.NotEmpty(t, h.store.rows["app_a"].LastError)
	require.Nil(t, h.store.rows["app_a"].ShippedAt)

	h.plans.err = errs.New(errs.Internal, "No secrets.")
	h.store.apps[0].PinnedSpecID = "spec_2"
	h.svc.Once(ctx)
	require.Equal(t, "No secrets.", h.store.rows["app_a"].LastError)
}

func TestProvisionedServicesWithoutReplicationAreNamed(t *testing.T) {
	s := appSpec("", 1)
	s.Slots = []spec.Slot{
		{Key: "db", Type: spec.SlotPostgres, Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned}},
		{Key: "cache", Type: spec.SlotRedis, Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned}},
		{Key: "ext", Type: spec.SlotMySQL, Resolution: &spec.Resolution{Mode: spec.ResolutionBound}},
	}
	svc := &Service{}
	require.Equal(t, []string{"postgres", "redis"}, svc.unreplicated(s))
	svc.Services = servicesFor{}
	require.Equal(t, []string{"redis"}, svc.unreplicated(s), "postgres replicates natively here")
}

func TestRunStopsWithItsContext(t *testing.T) {
	h := newHarness(appSpec("", 0))
	h.svc.Every = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.svc.Run(ctx); close(done) }()
	time.Sleep(5 * time.Millisecond)
	cancel()
	<-done
	require.NotEmpty(t, h.rt.applied)
}
