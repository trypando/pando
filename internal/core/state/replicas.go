package state

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/trypando/pando/internal/errs"
)

// Replicas are the Pando processes sharing this database (issue #72).
//
// A replica is Pando's own process and never a place an app runs: nothing that
// plans or places a workload reads this table (R-010, R-256). It answers which
// of Pando's processes are alive, which three things need to know — whose
// in-flight work is abandoned, which signing keys are published, and where a
// deploy's live log is.
//
// Every time here is the database's, never a replica's own clock: replicas on
// different machines disagree about the time, and the database is the one
// clock they share.
type Replicas struct{ db *DB }

func NewReplicas(db *DB) *Replicas { return &Replicas{db: db} }

// Replica is one row.
type Replica struct {
	ID           string
	Hostname     string
	AdvertiseURL string
	Version      string
	AssertionKID string
	AssertionKey []byte // Ed25519 public key
	StartedAt    time.Time
	HeartbeatAt  time.Time
	StoppedAt    *time.Time
}

// HeartbeatInterval is how often a replica says it is alive, and ReplicaStale
// how long it may go without saying so before it is taken to have stopped.
//
// Stale is several heartbeats, so one slow query is not a death; and short
// enough that a deploy left by a lost pod is recorded as interrupted within a
// minute rather than blocking its app's next deploy.
const (
	HeartbeatInterval = 10 * time.Second
	ReplicaStale      = 45 * time.Second
)

