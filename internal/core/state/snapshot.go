package state

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/errs"
)

// The database copy an in-place upgrade rolls back to (R-359).
//
// Taken with CREATE DATABASE … TEMPLATE while Pando is stopped, so it is
// exactly the database the old version left, and restored by dropping the
// migrated one and copying it back. The copy is a database beside Pando's in
// the same cluster: it protects against a release that fails to start, not
// against losing the disk, which is what a full backup is for.

// SnapshotSuffix names the copy beside the database it copies.
const SnapshotSuffix = "_pre_upgrade"

// SnapshotName is the copy's name for the database ownerURL names.
func SnapshotName(ownerURL string) (string, error) {
	db, err := databaseName(ownerURL)
	if err != nil {
		return "", err
	}
	return db + SnapshotSuffix, nil
}

// CanCopyDatabase reports whether the account in ownerURL may copy Pando's
// database. The bundled Compose file's account is a superuser; an external
// database's may not be (design 00 §1.1).
func CanCopyDatabase(ctx context.Context, ownerURL string) error {
	conn, err := pgx.Connect(ctx, ownerURL)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not reach Pando's database to check it can be copied.", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var super, createdb, owns bool
	err = conn.QueryRow(ctx, `
		SELECT r.rolsuper, r.rolcreatedb, d.datdba = r.oid
		FROM pg_roles r, pg_database d
		WHERE r.rolname = current_user AND d.datname = current_database()`).Scan(&super, &createdb, &owns)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not read the database account's privileges.", err)
	}
	if super || (createdb && owns) {
		return nil
	}
	return errs.New(errs.ValidInvalid,
		"Pando's database account cannot copy Pando's database, so an in-place upgrade would have nothing to roll back to. It needs CREATEDB and to own the database.").
		WithRemedy("Grant it with ALTER ROLE … CREATEDB as a database administrator, or upgrade by changing Pando's image where it is deployed.")
}

// Snapshot copies Pando's database to SnapshotName, replacing an older copy.
// Pando must be stopped: a database cannot be copied while anything is
// connected to it, so this retries briefly while the last connections close.
func Snapshot(ctx context.Context, ownerURL string) error {
	db, err := databaseName(ownerURL)
	if err != nil {
		return err
	}
	return onMaintenance(ctx, ownerURL, func(conn *pgx.Conn) error {
		snap := pgx.Identifier{db + SnapshotSuffix}.Sanitize()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+snap); err != nil {
			return fmt.Errorf("removing an older copy: %w", err)
		}
		return retryInUse(ctx, func() error {
			_, err := conn.Exec(ctx, "CREATE DATABASE "+snap+" TEMPLATE "+pgx.Identifier{db}.Sanitize())
			return err
		})
	})
}

// RestoreSnapshot replaces Pando's database with the copy and removes the
// copy. Anything still connected to the migrated database is disconnected.
func RestoreSnapshot(ctx context.Context, ownerURL string) error {
	db, err := databaseName(ownerURL)
	if err != nil {
		return err
	}
	return onMaintenance(ctx, ownerURL, func(conn *pgx.Conn) error {
		live, snap := pgx.Identifier{db}.Sanitize(), pgx.Identifier{db + SnapshotSuffix}.Sanitize()
		if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+live+" WITH (FORCE)"); err != nil {
			return fmt.Errorf("removing the migrated database: %w", err)
		}
		if err := retryInUse(ctx, func() error {
			_, err := conn.Exec(ctx, "CREATE DATABASE "+live+" TEMPLATE "+snap)
			return err
		}); err != nil {
			return fmt.Errorf("copying the database back: %w", err)
		}
		_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+snap)
		return err
	})
}

// DropSnapshot removes the copy, once the new version has been healthy long
// enough (R-359). Removing one that does not exist is not an error.
func DropSnapshot(ctx context.Context, ownerURL string) error {
	db, err := databaseName(ownerURL)
	if err != nil {
		return err
	}
	return onMaintenance(ctx, ownerURL, func(conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{db + SnapshotSuffix}.Sanitize()+" WITH (FORCE)")
		return err
	})
}

// onMaintenance runs fn connected to the cluster's maintenance database
// rather than Pando's, which is the one being copied or replaced.
func onMaintenance(ctx context.Context, ownerURL string, fn func(*pgx.Conn) error) error {
	var lastErr error
	for _, maintenance := range []string{"postgres", "template1"} {
		u, err := url.Parse(ownerURL)
		if err != nil {
			return errs.Wrap(errs.Internal, "The database connection URL is malformed.", err)
		}
		u.Path = "/" + maintenance
		conn, err := pgx.Connect(ctx, u.String())
		if err != nil {
			lastErr = err
			continue
		}
		defer func() { _ = conn.Close(ctx) }()
		return fn(conn)
	}
	return fmt.Errorf("could not connect to the database cluster's maintenance database: %w", lastErr)
}

// retryInUse retries while the source database still has connections: Pando's
// pool closes them as the process exits, a moment after the container stops.
func retryInUse(ctx context.Context, fn func() error) error {
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := fn()
		var pg *pgconn.PgError
		// 55006: object_in_use, "source database is being accessed by other users".
		if err == nil || !errors.As(err, &pg) || pg.Code != "55006" || time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

func databaseName(ownerURL string) (string, error) {
	cfg, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "The database connection URL is malformed.", err)
	}
	if strings.TrimSpace(cfg.Database) == "" {
		return "", errs.New(errs.Internal, "The database connection URL names no database.")
	}
	return cfg.Database, nil
}
