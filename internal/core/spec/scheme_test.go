package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR131_OnlyAPostgresSlotTakesAScheme asserts R-131: a slot's Scheme is how
// a provisioned Postgres URL is spelled, and nothing else. A scheme Pando
// cannot write is refused when the spec is saved, not ignored at deploy.
func TestR131_OnlyAPostgresSlotTakesAScheme(t *testing.T) {
	withSlot := func(sl spec.Slot) *spec.AppSpec {
		s := valid()
		s.Slots = []spec.Slot{sl}
		return s
	}

	for _, scheme := range []string{"", "postgres", "postgresql"} {
		require.NoError(t, spec.Validate(withSlot(spec.Slot{Key: "DATABASE_URL", Type: spec.SlotPostgres, Scheme: scheme})))
	}

	for _, sl := range []spec.Slot{
		{Key: "DATABASE_URL", Type: spec.SlotPostgres, Scheme: "postgresql+asyncpg"},
		{Key: "REDIS_URL", Type: spec.SlotRedis, Scheme: "postgres"},
	} {
		err := spec.Validate(withSlot(sl))
		require.Error(t, err, sl.Scheme)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
		require.Contains(t, err.Error(), sl.Key, "R-105: the message names the slot")
	}
}

// A changed spelling rewrites the URL the app reads at start, so it restarts.
func TestR131_ARespelledPostgresURLRestartsTheApp(t *testing.T) {
	slotted := func(scheme string) *spec.AppSpec {
		s := valid()
		s.Slots = []spec.Slot{{
			Key: "DATABASE_URL", Type: spec.SlotPostgres, Scheme: scheme,
			Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
		}}
		return s
	}
	c, ok := changeAt(spec.Compare(slotted(""), slotted("postgres")), "slots.DATABASE_URL.scheme")
	require.True(t, ok)
	require.Equal(t, spec.Restart, c.Class)
	require.True(t, spec.Compare(slotted("postgres"), slotted("postgres")).Empty())
}
