package liveness

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type fakeReturns struct {
	left      map[string][]string
	removed   []string
	leftErr   error
	removeErr error
}

func (f *fakeReturns) AppsLeftOn(_ context.Context, hostID string) ([]string, error) {
	return f.left[hostID], f.leftErr
}

func (f *fakeReturns) RemoveStale(_ context.Context, appID string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	f.removed = append(f.removed, appID)
	return nil
}

// TestR419_ALostHostThatAnswersAgainHasItsCopiesRemovedThenItsMarkCleared
// asserts the leader's side of a lost host's return: only a host that
// answers, and that the majority no longer takes for lost, is back; its
// copies are removed first, and its lost mark is cleared only once every
// one is gone.
func TestR419_ALostHostThatAnswersAgainHasItsCopiesRemovedThenItsMarkCleared(t *testing.T) {
	ctx := context.Background()
	r := newWatchRig("a", "b", "c", "x")
	returns := &fakeReturns{left: map[string][]string{"x": {"app_ok"}}}
	r.svc.Returns = returns
	r.store.lost["x"] = "liveness"

	// x answers the leader, but a, b and c have not reached it for an hour:
	// still lost.
	silent := map[string]time.Duration{"x": time.Hour}
	r.say("a", silent)
	r.say("b", silent)
	r.say("c", silent)
	r.say("x", nil)
	r.watch.Round(ctx)
	require.Empty(t, returns.removed)
	require.Contains(t, r.store.lost, "x")

	// The majority reaches it again; each step that fails keeps the mark.
	r.say("a", nil)
	r.say("b", nil)
	r.say("c", nil)
	returns.leftErr = errors.New("database away")
	r.watch.Round(ctx)
	require.Contains(t, r.store.lost, "x")
	returns.leftErr, returns.removeErr = nil, errors.New("Docker on x did not answer")
	r.watch.Round(ctx)
	require.Contains(t, r.store.lost, "x")
	returns.removeErr, r.store.clearErr = nil, errors.New("database away")
	r.watch.Round(ctx)
	require.Contains(t, r.store.lost, "x")

	r.store.clearErr = nil
	r.watch.Round(ctx)
	require.NotContains(t, r.store.lost, "x")
	require.Equal(t, []string{"app_ok", "app_ok"}, returns.removed, "removed again, which is not an error")
	last := r.audit.events[len(r.audit.events)-1]
	require.Equal(t, "cluster.host.returned", last.Action)
	require.Equal(t, []string{"app_ok"}, last.Detail["removed_copies"])
	require.Empty(t, r.failover.promoted, "nothing is started")
}

// TestALostHostIsKeptLostWithoutAWayToRemoveItsCopies covers an install
// that keeps no standbys, and a cluster of two, where a person marked the
// host lost: it comes back the same way.
func TestALostHostIsKeptLostWithoutAWayToRemoveItsCopies(t *testing.T) {
	ctx := context.Background()
	r := newWatchRig("a", "x")
	r.store.lost["x"] = "usr_admin"
	r.say("x", nil)
	r.watch.Round(ctx)
	require.Contains(t, r.store.lost, "x", "nothing would remove its copies")

	r.svc.Returns = &fakeReturns{}
	r.watch.Round(ctx)
	require.NotContains(t, r.store.lost, "x")
	require.Equal(t, []string{}, r.audit.events[len(r.audit.events)-1].Detail["removed_copies"])
}
