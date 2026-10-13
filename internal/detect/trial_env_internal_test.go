package detect

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// recordingTrial remembers what the trial was started with.
type recordingTrial struct{ req *api.TrialRequest }

func (recordingTrial) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsTrialRun: true, SupportsPortObservation: true}, nil
}

func (r recordingTrial) Trial(_ context.Context, req api.TrialRequest) (api.TrialResult, error) {
	*r.req = req
	return api.TrialResult{Started: true, ObservedPorts: []int{1337}}, nil
}

// TestR097_ATrialRunLeavesAnEmptyVariableUnset asserts R-097. A trial is the
// app as it would deploy, and a deploy leaves a variable read out of
// .env.example with no value unset. Set to an empty string instead, it
// overrode the image's ENV and the app's own defaults — Strapi stopped on
// "http://:NaN/admin" — and the log described a failure no deploy has.
func TestR097_ATrialRunLeavesAnEmptyVariableUnset(t *testing.T) {
	var req api.TrialRequest
	empty, set, typed := "", "info", ""
	draft := Draft{Workloads: []spec.Workload{{
		Name: "web", Image: "example/app", Primary: true,
		Env: []spec.EnvEntry{
			{Key: "HOST", Value: &empty, Source: spec.EnvFromDetection},
			{Key: "LOG_LEVEL", Value: &set, Source: spec.EnvFromDetection},
			{Key: "BANNER", Value: &typed, Source: spec.EnvFromUser},
		},
	}}}
	(&Job{Runtime: recordingTrial{req: &req}}).trialWith(context.Background(), draft, nil)

	require.NotContains(t, req.Env, "HOST", "an empty variable detection read is left unset")
	require.Contains(t, req.Env, "LOG_LEVEL")
	require.Contains(t, req.Env, "BANNER", "an empty value somebody typed is an answer")
}
