package idle_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/idle"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type settingsStore struct {
	status state.IdleStatus
	saved  *state.IdleSettings
}

func (s *settingsStore) IdleStatus(context.Context, string) (state.IdleStatus, error) {
	return s.status, nil
}

func (s *settingsStore) SetIdleSettings(_ context.Context, _ string, own state.IdleSettings) error {
	s.saved = &own
	s.status.IdleSettings = own
	return nil
}

func intp(n int) *int { return &n }

// TestR397_AnAppsSettingsSayWhatIsInForceAndWhen asserts R-397 and R-395:
// the report says which numbers apply, and the dates a person can act on.
func TestR397_AnAppsSettingsSayWhatIsInForceAndWhen(t *testing.T) {
	c := clock.NewFake(time.Time{})
	store := &settingsStore{status: state.IdleStatus{
		DesiredState: state.StateRunning, State: state.StateRunning,
		LastActivity: c.Now(),
	}}
	s := &idle.Settings{Store: store, Policy: fakePolicy{policy.Document{IdleStopDays: 30, IdleDeleteDays: 90}}, Clock: c}

	r, err := s.Report(context.Background(), "app_1")
	require.NoError(t, err)
	assert.Nil(t, r.StopDays)
	assert.Equal(t, 30, r.EffectiveStopDays)
	assert.Equal(t, 90, r.EffectiveDeleteDays)
	require.NotNil(t, r.StopsAt)
	assert.Equal(t, c.Now().Add(30*day), *r.StopsAt)
	assert.Equal(t, c.Now().Add(90*day), *r.DeletesAt)

	// Turned off stopping, kept the installation's delete.
	r, err = s.Set(context.Background(), "app_1", state.IdleSettings{StopDays: intp(0)})
	require.NoError(t, err)
	assert.Equal(t, 0, r.EffectiveStopDays)
	assert.Nil(t, r.StopsAt, "nothing to stop")
	assert.NotNil(t, r.DeletesAt)

	// An app idle past its setting with no notice yet acts a notice's length
	// from now, never in the past.
	c.Advance(200 * day)
	r, err = s.Report(context.Background(), "app_1")
	require.NoError(t, err)
	assert.Equal(t, c.Now().Add(idle.NoticeDays*day), *r.DeletesAt)
}

// TestR105_AnIdleSettingThatCannotWorkSaysWhy asserts R-393 and R-105: a
// delete no later than the stop in force is refused, naming where the stop
// comes from.
func TestR105_AnIdleSettingThatCannotWorkSaysWhy(t *testing.T) {
	store := &settingsStore{}
	s := &idle.Settings{Store: store, Policy: fakePolicy{policy.Document{IdleStopDays: 30}}}

	_, err := s.Set(context.Background(), "app_1", state.IdleSettings{DeleteDays: intp(20)})
	require.Error(t, err)
	assert.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	assert.Contains(t, err.Error(), "the installation's idle_stop_days is 30")
	assert.Nil(t, store.saved, "nothing is saved")

	_, err = s.Set(context.Background(), "app_1", state.IdleSettings{StopDays: intp(-1)})
	require.Error(t, err)
}

type activityStore struct {
	mu      sync.Mutex
	fail    bool
	batches []map[string]time.Time
}

func (a *activityStore) RecordActivity(_ context.Context, seen map[string]time.Time) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.fail {
		return errors.New("database unavailable")
	}
	a.batches = append(a.batches, seen)
	return nil
}

// TestR394_ActivityIsWrittenInBatchesAndNeverLost asserts R-394: many
// requests are one row per app per flush, and a failed write is kept for the
// next one.
func TestR394_ActivityIsWrittenInBatchesAndNeverLost(t *testing.T) {
	store := &activityStore{}
	rec := &idle.Recorder{Store: store}

	for range 1000 {
		rec.Touch("app_a")
	}
	rec.Touch("app_b")

	store.fail = true
	rec.Flush(context.Background())
	assert.Empty(t, store.batches)

	store.fail = false
	rec.Touch("app_c")
	rec.Flush(context.Background())
	require.Len(t, store.batches, 1)
	assert.Len(t, store.batches[0], 3, "the failed batch and the new one, one entry an app")

	rec.Flush(context.Background())
	assert.Len(t, store.batches, 1, "nothing new, nothing written")
}
