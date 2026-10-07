package deploy_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/deploy"
)

// TestR256_AReplicaKnowsWhichDeployLogsItHolds asserts what decides whether a
// deploy's live log is served here or relayed to the replica running it
// (R-256, issue #72): a log is held once this process writes or follows it.
func TestR256_AReplicaKnowsWhichDeployLogsItHolds(t *testing.T) {
	logs := deploy.NewLogStore()
	require.False(t, logs.Has("dep_ran_elsewhere"))

	_, err := logs.Writer("dep_ran_here").Write([]byte("building\n"))
	require.NoError(t, err)
	require.True(t, logs.Has("dep_ran_here"))

	_, _, cancel := logs.Follow("dep_followed_here")
	defer cancel()
	require.True(t, logs.Has("dep_followed_here"))
	require.False(t, logs.Has("dep_ran_elsewhere"))
}
