package state

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
	"github.com/trypando/pando/migrations"
)

// AppRole is the Postgres role Pando serves traffic as.
//
// It is deliberately not the role that owns the schema, and the reason is more
// specific than it first appears. REVOKE does work against a table's owner —
// after REVOKE UPDATE, has_table_privilege reports false even for the owner. But
// an owner holds grant option implicitly, so it can hand the privilege straight
// back to itself:
//
//	GRANT UPDATE ON audit_events TO pando_app;   -- succeeds, run as pando_app
//	UPDATE audit_events SET action = 'something else';
//
// Against a role that owns the table, the REVOKE is therefore a speed bump and
// not a boundary: one statement undoes it, and that statement is available to
// exactly the process an attacker would be running inside. Ownership is the
// property that has to be denied, not the privilege.
//
// So the owner creates and migrates; this role reads and writes, owns nothing,
// and cannot grant itself anything (R-027).
const AppRole = "pando_app"

// ArchiverRole is the Postgres role audit retention runs as (R-348).
//
// Retention removes months of audit events, which the application role must
// never be able to do (R-027). So it is a third role: it owns nothing, reads
// the log, records archives, and may call the two functions that make and
// drop a month's partition. Those functions run as the owner and refuse, in
// the database, a month younger than the floor or one without an archive of
// every row — so this role cannot remove recent history either, and nothing
// else can remove any.
const ArchiverRole = "pando_audit_archiver"

// DB is a connection to the state store, held as the application role.
type DB struct {
	*pgxpool.Pool

	// schemaVersion is the migration the database is at, recorded when
	// migrations run. A DR bundle carries it so a restore can refuse a bundle
	// from a newer schema rather than half-applying it (R-215).
	schemaVersion uint

	// archiver is a pool held as ArchiverRole, for audit retention and
	// nothing else. Nil on a copy connected for a test.
	archiver *pgxpool.Pool

	// replica is this process's identity among the replicas sharing the
	// database (issue #72), fresh on every connect. Work this process starts
	// is stamped with it, so work left by a process that stopped is told apart
	// from work a live one is still doing.
	replica string
}

// Replica is this process's replica ID.
func (db *DB) Replica() string { return db.replica }

// SchemaVersion is the migration version this database is at.
func (db *DB) SchemaVersion() uint { return db.schemaVersion }

// Archiver is the pool held as ArchiverRole, or nil when this connection has
// none (R-348). Only audit retention uses it.
func (db *DB) Archiver() *pgxpool.Pool { return db.archiver }

// ConnectOptions configures the bootstrap sequence.
type ConnectOptions struct {
	// OwnerURL is the connection string Pando is given. It must be able to run
	// migrations and manage AppRole.
	OwnerURL string

	// ConnectTimeout bounds the retry loop. Compose starts Pando and Postgres
	// together, so Postgres will not be accepting connections when Pando first
	// dials — this is the standard Compose race and is handled rather than
	// assumed away.
	ConnectTimeout time.Duration

	// SkipMigrate is for tests that manage schema themselves.
	SkipMigrate bool
}

func (o *ConnectOptions) setDefaults() {
	if o.ConnectTimeout == 0 {
		o.ConnectTimeout = 60 * time.Second
	}
}

