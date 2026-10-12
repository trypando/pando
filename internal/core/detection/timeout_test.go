package detection

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR311_ADetectionHasTheTimeADeploysBuildGets asserts that detection, which
// builds what the app would deploy (R-311), is given the time a deploy's build
// is given on top of the time to read the app. Held to the reading's ten
// minutes, it failed builds a deploy would have finished (issue #90).
func TestR311_ADetectionHasTheTimeADeploysBuildGets(t *testing.T) {
	t.Parallel()
	build := time.Duration(spec.StandardDefaults().BuildTimeoutSeconds) * time.Second
	require.Equal(t, ReadTimeout+build, Timeout)
	require.Greater(t, Timeout, build, "a detection that builds can finish any build a deploy can")
}

type ctxKey struct{}

// TestR311_AResultReachedAsTheDeadlinePassesIsStillRecorded asserts that a
// detection's outcome is written under a context its own deadline cannot
// cancel. A detection that finished as its time ran out used to fail the write
// and show "Could not record the detection result" instead of its plan
// (issue #90). The write keeps the run's values and has its own short bound.
func TestR311_AResultReachedAsTheDeadlinePassesIsStillRecorded(t *testing.T) {
	t.Parallel()
	run, cancel := context.WithDeadline(context.WithValue(context.Background(), ctxKey{}, "app_1"), time.Now().Add(-time.Second))
	defer cancel()
	require.Error(t, run.Err(), "the run's own deadline has passed")

	rec, done := recording(run)
	defer done()
	require.NoError(t, rec.Err(), "the outcome can still be written")
	require.Equal(t, "app_1", rec.Value(ctxKey{}), "with the run's values")
	deadline, ok := rec.Deadline()
	require.True(t, ok, "the write is bounded")
	require.WithinDuration(t, time.Now().Add(recordTimeout), deadline, 5*time.Second)
}
