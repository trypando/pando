package liveness

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type fakeStore struct {
	mu       sync.Mutex
	hosts    map[string]state.Host
	lost     map[string]string
	fences   map[string]string
	lostErr  error
	markErr  error
	clearErr error
}

func newFakeStore(ids ...string) *fakeStore {
	s := &fakeStore{hosts: map[string]state.Host{}, lost: map[string]string{}, fences: map[string]string{}}
	for _, id := range ids {
		s.hosts[id] = state.Host{ID: id, Name: "name-" + id}
	}
	return s
}

func (s *fakeStore) Host(_ context.Context, id string) (state.Host, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hosts[id]
	if !ok {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", id)
	}
	return h, nil
}

func (s *fakeStore) MarkHostLost(_ context.Context, id, by string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.markErr != nil {
		return false, s.markErr
	}
	if _, ok := s.lost[id]; ok {
		return false, nil
	}
	s.lost[id] = by
	return true, nil
}

func (s *fakeStore) ClearHostLost(_ context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.clearErr != nil {
		return s.clearErr
	}
	delete(s.lost, id)
	return nil
}

func (s *fakeStore) LostHosts(context.Context) (map[string]bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lostErr != nil {
		return nil, s.lostErr
	}
	out := map[string]bool{}
	for id := range s.lost {
		out[id] = true
	}
	return out, nil
}

func (s *fakeStore) RecordHostFence(_ context.Context, id, fence string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fences[id] == fence {
		return false, nil
	}
	s.fences[id] = fence
	return true, nil
}

type fakeHomes struct {
	apps map[string][]HomedApp
	err  error
}

func (h fakeHomes) AppsHomedOn(_ context.Context, id string) ([]HomedApp, error) {
	return h.apps[id], h.err
}

type fakeApps map[string]state.App

func (a fakeApps) ByID(_ context.Context, id string) (state.App, bool, error) {
	if id == "app_broken" {
		return state.App{}, false, errors.New("database away")
	}
	app, ok := a[id]
	return app, ok, nil
}

type fakeFailover struct {
	mu       sync.Mutex
	promoted []string
	fail     map[string]bool
}

func (f *fakeFailover) PromoteStandby(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail[id] {
		return errs.New(errs.AdapterFailed, "The standby host did not start the app.")
	}
	f.promoted = append(f.promoted, id)
	return nil
}

type fakeAudit struct {
	mu     sync.Mutex
	events []audit.Event
	err    error
}

func (a *fakeAudit) Write(_ context.Context, e audit.Event) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, e)
	return a.err
}

func (a *fakeAudit) actions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []string
	for _, e := range a.events {
		out = append(out, e.Action)
	}
	return out
}

type serviceRig struct {
	store    *fakeStore
	failover *fakeFailover
	audit    *fakeAudit
	svc      *Service
}

func newServiceRig(hosts ...string) *serviceRig {
	r := &serviceRig{store: newFakeStore(hosts...), failover: &fakeFailover{fail: map[string]bool{}}, audit: &fakeAudit{}}
	r.svc = &Service{
		Store: r.store,
		Apps: fakeApps{
			"app_ok":     {ID: "app_ok", State: state.StateRunning},
			"app_failed": {ID: "app_failed", State: state.StateFailed},
			"app_err":    {ID: "app_err", State: state.StateStopped},
		},
		Audit:    r.audit,
		Failover: r.failover,
		Homes: fakeHomes{apps: map[string][]HomedApp{"x": {
			{AppID: "app_ok", HasStandby: true},
			{AppID: "app_failed", HasStandby: true},
			{AppID: "app_none", HasStandby: false},
			{AppID: "app_gone", HasStandby: true},
			{AppID: "app_broken", HasStandby: true},
			{AppID: "app_err", HasStandby: true},
		}}},
	}
	r.failover.fail["app_err"] = true
	return r
}

var person = Actor{Kind: audit.KindUser, ID: "usr_1", RequestID: "req_1"}

// TestR421_FailedAppsAreNeverFailedOver asserts R-419 and R-151: an app that
// is failed stays failed, on whichever host; its standby is not started.
func TestR421_FailedAppsAreNeverFailedOver(t *testing.T) {
	r := newServiceRig("x")
	out, err := r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
	require.Equal(t, []string{"app_ok"}, out.Started)
	require.Equal(t, []string{"app_ok"}, r.failover.promoted)
	require.NotContains(t, r.failover.promoted, "app_failed")

	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.AppID] = s.Reason
	}
	require.Equal(t, map[string]string{
		"app_failed": SkipFailed, "app_none": SkipNoStandby, "app_gone": SkipGone,
		"app_broken": SkipPromoteErr, "app_err": SkipPromoteErr,
	}, reasons)
}

