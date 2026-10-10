package disklimit

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
)

const gib = int64(1) << 30

// fakeStore is one app, and what the pass did to it.
type fakeStore struct {
	mu      sync.Mutex
	app     state.DiskApp
	stopped bool
}

func (s *fakeStore) isStopped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *fakeStore) DiskCandidates(context.Context) ([]state.DiskApp, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return nil, nil
	}
	return []state.DiskApp{s.app}, nil
}

func (s *fakeStore) SetDiskMark(_ context.Context, _ string, which state.DiskMark, at *time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if which == state.DiskOver {
		s.app.OverAt = at
	} else {
		s.app.WarnedAt = at
	}
	return nil
}

func (s *fakeStore) StopForDisk(context.Context, string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	return true, nil
}

// fakeUsage answers with one container layer of the given size.
type fakeUsage struct {
	bytes     int64
	supported bool
}

func (u *fakeUsage) Usage(context.Context, string, string) (api.BundleUsage, bool, error) {
	return api.BundleUsage{Workloads: []api.WorkloadUsage{{DiskBytes: u.bytes}}}, u.supported, nil
}

type said struct{ notes []api.Notification }

func (s *said) Notify(_ context.Context, n api.Notification) error {
	s.notes = append(s.notes, n)
	return nil
}

type audited struct{ actions []string }

func (a *audited) Write(_ context.Context, e audit.Event) error {
	a.actions = append(a.actions, e.Action)
	return nil
}

func newPass(used int64) (*Pass, *fakeStore, *fakeUsage, *said, *audited) {
	store := &fakeStore{app: state.DiskApp{AppID: "app_1", Name: "notes", OwnerUserID: "usr_1", RuntimeRef: "rt_docker", DiskBytes: 10 * gib}}
	usage := &fakeUsage{bytes: used, supported: true}
	n, a := &said{}, &audited{}
	return &Pass{Store: store, Usage: usage, Notifier: n, Auditor: a}, store, usage, n, a
}

// TestR403_AnAppOverItsLimitIsStoppedAtTheSecondReading asserts R-403: one
// reading over is a warning, the next one over is a stop, with the owner told
// each time and both audited.
func TestR403_AnAppOverItsLimitIsStoppedAtTheSecondReading(t *testing.T) {
	ctx := context.Background()
	p, store, _, n, a := newPass(11 * gib)

	p.Once(ctx)
	require.False(t, store.stopped, "one reading over is a warning")
	require.NotNil(t, store.app.OverAt)
	require.Len(t, n.notes, 1)
	require.Equal(t, api.NotifyAppDisk, n.notes[0].Kind)
	require.Equal(t, "notes is over its disk limit", n.notes[0].Subject)
	require.Contains(t, n.notes[0].Body, "11.0 GiB, over its 10.0 GiB disk limit")
	require.Contains(t, n.notes[0].Body, "in about 10 minutes, Pando will stop it")

	p.Once(ctx)
	require.True(t, store.stopped)
	require.Len(t, n.notes, 2)
	require.Equal(t, "notes was stopped", n.notes[1].Subject)
	require.Contains(t, n.notes[1].Body, "resources.disk_bytes")
	require.Equal(t, []string{"app.disk.warning", "app.disk.stopped"}, a.actions)
}

// TestR403_AMomentOverIsNotAStop asserts a dip back under the limit between
// readings withdraws the reading over, so the next one over starts again.
func TestR403_AMomentOverIsNotAStop(t *testing.T) {
	ctx := context.Background()
	p, store, usage, _, _ := newPass(11 * gib)

	p.Once(ctx)
	usage.bytes = 5 * gib
	p.Once(ctx)
	require.Nil(t, store.app.OverAt)
	usage.bytes = 11 * gib
	p.Once(ctx)
	require.False(t, store.stopped, "over, under, over is two first readings, not a stop")
}

// TestR403_TheOwnerIsWarnedOnceNearTheLimit asserts the warning at 90% is
// sent once, and withdrawn below 80% so a later climb is warned again.
func TestR403_TheOwnerIsWarnedOnceNearTheLimit(t *testing.T) {
	ctx := context.Background()
	p, store, usage, n, _ := newPass(9*gib + gib/2)

	p.Once(ctx)
	p.Once(ctx)
	require.Len(t, n.notes, 1, "warned once, not every pass")
	require.Equal(t, "notes is near its disk limit", n.notes[0].Subject)
	require.Contains(t, n.notes[0].Body, "9.5 GiB of its 10.0 GiB")

	usage.bytes = 7 * gib
	p.Once(ctx)
	require.Nil(t, store.app.WarnedAt)
	usage.bytes = 9*gib + gib/2
	p.Once(ctx)
	require.Len(t, n.notes, 2, "warned again after it came down and went back up")
}

// TestR403_NothingIsStoppedOnARuntimeThatCannotMeasure asserts an app whose
// runtime cannot report its disk use is never stopped for it.
func TestR403_NothingIsStoppedOnARuntimeThatCannotMeasure(t *testing.T) {
	ctx := context.Background()
	p, store, usage, n, _ := newPass(50 * gib)
	usage.supported = false
	p.Once(ctx)
	p.Once(ctx)
	require.False(t, store.stopped)
	require.Empty(t, n.notes)

	usage.supported, usage.bytes = true, -1
	p.Once(ctx)
	p.Once(ctx)
	require.False(t, store.stopped, "-1 is unknown, not zero and not over")
}

func TestUsedCountsWhatCanBeMeasured(t *testing.T) {
	used, known := Used(api.BundleUsage{
		Workloads: []api.WorkloadUsage{{DiskBytes: 100}, {DiskBytes: -1}},
		Volumes:   []api.VolumeUsage{{Bytes: 50}, {Bytes: -1}},
	})
	require.True(t, known)
	require.Equal(t, int64(150), used)

	_, known = Used(api.BundleUsage{Workloads: []api.WorkloadUsage{{DiskBytes: -1}}})
	require.False(t, known)
}
