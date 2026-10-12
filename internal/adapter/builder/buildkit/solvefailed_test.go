package buildkit

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR105_AFailedBuildSaysTheSolversReason asserts R-105 for a build that
// fails in the frontend before any step runs: the solver's own reason is in the
// message, without the gRPC and "failed to solve" wrapping it travelled in.
// It used to read "The build failed." with nothing in the log (issue #90).
func TestR105_AFailedBuildSaysTheSolversReason(t *testing.T) {
	err := errs.As(solveFailed(errors.New(
		"failed to solve: rpc error: code = Unknown desc = go: go.mod requires go >= 1.26.0 (running go 1.25.1)\nstack trace follows")))
	require.Equal(t, errs.BuildFailed, err.Code)
	require.Equal(t, "The build failed: go: go.mod requires go >= 1.26.0 (running go 1.25.1)", err.Message)
	require.NotContains(t, err.Message, "rpc error")
	require.NotContains(t, err.Message, "stack trace")

	step := errs.As(solveFailed(errors.New(`process "/bin/sh -c npm ci" did not complete successfully: exit code: 1`)))
	require.Contains(t, step.Message, `npm ci" did not complete successfully`)

	long := errs.As(solveFailed(errors.New(strings.Repeat("x", 2000))))
	require.Less(t, len(long.Message), 700, "held to a sentence; the log carries the rest")

	bare := errs.As(solveFailed(errors.New("failed to solve: ")))
	require.Equal(t, "The build failed.", bare.Message)
}