// TestR421_MarkingAHostLostIsAudited asserts R-421: every manual start and
// failover is audited, as the person who said the host was gone.
func TestR421_MarkingAHostLostIsAudited(t *testing.T) {
	r := newServiceRig("x")
	out, err := r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
	require.False(t, out.AlreadyLost)
	require.Equal(t, "name-x", out.Name)
	require.Equal(t, []string{"cluster.host.lost", "cluster.failover", "cluster.failover"}, r.audit.actions())
	for _, e := range r.audit.events {
		require.Equal(t, audit.KindUser, e.PrincipalKind)
		require.Equal(t, "usr_1", e.PrincipalID)
		require.Equal(t, "req_1", e.RequestID)
	}
	require.Equal(t, "manual", r.audit.events[0].Detail["trigger"])
	require.Equal(t, "usr_1", r.store.lost["x"])

	// Again: already lost, and the standbys still homed there are tried.
	out, err = r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
	require.True(t, out.AlreadyLost)
	require.Equal(t, true, r.audit.events[3].Detail["already_lost"])
}

type fakeWitness struct{ err error }

func (f fakeWitness) Silent(context.Context, state.Host) error { return f.err }

func TestMarkLostRefusesAHostHeardFromLately(t *testing.T) {
	r := newServiceRig("x")
	r.svc.Witness = fakeWitness{err: errs.New(errs.StateInvalid, "Host x has been heard from in the last 30s, so it is not gone.")}
	_, err := r.svc.MarkLost(context.Background(), "x", person)
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Empty(t, r.store.lost)
	require.Empty(t, r.failover.promoted)

	r.svc.Witness = fakeWitness{}
	_, err = r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
}

func TestMarkLostErrors(t *testing.T) {
	r := newServiceRig("x")
	_, err := r.svc.MarkLost(context.Background(), "nope", person)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	r.store.markErr = errors.New("database away")
	_, err = r.svc.MarkLost(context.Background(), "x", person)
	require.Error(t, err)
}

func TestMarkLostWithoutStandbys(t *testing.T) {
	r := newServiceRig("x")
	r.svc.Homes, r.svc.Failover = nil, nil
	out, err := r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
	require.Equal(t, SkipNotWired, out.Skipped[0].Reason)
	require.Equal(t, []string{"cluster.host.lost"}, r.audit.actions())

	r = newServiceRig("x")
	r.svc.Homes = fakeHomes{err: errs.New(errs.Internal, "Could not read the apps on the host.")}
	out, err = r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
	require.Equal(t, "Could not read the apps on the host.", out.Skipped[0].Detail)
	require.Equal(t, "Pando could not start the standby: plain", messageOf(errors.New("plain")))
}

func TestServiceWithoutAuditOrLoggerStillWorks(t *testing.T) {
	r := newServiceRig("x")
	r.svc.Audit = nil
	_, err := r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)

	r = newServiceRig("x")
	r.audit.err = errors.New("audit away")
	_, err = r.svc.MarkLost(context.Background(), "x", person)
	require.NoError(t, err)
}

// watchRig is a leader watching hosts whose views it is handed.
type watchRig struct {
	*serviceRig
	clk     *clock.Fake
	watch   *Watch
	mu      sync.Mutex
	views   map[string]View
	hostErr error
	ids     []string
}

func newWatchRig(ids ...string) *watchRig {
	r := &watchRig{serviceRig: newServiceRig(ids...), clk: clock.NewFake(time.Time{}), views: map[string]View{}, ids: ids}
	r.watch = &Watch{
		Hosts: func(context.Context) ([]Peer, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.hostErr != nil {
				return nil, r.hostErr
			}
			out := make([]Peer, len(r.ids))
			for i, id := range r.ids {
				out[i] = Peer{ID: id}
			}
			return out, nil
		},
		Probe: func(_ context.Context, p Peer) ([]byte, error) {
			r.mu.Lock()
			defer r.mu.Unlock()
			if p.ID == "garbled" {
				return []byte("{"), nil
			}
			v, ok := r.views[p.ID]
			if !ok {
				return nil, errors.New("no route to host")
			}
			return json.Marshal(v)
		},
		Service: r.svc,
		Clock:   r.clk,
	}
	return r
}

// round is one of the leader's rounds, and the failovers it began.
func (r *watchRig) round(ctx context.Context) {
	r.watch.Round(ctx)
	r.svc.Settle()
}

func (r *watchRig) say(observer string, unreached map[string]time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := View{Host: observer, UnreachedMS: map[string]int64{}}
	for k, d := range unreached {
		v.UnreachedMS[k] = d.Milliseconds()
	}
	r.views[observer] = v
}

