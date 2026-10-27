package spec

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR419_TheSpecOverridesHostPolicysStandby asserts availability.standby:
// on and off decide, default and empty follow host policy.
func TestR419_TheSpecOverridesHostPolicysStandby(t *testing.T) {
	for _, policyOn := range []bool{true, false} {
		require.True(t, Availability{Standby: StandbyOn}.WantsStandby(policyOn))
		require.False(t, Availability{Standby: StandbyOff}.WantsStandby(policyOn))
		require.Equal(t, policyOn, Availability{Standby: StandbyDefault}.WantsStandby(policyOn))
		require.Equal(t, policyOn, Availability{}.WantsStandby(policyOn))
	}
}

func TestR419_AnUnknownStandbySettingIsRefused(t *testing.T) {
	var problems []*errs.Error
	add := func(e *errs.Error) { problems = append(problems, e) }
	for _, ok := range []StandbyMode{"", StandbyDefault, StandbyOn, StandbyOff} {
		validateAvailability(&AppSpec{Availability: Availability{Standby: ok}}, add)
	}
	require.Empty(t, problems)
	validateAvailability(&AppSpec{Availability: Availability{Standby: "always"}}, add)
	require.Len(t, problems, 1)
	require.Contains(t, problems[0].Message, `availability.standby is "always"`)
}

func TestR419_AStandbyChangeIsBenignAndAbsentFromASpecThatSaysNothing(t *testing.T) {
	raw, err := json.Marshal(AppSpec{})
	require.NoError(t, err)
	require.NotContains(t, string(raw), "availability")

	old, next := &AppSpec{}, &AppSpec{Availability: Availability{Standby: StandbyOff}}
	var found bool
	for _, c := range Compare(old, next).Changes {
		if c.Path == "availability.standby" {
			found = true
			require.Equal(t, Benign, c.Class)
		}
	}
	require.True(t, found)
}
