package planner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// TestR420_AStandbyNeverBlocksADeploy asserts that an app asking for a
// standby, with storage whose copy may be inconsistent there, plans on a
// runtime that keeps no standby: the standby and its notice are never a
// blocker (R-420, warnings are never blockers).
func TestR420_AStandbyNeverBlocksADeploy(t *testing.T) {
	s := plannableSpec()
	s.Availability.Standby = spec.StandbyOn
	s.Volumes = []spec.Volume{{ID: "vol_data", Name: "data"}}
	s.Workloads[0].Mounts = []spec.Mount{{VolumeID: "vol_data", Path: "/data"}}

	plan, err := newPlanner(t).Check(context.Background(), s)
	require.NoError(t, err)
	require.Equal(t, "ok", plan.Checks["capabilities"])
}
