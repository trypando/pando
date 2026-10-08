//go:build integration

package audit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/audit"
)

// TestR386_AHoldThatCannotBeReadStopsThePass asserts R-386's failure mode: an
// archiver that cannot tell which sinks hold a month archives nothing, and
// says why, rather than archiving a month a sink has not been sent.
func TestR386_AHoldThatCannotBeReadStopsThePass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC), "app.create")

	a := r.archiverWith(audit.Retention{})
	holdErr := errors.New("audit sink state unreadable")
	a.Holds = func(context.Context, time.Time, time.Time) ([]string, error) { return nil, holdErr }

	recs, err := a.Pass(ctx)
	require.ErrorIs(t, err, holdErr)
	assert.Empty(t, recs)
	assert.Equal(t, 1, r.count(t, "action = 'app.create'"), "the month is still in the live log")
	assert.Empty(t, r.kept.objects, "and no archive was written")
}

// TestR386_EachHeldMonthIsLoggedEveryPassAndAuditedOnce asserts R-386: every
// pass logs each held month and the sinks holding it, and each month's hold is
// audited once per process, not once per pass. The holds are asked about each
// month on its own bounds.
func TestR386_EachHeldMonthIsLoggedEveryPassAndAuditedOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC), "app.create")
	r.plant(t, time.Date(2025, 4, 2, 8, 0, 0, 0, time.UTC), "app.deploy")

	core, logs := observer.New(zapcore.WarnLevel)
	a := r.archiverWith(audit.Retention{})
	a.Logger = zap.New(core)
	var asked [][2]time.Time
	a.Holds = func(_ context.Context, lo, hi time.Time) ([]string, error) {
		asked = append(asked, [2]time.Time{lo, hi})
		return []string{"as_siem"}, nil
	}
	var audited []audit.Event
	a.Audit = func(_ context.Context, e audit.Event) { audited = append(audited, e) }

	for pass := 0; pass < 2; pass++ {
		recs, err := a.Pass(ctx)
		require.NoError(t, err)
		assert.Empty(t, recs, "nothing archived while held")
	}
	assert.Equal(t, 2, r.count(t, "action IN ('app.create', 'app.deploy')"))

	march := time.Date(2025, 3, 1, 0, 0, 0, 0, time.UTC)
	assert.Contains(t, asked, [2]time.Time{march, march.AddDate(0, 1, 0)}, "a month is asked about on its own bounds")

	held := logs.FilterMessage("audit month kept in the live log until every audit sink has been sent it").All()
	require.Len(t, held, 4, "two months, logged on each of two passes")
	assert.Equal(t, "2025-03", held[0].ContextMap()["month"])
	assert.Equal(t, []any{"as_siem"}, held[0].ContextMap()["audit_sinks"])

	require.Len(t, audited, 2, "each month's hold audited once")
	for i, month := range []string{"2025-03", "2025-04"} {
		e := audited[i]
		assert.Equal(t, "audit.archive.held", e.Action)
		assert.Equal(t, audit.KindSystem, e.PrincipalKind)
		assert.Equal(t, "audit_month", e.TargetKind)
		assert.Equal(t, month, e.TargetID)
		assert.Equal(t, []string{"as_siem"}, e.Detail["audit_sinks"])
	}
}

// TestR386_AHoldWithNoAuditWriterIsStillAHold asserts a hold does not depend on
// being able to record it: with no audit writer the month is kept and logged.
func TestR386_AHoldWithNoAuditWriterIsStillAHold(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 3, 10, 12, 0, 0, 0, time.UTC), "app.create")

	core, logs := observer.New(zapcore.WarnLevel)
	a := r.archiverWith(audit.Retention{})
	a.Logger = zap.New(core)
	a.Holds = func(context.Context, time.Time, time.Time) ([]string, error) { return []string{"as_siem"}, nil }

	recs, err := a.Pass(ctx)
	require.NoError(t, err)
	assert.Empty(t, recs)
	assert.Equal(t, 1, r.count(t, "action = 'app.create'"))
	assert.Equal(t, 1, logs.FilterMessage("audit month kept in the live log until every audit sink has been sent it").Len())
}