// Connect brings the state store up and returns a pool held as AppRole.
//
// The sequence is: wait for Postgres, migrate as owner, provision the
// application role, apply the grant policy, verify it, then reconnect as the
// application role. Every step after the wait is idempotent and re-runs on each
// start, so a grant that drifts is corrected rather than discovered later.
func Connect(ctx context.Context, opts ConnectOptions) (*DB, error) {
	opts.setDefaults()
	l := log.From(ctx)

	owner, err := waitForPostgres(ctx, opts.OwnerURL, opts.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	defer owner.Close()

	// One replica bootstraps at a time (issue #72), until its grants are
	// verified; the application pool opens after, so nothing serves early.
	release, err := lockBootstrap(ctx, owner)
	if err != nil {
		return nil, err
	}
	defer release()

	var version uint
	if opts.SkipMigrate {
		version, err = readSchemaVersion(ctx, owner)
	} else {
		version, err = migrateUp(ctx, opts.OwnerURL)
	}
	if err != nil {
		return nil, err
	}

	passwords, err := provisionRoles(ctx, owner, opts.OwnerURL)
	if err != nil {
		return nil, err
	}
	if err := applyGrants(ctx, owner); err != nil {
		return nil, err
	}
	if err := verifyAuditImmutability(ctx, owner); err != nil {
		return nil, err
	}

	db, err := connectAsApp(ctx, opts.OwnerURL, passwords.App, version)
	if err != nil {
		return nil, err
	}
	db.archiver, err = ConnectArchiver(ctx, opts.OwnerURL, passwords.Archiver)
	if err != nil {
		db.Close()
		return nil, err
	}
	l.Info("state store ready", zap.String("role", AppRole))
	return db, nil
}

// Passwords are the ones provisionRoles gave Pando's two restricted roles.
type Passwords struct {
	App      secret.Value
	Archiver secret.Value
}

// provisionRoles creates or updates AppRole and ArchiverRole.
func provisionRoles(ctx context.Context, owner *pgxpool.Pool, ownerURL string) (Passwords, error) {
	if err := ensurePrivateSchema(ctx, owner); err != nil {
		return Passwords{}, err
	}
	app, err := provisionRole(ctx, owner, ownerURL, AppRole)
	if err != nil {
		return Passwords{}, err
	}
	archiver, err := provisionRole(ctx, owner, ownerURL, ArchiverRole)
	if err != nil {
		return Passwords{}, err
	}
	return Passwords{App: app, Archiver: archiver}, nil
}

// ConnectArchiver opens a small pool held as ArchiverRole (R-348).
//
// Two connections: retention is one loop doing one month at a time.
func ConnectArchiver(ctx context.Context, ownerURL string, password secret.Value) (*pgxpool.Pool, error) {
	archiverURL, err := withCredentials(ownerURL, ArchiverRole, password)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(archiverURL)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "The database connection URL is malformed.", err)
	}
	cfg.MaxConns = 2
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not connect to the state database as the audit archiver role.", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errs.Wrap(errs.Internal, "Could not connect to the state database as the audit archiver role.", err)
	}
	return pool, nil
}

// Regrant applies the grant policy again and verifies it, as a start does.
//
// For after a DR restore (R-212): pg_restore recreates every table, partition
// and function, and a recreated table arrives with the owner's default
// privileges — which hand the application role UPDATE and DELETE — until the
// next start re-applies the policy. This closes that window instead.
func Regrant(ctx context.Context, ownerURL string) error {
	owner, err := waitForPostgres(ctx, ownerURL, 60*time.Second)
	if err != nil {
		return err
	}
	defer owner.Close()
	if err := applyGrants(ctx, owner); err != nil {
		return err
	}
	return verifyAuditImmutability(ctx, owner)
}

// connectAsApp opens the pool the rest of the process uses, held as AppRole.
func connectAsApp(ctx context.Context, ownerURL string, appPassword secret.Value, version uint) (*DB, error) {
	appURL, err := withCredentials(ownerURL, AppRole, appPassword)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.New(ctx, appURL)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not connect to the state database as the application role.", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, errs.Wrap(errs.Internal, "Could not connect to the state database as the application role.", err)
	}
	return &DB{Pool: pool, schemaVersion: version, replica: id.New(id.Replica)}, nil
}

// readSchemaVersion is the migration a database is at, for one that was not
// migrated by this call.
//
// A database that was never migrated is an error here rather than version
// zero: nothing Pando does next — granting on its tables, checking the audit
// log — can succeed on it either.
func readSchemaVersion(ctx context.Context, owner *pgxpool.Pool) (uint, error) {
	var version int64
	if err := owner.QueryRow(ctx, `SELECT version FROM schema_migrations LIMIT 1`).Scan(&version); err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read the database schema version.", err).
			WithRemedy("Run `pando migrate` against this database, or start Pando without skipping migrations.")
	}
	return uint(version), nil //nolint:gosec // G115: a migration version is a small positive number.
}

