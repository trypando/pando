package state

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// PasscodeFailures counts wrong passcodes, for the limit on guessing one
// (R-075a).
//
// In the database rather than in memory (issue #72): with N replicas behind a
// load balancer, a count kept per process gave an attacker N times the
// attempts, and one more set every time a replica restarted.
type PasscodeFailures struct{ db *DB }

func NewPasscodeFailures(db *DB) *PasscodeFailures { return &PasscodeFailures{db: db} }

// Recent counts failures for key within window.
func (p *PasscodeFailures) Recent(ctx context.Context, key string, window time.Duration) (int, error) {
	var n int
	err := p.db.QueryRow(ctx, `
		SELECT count(*) FROM passcode_failures
		WHERE key = $1 AND failed_at > now() - make_interval(secs => $2)`,
		key, window.Seconds()).Scan(&n)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read recent passcode attempts.", err)
	}
	return n, nil
}

// Record counts one failure for key, and forgets failures older than window
// for it, which nothing will count again.
func (p *PasscodeFailures) Record(ctx context.Context, key string, window time.Duration) error {
	if _, err := p.db.Exec(ctx, `INSERT INTO passcode_failures (key) VALUES ($1)`, key); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a passcode attempt.", err)
	}
	if _, err := p.db.Exec(ctx, `
		DELETE FROM passcode_failures WHERE key = $1 AND failed_at <= now() - make_interval(secs => $2)`,
		key, window.Seconds()); err != nil {
		return errs.Wrap(errs.Internal, "Could not record a passcode attempt.", err)
	}
	return nil
}

// Clear forgets key's failures, after a right passcode.
func (p *PasscodeFailures) Clear(ctx context.Context, key string) error {
	if _, err := p.db.Exec(ctx, `DELETE FROM passcode_failures WHERE key = $1`, key); err != nil {
		return errs.Wrap(errs.Internal, "Could not clear passcode attempts.", err)
	}
	return nil
}

// Prune forgets every failure older than window: keys nobody tried again.
func (p *PasscodeFailures) Prune(ctx context.Context, window time.Duration) error {
	if _, err := p.db.Exec(ctx, `
		DELETE FROM passcode_failures WHERE failed_at <= now() - make_interval(secs => $1)`,
		window.Seconds()); err != nil {
		return errs.Wrap(errs.Internal, "Could not prune passcode attempts.", err)
	}
	return nil
}
