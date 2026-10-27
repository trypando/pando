//go:build integration

package reconciler_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/reconciler"
	"github.com/trypando/pando/internal/core/state"
)

// TestR153_TheReconcilerNeverRestartsAFencedOrFailedOverCopy asserts R-153
// and R-421 against R-148: an app whose host is marked lost, whose host is
// fenced, or whose standby is being promoted is not restarted however it is
// observed, and is reported unobservable rather than counted toward failing
// (R-151); once nothing holds it, a missing workload is restored as before.
// A replica whose own host is fenced reconciles nothing at all.
func TestR153_TheReconcilerNeverRestartsAFencedOrFailedOverCopy(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)
	h.rec.Holds = state.NewStandbys(h.db)
	h.runtime.setObserved(api.ObservedBundle{Exists: true}) // stopped by the fence

	hostID := "host_" + strings.ToLower(h.appID[4:])
	name := "h-" + strings.ToLower(h.appID[len(h.appID)-10:])
	_, err := h.db.Exec(ctx, `INSERT INTO cluster_hosts (id, name, roles, address, cert_sha256)
		VALUES ($1, $2, '{app}', '10.0.0.6:7443', $3)`, hostID, name, strings.Repeat("0", 64))
	require.NoError(t, err)
	_, err = h.db.Exec(ctx, `INSERT INTO app_standbys (app_id, host, home_host) VALUES ($1, 'other', $2)`, h.appID, name)
	require.NoError(t, err)

	for _, hold := range []string{
		`UPDATE cluster_hosts SET lost_at = now(), lost_by = 'liveness' WHERE id = $1`,
		`UPDATE cluster_hosts SET lost_at = NULL, lost_by = NULL, fenced_at = now() WHERE id = $1`,
	} {
		_, err = h.db.Exec(ctx, hold, hostID)
		require.NoError(t, err)
		h.rec.Tick(ctx)
		_, applies, _ := h.runtime.counts()
		require.Zero(t, applies, hold)
		require.Equal(t, state.StateRunning, h.state(t), "counted toward nothing")
	}
	_, err = h.db.Exec(ctx, `UPDATE cluster_hosts SET fenced_at = NULL WHERE id = $1`, hostID)
	require.NoError(t, err)
	_, err = h.db.Exec(ctx, `UPDATE app_standbys SET promoting_at = now() WHERE app_id = $1`, h.appID)
	require.NoError(t, err)
	h.rec.Tick(ctx)
	_, applies, _ := h.runtime.counts()
	require.Zero(t, applies, "a promotion is starting it")

	var reason string
	require.NoError(t, h.db.QueryRow(ctx, `SELECT coalesce(last_reconcile_error, '') FROM apps WHERE id = $1`, h.appID).Scan(&reason))
	require.Contains(t, reason, "being started on")

	// This replica's own host fenced: nothing is even observed.
	_, err = h.db.Exec(ctx, `UPDATE app_standbys SET promoting_at = NULL WHERE app_id = $1`, h.appID)
	require.NoError(t, err)
	h.rec.LocalFenced = func() bool { return true }
	observes, _, _ := h.runtime.counts()
	h.rec.Tick(ctx)
	after, applies, _ := h.runtime.counts()
	require.Equal(t, observes, after)
	require.Zero(t, applies)

	h.rec.LocalFenced = nil
	h.rec.Tick(ctx)
	_, applies, _ = h.runtime.counts()
	require.Equal(t, 1, applies, "held by nothing, a missing workload is restored (R-148)")
}

// TestR151_AnAppWithoutAStandbyIsNotFailedWhileItsHostIsDown asserts R-151
// with R-421: an app with no standby, whose host is lost or fenced, is left
// alone and reported unobservable, however many visits pass, rather than
// counted toward failed because its host stopped it. Its host is the one the
// runtime found it on. Once the host is back, it is restored (R-148).
func TestR151_AnAppWithoutAStandbyIsNotFailedWhileItsHostIsDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, state.StateRunning)
	h.rec.Holds = state.NewStandbys(h.db)
	clk := &steppingClock{now: time.Now().UTC()}
	h.rec.Clock = clk

	hostID := "host_" + strings.ToLower(h.appID[4:])
	name := "h-" + strings.ToLower(h.appID[len(h.appID)-10:])
	_, err := h.db.Exec(ctx, `INSERT INTO cluster_hosts (id, name, roles, address, cert_sha256)
		VALUES ($1, $2, '{app}', '10.0.0.6:7443', $3)`, hostID, name, strings.Repeat("0", 64))
	require.NoError(t, err)
	// Stopped by the fence, on that host; and its agent refuses to start it.
	h.runtime.setObserved(api.ObservedBundle{Exists: true, Host: name})
	h.runtime.mu.Lock()
	h.runtime.applyErr = errApplyFailed
	h.runtime.mu.Unlock()

	for _, hold := range []string{
		`UPDATE cluster_hosts SET fenced_at = now() WHERE id = $1`,
		`UPDATE cluster_hosts SET fenced_at = NULL, lost_at = now(), lost_by = 'liveness' WHERE id = $1`,
	} {
		_, err = h.db.Exec(ctx, hold, hostID)
		require.NoError(t, err)
		for range reconciler.DefaultFailureThreshold {
			h.rec.Tick(ctx)
			clk.advance(10 * time.Minute)
		}
		_, applies, _ := h.runtime.counts()
		require.Zero(t, applies, hold)
		require.Equal(t, state.StateRunning, h.state(t), "counted toward nothing: %s", hold)
		var reason string
		require.NoError(t, h.db.QueryRow(ctx, `SELECT coalesce(last_reconcile_error, '') FROM apps WHERE id = $1`, h.appID).Scan(&reason))
		require.Contains(t, reason, name)
	}

	// The host is back: the app is restored.
	_, err = h.db.Exec(ctx, `UPDATE cluster_hosts SET lost_at = NULL, lost_by = NULL WHERE id = $1`, hostID)
	require.NoError(t, err)
	h.runtime.mu.Lock()
	h.runtime.applyErr = nil
	h.runtime.mu.Unlock()
	h.rec.Tick(ctx)
	_, applies, _ := h.runtime.counts()
	require.Equal(t, 1, applies, "held by nothing, a missing workload is restored (R-148)")
}