// waitForPostgres dials until Postgres answers or the timeout expires.
func waitForPostgres(ctx context.Context, dsn string, timeout time.Duration) (*pgxpool.Pool, error) {
	l := log.From(ctx)
	deadline := time.Now().Add(timeout)

	var lastErr error
	for attempt := 1; ; attempt++ {
		pool, err := pgxpool.New(ctx, dsn)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		lastErr = err

		if time.Now().After(deadline) {
			break
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		wait := min(time.Duration(attempt)*500*time.Millisecond, 5*time.Second)
		l.Debug("waiting for postgres", zap.Int("attempt", attempt), zap.Duration("retry_in", wait), zap.Error(err))
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}

	return nil, errs.Wrap(errs.Internal,
		"Pando could not reach its state database. Check that Postgres is running and that the connection URL is correct.",
		lastErr).WithRemedy("If you are running the bundled Compose file, check `docker compose logs postgres`. If you set PANDO_DATABASE_URL, verify the host, port, and credentials.")
}

// Migrate applies pending migrations as the owning role.
func Migrate(ctx context.Context, ownerURL string) error {
	_, err := migrateUp(ctx, ownerURL)
	return err
}

// migrateUp applies pending migrations and returns the version it left the
// database at.
//
// Returned rather than kept in a package variable, which it used to be: two
// Connects in one process — a parallel test run — wrote it at once (issue #31).
func migrateUp(ctx context.Context, ownerURL string) (uint, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read the embedded migrations.", err)
	}

	cfg, err := pgx.ParseConfig(ownerURL)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "The database connection URL is malformed.", err)
	}
	db := stdlib.OpenDB(*cfg)
	defer db.Close()

	driver, err := postgres.WithInstance(db, &postgres.Config{})
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not prepare the database for migration.", err)
	}

	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not prepare the database for migration.", err)
	}
	// The driver holds a connection of its own, which closing db above does
	// not reclaim: without this every migration left one open for the life of
	// the process, and a database cannot be copied while anything is
	// connected to it (issue #31).
	defer func() { _, _ = m.Close() }()

	// A database a newer Pando migrated is refused before anything touches it
	// (R-354). Up would fail on it anyway — it has no file for the version it
	// finds — but with "Database migration failed", which says nothing about
	// the cause: an older image put back by a Compose file or IaC that still
	// names the version this database was upgraded from.
	if err := refuseNewerSchema(m, src); err != nil {
		return 0, err
	}

	// A dirty schema is refused by Up itself, so it is recognized here. It used
	// to be checked after Up, on a path Up never let it reach, which reported
	// "migration failed" without the one thing worth knowing.
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		var dirty migrate.ErrDirty
		if errors.As(err, &dirty) {
			return 0, errs.New(errs.Internal,
				fmt.Sprintf("The database schema is marked dirty at version %d, which means a previous migration failed partway.", dirty.Version)).
				WithRemedy("Restore from a backup, or resolve the failed migration manually before starting Pando again.")
		}
		return 0, errs.Wrap(errs.Internal, "Database migration failed.", err)
	}

	version, _, err := m.Version()
	if err != nil && !errors.Is(err, migrate.ErrNilVersion) {
		return 0, errs.Wrap(errs.Internal, "Could not read the database schema version.", err)
	}

	log.From(ctx).Info("migrations applied", zap.Uint("version", version))
	return version, nil
}

// refuseNewerSchema refuses a database whose schema version is past the newest
// migration this binary carries: a newer Pando migrated it (R-354).
func refuseNewerSchema(m *migrate.Migrate, src source.Driver) error {
	current, _, err := m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return nil
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not read the database schema version.", err)
	}
	newest, err := newestMigration(src)
	if err != nil {
		return err
	}
	if current <= newest {
		return nil
	}
	return errs.New(errs.Internal,
		fmt.Sprintf("This database was migrated by a newer version of Pando: its schema is at version %d, and this Pando knows versions up to %d. Pando does not start against a database a newer version has migrated.", current, newest)).
		WithRemedy("Run the Pando version this database was upgraded to, or a newer one. If Pando was upgraded and then put back by a Compose file or infrastructure-as-code that still names the older version, change the version there. To go back to the older version, restore the backup taken before the upgrade.").
		WithDetail("schema_version", current).
		WithDetail("supported_schema_version", newest)
}

