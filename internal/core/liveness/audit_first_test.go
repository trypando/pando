package liveness

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR421_AFailoverWhoseAuditCannotBeWrittenDoesNotHappen asserts R-421 and
// R-228: every failover is audited, and the audit event is written before
// the standby starts. When it cannot be written nothing is started; the
// leader tries again the next round, and starts it once it can.
func TestR421_AFailoverWhoseAuditCannotBeWrittenDoesNotHappen(t *testing.T) {
	ctx := context.Background()
	silent := map[string]time.Duration{"x": 3 * tPeriod}

	// The host's loss cannot be audited: it is not marked lost, and nothing
	// starts.
	r := newWatchRig("a", "b", "c", "x")
	for _, h := range []string{"a", "b", "c"} {
		r.say(h, silent)
	}
	r.audit.failing("cluster.host.lost")
	r.round(ctx)
	require.Empty(t, r.store.lost, "a loss that cannot be audited is not recorded")
	require.Empty(t, r.failover.promoted)

	// The failover itself cannot be audited: the host is lost, its apps'
	// standbys are not started, round after round.
	r.audit.failing("cluster.failover")
	r.round(ctx)
	require.Equal(t, "liveness", r.store.lost["x"])
	require.Empty(t, r.failover.promoted)
	r.round(ctx)
	require.Empty(t, r.failover.promoted, "never started without its audit event")

	// The audit log is back: the next round starts them, once.
	r.audit.failing()
	r.round(ctx)
	require.Equal(t, []string{"app_ok"}, r.failover.promoted)
	r.round(ctx)
	require.Equal(t, []string{"app_ok"}, r.failover.promoted, "once")
	failovers := 0
	for _, e := range r.audit.events {
		if e.Action == "cluster.failover" {
			failovers++
		}
	}
	require.Equal(t, 2, failovers, "app_ok, and app_err whose promotion failed after its event")

	// By hand: refused, nothing marked and nothing started.
	m := newServiceRig("x")
	m.audit.failing("cluster.host.lost")
	_, err := m.svc.MarkLost(ctx, "x", person)
	require.Equal(t, errs.Internal, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "audit log")
	require.Empty(t, m.store.lost)
	require.Empty(t, m.failover.promoted)

	m.audit.failing("cluster.failover")
	out, err := m.svc.MarkLost(ctx, "x", person)
	require.NoError(t, err)
	require.Empty(t, m.failover.promoted)
	reasons := map[string]string{}
	for _, s := range out.Skipped {
		reasons[s.AppID] = s.Reason
	}
	require.Equal(t, "audit_unavailable", reasons["app_ok"])

	// A host no longer lost is not tried again; one that cannot be read is
	// tried the round after.
	m.audit.failing()
	m.svc.refail(ctx, map[string]bool{})
	m.svc.Settle()
	require.Empty(t, m.failover.promoted)
	m.svc.retryLater("nope", asked{person, triggerManual}, true)
	m.svc.refail(ctx, map[string]bool{"nope": true})
	m.svc.Settle()
	require.Empty(t, m.failover.promoted)
	m.svc.mu.Lock()
	require.Contains(t, m.svc.retry, "nope")
	require.NotContains(t, m.svc.retry, "x")
	m.svc.mu.Unlock()
}
