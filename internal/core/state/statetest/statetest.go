// Package statetest gives integration tests a database of their own, quickly.
//
// One Postgres container is started per test binary, one database in it is
// migrated and granted once, and each test gets a copy of that database made
// with CREATE DATABASE … TEMPLATE. Copying files is much cheaper than replaying
// every migration, and a test that owns its database can run in parallel with
// every other (issue #31).
//
// A database rather than truncated tables. Migrations seed rows — the built-in
// roles (R-081) and the host policy singleton (R-015) — so emptying every table
// between tests leaves an install whose administrator role does not exist, and
// the next bootstrap fails somewhere far from the cause. Which tables are
// seeded is also a thing migrations may change, and a test harness that has to
// be updated when they do is one that will not be.
//
// A test about the Postgres cluster itself — creating roles, changing who owns
// what, connecting twice — needs a cluster nobody else is using, and should
// start its own container rather than use this one.
package statetest

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

const templateName = "pando_template"

var (
	once       sync.Once
	clusterURL string
	passwords  state.Passwords
	setupErr   error

	nextDatabase atomic.Int64
)

// Connect returns a connection to a fresh, migrated database, held as the
// application role the way a server holds it, and the owner URL of that
// database. The connection is closed when the test ends.
func Connect(t testing.TB) (*state.DB, string) {
	t.Helper()
	ownerURL, password := Database(t)
	db, err := state.ConnectCopy(context.Background(), ownerURL, password)
	if err != nil {
		t.Fatalf("connecting to the test database: %v", err)
	}
	t.Cleanup(db.Close)
	return db, ownerURL
}

// Database creates a fresh, migrated database and returns its owner URL and
// the application role's password, for a test that connects on its own terms.
func Database(t testing.TB) (string, secret.Value) {
	t.Helper()
	ctx := context.Background()

	once.Do(func() { clusterURL, passwords, setupErr = setup(ctx) })
	if setupErr != nil {
		t.Fatalf("starting the test Postgres: %v", setupErr)
	}

	name := fmt.Sprintf("pando_test_%d", nextDatabase.Add(1))
	if err := exec(ctx, clusterURL, `CREATE DATABASE "`+name+`" TEMPLATE `+templateName); err != nil {
		t.Fatalf("copying the test database: %v", err)
	}
	return withDatabase(clusterURL, name), passwords.App
}

// Archiver connects to a database Database made, as the audit archiver role
// (R-348). The pool is closed when the test ends.
func Archiver(t testing.TB, ownerURL string) *pgxpool.Pool {
	t.Helper()
	pool, err := state.ConnectArchiver(context.Background(), ownerURL, passwords.Archiver)
	if err != nil {
		t.Fatalf("connecting to the test database as the archiver: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// setup starts the container and prepares the template every test copies.
func setup(ctx context.Context) (string, state.Passwords, error) {
	container, err := postgres.Run(ctx, "postgres:17-alpine",
		postgres.WithDatabase("pando"),
		postgres.WithUsername("pando"),
		postgres.WithPassword("test-password"),
		// Every parallel test holds a pool of its own. The default of 100
		// connections is fewer than a busy run opens at once.
		testcontainers.WithCmdArgs("-c", "max_connections=500"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(90*time.Second)),
	)
	if err != nil {
		return "", state.Passwords{}, err
	}
	// Not terminated here: the container lives as long as the test binary,
	// and Ryuk reaps it afterwards.
	base, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		return "", state.Passwords{}, err
	}

	if err := exec(ctx, base, `CREATE DATABASE `+templateName); err != nil {
		return "", state.Passwords{}, err
	}
	prepared, err := state.PrepareTemplate(ctx, withDatabase(base, templateName))
	if err != nil {
		return "", state.Passwords{}, err
	}
	// A copy cannot be made while anything is connected to the template, so
	// nothing is allowed to be, and a backend still on its way out is ended
	// rather than waited for.
	if err := exec(ctx, base, `ALTER DATABASE `+templateName+` WITH ALLOW_CONNECTIONS false`); err != nil {
		return "", state.Passwords{}, err
	}
	if err := exec(ctx, base, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity
		WHERE datname = '`+templateName+`' AND pid <> pg_backend_pid()`); err != nil {
		return "", state.Passwords{}, err
	}
	return base, prepared, nil
}

func exec(ctx context.Context, dsn, stmt string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close(ctx) }()
	_, err = conn.Exec(ctx, stmt)
	return err
}

// withDatabase is dsn pointed at another database in the same cluster.
func withDatabase(dsn, name string) string {
	u, err := url.Parse(dsn)
	if err != nil {
		// The container produced dsn; one that does not parse is a bug here.
		panic(err)
	}
	u.Path = "/" + name
	return u.String()
}