// newestMigration is the highest version among the embedded migrations.
func newestMigration(src source.Driver) (uint, error) {
	v, err := src.First()
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read the embedded migrations.", err)
	}
	for {
		next, err := src.Next(v)
		if errors.Is(err, os.ErrNotExist) {
			return v, nil
		}
		if err != nil {
			return 0, errs.Wrap(errs.Internal, "Could not read the embedded migrations.", err)
		}
		v = next
	}
}

// provisionRole creates or updates one of Pando's restricted roles and returns
// its password.
//
// A password that already works is kept, and is recorded where only the
// owning role can read it (privateSchema). It used to be regenerated on every
// start and held only in memory, which was one process's view of the world:
// every replica provisions on start, and a second replica setting a fresh
// password refused every new connection the first one made. A rolling restart
// or a scale-up took the rest of the install down with it (issue #72).
func provisionRole(ctx context.Context, owner *pgxpool.Pool, ownerURL, role string) (secret.Value, error) {
	stored, found, err := storedPassword(ctx, owner, role)
	if err != nil {
		return secret.Value{}, err
	}
	if found && canLogIn(ctx, ownerURL, role, stored) {
		return stored, nil
	}

	password, err := randomPassword()
	if err != nil {
		return secret.Value{}, err
	}

	// The role name is a compile-time constant and the password is passed as a
	// literal only because Postgres does not accept parameters in CREATE ROLE.
	// quoteLiteral escapes it; nothing here is caller-controlled.
	stmt := fmt.Sprintf(`
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%s') THEN
				CREATE ROLE %s LOGIN PASSWORD %s;
			ELSE
				ALTER ROLE %s LOGIN PASSWORD %s;
			END IF;
		END
		$$;`, role, role, quoteLiteral(password.Reveal()), role, quoteLiteral(password.Reveal()))

	if _, err := owner.Exec(ctx, stmt); err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal,
			"Pando could not create a restricted database role it runs as.",
			err).
			WithDetail("role", role).
			WithRemedy("Pando needs a database account that can CREATE ROLE and GRANT. If you set PANDO_DATABASE_URL to an existing database, grant those privileges or point Pando at a database it owns. This is required: without a separate role, the audit log cannot be made tamper-proof.")
	}
	if _, err := owner.Exec(ctx, `
		INSERT INTO `+privateSchema+`.role_passwords (role, password) VALUES ($1, $2)
		ON CONFLICT (role) DO UPDATE SET password = EXCLUDED.password, updated_at = now()`,
		role, password.Reveal()); err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal, "Pando could not record a database role's password.", err).
			WithDetail("role", role)
	}
	return password, nil
}

// privateSchema holds what only the owning role may read: the passwords of the
// two restricted roles.
//
// Outside public on purpose. applyGrants hands the application role every
// table in public, and the archiver's password is what the application role
// must not be able to read — with it, the role serving traffic could log in as
// the one that removes audit history (R-348). Nothing is ever granted on this
// schema, and the DR bundle's pg_dump leaves it out, so a bundle carries no
// database password (R-194); a restore onto a new server provisions new ones.
const privateSchema = "pando_private"

// ensurePrivateSchema creates privateSchema and its one table.
func ensurePrivateSchema(ctx context.Context, owner *pgxpool.Pool) error {
	for _, stmt := range []string{
		`CREATE SCHEMA IF NOT EXISTS ` + privateSchema,
		`REVOKE ALL ON SCHEMA ` + privateSchema + ` FROM PUBLIC`,
		`CREATE TABLE IF NOT EXISTS ` + privateSchema + `.role_passwords (
			role       text PRIMARY KEY,
			password   text NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT now()
		)`,
	} {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			return errs.Wrap(errs.Internal, "Pando could not prepare the schema that holds its database role passwords.", err).
				WithRemedy("Pando's database account needs CREATE on the database it owns.")
		}
	}
	return nil
}

