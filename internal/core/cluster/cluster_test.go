package cluster_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/cluster"
	"github.com/trypando/pando/internal/core/state"
)

// fakeStore is the replicas table a Member sees, in memory.
type fakeStore struct {
	mu         sync.Mutex
	registered []state.Replica
	stopped    []string
	stopErr    error

	heartbeats   atomic.Int32
	heartbeatErr error
	alive        bool
	pending      bool
	pendingErr   error

	keys    []state.Replica
	keysErr error
	asked   time.Duration
}

func (f *fakeStore) Register(_ context.Context, rep state.Replica) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.registered = append(f.registered, rep)
	return nil
}

func (f *fakeStore) Heartbeat(context.Context, string) (bool, error) {
	f.heartbeats.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.alive, f.heartbeatErr
}

func (f *fakeStore) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = append(f.stopped, id)
	return f.stopErr
}

func (f *fakeStore) RestartPending(context.Context, string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pending, f.pendingErr
}

func (f *fakeStore) VerificationKeys(_ context.Context, validity time.Duration) ([]state.Replica, error) {
	f.asked = validity
	return f.keys, f.keysErr
}

func member(t *testing.T, store *fakeStore) *cluster.Member {
	t.Helper()
	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	return &cluster.Member{
		Store: store, ID: "rep_a", Hostname: "host-a", AdvertiseURL: "http://host-a:8080",
		Version: "v1.2.3", Minter: minter, Logger: zap.NewNop(), Interval: 5 * time.Millisecond,
	}
}

// runFor runs m until it returns or d passes, and returns its reason.
func runFor(t *testing.T, m *cluster.Member, d time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return m.Run(ctx)
}

// TestR256_AJoiningReplicaPublishesItsSigningKey asserts that a replica
// registers under its own identity with the public half of the key it signs
// with, so an assertion it signs verifies on every replica (R-256, R-051).
func TestR256_AJoiningReplicaPublishesItsSigningKey(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	m := member(t, store)

	require.NoError(t, m.Join(context.Background()))
	require.Len(t, store.registered, 1)
	got := store.registered[0]
	key := m.Minter.SigningKey()
	require.Equal(t, "rep_a", got.ID)
	require.Equal(t, "host-a", got.Hostname)
	require.Equal(t, "http://host-a:8080", got.AdvertiseURL)
	require.Equal(t, "v1.2.3", got.Version)
	require.Equal(t, key.ID, got.AssertionKID)
	require.Equal(t, []byte(key.Public), got.AssertionKey)
}

// TestR256_PeerKeysAreEveryReplicasPublishedKey asserts that the JWKS reads
// the keys of every replica whose assertions may still be valid.
func TestR256_PeerKeysAreEveryReplicasPublishedKey(t *testing.T) {
	t.Parallel()
	store := &fakeStore{keys: []state.Replica{
		{ID: "rep_a", AssertionKID: "kid-a", AssertionKey: []byte("aaaa")},
		{ID: "rep_b", AssertionKID: "kid-b", AssertionKey: []byte("bbbb")},
	}}
	keys, err := member(t, store).PeerKeys(context.Background())
	require.NoError(t, err)
	require.Equal(t, assertion.Lifetime, store.asked, "keys stay published for as long as an assertion lives")
	require.Equal(t, []assertion.PublicKey{
		{ID: "kid-a", Public: []byte("aaaa")},
		{ID: "kid-b", Public: []byte("bbbb")},
	}, keys)

	store.keysErr = errors.New("database away")
	_, err = member(t, store).PeerKeys(context.Background())
	require.Error(t, err)
}

// TestR256_AReplicaHeartbeatsUntilItsContextEnds asserts that a healthy
// replica keeps heartbeating and asks for no restart.
func TestR256_AReplicaHeartbeatsUntilItsContextEnds(t *testing.T) {
	t.Parallel()
	store := &fakeStore{alive: true}
	require.Empty(t, runFor(t, member(t, store), 100*time.Millisecond))
	require.Greater(t, store.heartbeats.Load(), int32(1))
}

// TestR256_ZeroIntervalHeartbeatsAtTheDefaultPace asserts that a Member with
// no Interval set heartbeats on state.HeartbeatInterval rather than spinning,
// and still stops with its context.
func TestR256_ZeroIntervalHeartbeatsAtTheDefaultPace(t *testing.T) {
	t.Parallel()
	store := &fakeStore{alive: true}
	m := member(t, store)
	m.Interval = 0
	require.Empty(t, runFor(t, m, 100*time.Millisecond))
	require.Zero(t, store.heartbeats.Load(), "the first heartbeat waits a full default interval")
}

