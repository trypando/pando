//go:build integration

package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// TestR048_EveryRevocationReachesTheProxyCache asserts the mechanism the proxy
// cache's correctness rests on (issue #93, migration 68): each kind of change
// that could make a kept answer wrong — a session revoked, a grant removed, a
// person suspended or dropped from a group, an app stopped — notifies every
// listening replica when it commits. And the busiest write that changes
// nothing the proxy reads, an app's reconcile lease, does not.
func TestR048_EveryRevocationReachesTheProxyCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, _ := statetest.Connect(t)

	first, err := bootstrap.Run(ctx, state.NewUsers(db), state.NewGrants(db), db, audit.New(db.Pool),
		secret.New("a-first-password-123"))
	require.NoError(t, err)
	owner := first.User.ID
	src := spec.Source{Type: spec.SourceGit, URL: "https://example.test/notes", Ref: "main"}
	app, err := state.NewApps(db).Create(ctx, "notes", id.New(id.App), owner, owner, src)
	require.NoError(t, err)
	sess, err := state.NewSessions(db).Create(ctx, owner, first.User.AdapterID, time.Hour, "test", "")
	require.NoError(t, err)
	groupID := id.New(id.Group)
	_, err = db.Exec(ctx, `INSERT INTO groups (id, name) VALUES ($1, 'engineering')`, groupID)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, groupID, owner)
	require.NoError(t, err)

	c := &Cache{}
	go c.Listen(ctx, db)
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.listening
	}, 10*time.Second, 10*time.Millisecond)

	notifies := func(t *testing.T, what string, sql string, args ...any) {
		t.Helper()
		before := generationOf(c)
		_, err := db.Exec(ctx, sql, args...)
		require.NoError(t, err, what)
		require.Eventually(t, func() bool { return generationOf(c) > before }, 5*time.Second, 5*time.Millisecond,
			"%s must empty the cache on every replica", what)
	}

	notifies(t, "a session revoked", `UPDATE sessions SET revoked_at = now() WHERE id = $1`, sess.ID)
	notifies(t, "a group membership removed", `DELETE FROM group_members WHERE group_id = $1`, groupID)
	notifies(t, "a grant removed", `DELETE FROM grants WHERE app_id = $1 AND plane = 'data'`, app.ID)
	notifies(t, "an app stopped", `UPDATE apps SET desired_state = 'stopped', state = 'stopped' WHERE id = $1`, app.ID)
	notifies(t, "a person suspended", `UPDATE users SET status = 'suspended' WHERE id = $1`, owner)

	before := generationOf(c)
	_, err = db.Exec(ctx, `UPDATE apps SET reconcile_lease_until = now() + interval '30 seconds' WHERE id = $1`, app.ID)
	require.NoError(t, err)
	require.Never(t, func() bool { return generationOf(c) > before }, 300*time.Millisecond, 10*time.Millisecond,
		"a reconcile lease changes nothing the proxy reads")
}

// TestR023_TheResolverKeepsItsLookupsInTheCache asserts the resolver's side
// of issue #93 against the real store: hostname, slug and port lookups go
// through the cache, misses included, and the router's hostname check is the
// same lookup as the proxy's.
func TestR023_TheResolverKeepsItsLookupsInTheCache(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	db, _ := statetest.Connect(t)

	c := &Cache{}
	go c.Listen(ctx, db)
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.listening
	}, 10*time.Second, 10*time.Millisecond)

	r := NewStateResolver(state.NewApps(db))
	r.Cache = c

	_, _, found, err := r.ByHostname(ctx, "nobody.example.com")
	require.NoError(t, err)
	require.False(t, found)
	_, _, found, err = r.BySlug(ctx, "nobody")
	require.NoError(t, err)
	require.False(t, found)
	_, _, found, err = r.ByPort(ctx, 9123)
	require.NoError(t, err)
	require.False(t, found)

	isApp, err := r.IsAppHostname(ctx, "nobody.example.com")
	require.NoError(t, err)
	require.False(t, isApp)
	isApp, err = r.IsAppHostname(ctx, "")
	require.NoError(t, err)
	require.False(t, isApp)

	c.mu.Lock()
	kept := len(c.apps)
	c.mu.Unlock()
	require.Equal(t, 3, kept, "each miss kept once; the router's check reused the proxy's")
}

func generationOf(c *Cache) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}