// storedPassword reads the password last given to role.
func storedPassword(ctx context.Context, owner *pgxpool.Pool, role string) (secret.Value, bool, error) {
	var pw string
	err := owner.QueryRow(ctx,
		`SELECT password FROM `+privateSchema+`.role_passwords WHERE role = $1`, role).Scan(&pw)
	if errors.Is(err, pgx.ErrNoRows) {
		return secret.Value{}, false, nil
	}
	if err != nil {
		return secret.Value{}, false, errs.Wrap(errs.Internal, "Pando could not read a database role's password.", err)
	}
	return secret.New(pw), true, nil
}

// canLogIn reports whether role can connect with password.
//
// Checked rather than assumed: the role may have been altered by hand, or the
// database restored onto a server whose roles differ. A password that does not
// work is replaced, which strands nobody — no replica could have been
// connecting with it either.
func canLogIn(ctx context.Context, ownerURL, role string, password secret.Value) bool {
	dsn, err := withCredentials(ownerURL, role, password)
	if err != nil {
		return false
	}
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := pgx.Connect(cctx, dsn)
	if err != nil {
		return false
	}
	_ = conn.Close(cctx)
	return true
}

// bootstrapLock is the advisory lock every replica holds while it bootstraps,
// so two starting at once do not race to migrate, provision or grant —
// concurrent GRANTs on one table fail with "tuple concurrently updated".
const bootstrapLock int64 = 0x70616e646f01

// lockBootstrap holds bootstrapLock on one owner connection until release.
//
// Blocking: a replica that starts while another is bootstrapping waits its turn
// and then finds the work already done, rather than failing.
func lockBootstrap(ctx context.Context, owner *pgxpool.Pool) (release func(), err error) {
	conn, err := owner.Acquire(ctx)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not take the startup lock in the state database.", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, bootstrapLock); err != nil {
		conn.Release()
		return nil, errs.Wrap(errs.Internal, "Could not take the startup lock in the state database.", err)
	}
	return func() {
		_, _ = conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, bootstrapLock)
		conn.Release()
	}, nil
}

// applyGrants applies the grant policy to every table.
//
// It runs on every start, after migrations, rather than being written into each
// migration. That is deliberate: a future migration that adds a table gets the
// right grants without anyone remembering to write them, and — more
// importantly — cannot accidentally hand out UPDATE on audit_events. The
// policy lives in exactly one place.
func applyGrants(ctx context.Context, owner *pgxpool.Pool) error {
	stmts := []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, AppRole),
		fmt.Sprintf(`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO %s`, AppRole),
		fmt.Sprintf(`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO %s`, AppRole),

		// R-027. The audit log is append-only, and this is the line that makes
		// it true of core as well as of adapters — which is stronger, and costs
		// nothing.
		fmt.Sprintf(`REVOKE UPDATE, DELETE, TRUNCATE ON audit_events FROM %s`, AppRole),

		// The record of what was archived is the archiver's to write and
		// nobody's to change. audit_drop_month trusts it, so a role that could
		// insert into it could vouch for an archive that does not exist.
		fmt.Sprintf(`REVOKE INSERT, UPDATE, DELETE, TRUNCATE ON audit_archives FROM %s`, AppRole),

		// R-348. The archiver reads the log and records archives, and may
		// call the two functions that make and drop a month. Nothing else:
		// it holds no UPDATE or DELETE anywhere, and the drop function
		// enforces the floor and the archive whoever calls it.
		fmt.Sprintf(`GRANT USAGE ON SCHEMA public TO %s`, ArchiverRole),
		fmt.Sprintf(`GRANT SELECT ON audit_events TO %s`, ArchiverRole),
		fmt.Sprintf(`GRANT SELECT, INSERT ON audit_archives TO %s`, ArchiverRole),
		`REVOKE ALL ON FUNCTION audit_ensure_partition(date) FROM PUBLIC`,
		`REVOKE ALL ON FUNCTION audit_drop_month(date, bigint, text) FROM PUBLIC`,
		fmt.Sprintf(`GRANT EXECUTE ON FUNCTION audit_ensure_partition(date) TO %s`, ArchiverRole),
		fmt.Sprintf(`GRANT EXECUTE ON FUNCTION audit_drop_month(date, bigint, text) TO %s`, ArchiverRole),

		// Deny by default for tables added later: a new table is unreachable
		// until the next start re-runs the GRANT above, rather than arriving
		// with whatever PUBLIC happens to have.
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO %s`, AppRole),
		fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT USAGE, SELECT ON SEQUENCES TO %s`, AppRole),
	}

	// Each month of the audit log is a partition, and a partition is a table:
	// the GRANT ON ALL TABLES above reached every one of them, and DELETE on
	// a partition is DELETE on the audit log (R-027). Revoked here on every
	// start, after that grant, so the order of the two can never be wrong.
	parts, err := auditPartitions(ctx, owner)
	if err != nil {
		return err
	}
	for _, part := range parts {
		stmts = append(stmts, fmt.Sprintf(`REVOKE UPDATE, DELETE, TRUNCATE ON %s FROM %s`,
			pgx.Identifier{part}.Sanitize(), AppRole))
	}

	for _, stmt := range stmts {
		if _, err := owner.Exec(ctx, stmt); err != nil {
			return errs.Wrap(errs.Internal, "Pando could not apply database permissions.", err).
				WithDetail("statement", stmt).
				WithRemedy("Pando's database account needs GRANT privileges on the schema it owns.")
		}
	}
	return nil
}

