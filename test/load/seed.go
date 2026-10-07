package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// localAdapterID is state.LocalAdapterID. Repeated rather than imported:
// internal/core/state pulls in the whole service layer, and the harness only
// needs the one string.
const localAdapterID = "idp_local"

// SeedOptions are the seed subcommand's inputs.
type SeedOptions struct {
	Tier          Tier
	DatabaseURL   string
	BaseURL       string
	BaseDomain    string
	AdminUser     string
	AdminPassword string
	UserPassword  string
	TokenSecret   string
	RealPortStart int
	Batch         int
}

// Seed writes a tier's users, groups, apps, grants and tokens directly into
// Postgres as the database owner, then deploys the tier's real apps through
// the API. Safe to run again: every row has a fixed ID and is inserted with
// ON CONFLICT DO NOTHING, in batches that commit on their own, so an
// interrupted seed resumes where it stopped.
func Seed(ctx context.Context, o SeedOptions) error {
	if err := o.Tier.Validate(); err != nil {
		return err
	}
	c := NewClient(o.BaseURL, 60*time.Second)
	if err := c.AwaitHealthy(ctx, 3*time.Minute); err != nil {
		return err
	}
	// The install must exist before anything is written into it: setup makes
	// the local identity adapter and the first administrator.
	if err := c.ClaimSetup(ctx, o.AdminUser, o.AdminPassword); err != nil {
		return fmt.Errorf("claiming setup: %w", err)
	}

	pool, err := pgxpool.New(ctx, o.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connecting to %s: %w", redactURL(o.DatabaseURL), err)
	}
	defer pool.Close()

	var adminID string
	if err := pool.QueryRow(ctx, `SELECT id FROM users WHERE adapter_id = $1 AND external_id = $2 AND deleted_at IS NULL`,
		localAdapterID, o.AdminUser).Scan(&adminID); err != nil {
		return fmt.Errorf("finding the administrator %q: %w", o.AdminUser, err)
	}

	start := time.Now()
	s := &seeder{pool: pool, o: o, adminID: adminID}
	for _, phase := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"users", s.users},
		{"groups", s.groups},
		{"group members", s.members},
		{"administrators", s.admins},
		{"apps", s.apps},
		{"pinned specs", s.specs},
		{"grants", s.grants},
		{"tokens", s.tokens},
	} {
		t := time.Now()
		if err := phase.run(ctx); err != nil {
			return fmt.Errorf("seeding %s: %w", phase.name, err)
		}
		log.Printf("seeded %s in %s", phase.name, time.Since(t).Round(time.Millisecond))
	}
	// Planner statistics for what was just written, as autovacuum would
	// gather in time; otherwise the first minutes of a run measure plans made
	// for empty tables.
	if _, err := pool.Exec(ctx, `ANALYZE users, groups, group_members, apps, spec_revisions, spec_pins, grants, tokens`); err != nil {
		return fmt.Errorf("analyzing: %w", err)
	}
	log.Printf("database seeded in %s", time.Since(start).Round(time.Millisecond))

	if o.Tier.RealApps > 0 {
		if err := s.realApps(ctx, c); err != nil {
			return fmt.Errorf("deploying real apps: %w", err)
		}
	}
	return nil
}

type seeder struct {
	pool    *pgxpool.Pool
	o       SeedOptions
	adminID string
}

// batches calls fn over [0, n) in batches.
func (s *seeder) batches(ctx context.Context, n int, fn func(ctx context.Context, lo, hi int) error) error {
	size := s.o.Batch
	if size <= 0 {
		size = 5_000
	}
	for lo := 0; lo < n; lo += size {
		hi := min(lo+size, n)
		if err := fn(ctx, lo, hi); err != nil {
			return fmt.Errorf("rows %d–%d: %w", lo, hi, err)
		}
	}
	return nil
}

// count answers a count query, for skipping a phase already complete.
func (s *seeder) count(ctx context.Context, query string, args ...any) (int, error) {
	var n int
	err := s.pool.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}

