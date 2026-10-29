//go:build integration

package state_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR165_OnePortGoesToOneAppHoweverPinsRace asserts R-165's one app per
// address for ports when pins race: apps pinning one port at once all pass
// the check before any of them commits, and the unique index on the
// allocation's port lets exactly one have it, though they pin it on two
// routing adapters: the proxy listens by number alone. The rest are told it
// was taken (issue #348).
func TestR165_OnePortGoesToOneAppHoweverPinsRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	const racers = 8
	port := testPort()
	type pin struct{ appID, specID string }
	pins := make([]pin, racers)
	for i := range pins {
		app, err := apps.Create(ctx, "racer", "racer-"+string(rune('a'+i)), alice.ID, alice.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/r"})
		require.NoError(t, err)
		s := minimalSpec()
		s.AppID = app.ID
		s.Routing = spec.Routing{AdapterRef: []string{"rte_loopback", "rte_traefik"}[i%2], Mode: spec.RoutingPort, Port: port}
		rev, err := apps.CreateRevision(ctx, app.ID, s, spec.OriginEdited, alice.ID)
		require.NoError(t, err)
		pins[i] = pin{app.ID, rev.ID}
	}

	start := make(chan struct{})
	results := make([]error, racers)
	var wg sync.WaitGroup
	for i, p := range pins {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = apps.Pin(ctx, p.appID, p.specID, state.StateProposed, alice.ID)
		}()
	}
	close(start)
	wg.Wait()

	won := 0
	for _, err := range results {
		if err == nil {
			won++
			continue
		}
		require.Equal(t, errs.StateAddressTaken, errs.CodeOf(err), "a losing pin is told the port was taken: %v", err)
	}
	require.Equal(t, 1, won, "exactly one app has the port")
}

// TestR165_APinThatLosesTheRaceForAPortIsToldItWasTaken asserts the race
// above one step at a time: another app's claim on the port is written but
// not committed, so the pin's check passes; the pin waits on the claim's row;
// and when the claim commits, the pin is told the port was taken on another
// adapter, rather than given it or failing with an internal error.
func TestR165_APinThatLosesTheRaceForAPortIsToldItWasTaken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	port := testPort()

	first, err := apps.Create(ctx, "first", "first-race", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/f"})
	require.NoError(t, err)
	second, err := apps.Create(ctx, "second", "second-race", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/s"})
	require.NoError(t, err)
	s := minimalSpec()
	s.AppID = second.ID
	s.Routing = spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: port}
	rev, err := apps.CreateRevision(ctx, second.ID, s, spec.OriginEdited, alice.ID)
	require.NoError(t, err)

	claim, err := db.Begin(ctx)
	require.NoError(t, err)
	defer func() { _ = claim.Rollback(ctx) }()
	_, err = claim.Exec(ctx, `INSERT INTO port_allocations (adapter_ref, port, app_id) VALUES ('rte_traefik', $1, $2)`, port, first.ID)
	require.NoError(t, err)

	pinned := make(chan error, 1)
	go func() { pinned <- apps.Pin(ctx, second.ID, rev.ID, state.StateProposed, alice.ID) }()
	require.Eventually(t, func() bool {
		var waiting int
		_ = db.QueryRow(ctx, `
			SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting)
		return waiting > 0
	}, 10*time.Second, 50*time.Millisecond, "the pin waits on the uncommitted claim")
	require.NoError(t, claim.Commit(ctx))

	err = <-pinned
	require.Equal(t, errs.StateAddressTaken, errs.CodeOf(err), "%v", err)
	require.Contains(t, err.Error(), "a moment ago")
}

// TestR166_AnAppKeepsAPortItWasPinnedToBeforePinsClaimedPorts asserts that an
// app pinned before pins claimed their port — reached at a port with no
// allocation behind it, outside the range the install now gives — can pin it
// again, and that doing so records the allocation, so the proxy listens on it.
func TestR166_AnAppKeepsAPortItWasPinnedToBeforePinsClaimedPorts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	app, err := apps.Create(ctx, "old", "old-port", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/o"})
	require.NoError(t, err)
	outside := spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: 41500}
	require.NoError(t, pinRouting(t, apps, app.ID, alice.ID, outside))
	_, err = db.Exec(ctx, `DELETE FROM port_allocations WHERE app_id = $1`, app.ID)
	require.NoError(t, err, "as an app pinned before this change has it")

	apps.PortRangeStart, apps.PortRangeEnd = 9000, 9019
	require.NoError(t, pinRouting(t, apps, app.ID, alice.ID, outside), "its own port, though outside the range")
	listening, err := state.NewPorts(db).InUse(ctx)
	require.NoError(t, err)
	require.Contains(t, listening, 41500)

	other, err := apps.Create(ctx, "new", "new-port", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/n"})
	require.NoError(t, err)
	refused := pinRouting(t, apps, other.ID, alice.ID, spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: 41501})
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(refused), "another app is held to the range: %v", refused)
}

// TestR165_APortReachesOnlyTheAppHoldingIt asserts R-165 for an install that
// already has two apps at one port when it upgrades: one that a spec naming
// another app's port was pinned to before pins checked ports, or one that
// lost a duplicate in migration 97. The port reaches the app holding its
// allocation, every time, and not the other; when the holder moves, the other
// is not served there until it pins the port and so claims it.
func TestR165_APortReachesOnlyTheAppHoldingIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	port := testPort()
	at := spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: port}

	holder, err := apps.Create(ctx, "holder", "holder-port", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/h"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, holder.ID, alice.ID, at))
	other, err := apps.Create(ctx, "other", "other-port", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/o"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, other.ID, alice.ID, spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: testPort()}))
	// As such an install has it: the other app is at the port, with nothing allocated.
	_, err = db.Exec(ctx, `DELETE FROM port_allocations WHERE app_id = $1`, other.ID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `UPDATE apps SET address_port = $2 WHERE id = $1`, other.ID, port)
	require.NoError(t, err)

	for range 5 {
		app, _, found, err := apps.ByRouting(ctx, "port", strconv.Itoa(port))
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, holder.ID, app.ID, "the port reaches the app holding it")
	}

	require.NoError(t, pinRouting(t, apps, holder.ID, alice.ID, spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: testPort()}))
	_, _, found, err := apps.ByRouting(ctx, "port", strconv.Itoa(port))
	require.NoError(t, err)
	require.False(t, found, "an app holding no allocation is not served at the port")

	require.NoError(t, pinRouting(t, apps, other.ID, alice.ID, at), "the port is free, so its pin claims it")
	app, _, found, err := apps.ByRouting(ctx, "port", strconv.Itoa(port))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, other.ID, app.ID)
}
