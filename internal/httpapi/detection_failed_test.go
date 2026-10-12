package httpapi

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR105_AcceptingAFailedDetectionReturnsItsBuildError asserts R-105 at
// accept: a detection whose build failed is refused with the build's own
// error, not pinned as an empty spec of schema version 0 and refused as "This
// spec has 3 problems" (issue #90).
func TestR105_AcceptingAFailedDetectionReturnsItsBuildError(t *testing.T) {
	recorded := errs.New(errs.BuildFailed,
		"Pando builds an app while reading it, to score the image it would run, and the build of this app failed: Could not read the app's Dockerfile.").
		WithRemedy("The build log shows the step that failed.").
		WithDetail("build_log", "=> Building web\n")
	body, err := json.Marshal(map[string]any{"error": recorded})
	require.NoError(t, err)

	got := errs.As(failedDetection(state.Detection{Status: state.DetectionFailed, Body: body}))
	require.NotNil(t, got)
	require.Equal(t, errs.BuildFailed, got.Code)
	require.Equal(t, recorded.Message, got.Message)
	require.Equal(t, recorded.Remedy, got.Remedy)
	require.Equal(t, "=> Building web\n", got.Details["build_log"])

	// A failure recorded with no envelope still says what it was.
	plain, err := json.Marshal(map[string]any{"error": map[string]any{"message": "clone timed out"}})
	require.NoError(t, err)
	got = errs.As(failedDetection(state.Detection{Status: state.DetectionFailed, Body: plain}))
	require.NotNil(t, got)
	require.Contains(t, got.Message, "clone timed out")
	require.NotEmpty(t, got.Remedy)

	require.NoError(t, failedDetection(state.Detection{Status: state.DetectionReady, Body: body}),
		"only a failed detection is refused this way")
}