// TestR256_AReplicaTakenForStoppedRestarts asserts that a replica whose row
// was marked stopped — its work already abandoned — restarts rather than
// carrying on under the same identity.
func TestR256_AReplicaTakenForStoppedRestarts(t *testing.T) {
	t.Parallel()
	store := &fakeStore{alive: false}
	reason := runFor(t, member(t, store), 5*time.Second)
	require.Contains(t, reason, "took this replica for stopped")
}

// TestR256_ARestartRequestReachesEveryReplica asserts that a replica restarts
// when a restart was asked for after it started, whichever replica took it.
func TestR256_ARestartRequestReachesEveryReplica(t *testing.T) {
	t.Parallel()
	store := &fakeStore{alive: true, pending: true}
	require.Equal(t, "a restart was asked for", runFor(t, member(t, store), 5*time.Second))

	// An unreadable restart signal is not a restart.
	store = &fakeStore{alive: true, pending: true, pendingErr: errors.New("database away")}
	require.Empty(t, runFor(t, member(t, store), 50*time.Millisecond))
}

// TestR256_ABriefDatabaseOutageDoesNotRestartAReplica asserts that a
// heartbeat that fails is retried: only an outage longer than the others wait
// (state.ReplicaStale) costs the replica its identity.
func TestR256_ABriefDatabaseOutageDoesNotRestartAReplica(t *testing.T) {
	t.Parallel()
	store := &fakeStore{heartbeatErr: errors.New("database away")}
	require.Empty(t, runFor(t, member(t, store), 100*time.Millisecond))
	require.Greater(t, store.heartbeats.Load(), int32(1), "it keeps trying")
}

// TestR256_ALeavingReplicaIsRecordedStopped asserts that shutdown records the
// replica as stopped, and that failing to is not fatal.
func TestR256_ALeavingReplicaIsRecordedStopped(t *testing.T) {
	t.Parallel()
	store := &fakeStore{}
	m := member(t, store)
	m.Leave(context.Background())
	require.Equal(t, []string{"rep_a"}, store.stopped)

	store.stopErr = errors.New("database away")
	m.Leave(context.Background())
	require.Len(t, store.stopped, 2)
}

// fakeLeadership hands out a lock to whoever asks while nobody holds it.
type fakeLeadership struct {
	mu     sync.Mutex
	tries  int
	grant  func(try int) (*state.Leadership, bool, error)
	called chan struct{}
}

func (f *fakeLeadership) TryLead(context.Context, time.Duration) (*state.Leadership, bool, error) {
	f.mu.Lock()
	f.tries++
	try := f.tries
	f.mu.Unlock()
	select {
	case f.called <- struct{}{}:
	default:
	}
	return f.grant(try)
}

