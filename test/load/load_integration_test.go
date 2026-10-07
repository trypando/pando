//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// installFake backs f's setup and app-changing routes with the database at
// ownerURL, so the harness seeds, runs against and cleans up an install whose
// schema is the real one. The routes do what Pando's do to the rows the
// harness reads back; they do not deploy anything.
func installFake(t *testing.T, f *fakePando, db *state.DB, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	users := state.NewUsers(db)
	port := f.Listener.Addr().(*net.TCPAddr).Port

	f.NeedsSetup.Store(true)
	f.SetupHook = func(username, password string) error {
		if err := users.EnsureLocalAdapter(ctx); err != nil {
			return err
		}
		digest, err := hash.New(secret.New(password))
		if err != nil {
			return err
		}
		_, _, err = users.ClaimFirst(ctx, username, "", digest, "role_administrator")
		return err
	}
	f.UserID = func(name string) string {
		var uid string
		_ = pool.QueryRow(ctx, `SELECT id FROM users WHERE external_id = $1 AND deleted_at IS NULL`, name).Scan(&uid)
		return uid
	}
	admin := func() string { return f.UserID(f.AdminUser) }
	revision := func(app string, body []byte) (int, error) {
		var n int
		err := pool.QueryRow(ctx, `
			INSERT INTO spec_revisions (id, app_id, revision, origin, body, created_by)
			SELECT $1, $2, coalesce(max(revision), 0) + 1, 'manual', $3, $4 FROM spec_revisions WHERE app_id = $2
			RETURNING revision`, id.New(id.Spec), app, body, admin()).Scan(&n)
		return n, err
	}
	reply := func(w http.ResponseWriter, err error, v any) bool {
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return true
		}
		_ = json.NewEncoder(w).Encode(v)
		return true
	}

	f.Mutate = func(w http.ResponseWriter, r *http.Request, path string) bool {
		parts := strings.Split(strings.Trim(path, "/"), "/")
		switch {
		case r.Method == http.MethodPost && path == "/apps":
			var body struct{ Name string }
			_ = json.NewDecoder(r.Body).Decode(&body)
			app := id.New(id.App)
			_, err := pool.Exec(ctx, `INSERT INTO apps (id, name, slug, owner_user_id) VALUES ($1, $2, $2, $3)`,
				app, body.Name, admin())
			return reply(w, err, map[string]string{"id": app})

		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "specs":
			b, _ := io.ReadAll(r.Body)
			n, err := revision(parts[1], b)
			return reply(w, err, map[string]int{"revision": n})

		case r.Method == http.MethodPost && len(parts) == 5 && parts[4] == "pin":
			n, _ := strconv.Atoi(parts[3])
			_, err := pool.Exec(ctx, `
				WITH s AS (SELECT id FROM spec_revisions WHERE app_id = $1 AND revision = $2),
				     p AS (INSERT INTO spec_pins (app_id, spec_id, pinned_by) SELECT $1, s.id, $3 FROM s)
				UPDATE apps SET pinned_spec_id = (SELECT id FROM s) WHERE id = $1`, parts[1], n, admin())
			return reply(w, err, map[string]string{})

		case r.Method == http.MethodPut && len(parts) == 3 && parts[2] == "routing":
			// The first real app is given this server's port, as the routing
			// endpoint allocates one; any other is left without, so a request
			// by port is unambiguous.
			var slug string
			_ = pool.QueryRow(ctx, `SELECT slug FROM apps WHERE id = $1`, parts[1]).Scan(&slug)
			routing := `{"mode":"port"}`
			if slug == realName(0) {
				routing = fmt.Sprintf(`{"mode":"port","port":%d}`, port)
				f.mu.Lock()
				f.ByPort = slug
				f.mu.Unlock()
			}
			var body []byte
			_ = pool.QueryRow(ctx, `
				SELECT jsonb_set(body, '{routing}', $2::jsonb) FROM spec_revisions
				WHERE app_id = $1 ORDER BY revision DESC LIMIT 1`, parts[1], routing).Scan(&body)
			n, err := revision(parts[1], body)
			return reply(w, err, map[string]int{"revision": n})

		case r.Method == http.MethodPost && len(parts) == 3 && parts[2] == "deployments":
			return reply(w, nil, map[string]string{"id": "dep_" + parts[1]})

		case r.Method == http.MethodGet && len(parts) == 4 && parts[2] == "deployments":
			_, err := pool.Exec(ctx, `UPDATE apps SET state = 'running', desired_state = 'running' WHERE id = $1`, parts[1])
			return reply(w, err, map[string]string{"status": "succeeded"})

		case r.Method == http.MethodDelete && len(parts) == 2:
			require.Equal(t, "true", r.URL.Query().Get("force"))
			_, err := pool.Exec(ctx, `UPDATE apps SET deleted_at = now(), bundle_destroyed_at = now() WHERE id = $1`, parts[1])
			if err == nil {
				w.WriteHeader(http.StatusNoContent)
				return true
			}
			return reply(w, err, nil)
		}
		return false
	}
}

