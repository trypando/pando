package reconciler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// What the console shows when a check finds nothing to deploy: the reason, in
// words the person can act on (R-105).
func TestAutoDeploySaysWhyItFoundNothing(t *testing.T) {
	git := spec.Source{Type: spec.SourceGit, Ref: "main"}
	for _, tc := range []struct {
		name string
		src  spec.Source
		ad   spec.AutoDeploy
		says string
	}{
		{"image", spec.Source{Type: spec.SourceImage}, spec.AutoDeploy{Enabled: true}, "not a git repository"},
		{"no release yet", git, spec.AutoDeploy{Trigger: spec.TriggerReleaseTagged}, "such as v1.2.3"},
		{"no matching tag", git, spec.AutoDeploy{Trigger: spec.TriggerReleaseTagged, TagPattern: "release-*"}, "release-*"},
		{"missing branch", git, spec.AutoDeploy{Branch: "staging"}, "The branch staging does not exist"},
		{"deployed branch missing", git, spec.AutoDeploy{}, "The branch main does not exist"},
		{"no branch at all", spec.Source{Type: spec.SourceGit}, spec.AutoDeploy{}, "does not name a branch"},
	} {
		require.Contains(t, nothingFound(tc.src, tc.ad), tc.says, tc.name)
	}
}

func TestAnAutoDeployFailureIsRecordedInItsOwnWords(t *testing.T) {
	require.Equal(t, "This app's plan does not pass.",
		message(errs.New(errs.ValidInvalid, "This app's plan does not pass.")))
	require.Equal(t, "Pando could not check this app for new commits.", message(errors.New("boom")),
		"an error with no envelope says something a person can read")
}

// Run stops when its context does, without waiting for a tick.
func TestAutoDeployRunStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		(&AutoDeploy{}).Run(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after its context was canceled")
	}
}
