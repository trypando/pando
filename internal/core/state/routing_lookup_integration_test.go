//go:build integration

package state_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/migrations"
)

// The proxy resolves every request by these lookups (issue #72), so they read
// indexed columns of apps rather than every pinned spec's JSON. These tests
// hold them to what the JSON lookups answered.

// TestR165_AHostnameIsFoundByItsAddressColumn asserts the hostname lookup:
// an app addressed by hostname is found under it, in any case, and not once it
// moves to another address or is deleted.
func TestR165_AHostnameIsFoundByItsAddressColumn(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	notes, err := apps.Create(ctx, "notes", "notes-h1", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/n"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, notes.ID, alice.ID,
		spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com"}))

	for _, host := range []string{"notes.example.com", "Notes.Example.COM"} {
		app, s, found, err := apps.ByRouting(ctx, "hostname", host)
		require.NoError(t, err)
		require.True(t, found, host)
		require.Equal(t, notes.ID, app.ID)
		require.Equal(t, "notes.example.com", s.Routing.Hostname, "the pinned spec comes with it")
	}
	_, _, found, err := apps.ByRouting(ctx, "hostname", "other.example.com")
	require.NoError(t, err)
	require.False(t, found)

	// Moved to a path: the hostname is no longer its address.
	require.NoError(t, pinRouting(t, apps, notes.ID, alice.ID, onPath("/notes")))
	_, _, found, err = apps.ByRouting(ctx, "hostname", "notes.example.com")
	require.NoError(t, err)
	require.False(t, found)

	// Back, then deleted.
	require.NoError(t, pinRouting(t, apps, notes.ID, alice.ID,
		spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.example.com"}))
	require.NoError(t, apps.Archive(ctx, notes.ID))
	_, _, found, err = apps.ByRouting(ctx, "hostname", "notes.example.com")
	require.NoError(t, err)
	require.False(t, found)
}

// TestR161_APortModeAppIsFoundByItsPortColumn asserts the port lookup: a
// port-mode app answers on its port, an app in another mode does not answer
// on a port its routing happens to record, and a moved app gives the port up.
func TestR161_APortModeAppIsFoundByItsPortColumn(t *testing.T) {
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
	onPort, onPathWithPort := mk("ported-p1"), mk("pathed-p2")

	require.NoError(t, pinRouting(t, apps, onPort, alice.ID,
		spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: 41001}))
	require.NoError(t, pinRouting(t, apps, onPathWithPort, alice.ID,
		spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPath, PathPrefix: "/pathed", Port: 41002}))

	app, s, found, err := apps.ByRouting(ctx, "port", "41001")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, onPort, app.ID)
	require.Equal(t, 41001, s.Routing.Port)

	for _, miss := range []string{"41002", "41003", "not-a-port"} {
		_, _, found, err := apps.ByRouting(ctx, "port", miss)
		require.NoError(t, err)
		require.False(t, found, miss)
	}

	require.NoError(t, pinRouting(t, apps, onPort, alice.ID, onPath("/ported")))
	_, _, found, err = apps.ByRouting(ctx, "port", "41001")
	require.NoError(t, err)
	require.False(t, found, "moved off its port")
}

// TestR167_APathMatchesOnWholeSegmentsOnly asserts the path lookup's edges:
// the app's own path, anything under it including a trailing slash, the
// longest of two candidates, and nothing that only shares characters.
func TestR167_APathMatchesOnWholeSegmentsOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	team, err := apps.Create(ctx, "team", "team-w1", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/t"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, team.ID, alice.ID, onPath("/team/wiki")))

	for _, hit := range []string{"/team/wiki", "/team/wiki/", "/team/wiki/a/b/c", "/team/wiki//x"} {
		app, _, prefix, found, err := apps.ByPath(ctx, hit)
		require.NoError(t, err)
		require.True(t, found, hit)
		require.Equal(t, team.ID, app.ID, hit)
		require.Equal(t, "/team/wiki", prefix, hit)
	}
	for _, miss := range []string{"", "/", "/team", "/team/", "/team/wikis", "/team/wik", "/Team/wiki", "team/wiki",
		"/x" + strings.Repeat("/y", 200) + "/team/wiki"} {
		_, _, _, found, err := apps.ByPath(ctx, miss)
		require.NoError(t, err)
		require.False(t, found, miss)
	}
}

// TestR161_MigrationFillsTheAddressPortFromPinnedSpecs asserts that migration
// 46 gives an app already pinned in port mode its address_port, and nothing
// to an app in another mode.
func TestR161_MigrationFillsTheAddressPortFromPinnedSpecs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	alice := seedUser(t, db, "alice")
	apps := state.NewApps(db)

	ported, err := apps.Create(ctx, "ported", "ported-m1", alice.ID, alice.ID, spec.Source{Type: spec.SourceGit, URL: "https://example.test/p"})
	require.NoError(t, err)
	require.NoError(t, pinRouting(t, apps, ported.ID, alice.ID,
		spec.Routing{AdapterRef: "rte_loopback", Mode: spec.RoutingPort, Port: 41010}))

	// As it was before the migration: the column empty.
	asOwner(t, ownerURL, `UPDATE apps SET address_port = NULL`)
	_, _, found, err := apps.ByRouting(ctx, "port", "41010")
	require.NoError(t, err)
	require.False(t, found)

	up, err := migrations.FS.ReadFile("000046_address_port.up.sql")
	require.NoError(t, err)
	var backfill string
	for _, stmt := range strings.Split(string(up), ";") {
		if i := strings.Index(stmt, "UPDATE apps"); i >= 0 {
			backfill = stmt[i:]
		}
	}
	require.NotEmpty(t, backfill)
	asOwner(t, ownerURL, backfill)

	app, _, found, err := apps.ByRouting(ctx, "port", "41010")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, ported.ID, app.ID)
}
