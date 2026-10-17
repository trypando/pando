package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// FailedAttempts counts wrong guesses at something secret, for the limits on
// guessing: a passcode, per app and client address (R-075a), and a password at
// sign-in, per username and per client address (R-404). A key says which.
//
// In the database rather than in memory (issue #72): with N replicas behind a
// load balancer, a count kept per process gave an attacker N times the
// attempts, and one more set every time a replica restarted.
type FailedAttempts struct{ db *DB }

func NewFailedAttempts(db *DB) *FailedAttempts { return &FailedAttempts{db: db} }

// maxKeyUsername bounds what a client can put in a row it causes.
const maxKeyUsername = 256

// SignInUsernameKey is the key wrong passwords for username are counted
// under (R-404): as typed, whether or not the account exists.
func SignInUsernameKey(username string) string {
	if len(username) > maxKeyUsername {
		username = username[:maxKeyUsername]
	}
	return "signin|user|" + username
}

// SignInAddrKey is the key wrong passwords from a client address are counted
// under (R-404).
func SignInAddrKey(addr string) string { return "signin|addr|" + addr }

// SignInCodeKey is the key wrong two-factor codes for an account are counted
// under (R-405). By account ID: the password was right, so it is known.
func SignInCodeKey(userID string) string { return "signin|code|" + userID }

// Recent counts failures for key within window.
func (p *FailedAttempts) Recent(ctx context.Context, key string, window time.Duration) (int, error) {
	var n int
	err := p.db.QueryRow(ctx, `
		SELECT count(*) FROM failed_attempts
		WHERE key = $1 AND failed_at > now() - make_interval(secs => $2)`,
		key, window.Seconds()).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read recent failed attempts.", err)
	}
	return n, nil
}

// Wait is how long until the oldest failure for key within window leaves it,
// which is when one more attempt is allowed once a limit is reached. Zero when
// there is none. Measured by the database's clock, which recorded them.
func (p *FailedAttempts) Wait(ctx context.Context, key string, window time.Duration) (time.Duration, error) {
	var secs float64
	err := p.db.QueryRow(ctx, `
		SELECT coalesce(extract(epoch FROM min(failed_at) + make_interval(secs => $2) - now()), 0)::float8
		FROM failed_attempts
		WHERE key = $1 AND failed_at > now() - make_interval(secs => $2)`,
		key, window.Seconds()).Scan(&secs)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read recent failed attempts.", err)
	}
	return max(time.Duration(secs*float64(time.Second)), 0), nil
}

// Record counts one failure for key, and forgets failures older than window
// for it, which nothing will count again.
func (p *FailedAttempts) Record(ctx context.Context, key string, window time.Duration) error {
	if _, err := p.db.Exec(ctx, `INSERT INTO failed_attempts (key) VALUES ($1)`, key); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a failed attempt.", err)
	}
	if _, err := p.db.Exec(ctx, `
		DELETE FROM failed_attempts WHERE key = $1 AND failed_at <= now() - make_interval(secs => $2)`,
		key, window.Seconds()); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a failed attempt.", err)
	}
	return nil
}

// Clear forgets key's failures, after a right guess.
func (p *FailedAttempts) Clear(ctx context.Context, key string) error {
	if _, err := p.db.Exec(ctx, `DELETE FROM failed_attempts WHERE key = $1`, key); err != nil {
		return errs.Wrap(errs.Internal, "Could not clear failed attempts.", err)
	}
	return nil
}

// Prune forgets every failure older than window: keys nobody tried again.
func (p *FailedAttempts) Prune(ctx context.Context, window time.Duration) error {
	if _, err := p.db.Exec(ctx, `
		DELETE FROM failed_attempts WHERE failed_at <= now() - make_interval(secs => $1)`,
		window.Seconds()); err != nil {
		return errs.Wrap(errs.Internal, "Could not prune failed attempts.", err)
	}
	return nil
}
