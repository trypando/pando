package main

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PGSample is what Postgres looked like while a step held: the busiest
// moment seen, and the connection limit it was measured against.
type PGSample struct {
	MaxConnections int `json:"max_connections"`
	// Peaks over the step's samples, all backends of the install's database.
	PeakTotal       int `json:"peak_total"`
	PeakActive      int `json:"peak_active"`
	PeakIdleInTx    int `json:"peak_idle_in_transaction"`
	PeakLockWaiting int `json:"peak_lock_waiting"`
	Samples         int `json:"samples"`
}

// Query is one statement's share of a step's database time.
type Query struct {
	Query   string  `json:"query"`
	Calls   int64   `json:"calls"`
	TotalMS float64 `json:"total_ms"`
	MeanMS  float64 `json:"mean_ms"`
	Rows    int64   `json:"rows"`
	// Seen is how many samples caught it running, for the fallback that has
	// no pg_stat_statements; zero otherwise.
	Seen int `json:"seen,omitempty"`
}

// PG samples the install's database as its owner.
type PG struct {
	pool *pgxpool.Pool
	// Statements is whether pg_stat_statements is loaded. Without it, the
	// top queries are the ones most often caught running by pg_stat_activity.
	Statements bool

	mu     sync.Mutex
	sample PGSample
	seen   map[string]int
}

func NewPG(ctx context.Context, url string) (*PG, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	p := &PG{pool: pool}
	// The extension needs shared_preload_libraries, which the load overlay
	// sets on its Postgres. Elsewhere this fails and the fallback is used.
	if _, err := pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_stat_statements`); err == nil {
		if _, err := pool.Exec(ctx, `SELECT pg_stat_statements_reset()`); err == nil {
			p.Statements = true
		}
	}
	if !p.Statements {
		log.Printf("pg_stat_statements is not available; top queries will be sampled from pg_stat_activity")
	}
	return p, nil
}

func (p *PG) Close() { p.pool.Close() }

// Begin resets the per-step counters.
func (p *PG) Begin(ctx context.Context) {
	p.mu.Lock()
	p.sample = PGSample{}
	p.seen = map[string]int{}
	p.mu.Unlock()
	if p.Statements {
		if _, err := p.pool.Exec(ctx, `SELECT pg_stat_statements_reset()`); err != nil {
			log.Printf("resetting pg_stat_statements: %v", err)
		}
	}
}

// Watch samples pg_stat_activity every interval until ctx ends.
func (p *PG) Watch(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		p.once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *PG) once(ctx context.Context) {
	var s PGSample
	err := p.pool.QueryRow(ctx, `
		SELECT current_setting('max_connections')::int,
		       count(*),
		       count(*) FILTER (WHERE state = 'active'),
		       count(*) FILTER (WHERE state = 'idle in transaction'),
		       count(*) FILTER (WHERE wait_event_type = 'Lock')
		FROM pg_stat_activity
		WHERE datname = current_database() AND pid <> pg_backend_pid()`).
		Scan(&s.MaxConnections, &s.PeakTotal, &s.PeakActive, &s.PeakIdleInTx, &s.PeakLockWaiting)
	if err != nil {
		return
	}
	var running []string
	if !p.Statements {
		rows, err := p.pool.Query(ctx, `
			SELECT query FROM pg_stat_activity
			WHERE datname = current_database() AND state = 'active' AND pid <> pg_backend_pid()`)
		if err == nil {
			running, _ = pgx.CollectRows(rows, pgx.RowTo[string])
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cur := &p.sample
	cur.MaxConnections = s.MaxConnections
	cur.PeakTotal = max(cur.PeakTotal, s.PeakTotal)
	cur.PeakActive = max(cur.PeakActive, s.PeakActive)
	cur.PeakIdleInTx = max(cur.PeakIdleInTx, s.PeakIdleInTx)
	cur.PeakLockWaiting = max(cur.PeakLockWaiting, s.PeakLockWaiting)
	cur.Samples++
	for _, q := range running {
		if p.seen == nil {
			p.seen = map[string]int{}
		}
		p.seen[normalizeQuery(q)]++
	}
}

// End returns the step's sample and its top n queries by total time (or by
// how often they were caught running).
func (p *PG) End(ctx context.Context, n int) (PGSample, []Query) {
	p.mu.Lock()
	sample := p.sample
	seen := p.seen
	p.mu.Unlock()

	if !p.Statements {
		return sample, topSeen(seen, n)
	}
	rows, err := p.pool.Query(ctx, `
		SELECT s.query, s.calls, s.total_exec_time, s.mean_exec_time, s.rows
		FROM pg_stat_statements s
		JOIN pg_database d ON d.oid = s.dbid
		WHERE d.datname = current_database()
		  AND s.query NOT LIKE '%pg_stat_statements%'
		  AND s.query NOT LIKE '%pg_stat_activity%'
		ORDER BY s.total_exec_time DESC
		LIMIT $1`, n)
	if err != nil {
		log.Printf("reading pg_stat_statements: %v", err)
		return sample, nil
	}
	defer rows.Close()
	var out []Query
	for rows.Next() {
		var q Query
		if err := rows.Scan(&q.Query, &q.Calls, &q.TotalMS, &q.MeanMS, &q.Rows); err != nil {
			continue
		}
		q.Query = normalizeQuery(q.Query)
		out = append(out, q)
	}
	return sample, out
}

func topSeen(seen map[string]int, n int) []Query {
	out := make([]Query, 0, len(seen))
	for q, c := range seen {
		out = append(out, Query{Query: q, Seen: c})
	}
	sortQueries(out)
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// sortQueries orders by total time, then by times seen, then by text so the
// order is stable.
func sortQueries(qs []Query) {
	sort.Slice(qs, func(i, j int) bool {
		a, b := qs[i], qs[j]
		if a.TotalMS != b.TotalMS {
			return a.TotalMS > b.TotalMS
		}
		if a.Seen != b.Seen {
			return a.Seen > b.Seen
		}
		return a.Query < b.Query
	})
}

// normalizeQuery collapses a statement's whitespace so it fits a table cell.
func normalizeQuery(q string) string { return strings.Join(strings.Fields(q), " ") }

// Counts reads how much of each kind of thing the install holds, for the
// report's header.
func (p *PG) Counts(ctx context.Context) map[string]int {
	out := map[string]int{}
	for name, q := range map[string]string{
		"users":          `SELECT count(*) FROM users WHERE deleted_at IS NULL`,
		"groups":         `SELECT count(*) FROM groups`,
		"group_members":  `SELECT count(*) FROM group_members`,
		"apps":           `SELECT count(*) FROM apps WHERE deleted_at IS NULL`,
		"apps_running":   `SELECT count(*) FROM apps WHERE deleted_at IS NULL AND state = 'running'`,
		"grants":         `SELECT count(*) FROM grants`,
		"tokens":         `SELECT count(*) FROM tokens WHERE revoked_at IS NULL`,
		"spec_revisions": `SELECT count(*) FROM spec_revisions`,
		"replicas_live":  `SELECT count(*) FROM pando_replicas WHERE stopped_at IS NULL AND heartbeat_at > now() - interval '45 seconds'`,
	} {
		var n int
		if err := p.pool.QueryRow(ctx, q).Scan(&n); err == nil {
			out[name] = n
		}
	}
	return out
}
