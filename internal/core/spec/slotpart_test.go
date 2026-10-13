package spec_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR131_APartIsReadOutOfTheSlotsConnectionString asserts R-131: one slot,
// filled once, gives each variable that takes a piece of it that piece — the
// port a URL leaves out is its scheme's.
func TestR131_APartIsReadOutOfTheSlotsConnectionString(t *testing.T) {
	url := "mysql://app:p%40ss@svc-01hq9/app"
	for part, want := range map[spec.SlotPart]string{
		"": url, spec.SlotPartHost: "svc-01hq9", spec.SlotPartPort: "3306", spec.SlotPartHostPort: "svc-01hq9:3306",
		spec.SlotPartUser: "app", spec.SlotPartPassword: "p@ss", spec.SlotPartDatabase: "app",
	} {
		got, ok := spec.SlotPartOf(url, part)
		require.True(t, ok, part)
		require.Equal(t, want, got, part)
	}
	got, _ := spec.SlotPartOf("postgresql://app:x@db:6543/app", spec.SlotPartPort)
	require.Equal(t, "6543", got)
	_, ok := spec.SlotPartOf("not a url", spec.SlotPartHost)
	require.False(t, ok)
}

// A part is only read out of a slot, and only one Pando knows how to read.
func TestR131_APartWithoutASlotIsRefused(t *testing.T) {
	ref := "DB"
	for _, e := range []spec.EnvEntry{
		{Key: "DB_HOST", SlotRef: &ref, SlotPart: "socket"},
		{Key: "DB_HOST", Value: &ref, SlotPart: spec.SlotPartHost},
	} {
		s := valid()
		s.Slots = append(s.Slots, spec.Slot{Key: "DB", Type: spec.SlotMySQL})
		s.Workloads[0].Env = []spec.EnvEntry{e}
		err := spec.Validate(s)
		require.Error(t, err)
		e := errs.As(err)
		require.Contains(t, e.Message+fmt.Sprint(e.Details), "DB_HOST")
	}

	ok := valid()
	ok.Slots = append(ok.Slots, spec.Slot{Key: "DB", Type: spec.SlotMySQL})
	ok.Workloads[0].Env = []spec.EnvEntry{{Key: "DB_HOST", SlotRef: &ref, SlotPart: spec.SlotPartHost}}
	require.NoError(t, spec.Validate(ok))
}
