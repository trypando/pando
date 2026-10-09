package state

import (
	"context"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
)

// queryAll runs query and returns every row through scan, as a non-nil slice
// so an empty list encodes as [] rather than null. A failure in the query or
// in any row is reported as msg: what a caller can do about either is the same.
//
// Query's own error is not checked separately: pgx documents the Rows it
// returns as safe to read when it fails, with the error in rows.Err(), which
// CollectRows returns.
func queryAll[T any](ctx context.Context, db *DB, msg string, scan func(pgx.CollectableRow) (T, error), query string, args ...any) ([]T, error) {
	rows, _ := db.Query(ctx, query, args...)
	out, err := pgx.CollectRows(rows, scan)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, msg, err)
	}
	return out, nil
}