// Register records this replica as started.
func (r *Replicas) Register(ctx context.Context, rep Replica) error {
	_, err := r.db.Exec(ctx, `
		INSERT INTO pando_replicas (id, hostname, advertise_url, version, assertion_kid, assertion_key)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		rep.ID, rep.Hostname, rep.AdvertiseURL, rep.Version, rep.AssertionKID, rep.AssertionKey)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record this Pando process among the install's replicas.", err)
	}
	return nil
}

// Heartbeat records that a replica is alive, and reports whether its row
// still exists and is not marked stopped. A replica that finds itself gone was
// taken for dead — its work has been abandoned by now — and the honest thing
// for it to do is restart as a new replica rather than carry on as if not.
func (r *Replicas) Heartbeat(ctx context.Context, replicaID string) (bool, error) {
	tag, err := r.db.Exec(ctx, `
		UPDATE pando_replicas SET heartbeat_at = now()
		WHERE id = $1 AND stopped_at IS NULL`, replicaID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not record that this Pando process is alive.", err)
	}
	return tag.RowsAffected() == 1, nil
}

// Stop records a replica as stopped, at shutdown. Its key stays published for
// as long as an assertion it signed can still be valid.
func (r *Replicas) Stop(ctx context.Context, replicaID string) error {
	_, err := r.db.Exec(ctx, `
		UPDATE pando_replicas SET stopped_at = now() WHERE id = $1 AND stopped_at IS NULL`, replicaID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record that this Pando process stopped.", err)
	}
	return nil
}

const replicaColumns = `r.id, r.hostname, r.advertise_url, r.version, r.assertion_kid, r.assertion_key,
	r.started_at, r.heartbeat_at, r.stopped_at`

func scanReplicas(rows pgx.Rows) ([]Replica, error) {
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Replica, error) {
		var rep Replica
		err := row.Scan(&rep.ID, &rep.Hostname, &rep.AdvertiseURL, &rep.Version, &rep.AssertionKID,
			&rep.AssertionKey, &rep.StartedAt, &rep.HeartbeatAt, &rep.StoppedAt)
		return rep, err
	})
}

// Live lists the replicas that have heartbeated within ReplicaStale, oldest
// first.
func (r *Replicas) Live(ctx context.Context) ([]Replica, error) {
	rows, err := r.db.Query(ctx, `SELECT `+replicaColumns+` FROM pando_replicas r
		WHERE r.stopped_at IS NULL AND r.heartbeat_at > now() - make_interval(secs => $1)
		ORDER BY r.started_at, r.id`, ReplicaStale.Seconds())
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the install's replicas.", err)
	}
	out, err := scanReplicas(rows)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the install's replicas.", err)
	}
	return out, nil
}

// ByID returns one replica, live or not.
func (r *Replicas) ByID(ctx context.Context, replicaID string) (Replica, bool, error) {
	rows, err := r.db.Query(ctx, `SELECT `+replicaColumns+` FROM pando_replicas r WHERE r.id = $1`, replicaID)
	if err != nil {
		return Replica{}, false, errs.Wrap(errs.Internal, "Could not read a replica.", err)
	}
	out, err := scanReplicas(rows)
	if err != nil {
		return Replica{}, false, errs.Wrap(errs.Internal, "Could not read a replica.", err)
	}
	if len(out) == 0 {
		return Replica{}, false, nil
	}
	return out[0], true, nil
}

// VerificationKeys lists the replicas whose signing keys must still be
// published: every live one, and every one that stopped or went silent less
// than validity ago, since an assertion it signed may still be in use.
func (r *Replicas) VerificationKeys(ctx context.Context, validity time.Duration) ([]Replica, error) {
	rows, err := r.db.Query(ctx, `SELECT `+replicaColumns+` FROM pando_replicas r
		WHERE coalesce(r.stopped_at, r.heartbeat_at) > now() - make_interval(secs => $1)
		ORDER BY r.started_at, r.id`, (validity + ReplicaStale).Seconds())
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the published signing keys.", err)
	}
	out, err := scanReplicas(rows)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the published signing keys.", err)
	}
	return out, nil
}

// Prune removes replicas gone for longer than olderThan. Their keys have long
// since stopped verifying anything and their work has long since been
// abandoned; the row is only history.
func (r *Replicas) Prune(ctx context.Context, olderThan time.Duration) (int64, error) {
	tag, err := r.db.Exec(ctx, `
		DELETE FROM pando_replicas WHERE coalesce(stopped_at, heartbeat_at) < now() - make_interval(secs => $1)`,
		olderThan.Seconds())
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not prune stopped replicas.", err)
	}
	return tag.RowsAffected(), nil
}

// RequestRestart asks every replica running now to restart (POST /restart).
func (r *Replicas) RequestRestart(ctx context.Context) error {
	if _, err := r.db.Exec(ctx, `UPDATE cluster_signals SET restart_requested_at = now() WHERE id = 1`); err != nil {
		return errs.Wrap(errs.Internal, "Could not ask the install's replicas to restart.", err)
	}
	return nil
}

// RestartPending reports whether a restart was asked for after replicaID
// started.
func (r *Replicas) RestartPending(ctx context.Context, replicaID string) (bool, error) {
	var pending bool
	err := r.db.QueryRow(ctx, `
		SELECT coalesce(s.restart_requested_at > r.started_at, false)
		FROM cluster_signals s, pando_replicas r WHERE s.id = 1 AND r.id = $1`, replicaID).Scan(&pending)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read whether a restart was asked for.", err)
	}
	return pending, nil
}

// FirstAccountLock serializes making the install's first account, whether a
// person submits the setup form (Users.ClaimFirst) or a starting replica
// makes it from PANDO_ADMIN_PASSWORD (bootstrap.Run). Session and transaction
// advisory locks on one key exclude each other, so both paths share it.
const FirstAccountLock int64 = 46046

// Exclusive runs fn holding the session advisory lock key, waiting for it if
// another process holds it.
//
// For work that must happen once however many replicas start at the same
// moment: the second waits, then finds it done.
func (db *DB) Exclusive(ctx context.Context, key int64, fn func() error) error {
	conn, err := db.Acquire(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not take a lock in the state database.", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return errs.Wrap(errs.Internal, "Could not take a lock in the state database.", err)
	}
	defer func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, key)
	}()
	return fn()
}

// leaderLock is the advisory lock the leader holds. One install has one.
const leaderLock int64 = 0x70616e646f02

// Leadership is the lock that makes one replica the one that runs the
// install-wide background jobs (issue #72): garbage collection, auto-deploy,
// audit retention, and the rest of what must happen once per install rather
// than once per process.
//
// A session advisory lock on a connection held for as long as the replica
// leads. If the process dies the connection closes and Postgres releases the
// lock itself; there is no lease to expire and no clock to trust. A leader
// that loses its connection stops leading at once (Lost closes), and another
// replica takes over at its next attempt.
type Leadership struct {
	mu   sync.Mutex // the connection is not safe for concurrent use
	conn *pgxpool.Conn
	done bool
	lost chan struct{}
	stop chan struct{}
}

// TryLead takes the leader lock if nobody holds it. ok is false, with no
// error, when another replica leads.
func (r *Replicas) TryLead(ctx context.Context, check time.Duration) (*Leadership, bool, error) {
	conn, err := r.db.Acquire(ctx)
	if err != nil {
		return nil, false, errs.Wrap(errs.Internal, "Could not reach the state database to take the leader lock.", err)
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, leaderLock).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, errs.Wrap(errs.Internal, "Could not take the leader lock.", err)
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}

	l := &Leadership{conn: conn, lost: make(chan struct{}), stop: make(chan struct{})}
	// Not canceled with ctx: the lock outlives this call, and so must its
	// watch. Resign or a failed ping ends it.
	go l.watch(context.WithoutCancel(ctx), check)

	return l, true, nil
}

// watch pings the lock's connection, and closes lost when it fails: a broken
// connection is a released lock, and another replica may already lead.
func (l *Leadership) watch(ctx context.Context, every time.Duration) {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			l.mu.Lock()
			if l.done {
				l.mu.Unlock()
				return
			}
			pingCtx, cancel := context.WithTimeout(ctx, every)
			err := l.conn.Ping(pingCtx)
			cancel()
			if err != nil {
				// Never returned to the pool: a connection that may still
				// hold the lock must not be handed to anyone else.
				_ = l.conn.Hijack().Close(ctx)
				l.done = true
				close(l.lost)
			}
			l.mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// Lost is closed when this replica stops leading without having resigned.
func (l *Leadership) Lost() <-chan struct{} { return l.lost }

// Resign gives the lock up. Safe to call after Lost has closed.
func (l *Leadership) Resign(ctx context.Context) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.done {
		return
	}
	l.done = true
	close(l.stop)
	_, _ = l.conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, leaderLock)
	l.conn.Release()
}
