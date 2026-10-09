package idle_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/idle"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
)

type brokenPolicy struct{}

func (brokenPolicy) Load(context.Context) (policy.Document, error) {
	return policy.Document{}, errors.New("database unavailable")
}

type brokenStore struct{ fakeStore }

func (*brokenStore) IdleCandidates(context.Context, state.IdleDefaults, time.Time) ([]state.IdleApp, error) {
	return nil, errors.New("database unavailable")
}

func (*brokenStore) SetIdleNotice(context.Context, string, state.IdleNotice, *time.Time) error {
	return errors.New("database unavailable")
}

func (*brokenStore) StopForIdle(context.Context, string) (bool, error) {
	return false, errors.New("database unavailable")
}

// A pass that cannot read policy or its candidates does nothing, and says
// nothing to anybody: a database outage is not a reason to stop apps.
func TestR393_APassThatCannotReadDoesNothing(t *testing.T) {
	notes := &notices{}
	(&idle.Pass{Store: newStore(), Policy: brokenPolicy{}, Notifier: notes}).Once(context.Background())
	(&idle.Pass{Store: &brokenStore{}, Policy: fakePolicy{policy.Document{IdleStopDays: 30}}, Notifier: notes}).
		Once(context.Background())
	assert.Empty(t, notes.sent)
}

// A notice that cannot be recorded is not sent, so an owner is never told of
// a stop the pass has no record of having warned about (R-395).
func TestR395_ANoticeThatCannotBeRecordedIsNotSent(t *testing.T) {
	r := newRig(policy.Document{IdleStopDays: 30})
	broken := &brokenStore{fakeStore: *newStore(r.runningApp("app_1"))}
	r.pass.Store = broken
	r.clock.Advance(40 * 24 * time.Hour)
	r.pass.Once(context.Background())
	assert.Empty(t, r.notes.sent)
	assert.Empty(t, r.audit.events)
}

// countingStore counts passes, safely across the goroutine Run is on.
type countingStore struct {
	fakeStore
	passes atomic.Int32
}

func (c *countingStore) IdleCandidates(context.Context, state.IdleDefaults, time.Time) ([]state.IdleApp, error) {
	c.passes.Add(1)
	return nil, nil
}

// Run passes until its context ends, and again on each tick.
func TestR393_RunPassesUntilCanceled(t *testing.T) {
	store := &countingStore{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&idle.Pass{Store: store, Policy: fakePolicy{policy.Document{IdleStopDays: 30}}, Every: time.Millisecond}).Run(ctx)
		close(done)
	}()
	require.Eventually(t, func() bool { return store.passes.Load() >= 2 }, time.Second, time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
}

// The recorder writes what it has on the way out, so a clean stop loses no
// activity (R-394).
func TestR394_TheRecorderFlushesOnTheWayOut(t *testing.T) {
	store := &activityStore{}
	rec := &idle.Recorder{Store: store}
	rec.Touch("app_a")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { rec.Run(ctx); close(done) }()
	cancel()
	<-done

	require.Len(t, store.batches, 1)
	assert.Contains(t, store.batches[0], "app_a")
}

// Without a notifier, an auditor, a clock or a logger the pass still works:
// they are optional, and nil says nothing.
func TestR393_ThePassRunsWithItsOptionalPartsLeftOut(t *testing.T) {
	store := newStore(state.IdleApp{
		AppID: "app_1", Name: "Expenses", OwnerUserID: "usr_owner",
		State: state.StateRunning, DesiredState: state.StateRunning,
		LastActivity:  time.Now().Add(-400 * 24 * time.Hour),
		StopNoticedAt: func() *time.Time { t := time.Now().Add(-10 * 24 * time.Hour); return &t }(),
	})
	(&idle.Pass{Store: store, Policy: fakePolicy{policy.Document{IdleStopDays: 30}}}).Once(context.Background())
	assert.True(t, store.apps["app_1"].StoppedForIdle)
}
