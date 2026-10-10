package disklimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/state"
)

var errDown = errors.New("down")

// brokenStore fails where it is told to, and otherwise behaves as fakeStore.
type brokenStore struct {
	fakeStore
	listErr, markErr, stopErr error
	stopNothing               bool
}

func (s *brokenStore) DiskCandidates(ctx context.Context) ([]state.DiskApp, error) {
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.fakeStore.DiskCandidates(ctx)
}

func (s *brokenStore) SetDiskMark(ctx context.Context, id string, which state.DiskMark, at *time.Time) error {
	if s.markErr != nil {
		return s.markErr
	}
	return s.fakeStore.SetDiskMark(ctx, id, which, at)
}

func (s *brokenStore) StopForDisk(ctx context.Context, id string) (bool, error) {
	if s.stopErr != nil {
		return false, s.stopErr
	}
	if s.stopNothing {
		return false, nil
	}
	return s.fakeStore.StopForDisk(ctx, id)
}

type failingUsage struct{}

func (failingUsage) Usage(context.Context, string, string) (api.BundleUsage, bool, error) {
	return api.BundleUsage{}, false, errDown
}

type failingNotifier struct{}

func (failingNotifier) Notify(context.Context, api.Notification) error { return errDown }

type failingAuditor struct{}

func (failingAuditor) Write(context.Context, audit.Event) error { return errDown }

func app() state.DiskApp {
	return state.DiskApp{AppID: "app_1", Name: "notes", OwnerUserID: "usr_1", RuntimeRef: "rt_docker", DiskBytes: 10 * gib}
}

func logged(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, logs := observer.New(zap.WarnLevel)
	return zap.New(core), logs
}

// TestR403_RunPassesUntilCanceled asserts the job runs a pass at once and
// then on its interval, and stops with its context.
func TestR403_RunPassesUntilCanceled(t *testing.T) {
	store := &fakeStore{app: app()}
	usage := &fakeUsage{bytes: 11 * gib, supported: true}
	p := &Pass{Store: store, Usage: usage, Every: 10 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	require.Eventually(t, store.isStopped, 2*time.Second, 5*time.Millisecond,
		"two passes over the limit stop it")
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop with its context")
	}
	require.Equal(t, DefaultEvery, (&Pass{}).every())
}

// TestR403_APassThatCannotReadOrWriteStopsNothingAndSaysSo asserts every
// failure is logged and none of them stops an app on a guess.
func TestR403_APassThatCannotReadOrWriteStopsNothingAndSaysSo(t *testing.T) {
	ctx := context.Background()

	logger, logs := logged(t)
	(&Pass{Store: &brokenStore{listErr: errDown}, Usage: &fakeUsage{}, Logger: logger}).Once(ctx)
	require.Equal(t, 1, logs.FilterMessage("could not list apps for the disk pass").Len())

	logger, logs = logged(t)
	store := &brokenStore{fakeStore: fakeStore{app: app()}}
	(&Pass{Store: store, Usage: failingUsage{}, Logger: logger}).Once(ctx)
	require.False(t, store.stopped)
	require.Equal(t, 1, logs.FilterMessage("could not read an app's disk use").Len())

	logger, logs = logged(t)
	marked := app()
	now := time.Now()
	marked.OverAt = &now
	store = &brokenStore{fakeStore: fakeStore{app: marked}, stopErr: errDown}
	(&Pass{Store: store, Usage: &fakeUsage{bytes: 11 * gib, supported: true}, Logger: logger}).Once(ctx)
	require.False(t, store.stopped)
	require.Equal(t, 1, logs.FilterMessage("could not stop an app over its disk limit").Len())

	logger, logs = logged(t)
	store = &brokenStore{fakeStore: fakeStore{app: app()}, markErr: errDown}
	(&Pass{Store: store, Usage: &fakeUsage{bytes: 11 * gib, supported: true}, Logger: logger,
		Notifier: failingNotifier{}, Auditor: failingAuditor{}}).Once(ctx)
	require.Positive(t, logs.FilterMessage("could not record an app's disk use").Len())
	require.Equal(t, 1, logs.FilterMessage("could not tell an app's owner about its disk use").Len())
}

// TestR403_AnAppStoppedBySomebodyElseFirstIsLeftToThem asserts a stop that
// finds the app already stopped or gone says and audits nothing.
func TestR403_AnAppStoppedBySomebodyElseFirstIsLeftToThem(t *testing.T) {
	marked := app()
	now := time.Now()
	marked.OverAt = &now
	store := &brokenStore{fakeStore: fakeStore{app: marked}, stopNothing: true}
	n, a := &said{}, &audited{}
	(&Pass{Store: store, Usage: &fakeUsage{bytes: 11 * gib, supported: true}, Notifier: n, Auditor: a}).Once(context.Background())
	require.Empty(t, n.notes)
	require.Empty(t, a.actions)
}

// TestAnAppWithNoLimitOrNoOwnerIsHandledQuietly covers the edges: an app
// with no limit or no runtime is not measured, and one with no owner is
// still held to its limit but nobody is notified.
func TestAnAppWithNoLimitOrNoOwnerIsHandledQuietly(t *testing.T) {
	ctx := context.Background()
	usage := &fakeUsage{bytes: 11 * gib, supported: true}

	unlimited := app()
	unlimited.DiskBytes = 0
	store := &fakeStore{app: unlimited}
	(&Pass{Store: store, Usage: usage}).Once(ctx)
	require.Nil(t, store.app.OverAt)

	orphan := app()
	orphan.OwnerUserID = ""
	store = &fakeStore{app: orphan}
	n := &said{}
	p := &Pass{Store: store, Usage: usage, Notifier: n}
	p.Once(ctx)
	p.Once(ctx)
	require.True(t, store.stopped)
	require.Empty(t, n.notes)
}

func TestSizesAndIntervalsReadAsAPersonWouldSayThem(t *testing.T) {
	require.Equal(t, "512 MiB", size(512<<20))
	require.Equal(t, "1.5 GiB", size(3<<29))
	require.Equal(t, "a minute", minutes(time.Minute))
	require.Equal(t, "10 minutes", minutes(10*time.Minute))
}