// shareRealApps tells the fake proxy who may use each real app, read from
// the grants the seed wrote.
func shareRealApps(t *testing.T, f *fakePando, pool *pgxpool.Pool) {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT a.slug,
		       EXISTS (SELECT 1 FROM grants g WHERE g.app_id = a.id AND g.principal_kind = 'anonymous'),
		       coalesce(array_agg(gm.user_id) FILTER (WHERE gm.user_id IS NOT NULL), '{}')
		FROM apps a
		LEFT JOIN grants g ON g.app_id = a.id AND g.plane = 'data' AND g.principal_kind = 'group'
		LEFT JOIN group_members gm ON gm.group_id = g.principal_id
		WHERE a.name LIKE 'load-real-%' AND a.deleted_at IS NULL GROUP BY a.id, a.slug`)
	require.NoError(t, err)
	defer rows.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for rows.Next() {
		var slug string
		var anon bool
		var members []string
		require.NoError(t, rows.Scan(&slug, &anon, &members))
		real := fakeReal{Anonymous: anon, Members: map[string]bool{}}
		for _, m := range members {
			real.Members[m] = true
		}
		f.Real[slug] = real
	}
}

func count(t *testing.T, pool *pgxpool.Pool, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), q, args...).Scan(&n))
	return n
}

// TestLoadHarnessSeedsRunsAndCleansUpAnInstall drives the harness's four
// subcommands, in the order `make load-test` runs them, against the real
// schema behind a fake Pando.
func TestLoadHarnessSeedsRunsAndCleansUpAnInstall(t *testing.T) {
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	pool, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)

	f := newFakePando(t)
	installFake(t, f, db, pool)
	f.HealthFailures.Store(1)

	flags := []string{
		"-db", ownerURL, "-url", f.URL + "/api/v1", "-admin-user", f.AdminUser, "-admin-password", f.AdminPassword,
		"-user-password", f.UserPassword, "-token-secret", f.TokenSecret,
		"-users", "40", "-apps", "30", "-groups", "4", "-admins", "2", "-tokens", "5", "-real-apps", "2",
	}
	seed := append([]string{"-batch", "7", "-base-domain", "load.test"}, flags...)

	// seed
	require.NoError(t, seedCmd(ctx, seed))
	require.False(t, f.NeedsSetup.Load(), "setup was claimed for the administrator")

	seeded := func() map[string]int {
		return map[string]int{
			"users":        count(t, pool, `SELECT count(*) FROM users WHERE external_id LIKE 'load-u%'`),
			"members":      count(t, pool, `SELECT count(*) FROM group_members`),
			"apps":         count(t, pool, `SELECT count(*) FROM apps WHERE slug LIKE 'load-a%'`),
			"pinned":       count(t, pool, `SELECT count(*) FROM apps WHERE slug LIKE 'load-a%' AND pinned_spec_id IS NOT NULL`),
			"pins":         count(t, pool, `SELECT count(*) FROM spec_pins`),
			"hostnames":    count(t, pool, `SELECT count(*) FROM apps WHERE address_hostname LIKE 'load-a%.load.test'`),
			"paths":        count(t, pool, `SELECT count(*) FROM apps WHERE address_path LIKE '/load/load-a%'`),
			"grants":       count(t, pool, `SELECT count(*) FROM grants`),
			"admin grants": count(t, pool, `SELECT count(*) FROM grants WHERE role_scope = 'install'`),
			"tokens":       count(t, pool, `SELECT count(*) FROM tokens WHERE name LIKE 'load-token-%'`),
			"real running": count(t, pool, `SELECT count(*) FROM apps WHERE name LIKE 'load-real-%' AND state = 'running'`),
		}
	}
	first := seeded()
	require.Equal(t, 40, first["users"])
	require.Equal(t, 30, first["apps"])
	require.Equal(t, 30, first["pinned"], "every seeded app has a pinned revision")
	require.Equal(t, 15, first["hostnames"], "half are addressed by hostname")
	require.Equal(t, 15, first["paths"], "and half by path")
	require.Equal(t, 3, first["admin grants"], "the two seeded administrators and the one setup made")
	require.Equal(t, 5, first["tokens"])
	require.Equal(t, 2, first["real running"])
	require.GreaterOrEqual(t, first["members"], 40, "every user is in at least one group")
	require.Equal(t, 30+2*2, first["pins"], "one pin per seeded app, two per real app (the spec, then its port)")

	var digest string
	require.NoError(t, pool.QueryRow(ctx, `SELECT password_hash FROM users WHERE external_id = $1`, userName(3)).Scan(&digest))
	ok, err := hash.Verify(secret.New(f.UserPassword), digest)
	require.NoError(t, err)
	require.True(t, ok, "a seeded user signs in with the user password")

	// A second seed finds everything there and changes nothing, including
	// the real apps, which are already running.
	require.NoError(t, seedCmd(ctx, seed))
	require.Equal(t, first, seeded(), "seeding is resumable: a second run writes nothing new")
	require.Equal(t, 2, f.seen("POST /api/v1/apps"), "real apps are not created twice")

	// The working set the run reads back.
	ws, err := loadWorkingSet(ctx, pool, f.TokenSecret)
	require.NoError(t, err)
	require.Len(t, ws.Users, 40)
	require.Len(t, ws.Seeded, 30)
	require.Len(t, ws.Tokens, 5)
	require.Len(t, ws.Real, 2)
	admins := 0
	for _, u := range ws.Users {
		if u.Admin {
			admins++
		}
	}
	require.Equal(t, 2, admins)
	for _, tok := range ws.Tokens {
		require.True(t, strings.HasSuffix(tok.Bearer, "."+f.TokenSecret))
		require.NotEmpty(t, tok.Owns, "token k is owned by the owner of app k")
	}
	port := f.Listener.Addr().(*net.TCPAddr).Port
	require.Equal(t, port, ws.Real[0].Port, "the allocated port is read back")
	require.Zero(t, ws.Real[1].Port)
	require.True(t, ws.Real[0].Anonymous, "every other real app is shared with anyone")
	require.False(t, ws.Real[1].Anonymous)
	require.NotEmpty(t, ws.Real[1].Members, "a real app's group's members may use it")

	// run
	shareRealApps(t, f, pool)
	out := filepath.Join(t.TempDir(), "results.json")
	require.NoError(t, runCmd(ctx, append([]string{
		"-steps", "0.5,1", "-hold", "1500ms", "-keep-going", "-console-users", "4", "-api-rate", "20", "-proxy-rate", "40",
		"-sign-in-rate", "50", "-workers", "16", "-replicas", "2", "-commit", "abc1234", "-out", out, "-p95", "1m",
	}, flags...)))
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	var res Results
	require.NoError(t, json.Unmarshal(b, &res))
	require.Equal(t, "abc1234", res.Commit)
	require.Equal(t, 2, res.Replicas)
	require.False(t, res.FinishedAt.IsZero(), "a run that ran every step says it finished")
	require.False(t, res.Statements, "the test Postgres has no pg_stat_statements; top queries are sampled")
	require.Equal(t, 32, res.Seeded["apps"])
	require.Equal(t, 2, res.Seeded["apps_running"])
	require.Equal(t, 5, res.Seeded["tokens"])
	require.Len(t, res.Steps, 2)
	require.Equal(t, 2, res.Steps[0].Online)
	require.Equal(t, 4, res.Steps[1].Online, "each step brings more users online")
	surfaces := map[string]uint64{}
	for _, s := range res.Steps {
		require.Positive(t, s.PG.Samples, "Postgres was sampled while the step held")
		require.Positive(t, s.PG.MaxConnections)
		for _, c := range s.Classes {
			surfaces[c.Surface] += c.Requests
			require.Zero(t, c.Errors, "%s %s: every answer was the one Pando should give (%v)", c.Surface, c.Class, c.Statuses)
		}
	}
	for _, s := range []string{"sign-in", "console", "api", "proxy"} {
		require.Positive(t, surfaces[s], "the run exercised the %s", s)
	}

	var report strings.Builder
	require.NoError(t, WriteReport(&report, []Results{res}))
	require.Contains(t, report.String(), "**Held** through step 2")

	// cleanup
	ids := filepath.Join(t.TempDir(), "real-apps.txt")
	require.NoError(t, cleanupCmd(ctx, append([]string{"-real-apps-file", ids, "-within", "10s"}, flags...)))
	written, err := os.ReadFile(ids)
	require.NoError(t, err)
	require.Len(t, strings.Fields(string(written)), 2, "every real app's ID is written for the Makefile")
	require.Equal(t, 2, count(t, pool, `SELECT count(*) FROM apps WHERE name LIKE 'load-real-%' AND deleted_at IS NOT NULL`))
	require.NoError(t, cleanupCmd(ctx, append([]string{"-within", "1s"}, flags...)), "a second cleanup finds nothing to delete")
	require.Equal(t, 1, f.seen("DELETE /api/v1/apps/"+ws.Real[0].ID), "the first cleanup deleted it; the second did not ask again")
}

func TestLoadHarnessRefusesAnInstallItCannotUse(t *testing.T) {
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	pool, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	f := newFakePando(t)
	installFake(t, f, db, pool)
	tier := Tier{Name: "tiny", Users: 3, Apps: 2, Groups: 1}

	// A run before anything was seeded says to seed first.
	err = Run(ctx, RunOptions{Tier: tier, DatabaseURL: ownerURL, BaseURL: f.URL})
	require.ErrorContains(t, err, "run `seed` first")
	err = Run(ctx, RunOptions{Tier: tier, DatabaseURL: "postgres://pando:hunter2@[::1"})
	require.ErrorContains(t, err, "pando:[redacted]@", "a password never reaches the error")
	require.NotContains(t, err.Error(), "hunter2")

	err = Seed(ctx, SeedOptions{Tier: Tier{}, DatabaseURL: ownerURL, BaseURL: f.URL})
	require.ErrorContains(t, err, "at least one user")

	canceled, cancel := context.WithCancel(ctx)
	cancel()
	err = Seed(canceled, SeedOptions{Tier: tier, DatabaseURL: ownerURL, BaseURL: closedURL(t)})
	require.ErrorIs(t, err, context.Canceled, "an install that never answers ends with the context")

	// Setup claimed by someone else: the administrator named is not there.
	f.NeedsSetup.Store(true)
	require.NoError(t, NewClient(f.URL, time.Second).ClaimSetup(ctx, "someone-else", "pw"))
	err = Seed(ctx, SeedOptions{Tier: tier, DatabaseURL: ownerURL, BaseURL: f.URL, AdminUser: "admin", AdminPassword: "x"})
	require.ErrorContains(t, err, `finding the administrator "admin"`)

	f.NeedsSetup.Store(true)
	f.SetupHook = func(string, string) error { return fmt.Errorf("refused") }
	err = Seed(ctx, SeedOptions{Tier: tier, DatabaseURL: ownerURL, BaseURL: f.URL, AdminUser: "admin", AdminPassword: "x"})
	require.ErrorContains(t, err, "claiming setup")
	f.NeedsSetup.Store(false)

	err = Seed(ctx, SeedOptions{Tier: tier, DatabaseURL: "postgres://pando:hunter2@[::1", BaseURL: f.URL, AdminUser: "admin"})
	require.ErrorContains(t, err, "connecting to postgres://pando:[redacted]@")

	require.Error(t, Cleanup(ctx, f.URL, "postgres://[::1", "admin", "x", "", time.Second))

	// Real apps whose containers outlive the wait are reported, by label.
	_, err = pool.Exec(ctx, `INSERT INTO apps (id, name, slug, deleted_at) VALUES ($1, 'load-real-009', 'load-real-009', now())`,
		id.New(id.App))
	require.NoError(t, err)
	err = Cleanup(ctx, f.URL, ownerURL, "admin", "x", filepath.Join(t.TempDir(), "no", "such", "file"), 0)
	require.Error(t, err, "the IDs file could not be written")
	err = Cleanup(ctx, f.URL, ownerURL, "admin", "x", "", 0)
	require.ErrorContains(t, err, "1 real apps still had containers")
	require.ErrorContains(t, err, "io.pando.bundle=<app ID>")
	canceled, cancel = context.WithCancel(ctx)
	cancel()
	require.Error(t, Cleanup(canceled, f.URL, ownerURL, "admin", "x", "", time.Hour))
}

func TestPGSamplesTheInstallsDatabase(t *testing.T) {
	ctx := context.Background()
	_, ownerURL := statetest.Connect(t)
	pg, err := NewPG(ctx, ownerURL)
	require.NoError(t, err)
	defer pg.Close()

	pg.Begin(ctx)
	// Something to catch running: a backend busy for the whole sample.
	busy, err := pgxpool.New(ctx, ownerURL)
	require.NoError(t, err)
	defer busy.Close()
	done := make(chan struct{})
	go func() { _, _ = busy.Exec(ctx, `SELECT pg_sleep(1.5)`); close(done) }()
	require.Eventually(t, func() bool {
		pg.once(ctx)
		_, top := pg.End(ctx, 5)
		return len(top) > 0
	}, 5*time.Second, 50*time.Millisecond)
	sample, top := pg.End(ctx, 5)
	<-done
	require.Positive(t, sample.Samples)
	require.Positive(t, sample.MaxConnections)
	require.GreaterOrEqual(t, sample.PeakTotal, 1)
	if !pg.Statements {
		require.Equal(t, "SELECT pg_sleep(1.5)", top[0].Query)
		require.Positive(t, top[0].Seen)
	}

	counts := pg.Counts(ctx)
	require.Equal(t, 0, counts["apps"])
	require.Contains(t, counts, "replicas_live")

	pg.Begin(ctx)
	sample, _ = pg.End(ctx, 5)
	require.Zero(t, sample.Samples, "Begin starts a new step's sample")

	_, err = NewPG(ctx, "postgres://[::1")
	require.Error(t, err)
}