// auditPartitions names every partition of audit_events.
func auditPartitions(ctx context.Context, owner *pgxpool.Pool) ([]string, error) {
	rows, err := owner.Query(ctx, `
		SELECT c.relname FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'public.audit_events'::regclass ORDER BY c.relname`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not list the audit log's partitions.", err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not list the audit log's partitions.", err)
	}
	return names, nil
}

// verifyAuditImmutability refuses to start if the audit log is rewritable.
//
// This is the preflight the external-database path needs. A degraded mode that
// runs anyway is not acceptable: the entire value of enforcing R-027 at the
// database is that it holds without anyone checking, so an install where it
// silently does not hold is worse than one that refuses to start and says why.
func verifyAuditImmutability(ctx context.Context, owner *pgxpool.Pool) error {
	var canUpdate, canDelete bool
	var tableOwner string
	err := owner.QueryRow(ctx, `
		SELECT
			has_table_privilege($1, 'audit_events', 'UPDATE'),
			has_table_privilege($1, 'audit_events', 'DELETE'),
			(SELECT tableowner FROM pg_tables WHERE tablename = 'audit_events')`,
		AppRole).Scan(&canUpdate, &canDelete, &tableOwner)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not verify that its audit log is tamper-proof.", err)
	}

	// Ownership is checked separately from privilege, and it is the check that
	// matters. A revoked privilege reads as absent right up until the owner
	// grants it back to itself, which takes one statement and no extra access.
	if tableOwner == AppRole {
		return errs.New(errs.Internal,
			"Pando's audit log is not tamper-proof: the account it serves traffic as owns the audit table, and an owner can grant itself permission to rewrite it at any time.").
			WithDetail("role", AppRole).
			WithDetail("table_owner", tableOwner).
			WithRemedy("Pando must connect as an account that does not own its schema. Give it a database it owns, or an account separate from the schema owner. Refer to the external-database setup notes.")
	}

	if canUpdate || canDelete {
		return errs.New(errs.Internal,
			"Pando's audit log is not tamper-proof: the account it serves traffic as can modify audit records.").
			WithDetail("role", AppRole).
			WithDetail("can_update", canUpdate).
			WithDetail("can_delete", canDelete).
			WithRemedy("This usually means the application role owns the audit_events table. An owner can grant itself UPDATE at any time, so revoking it is not enough — Pando must connect as an account that does not own its schema. Refer to the external-database setup notes.")
	}
	return verifyRetentionIsTheArchiversAlone(ctx, owner)
}