// TestR256_AFollowerRunsNoInstallWideJob asserts that a replica that cannot
// take the leader lock runs none of the jobs, and keeps trying.
func TestR256_AFollowerRunsNoInstallWideJob(t *testing.T) {
	t.Parallel()
	var ran atomic.Bool
	store := &fakeLeadership{called: make(chan struct{}, 8), grant: func(try int) (*state.Leadership, bool, error) {
		if try == 1 {
			return nil, false, errors.New("database away")
		}
		return nil, false, nil
	}}
	l := &cluster.Leader{
		Store: store, Logger: zap.NewNop(), Retry: 5 * time.Millisecond,
		Jobs: []cluster.Job{{Name: "gc", Run: func(context.Context) { ran.Store(true) }}},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()

	for range 3 {
		<-store.called
	}
	require.False(t, l.Leading())
	cancel()
	<-done
	require.False(t, ran.Load(), "a follower runs no install-wide job")
}

// TestR256_ZeroRetryDefaultsAndRunEndsWithItsContext asserts that a Leader
// with no Retry set still tries to lead once and stops with its context.
func TestR256_ZeroRetryDefaultsAndRunEndsWithItsContext(t *testing.T) {
	t.Parallel()
	store := &fakeLeadership{called: make(chan struct{}, 1), grant: func(int) (*state.Leadership, bool, error) {
		return nil, false, nil
	}}
	l := &cluster.Leader{Store: store, Logger: zap.NewNop()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	<-store.called
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not end with its context")
	}
}

// TestR256_SweepRecordsAbandonedWorkAndPrunes asserts that a sweep runs every
// abandon step and the prune, and that one failing does not stop the rest.
func TestR256_SweepRecordsAbandonedWorkAndPrunes(t *testing.T) {
	t.Parallel()
	var calls []string
	var mu sync.Mutex
	note := func(s string) {
		mu.Lock()
		calls = append(calls, s)
		mu.Unlock()
	}
	s := &cluster.Sweeper{
		Logger: zap.NewNop(),
		Abandon: []func(context.Context) (int64, error){
			func(context.Context) (int64, error) { note("deploys"); return 0, errors.New("database away") },
			func(context.Context) (int64, error) { note("detections"); return 2, nil },
			func(context.Context) (int64, error) { note("none"); return 0, nil },
		},
		Prune: func(context.Context) error { note("prune"); return errors.New("database away") },
	}
	s.Sweep(context.Background())
	require.Equal(t, []string{"deploys", "detections", "none", "prune"}, calls)

	// Without a Prune, the abandon steps still run.
	calls = nil
	s.Prune = nil
	s.Sweep(context.Background())
	require.Equal(t, []string{"deploys", "detections", "none"}, calls)
}

// TestR256_SweeperRunSweepsRepeatedlyUntilCanceled asserts that the leader's
// sweep loop sweeps at once and then on every tick, and ends with ctx.
func TestR256_SweeperRunSweepsRepeatedlyUntilCanceled(t *testing.T) {
	t.Parallel()
	var n atomic.Int32
	s := &cluster.Sweeper{
		Logger: zap.NewNop(), Every: 5 * time.Millisecond,
		Prune: func(context.Context) error { n.Add(1); return nil },
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return n.Load() >= 3 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done

	// Zero Every falls back to a default and still sweeps once at the start.
	n.Store(0)
	s.Every = 0
	ctx, cancel = context.WithCancel(context.Background())
	done = make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	require.Eventually(t, func() bool { return n.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
}

type fakeDeployments struct {
	runner    string
	runnerErr error
}

func (f *fakeDeployments) Runner(context.Context, string) (string, error) {
	return f.runner, f.runnerErr
}

// Waiting reports every deploy as claimed already; waiting for a claim is
// logowner_test.go's.
func (f *fakeDeployments) Waiting(context.Context, string) (bool, error) { return false, nil }

type fakeReplicas map[string]state.Replica

func (f fakeReplicas) ByID(_ context.Context, id string) (state.Replica, bool, error) {
	if id == "rep_broken" {
		return state.Replica{}, false, errors.New("database away")
	}
	r, ok := f[id]
	return r, ok, nil
}

// TestR256_ADeploysLiveLogIsOnTheReplicaRunningIt asserts where a deploy's
// live log is answered from: the live replica running it, or this replica
// when it runs the deploy itself, or nobody does.
func TestR256_ADeploysLiveLogIsOnTheReplicaRunningIt(t *testing.T) {
	t.Parallel()
	stopped := time.Now()
	replicas := fakeReplicas{
		"rep_b":    {ID: "rep_b", AdvertiseURL: "http://host-b:8080"},
		"rep_gone": {ID: "rep_gone", AdvertiseURL: "http://host-gone:8080", StoppedAt: &stopped},
	}
	where := func(d *fakeDeployments) (string, error) {
		return cluster.LogOwner{Self: "rep_a", Deployments: d, Replicas: replicas}.
			Where(context.Background(), "dep_1")
	}

	got, err := where(&fakeDeployments{runner: "rep_b"})
	require.NoError(t, err)
	require.Equal(t, "http://host-b:8080", got, "another live replica runs it")

	for name, d := range map[string]*fakeDeployments{
		"this replica runs it":   {runner: "rep_a"},
		"nobody has claimed it":  {runner: ""},
		"its runner has stopped": {runner: "rep_gone"},
		"its runner is unknown":  {runner: "rep_never"},
	} {
		got, err := where(d)
		require.NoError(t, err, name)
		require.Empty(t, got, name)
	}

	_, err = where(&fakeDeployments{runnerErr: errors.New("database away")})
	require.Error(t, err)
	_, err = where(&fakeDeployments{runner: "rep_broken"})
	require.Error(t, err)
}
