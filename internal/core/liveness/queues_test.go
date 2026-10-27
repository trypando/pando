package liveness

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// gatedFailover holds each app's promotion until its gate is closed, and
// records the order promotions began and ended in.
type gatedFailover struct {
	mu      sync.Mutex
	began   []string
	ended   []string
	gates   map[string]chan struct{}
	started map[string]chan struct{}
}

func newGatedFailover(ids ...string) *gatedFailover {
	f := &gatedFailover{gates: map[string]chan struct{}{}, started: map[string]chan struct{}{}}
	for _, id := range ids {
		f.gates[id] = make(chan struct{})
		f.started[id] = make(chan struct{})
	}
	return f
}

func (f *gatedFailover) PromoteStandby(ctx context.Context, id string) error {
	f.mu.Lock()
	f.began = append(f.began, id)
	f.mu.Unlock()
	close(f.started[id])
	select {
	case <-f.gates[id]:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(5 * time.Second):
		return errs.New(errs.AdapterFailed, "The test's promotion was never let through.")
	}
	f.mu.Lock()
	f.ended = append(f.ended, id)
	f.mu.Unlock()
	return nil
}

func (f *gatedFailover) order() (began, ended []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.began...), append([]string(nil), f.ended...)
}

func gatedRig(f *gatedFailover, homed ...HomedApp) *Service {
	apps := fakeApps{}
	for _, a := range homed {
		apps[a.AppID] = state.App{ID: a.AppID, State: state.StateRunning}
	}
	return &Service{
		Store: newFakeStore("x", "y", "z"), Apps: apps, Audit: &fakeAudit{}, Failover: f,
		Homes: fakeHomes{apps: map[string][]HomedApp{"x": homed}},
	}
}

// TestR421_ASlowPromotionDoesNotDelayTheNext asserts that a lost host's apps
// in different queues start side by side: one whose standby is slow to come
// up does not hold back the next app's start.
func TestR421_ASlowPromotionDoesNotDelayTheNext(t *testing.T) {
	f := newGatedFailover("app_slow", "app_quick")
	svc := gatedRig(f,
		HomedApp{AppID: "app_slow", HasStandby: true, Queue: "app_slow"},
		HomedApp{AppID: "app_quick", HasStandby: true, Queue: "app_quick"})
	close(f.gates["app_quick"])

	done := markLostAsync(t, svc)
	select {
	case <-f.started["app_quick"]:
	case <-time.After(5 * time.Second):
		t.Fatal("the quick app's promotion waited on the slow one")
	}
	close(f.gates["app_slow"])
	out := <-done
	require.Equal(t, []string{"app_slow", "app_quick"}, out.Started, "reported in the order they were listed")
	_, ended := f.order()
	require.Equal(t, []string{"app_quick", "app_slow"}, ended)
}

// TestR422_AppsInOneQueueStartInOrder asserts that apps sharing a queue —
// standbys on one host past the reservation — start one after another in
// the order listed, highest priority first, so the ones that matter most
// take the room (multidocker failoverOrder); the next waits for the one
// before to end.
func TestR422_AppsInOneQueueStartInOrder(t *testing.T) {
	f := newGatedFailover("app_first", "app_second", "app_third")
	svc := gatedRig(f,
		HomedApp{AppID: "app_first", HasStandby: true, Queue: "app-2"},
		HomedApp{AppID: "app_second", HasStandby: true, Queue: "app-2"},
		HomedApp{AppID: "app_third", HasStandby: true, Queue: "app-3"})
	close(f.gates["app_second"])
	close(f.gates["app_third"])

	done := markLostAsync(t, svc)
	<-f.started["app_third"]
	<-f.started["app_first"]
	select {
	case <-f.started["app_second"]:
		t.Fatal("the second app in a queue started before the first ended")
	case <-time.After(50 * time.Millisecond):
	}
	close(f.gates["app_first"])
	out := <-done
	require.Equal(t, []string{"app_first", "app_second", "app_third"}, out.Started)
	began, _ := f.order()
	require.Less(t, indexOf(began, "app_first"), indexOf(began, "app_second"))
}

// TestFailoverConcurrencyIsBounded asserts that no more queues start at once
// than FailoverConcurrency allows.
func TestFailoverConcurrencyIsBounded(t *testing.T) {
	f := newGatedFailover("app_1", "app_2")
	svc := gatedRig(f,
		HomedApp{AppID: "app_1", HasStandby: true, Queue: "app_1"},
		HomedApp{AppID: "app_2", HasStandby: true, Queue: "app_2"})
	svc.FailoverConcurrency = 1
	done := markLostAsync(t, svc)
	<-f.started["app_1"]
	select {
	case <-f.started["app_2"]:
		t.Fatal("two apps started at once with room for one")
	case <-time.After(50 * time.Millisecond):
	}
	close(f.gates["app_1"])
	close(f.gates["app_2"])
	require.Equal(t, []string{"app_1", "app_2"}, (<-done).Started)
}

// markLostAsync marks host x lost by hand in the background, and sends what
// it did once it returns.
func markLostAsync(t *testing.T, svc *Service) <-chan HostLoss {
	t.Helper()
	done := make(chan HostLoss, 1)
	go func() {
		out, err := svc.MarkLost(context.Background(), "x", person)
		if err != nil {
			t.Error(err)
		}
		done <- out
	}()
	return done
}

func indexOf(list []string, s string) int {
	for i, x := range list {
		if x == s {
			return i
		}
	}
	return -1
}
