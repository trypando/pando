//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/proxy"
)

// pinRouting pins a revision of app with the given routing.
func pinRouting(t *testing.T, apps *state.Apps, appID, userID string, r spec.Routing) error {
	t.Helper()
	s := minimalSpec()
	s.AppID = appID
	s.Routing = r
	rev, err := apps.CreateRevision(context.Background(), appID, s, spec.OriginEdited, userID)
	require.NoError(t, err)
	return apps.Pin(context.Background(), appID, rev.ID, state.StateProposed, userID)
}

func onPath(p string) spec.Routing {
	return spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPath, PathPrefix: p}
}

// TestR167_APathIsFoundByItsLongestPrefix asserts the lookup the proxy makes:
// the longest whole-segment prefix, case-sensitive, and nothing for a path
// that merely starts with the same characters.
func TestR167_APathIsFoundByItsLongestPrefix(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	notes, err := apps.Create(ctx, "notes", "notes-x1", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/n"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, notes.ID, alice.ID, onPath("/team/notes")))

	app, _, prefix, found, err := apps.ByPath(ctx, "/team/notes/dashboard")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, notes.ID, app.ID)
	require.Equal(t, "/team/notes", prefix)

	// The same lookup as the proxy makes it.
	_, _, prefix, found, err = proxy.NewStateResolver(apps).ByPath(ctx, "/team/notes")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "/team/notes", prefix)

	for _, miss := range []string{"/team/notes-archive", "/team", "/Team/notes/x", "/other"} {
		_, _, _, found, err := apps.ByPath(ctx, miss)
		require.NoError(t, err)
		require.False(t, found, miss)
	}
}

// TestR167_APinClaimsTheAddress asserts the rule at the database: one live app
// per hostname and path, no path inside or around another's, and none that
// takes another app's slug. A deleted app gives its address up.
func TestR167_APinClaimsTheAddress(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	mk := func(slug string) string {
		a, err := apps.Create(ctx, slug, slug, alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/" + slug})
		require.NoError(t, err)
		return a.ID
	}
	a, b := mk("alpha-a1"), mk("beta-b2")

	require.NoError(t, pinRouting(t, apps, a, alice.ID, onPath("/team")))
	for _, clash := range []string{"/team", "/team/wiki", "/alpha-a1", "/alpha-a1/x"} {
		err := pinRouting(t, apps, b, alice.ID, onPath(clash))
		require.Equal(t, errs.StateAddressTaken, errs.CodeOf(err), clash)
	}

	host := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.bemeek.io"}
	require.NoError(t, pinRouting(t, apps, a, alice.ID, host), "the same app moving from its path to a hostname")
	require.Equal(t, errs.StateAddressTaken, errs.CodeOf(pinRouting(t, apps, b, alice.ID, host)))

	// Moving off the path released it.
	require.NoError(t, pinRouting(t, apps, b, alice.ID, onPath("/team")))

	// And deleting an app releases its hostname.
	require.NoError(t, apps.Archive(ctx, a))
	require.NoError(t, pinRouting(t, apps, b, alice.ID, host))
}

// TestR165_APinRefusesPandosOwnHostname asserts R-165's reserved hostnames
// where the address is decided: a spec written whole, through POST /specs,
// never passes through address.Resolve, and its pin is what refuses it.
func TestR165_APinRefusesPandosOwnHostname(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)
	apps.ReservedHostnames = func(context.Context) []string { return []string{"pando.example.com"} }

	a, err := apps.Create(ctx, "squatter", "squatter-s1", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/s"})
	require.NoError(t, err)

	console := spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: "pando.example.com"}
	require.Equal(t, errs.StateAddressTaken, errs.CodeOf(pinRouting(t, apps, a.ID, alice.ID, console)))
	require.Equal(t, errs.StateAddressTaken, errs.CodeOf(apps.CheckAddress(ctx, a.ID, console)))

	beside := spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: "notes.pando.example.com"}
	require.NoError(t, pinRouting(t, apps, a.ID, alice.ID, beside), "a name under it is an app's to have")
}
