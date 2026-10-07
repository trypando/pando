//go:build integration

package reconciler_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR211_RollingBackupsRunAsAJobOfTheirOwn asserts the backups job as the
// leader runs it (issue #72): a pass at start that takes what is due, and
// passes on its interval after that which take nothing more until the app is
// due again, until its context ends.
func TestR211_RollingBackupsRunAsAJobOfTheirOwn(t *testing.T) {
	t.Parallel()
	runner := &fakeBackups{}
	job, appID, logs := backupGC(t, runner, true)
	job.RetryAfter = 0 // production's hour
	job.Every = 10 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		job.Run(ctx)
		close(done)
	}()

	require.Eventually(t, func() bool {
		attempts, err := job.Backups.Attempts(context.Background(), appID)
		return err == nil && len(attempts) == 1 && attempts[0].Outcome == state.AttemptTaken
	}, 30*time.Second, 10*time.Millisecond, "the first pass took the due backup")
	time.Sleep(100 * time.Millisecond) // several more passes

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the backups job did not stop")
	}
	require.Len(t, runner.taken, 1, "once a day, not once a pass")
	require.Equal(t, 1, logs.FilterMessage("took a rolling backup").Len())
}

// TestR211_RollingBackupsWithNowhereToPutThemDoNothing asserts that a job
// with no backup service configured passes without touching an app.
func TestR211_RollingBackupsWithNowhereToPutThemDoNothing(t *testing.T) {
	t.Parallel()
	runner := &fakeBackups{}
	job, appID, _ := backupGC(t, runner, true)
	job.Backup = nil

	job.Pass(context.Background())

	attempts, err := job.Backups.Attempts(context.Background(), appID)
	require.NoError(t, err)
	require.Empty(t, attempts, "nothing was attempted, so nothing is recorded")
	require.Empty(t, runner.taken)
}
