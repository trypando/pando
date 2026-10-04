package policy_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/policy"
)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestR361_AMaintenanceWindowIsWeekdaysAStartAndALengthInUTC asserts the
// window half of R-361.
func TestR361_AMaintenanceWindowIsWeekdaysAStartAndALengthInUTC(t *testing.T) {
	w, err := policy.ParseWindow("sun,wed 02:00 2h")
	require.NoError(t, err)
	require.Equal(t, []time.Weekday{time.Sunday, time.Wednesday}, w.Days)

	require.True(t, w.Open(at("2026-10-04T02:00:00Z")), "Sunday at the start")
	require.True(t, w.Open(at("2026-10-04T03:59:59Z")))
	require.False(t, w.Open(at("2026-10-04T04:00:00Z")), "closed at start plus length")
	require.False(t, w.Open(at("2026-10-05T02:30:00Z")), "Monday is not in it")
	require.True(t, w.Open(at("2026-10-07T02:30:00+00:00")), "Wednesday")
	require.True(t, w.Open(at("2026-10-07T04:30:00+02:00")), "compared in UTC whatever the zone")

	// Running past midnight belongs to the day it opened on.
	late, err := policy.ParseWindow("sat 23:00 3h")
	require.NoError(t, err)
	require.True(t, late.Open(at("2026-10-04T01:30:00Z")), "Saturday's window, early Sunday")
	require.False(t, late.Open(at("2026-10-04T23:30:00Z")), "Sunday night is not Saturday's")

	daily, err := policy.ParseWindow("daily 03:30 1h")
	require.NoError(t, err)
	require.Len(t, daily.Days, 7)

	for _, bad := range []string{"", "sun 02:00", "funday 02:00 2h", "sun 2am 2h", "sun 25:00 2h", "sun 02:0 2h", "sun 02:00 0h", "sun 02:00 90m", "sun 02:00 25h"} {
		_, err := policy.ParseWindow(bad)
		require.Error(t, err, bad)
	}
}

// Automatic upgrades that could never run are refused when saved (R-361).
func TestR361_AutomaticUpgradesNeedInPlaceAndAWindow(t *testing.T) {
	require.NoError(t, policy.Default().ValidateRules(), "everything off is valid")
	require.ErrorContains(t, policy.Document{AutoUpgradePatches: true, MaintenanceWindow: "sun 02:00 2h"}.ValidateRules(),
		"upgrade_in_place is off")
	require.ErrorContains(t, policy.Document{AutoUpgradePatches: true, UpgradeInPlace: true}.ValidateRules(),
		"without a maintenance_window")
	require.ErrorContains(t, policy.Document{MaintenanceWindow: "whenever"}.ValidateRules(), "not a window")
	require.NoError(t, policy.Document{UpgradeInPlace: true, AutoUpgradePatches: true, MaintenanceWindow: "daily 03:00 1h"}.ValidateRules())
	require.False(t, policy.Default().UpgradeInPlace, "off by default (R-355)")
}
