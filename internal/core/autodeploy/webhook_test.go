package autodeploy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR142_AWebhookStartsACheckOnlyForWhatTheAppWatches asserts which
// deliveries are worth a check: a push to the followed branch for the branch
// trigger, and a tag or release for the release trigger.
func TestR142_AWebhookStartsACheckOnlyForWhatTheAppWatches(t *testing.T) {
	src := spec.Source{Type: spec.SourceGit, Ref: "main"}
	branch := spec.AutoDeploy{Enabled: true}
	staging := spec.AutoDeploy{Enabled: true, Branch: "staging"}
	release := spec.AutoDeploy{Enabled: true, Trigger: spec.TriggerReleaseTagged}

	for _, tc := range []struct {
		name  string
		ad    spec.AutoDeploy
		event string
		body  string
		check bool
	}{
		{"push to the deployed branch", branch, "push", `{"ref":"refs/heads/main"}`, true},
		{"push to another branch", branch, "push", `{"ref":"refs/heads/feature"}`, false},
		{"push to the chosen branch", staging, "push", `{"ref":"refs/heads/staging"}`, true},
		{"push to the deployed branch, following another", staging, "push", `{"ref":"refs/heads/main"}`, false},
		{"a release, following a branch", branch, "release", `{"action":"published"}`, false},
		{"a release", release, "release", `{"action":"published"}`, true},
		{"a pushed tag", release, "push", `{"ref":"refs/tags/v1.2.0"}`, true},
		{"a created tag", release, "create", `{"ref":"v1.2.0","ref_type":"tag"}`, true},
		{"a created branch", release, "create", `{"ref":"feature","ref_type":"branch"}`, false},
		{"a branch push, following releases", release, "push", `{"ref":"refs/heads/main"}`, false},
		{"a payload that is not JSON", branch, "push", `not json`, false},
	} {
		why := concerns(Delivery{Event: tc.event, Body: []byte(tc.body)}, src, tc.ad)
		require.Equal(t, tc.check, why == "", "%s: %s", tc.name, why)
	}
}