// TestR421_TheLeaderFailsOverOnlyByMajorityAfter3T asserts R-421 on the
// leader's side: a host is taken for lost, and its standbys started, once
// more than half the hosts report not having reached it for 3T — and only
// once.
func TestR421_TheLeaderFailsOverOnlyByMajorityAfter3T(t *testing.T) {
	r := newWatchRig("a", "b", "c", "d", "x")
	ctx := context.Background()

	silentFor := func(d time.Duration) map[string]time.Duration { return map[string]time.Duration{"x": d} }
	r.say("a", silentFor(3*tPeriod))
	r.say("b", silentFor(3*tPeriod))
	r.say("c", silentFor(3*tPeriod-time.Second))
	r.say("d", silentFor(0))
	r.round(ctx)
	require.Empty(t, r.failover.promoted, "two of five is not a majority, and c is short of 3T")

	r.say("c", silentFor(3*tPeriod))
	r.round(ctx)
	require.Equal(t, []string{"app_ok"}, r.failover.promoted)
	require.Equal(t, "liveness", r.store.lost["x"])
	require.Equal(t, audit.KindSystem, r.audit.events[0].PrincipalKind)
	require.Equal(t, "majority", r.audit.events[0].Detail["trigger"])

	r.round(ctx)
	require.Equal(t, []string{"app_ok"}, r.failover.promoted, "once")
}

func TestWatchTakesStaleOrBrokenReportsForNothing(t *testing.T) {
	r := newWatchRig("a", "b", "c", "garbled", "x")
	ctx := context.Background()
	silent := map[string]time.Duration{"x": time.Hour}
	r.say("a", silent)
	r.say("b", silent)
	// c answers as somebody else: dropped.
	r.mu.Lock()
	r.views["c"] = View{Host: "a", UnreachedMS: map[string]int64{"x": time.Hour.Milliseconds()}}
	r.mu.Unlock()
	r.round(ctx)
	require.Empty(t, r.failover.promoted)

	r.say("c", silent)
	r.round(ctx)
	require.Len(t, r.failover.promoted, 1)

	// Now a and b stop answering: their reports go stale, and with them
	// the evidence against a.
	r2 := newWatchRig("a", "b", "c", "d", "x")
	for _, o := range []string{"a", "b", "c"} {
		r2.say(o, silent)
	}
	r2.store.lostErr = errors.New("database away")
	r2.round(ctx)
	require.Empty(t, r2.failover.promoted, "nothing is decided without the database")
	r2.store.lostErr = nil
	r2.mu.Lock()
	r2.views = map[string]View{}
	r2.mu.Unlock()
	r2.clk.Advance(time.Minute)
	r2.round(ctx)
	require.Empty(t, r2.failover.promoted, "stale reports say nothing")
}

func TestWatchKeepsTheLastHostsAndForgetsRemovedOnes(t *testing.T) {
	r := newWatchRig("a", "b", "c", "x")
	ctx := context.Background()
	silent := map[string]time.Duration{"x": time.Hour}
	for _, o := range []string{"a", "b", "c"} {
		r.say(o, silent)
	}
	r.hostErr = errors.New("database away")
	r.round(ctx)
	require.Empty(t, r.watch.hosts, "no host list yet")

	r.hostErr = nil
	r.ids = []string{"a", "b", "c", "d", "e", "x"}
	r.round(ctx)
	require.Empty(t, r.failover.promoted, "three of six is not more than half")
	r.ids = []string{"a", "b", "c", "x"}
	r.round(ctx)
	require.Len(t, r.failover.promoted, 1)

	r.mu.Lock()
	r.hostErr = errors.New("database away")
	r.mu.Unlock()
	r.round(ctx)
	require.Len(t, r.watch.hosts, 4, "the last list is kept")

	r.hostErr = nil
	r.ids = []string{"a", "x"}
	r.round(ctx)
	_, kept := r.watch.ledger.reports["b"]
	require.False(t, kept, "a removed host's report is dropped")
}

// TestR421_EveryFenceIsAuditedOnce asserts R-421's "every fence is
// audited": the leader writes cluster.host.fence the first time it hears of
// each fence.
func TestR421_EveryFenceIsAuditedOnce(t *testing.T) {
	r := newWatchRig("a", "b")
	ctx := context.Background()
	r.views["a"] = View{Host: "a", Fenced: true, FenceID: "fnc_1", Stopped: 3}
	r.views["b"] = View{Host: "b"}
	r.round(ctx)
	r.round(ctx)
	require.Equal(t, []string{"cluster.host.fence"}, r.audit.actions())
	e := r.audit.events[0]
	require.Equal(t, "a", e.TargetID)
	require.Equal(t, 3, e.Detail["stopped"])

	r.views["a"] = View{Host: "a", FenceID: "fnc_2"}
	r.round(ctx)
	require.Equal(t, []string{"cluster.host.fence", "cluster.host.fence"}, r.audit.actions())
}

func TestWatchRunStopsWithItsContext(t *testing.T) {
	r := newWatchRig()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	go func() {
		r.watch.Run(ctx)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context ended")
	}
	w := &Watch{}
	w.init()
	require.NotNil(t, w.Logger)
}

func TestDeclareLogsWhatItCannotDo(t *testing.T) {
	r := newServiceRig("x")
	r.svc.declare(context.Background(), "nope")
	r.store.markErr = errors.New("database away")
	r.svc.declare(context.Background(), "x")
	require.Empty(t, r.failover.promoted)

	r.store.markErr = nil
	r.svc.RecordFence(context.Background(), "x", View{})
	require.Empty(t, r.audit.events)
}