// copyInsert copies rows into a staging table shaped like table and inserts
// them, skipping any that are already there, in one transaction.
func (s *seeder) copyInsert(ctx context.Context, table string, cols []string, rows [][]any) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		return copyInsertTx(ctx, tx, table, cols, rows, "")
	})
}

// copyInsertTx is copyInsert inside a caller's transaction. where filters the
// staged rows (alias s) before they are inserted.
func copyInsertTx(ctx context.Context, tx pgx.Tx, table string, cols []string, rows [][]any, where string) error {
	stage := "load_stage_" + table
	if _, err := tx.Exec(ctx, fmt.Sprintf(`CREATE TEMP TABLE %s (LIKE %s INCLUDING DEFAULTS) ON COMMIT DROP`, stage, table)); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{stage}, cols, pgx.CopyFromRows(rows)); err != nil {
		return err
	}
	list := strings.Join(cols, ", ")
	q := fmt.Sprintf(`INSERT INTO %s (%s) SELECT %s FROM %s s %s ON CONFLICT DO NOTHING`, table, list, prefixed("s.", cols), stage, where)
	_, err := tx.Exec(ctx, q)
	return err
}

func prefixed(p string, cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = p + c
	}
	return strings.Join(out, ", ")
}

func (s *seeder) users(ctx context.Context) error {
	t := s.o.Tier
	have, err := s.count(ctx, `SELECT count(*) FROM users WHERE adapter_id = $1 AND external_id LIKE $2`, localAdapterID, userPrefix+"%")
	if err != nil || have >= t.Users {
		return err
	}
	// One hash for everyone. argon2id at its shipped cost is tens of
	// milliseconds and 64 MiB each; a hundred thousand of them would make the
	// seed the slowest part of the run. A hash records its own salt and
	// parameters, so the same one verifies for every account.
	digest, err := hash.New(secret.New(s.o.UserPassword))
	if err != nil {
		return err
	}
	cols := []string{"id", "adapter_id", "external_id", "email", "display_name", "status", "password_hash", "must_change_password"}
	return s.batches(ctx, t.Users, func(ctx context.Context, lo, hi int) error {
		rows := make([][]any, 0, hi-lo)
		for i := lo; i < hi; i++ {
			name := userName(i)
			rows = append(rows, []any{userID(i), localAdapterID, name, name + "@load.example", fmt.Sprintf("Load User %d", i),
				"active", digest, false})
		}
		return s.copyInsert(ctx, "users", cols, rows)
	})
}

func (s *seeder) groups(ctx context.Context) error {
	t := s.o.Tier
	cols := []string{"id", "adapter_id", "external_id", "name"}
	rows := make([][]any, 0, t.Groups)
	for g := 0; g < t.Groups; g++ {
		rows = append(rows, []any{groupID(g), nil, nil, groupName(g)})
	}
	return s.copyInsert(ctx, "groups", cols, rows)
}

func (s *seeder) members(ctx context.Context) error {
	t := s.o.Tier
	cols := []string{"group_id", "user_id"}
	return s.batches(ctx, t.Users, func(ctx context.Context, lo, hi int) error {
		rows := make([][]any, 0, 2*(hi-lo))
		for i := lo; i < hi; i++ {
			for _, g := range userGroups(t, i) {
				rows = append(rows, []any{groupID(g), userID(i)})
			}
		}
		return s.copyInsert(ctx, "group_members", cols, rows)
	})
}

var grantCols = []string{"id", "app_id", "plane", "principal_kind", "principal_id", "role_id", "role_scope", "created_by"}

func (s *seeder) grantRows(rows []grantRow) [][]any {
	out := make([][]any, 0, len(rows))
	for _, g := range rows {
		out = append(out, []any{g.ID, g.AppID, g.Plane, g.PrincipalKind, g.PrincipalID, g.RoleID, g.RoleScope, s.adminID})
	}
	return out
}

func (s *seeder) admins(ctx context.Context) error {
	rows := make([]grantRow, 0, s.o.Tier.Admins)
	for i := 0; i < s.o.Tier.Admins; i++ {
		rows = append(rows, adminGrant(i))
	}
	return s.copyInsert(ctx, "grants", grantCols, s.grantRows(rows))
}

