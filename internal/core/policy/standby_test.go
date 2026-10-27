package policy_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/policy"
)

// TestR419_StandbyIsOnByHostPolicyUnlessTurnedOff asserts the [P] default.
func TestR419_StandbyIsOnByHostPolicyUnlessTurnedOff(t *testing.T) {
	require.True(t, policy.Document{}.KeepsStandbys(), "a stored policy from before the setting has it on")
	require.True(t, policy.Default().KeepsStandbys())
	off := false
	require.False(t, policy.Document{Standby: &off}.KeepsStandbys())
}
