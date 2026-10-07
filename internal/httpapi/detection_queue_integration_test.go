//go:build integration

package httpapi_test

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// recordingQueue is a detection queue that records what it was asked to
// queue, and refuses when told to.
type recordingQueue struct {
	mu     sync.Mutex
	queued []string
	err    error
}

func (q *recordingQueue) Enqueue(_ context.Context, appID string) (state.Detection, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.err != nil {
		return state.Detection{}, q.err
	}
	q.queued = append(q.queued, appID)
	return state.Detection{AppID: appID, Status: state.DetectionRunning}, nil
}

// TestR022_CreatingAnAppQueuesItsDetection asserts that an app created from a
// repository has its detection queued (issue #72, O-32) rather than run inside
// the request, and that a queue that refuses does not cost the person the app:
// it is created, and detection can be started again from its page.
func TestR022_CreatingAnAppQueuesItsDetection(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	queue := &recordingQueue{}
	i.Server.Detector = &fakeDetector{}
	i.Server.DetectionQueue = queue

	appID := i.createApp(admin, "notes")
	require.Equal(t, []string{appID}, queue.queued, "queued, once, for the app just made")

	queue.err = errors.New("database is gone")
	second := i.createApp(admin, "billing")
	got := i.do(admin, http.MethodGet, "/apps/"+second, nil)
	require.Equal(t, http.StatusOK, got.Code, "the app exists although its detection could not be queued")
	require.Equal(t, []string{appID}, queue.queued)
}

// TestR105_ARerunOnAnInstallWithoutDetectionSaysSo asserts that asking for a
// detection where none is configured is refused in words a person can act on,
// not with a crash or a detection that never runs.
func TestR105_ARerunOnAnInstallWithoutDetectionSaysSo(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")
	i.Server.Detector = &fakeDetector{}
	i.Server.DetectionQueue = nil

	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/detection/rerun", map[string]any{})
	require.Equal(t, errs.AdapterUnavailable, errs.Code(got.ErrorCode()), got.String())
	require.Contains(t, got.String(), "Detection is not configured on this install.")
}