func (s *seeder) apps(ctx context.Context) error {
	t := s.o.Tier
	have, err := s.count(ctx, `SELECT count(*) FROM apps WHERE slug LIKE $1 AND deleted_at IS NULL`, appPrefix+"%")
	if err != nil || have >= t.Apps {
		return err
	}
	source, err := json.Marshal(seedSource)
	if err != nil {
		return err
	}
	// Stopped, and wanted stopped: the reconciler observes these and finds
	// nothing to do (R-140), so the install is not asked to start twenty
	// thousand containers the host cannot hold.
	cols := []string{"id", "name", "slug", "owner_user_id", "state", "desired_state", "source"}
	return s.batches(ctx, t.Apps, func(ctx context.Context, lo, hi int) error {
		rows := make([][]any, 0, hi-lo)
		for j := lo; j < hi; j++ {
			slug := appSlug(j)
			rows = append(rows, []any{appID(j), slug, slug, userID(appOwner(t, j)), "stopped", "stopped", string(source)})
		}
		return s.copyInsert(ctx, "apps", cols, rows)
	})
}

// specs writes each app's one revision and pins it, as state.Apps.Pin does:
// a spec_pins row, the app's pinned_spec_id, and its address columns.
func (s *seeder) specs(ctx context.Context) error {
	t := s.o.Tier
	have, err := s.count(ctx, `SELECT count(*) FROM apps WHERE slug LIKE $1 AND deleted_at IS NULL AND pinned_spec_id IS NOT NULL`, appPrefix+"%")
	if err != nil || have >= t.Apps {
		return err
	}
	return s.batches(ctx, t.Apps, func(ctx context.Context, lo, hi int) error {
		revs := make([][]any, 0, hi-lo)
		pins := make([][]any, 0, hi-lo)
		for j := lo; j < hi; j++ {
			body, err := appSpec(t, j, s.o.BaseDomain, s.adminID)
			if err != nil {
				return err
			}
			b, err := json.Marshal(body)
			if err != nil {
				return err
			}
			revs = append(revs, []any{specID(j), appID(j), 1, "manual", string(b), s.adminID})
			host, path := "", ""
			if appUsesHostname(j) {
				host = strings.ToLower(appHostname(j, s.o.BaseDomain))
			} else {
				path = appPath(j)
			}
			pins = append(pins, []any{appID(j), specID(j), host, path})
		}
		return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
			if err := copyInsertTx(ctx, tx, "spec_revisions",
				[]string{"id", "app_id", "revision", "origin", "body", "created_by"}, revs, ""); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `CREATE TEMP TABLE load_stage_pins (app_id text, spec_id text, hostname text, path text) ON COMMIT DROP`); err != nil {
				return err
			}
			if _, err := tx.CopyFrom(ctx, pgx.Identifier{"load_stage_pins"}, []string{"app_id", "spec_id", "hostname", "path"},
				pgx.CopyFromRows(pins)); err != nil {
				return err
			}
			// Only apps not yet pinned, so a resumed seed writes no second
			// pin event for an app it already pinned.
			if _, err := tx.Exec(ctx, `
				INSERT INTO spec_pins (app_id, spec_id, pinned_by)
				SELECT p.app_id, p.spec_id, $1 FROM load_stage_pins p
				JOIN apps a ON a.id = p.app_id WHERE a.pinned_spec_id IS NULL`, s.adminID); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				UPDATE apps a SET pinned_spec_id = p.spec_id,
				       address_hostname = NULLIF(p.hostname, ''), address_path = NULLIF(p.path, '')
				FROM load_stage_pins p WHERE a.id = p.app_id AND a.pinned_spec_id IS NULL`)
			return err
		})
	})
}

func (s *seeder) grants(ctx context.Context) error {
	t := s.o.Tier
	return s.batches(ctx, t.Apps, func(ctx context.Context, lo, hi int) error {
		var rows []grantRow
		for j := lo; j < hi; j++ {
			rows = append(rows, appGrants(t, j)...)
		}
		return s.copyInsert(ctx, "grants", grantCols, s.grantRows(rows))
	})
}

// tokens are delegated tokens, token t owned by the owner of app t, so an API
// client always has an app of its own to read.
func (s *seeder) tokens(ctx context.Context) error {
	t := s.o.Tier
	if t.Tokens == 0 {
		return nil
	}
	have, err := s.count(ctx, `SELECT count(*) FROM tokens WHERE name LIKE $1`, tokenPrefix+"%")
	if err != nil || have >= t.Tokens {
		return err
	}
	// One secret for every seeded token, for the reason one password serves
	// every user. The token string is "<id>.<secret>" (state.Tokens).
	digest, err := hash.New(secret.New(s.o.TokenSecret))
	if err != nil {
		return err
	}
	cols := []string{"id", "kind", "name", "hash", "owner_user_id", "created_by"}
	return s.batches(ctx, t.Tokens, func(ctx context.Context, lo, hi int) error {
		rows := make([][]any, 0, hi-lo)
		for k := lo; k < hi; k++ {
			owner := userID(appOwner(t, k))
			rows = append(rows, []any{tokenID(k), "delegated", tokenName(k), digest, owner, owner})
		}
		return s.copyInsert(ctx, "tokens", cols, rows)
	})
}

// realApps deploys the tier's real apps through the API, as an administrator
// would, then shares each one: with a seeded group, and every other one with
// anyone (R-074), so the proxy sees allowed, refused and anonymous traffic.
func (s *seeder) realApps(ctx context.Context, c *Client) error {
	cookie, _, err := c.SignIn(ctx, s.o.AdminUser, s.o.AdminPassword)
	if err != nil {
		return err
	}
	cred := Credential{Cookie: cookie}
	t := s.o.Tier

	ids := make([]string, t.RealApps)
	errs := make([]error, t.RealApps)
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for k := 0; k < t.RealApps; k++ {
		wg.Add(1)
		go func(k int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ids[k], errs[k] = s.realApp(ctx, c, cred, k)
		}(k)
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}

	var rows []grantRow
	for k, app := range ids {
		rows = append(rows, grantRow{ID: seedID(id.Grant, "real-group", k), AppID: strp(app), Plane: "data",
			PrincipalKind: "group", PrincipalID: strp(groupID(k % t.Groups)), RoleScope: "app"})
		if k%2 == 0 {
			rows = append(rows, grantRow{ID: seedID(id.Grant, "real-anon", k), AppID: strp(app), Plane: "data",
				PrincipalKind: "anonymous", RoleScope: "app"})
		}
	}
	if err := s.copyInsert(ctx, "grants", grantCols, s.grantRows(rows)); err != nil {
		return err
	}
	log.Printf("deployed %d real apps", len(ids))
	return nil
}

func (s *seeder) realApp(ctx context.Context, c *Client, cred Credential, k int) (string, error) {
	name := realName(k)
	var app, state string
	var pinned *string
	err := s.pool.QueryRow(ctx, `SELECT id, state, pinned_spec_id FROM apps WHERE name = $1 AND deleted_at IS NULL`, name).
		Scan(&app, &state, &pinned)
	if errors.Is(err, pgx.ErrNoRows) {
		var created struct {
			ID string `json:"id"`
		}
		if _, err := c.JSON(ctx, http.MethodPost, "/apps", cred, fmt.Sprintf(`{"name":%q}`, name), &created); err != nil {
			return "", err
		}
		app, state = created.ID, "draft"
	} else if err != nil {
		return "", err
	}
	if state == "running" {
		return app, nil
	}
	if pinned == nil {
		var rev struct {
			Revision int `json:"revision"`
		}
		if _, err := c.JSON(ctx, http.MethodPost, "/apps/"+app+"/specs", cred, realAppSpec(s.o.RealPortStart+k), &rev); err != nil {
			return "", err
		}
		if _, err := c.JSON(ctx, http.MethodPost, fmt.Sprintf("/apps/%s/specs/%d/pin", app, rev.Revision), cred, "", nil); err != nil {
			return "", err
		}
		// A port written into a spec is not an allocation: only the routing
		// endpoint allocates one (design 03 §4.2), and only an allocated port
		// has a listener on every replica. Without this every request by port
		// was refused at the balancer.
		if _, err := c.JSON(ctx, http.MethodPut, "/apps/"+app+"/routing", cred,
			`{"adapter_ref":"rte_loopback","mode":"port","confirm":true}`, nil); err != nil {
			return "", fmt.Errorf("allocating a port for %s: %w", name, err)
		}
	}
	var dep struct {
		ID string `json:"id"`
	}
	// Retried: a deploy request resolves the image's digest at the registry
	// before it returns, and a public registry that is briefly slow or
	// rate-limiting answers 502 here without anything being wrong with Pando.
	for attempt := 1; attempt <= 4; attempt++ {
		if _, err = c.JSON(ctx, http.MethodPost, "/apps/"+app+"/deployments", cred, "{}", &dep); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * 10 * time.Second):
		}
	}
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(5 * time.Minute)
	for time.Now().Before(deadline) {
		var got struct {
			Status string `json:"status"`
		}
		if _, err := c.JSON(ctx, http.MethodGet, "/apps/"+app+"/deployments/"+dep.ID, cred, "", &got); err != nil {
			return "", err
		}
		switch got.Status {
		case "succeeded":
			return app, nil
		case "failed", "superseded":
			return "", fmt.Errorf("%s: deploy %s %s", name, dep.ID, got.Status)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("%s: deploy %s did not finish in five minutes", name, dep.ID)
}

// Cleanup deletes the real apps through the API, so Pando removes their
// containers and networks, and waits for it to say it has. Docker objects
// are not part of the Compose project, so `docker compose down` alone would
// leave them on the host. Every real app's ID, deleted or not, is written to
// idsFile first, for the Makefile to remove by label whatever Pando did not.
func Cleanup(ctx context.Context, baseURL, databaseURL, adminUser, adminPassword, idsFile string, within time.Duration) error {
	c := NewClient(baseURL, 60*time.Second)
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	type real struct {
		ID   string
		Live bool
	}
	rows, err := pool.Query(ctx, `SELECT id, deleted_at IS NULL FROM apps WHERE name LIKE $1`, realPrefix+"%")
	if err != nil {
		return err
	}
	apps, err := pgx.CollectRows(rows, pgx.RowToStructByPos[real])
	if err != nil {
		return err
	}
	if idsFile != "" {
		var b strings.Builder
		for _, a := range apps {
			b.WriteString(a.ID + "\n")
		}
		if err := os.WriteFile(idsFile, []byte(b.String()), 0o600); err != nil {
			return err
		}
	}

	var cookie string
	for _, a := range apps {
		if !a.Live {
			continue
		}
		if cookie == "" {
			if cookie, _, err = c.SignIn(ctx, adminUser, adminPassword); err != nil {
				return err
			}
		}
		if _, err := c.JSON(ctx, http.MethodDelete, "/apps/"+a.ID+"?force=true", Credential{Cookie: cookie}, "", nil); err != nil {
			log.Printf("deleting %s: %v", a.ID, err)
		}
	}

	deadline := time.Now().Add(within)
	for {
		var standing int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM apps WHERE name LIKE $1 AND bundle_destroyed_at IS NULL`,
			realPrefix+"%").Scan(&standing); err != nil {
			return err
		}
		if standing == 0 {
			log.Printf("real apps removed")
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("%d real apps still had containers after %s; remove them by the label io.pando.bundle=<app ID>", standing, within)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
}

// redactURL drops a connection string's password before it is printed
// (R-194 applies to the harness's own output too).
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	scheme := strings.Index(raw, "://")
	if at < 0 || scheme < 0 || at < scheme {
		return raw
	}
	creds := raw[scheme+3 : at]
	if user, _, ok := strings.Cut(creds, ":"); ok {
		return raw[:scheme+3] + user + ":[redacted]" + raw[at:]
	}
	return raw
}