// verifyRetentionIsTheArchiversAlone refuses to start if the role serving
// traffic could remove audit events some other way than through the table
// (R-348): through a month's partition, through the functions that drop one,
// by becoming the archiver, or by vouching for an archive that was never
// written.
func verifyRetentionIsTheArchiversAlone(ctx context.Context, owner *pgxpool.Pool) error {
	refuse := func(why string, detail ...any) error {
		e := errs.New(errs.Internal, "Pando's audit log is not tamper-proof: "+why).
			WithDetail("role", AppRole).
			WithRemedy("Pando must connect as an account that does not own its schema, and the audit log's partitions and retention functions must belong to the schema owner. Starting Pando with its own database account re-applies these permissions; refer to the external-database setup notes.")
		for i := 0; i+1 < len(detail); i += 2 {
			e = e.WithDetail(fmt.Sprint(detail[i]), detail[i+1])
		}
		return e
	}

	rows, err := owner.Query(ctx, `
		SELECT c.relname, pg_get_userbyid(c.relowner) = $1,
		       has_table_privilege($1, c.oid, 'UPDATE') OR has_table_privilege($1, c.oid, 'DELETE')
		           OR has_table_privilege($1, c.oid, 'TRUNCATE')
		FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
		WHERE i.inhparent = 'public.audit_events'::regclass`, AppRole)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not verify that its audit log is tamper-proof.", err)
	}
	type partition struct {
		name      string
		owns, can bool
	}
	parts, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (partition, error) {
		var p partition
		return p, r.Scan(&p.name, &p.owns, &p.can)
	})
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not verify that its audit log is tamper-proof.", err)
	}
	for _, p := range parts {
		if p.owns || p.can {
			return refuse("the account it serves traffic as can modify or remove a month of audit records.",
				"partition", p.name, "owns", p.owns)
		}
	}

	var dropOwned, canDrop, canEnsure, isArchiver, canVouch bool
	err = owner.QueryRow(ctx, `
		SELECT
			pg_get_userbyid(d.proowner) = $1 OR pg_get_userbyid(e.proowner) = $1,
			has_function_privilege($1, d.oid, 'EXECUTE'),
			has_function_privilege($1, e.oid, 'EXECUTE'),
			CASE WHEN EXISTS (SELECT FROM pg_roles WHERE rolname = $2)
			     THEN pg_has_role($1, $2, 'MEMBER') ELSE false END,
			has_table_privilege($1, 'public.audit_archives', 'INSERT')
				OR has_table_privilege($1, 'public.audit_archives', 'UPDATE')
				OR has_table_privilege($1, 'public.audit_archives', 'DELETE')
		FROM pg_proc d, pg_proc e
		WHERE d.oid = 'public.audit_drop_month(date, bigint, text)'::regprocedure
		  AND e.oid = 'public.audit_ensure_partition(date)'::regprocedure`,
		AppRole, ArchiverRole).Scan(&dropOwned, &canDrop, &canEnsure, &isArchiver, &canVouch)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not verify that its audit log is tamper-proof.", err)
	}
	switch {
	case dropOwned:
		return refuse("the account it serves traffic as owns the functions that remove months of audit records, and could rewrite them.")
	case canDrop || canEnsure:
		return refuse("the account it serves traffic as may call the functions that make and remove months of audit records.")
	case isArchiver:
		return refuse("the account it serves traffic as is a member of the audit archiver role.")
	case canVouch:
		return refuse("the account it serves traffic as can write the record of audit archives, which decides what may be removed.")
	}
	return nil
}

// Close releases the pool.
func (db *DB) Close() {
	if db != nil && db.Pool != nil {
		db.Pool.Close()
	}
	if db != nil && db.archiver != nil {
		db.archiver.Close()
	}
}

func randomPassword() (secret.Value, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal, "Could not generate a database password.", err)
	}
	return secret.New(base64.RawURLEncoding.EncodeToString(b)), nil
}

// quoteLiteral renders a Postgres string literal, doubling embedded quotes.
func quoteLiteral(s string) string {
	out := make([]rune, 0, len(s)+2)
	out = append(out, '\'')
	for _, r := range s {
		if r == '\'' {
			out = append(out, '\'')
		}
		out = append(out, r)
	}
	out = append(out, '\'')
	return string(out)
}

// withCredentials rewrites a connection URL's user and password.
func withCredentials(dsn, user string, password secret.Value) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "The database connection URL is malformed.", err)
	}
	u.User = url.UserPassword(user, password.Reveal())
	return u.String(), nil
}
