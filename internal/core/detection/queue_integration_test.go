//go:build integration

package detection_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/detection"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// serveQueue runs a detection queue until the test ends.
func serveQueue(t *testing.T, q *detection.Queue) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		q.Serve(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
}

func settled(t *testing.T, detections *state.Detections, appID string) state.Detection {
	t.Helper()
	var got state.Detection
	require.Eventually(t, func() bool {
		d, err := detections.Get(context.Background(), appID)
		if err != nil || d.Status == state.DetectionRunning {
			return false
		}
		got = d
		return true
	}, 60*time.Second, 20*time.Millisecond, "the queued detection finished")
	return got
}

// TestR256_AQueuedDetectionRunsAndRecordsItsProposal asserts the detection
// queue end to end with the runner main wires into it (RunQueued): Enqueue
// answers with the detection as queued, and the queue runs it and records the
// proposal against the app.
func TestR256_AQueuedDetectionRunsAndRecordsItsProposal(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ctx := context.Background()
	repo := repoWith(t, map[string]string{
		"Dockerfile": "FROM nginx:alpine\nEXPOSE 8080\n",
		"index.html": "<h1>notes</h1>",
	})
	appID := appFrom(t, db, spec.Source{Type: spec.SourceGit, URL: repo})
	detections := state.NewDetections(db)

	runner := runnerOver(t, db, corepolicy.Static(corepolicy.Default()))
	q := &detection.Queue{Detections: detections, Detect: runner.RunQueued, Poll: time.Hour}
	serveQueue(t, q)

	queued, err := q.Enqueue(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionRunning, queued.Status, "queued reads as running from the first poll")

	got := settled(t, detections, appID)
	require.NotEqual(t, state.DetectionFailed, got.Status)
	require.NotEmpty(t, got.Commit, "the clone ran and its commit is recorded")
	require.Zero(t, q.Running())
	require.GreaterOrEqual(t, detection.DefaultConcurrency(), 2)
}

// TestR092_AQueuedDetectionIsRefusedIfTheAllowlistChangedWhileItWaited
// asserts that a detection is checked against the allowlist when it runs, not
// only when it was queued, and that the refusal is recorded against the app
// rather than leaving it running for good.
func TestR092_AQueuedDetectionIsRefusedIfTheAllowlistChangedWhileItWaited(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ctx := context.Background()
	repo := repoWith(t, map[string]string{"Dockerfile": "FROM nginx:alpine"})
	appID := appFrom(t, db, spec.Source{Type: spec.SourceGit, URL: repo})
	detections := state.NewDetections(db)

	narrowed := runnerOver(t, db, corepolicy.Static(corepolicy.Document{SourceAllowlist: []string{"github.com"}}))
	q := &detection.Queue{Detections: detections, Detect: narrowed.RunQueued, Poll: time.Hour, Limit: 1}
	serveQueue(t, q)

	_, err := q.Enqueue(ctx, appID)
	require.NoError(t, err)

	got := settled(t, detections, appID)
	require.Equal(t, state.DetectionFailed, got.Status)
	require.Contains(t, string(got.Body), string(errs.PolicySourceNotAllowed),
		"the app's page says why it failed")
}

// TestR256_AStoppingReplicaPutsItsDetectionBackInTheQueue asserts that a
// detection running when its replica stops is neither recorded as failed nor
// left claimed by a replica that is gone: it waits for the next one.
func TestR256_AStoppingReplicaPutsItsDetectionBackInTheQueue(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ctx := context.Background()
	appID := appFrom(t, db, spec.Source{Type: spec.SourceGit, URL: "https://example.test/notes"})
	detections := state.NewDetections(db)

	started := make(chan struct{}, 1)
	q := &detection.Queue{
		Detections: detections,
		Detect: func(ctx context.Context, _ string) (state.Detection, error) {
			started <- struct{}{}
			<-ctx.Done()
			return state.Detection{}, ctx.Err()
		},
		Poll:    time.Hour,
		Timeout: time.Hour,
	}
	qctx, cancel := context.WithCancel(ctx)
	defer cancel()
	served := make(chan struct{})
	go func() {
		q.Serve(qctx)
		close(served)
	}()

	_, err := q.Enqueue(ctx, appID)
	require.NoError(t, err)
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the queued detection never started")
	}
	require.Equal(t, 1, q.Running())

	cancel()
	select {
	case <-served:
	case <-time.After(30 * time.Second):
		t.Fatal("the queue did not stop")
	}

	got, err := detections.Get(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionRunning, got.Status, "stopping is not the detection's failure")
	claimed, err := detections.Claim(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, []string{appID}, claimed, "it is back in the queue for the next replica")
}

// TestR105_QueueingADetectionForAnAppThatDoesNotExistSaysSo asserts that
// Enqueue reports the store's refusal rather than queueing work nothing can
// run.
func TestR105_QueueingADetectionForAnAppThatDoesNotExistSaysSo(t *testing.T) {
	t.Parallel()
	db := connected(t)
	q := &detection.Queue{Detections: state.NewDetections(db)}
	_, err := q.Enqueue(context.Background(), id.New(id.App))
	require.Error(t, err)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Equal(t, "Could not start detection.", errs.As(err).Message)
}
