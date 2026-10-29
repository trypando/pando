//go:build integration

package state_test

import (
	"context"
	"sync"
	"testing"

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
