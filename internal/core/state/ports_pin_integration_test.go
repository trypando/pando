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
// the check before any of them commits, and the allocation's primary key
// lets exactly one have it. The rest are told it was taken (issue #348).
func TestR165_OnePortGoesToOneAppHoweverPinsRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	const racers = 8
	routing := spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: testPort()}
	type pin struct{ appID, specID string }
	pins := make([]pin, racers)
	for i := range pins {
		app, err := apps.Create(ctx, "racer", "racer-"+string(rune('a'+i)), alice.ID, alice.ID,
			spec.Source{Type: spec.SourceGit, URL: "https://example.test/r"})
		require.NoError(t, err)
		s := minimalSpec()
		s.AppID, s.Routing = app.ID, routing
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
