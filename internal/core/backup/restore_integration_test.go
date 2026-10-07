//go:build integration

package backup_test

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"

	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/secret"
)

// TestR212_ARestoreReplacesAnInstallWhoseAuditLogIsPartitioned asserts the
// database step of a DR restore against a database migrated the way a real
// install is, which since migration 000041 means a partitioned audit_events.
//
// pg_restore --clean could not drop a partition's inherited primary key, so
// every restore onto a running install failed with "cannot drop inherited
// constraint audit_events_default_pkey". And because the restore is the step
// that replaces everything, it must also be all or nothing: a dump that breaks
// off halfway leaves the install exactly as it was (R-215).
func TestR212_ARestoreReplacesAnInstallWhoseAuditLogIsPartitioned(t *testing.T) {
	ctx := context.Background()
	_, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })

	host, container := useServerTools(t, ownerURL)

	audit := func(action string) {
		t.Helper()
		_, err := owner.Exec(ctx, `INSERT INTO audit_events (principal_kind, principal_id, action)
			VALUES ('system', 'sys_test', $1)`, action)
		require.NoError(t, err)
	}
	actions := func() []string {
		t.Helper()
		rows, err := owner.Query(ctx, `SELECT action FROM audit_events WHERE principal_id = 'sys_test' ORDER BY id`)
		require.NoError(t, err)
		out, err := pgx.CollectRows(rows, pgx.RowTo[string])
		require.NoError(t, err)
		return out
	}

	// A month partition as well as the default one, as an install that has
	// run the archiver has.
	_, err = owner.Exec(ctx, `SELECT audit_ensure_partition(current_date)`)
	require.NoError(t, err)
	audit("before.backup")
	partitions := func() int {
		t.Helper()
		var n int
		require.NoError(t, owner.QueryRow(ctx,
			`SELECT count(*) FROM pg_inherits WHERE inhparent = 'public.audit_events'::regclass`).Scan(&n))
		return n
	}
	partitionsBefore := partitions()
	require.GreaterOrEqual(t, partitionsBefore, 2, "the default partition and at least one month's")

	u, err := url.Parse(ownerURL)
	require.NoError(t, err)
	var dump bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", "--host", host, "exec", container, "pg_dump", "--format=custom",
		"--no-owner", "--no-privileges", "--username", u.User.Username(), strings.TrimPrefix(u.Path, "/"))
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &dump, &stderr
	require.NoError(t, cmd.Run(), stderr.String())

	audit("after.backup")

	// Broken off halfway: refused, and nothing changed.
	half := dump.Bytes()[:dump.Len()*3/4]
	err = backup.RestoreDatabase(ctx, secret.New(ownerURL), bytes.NewReader(half))
	require.Error(t, err, "a truncated dump does not restore")
	require.Equal(t, []string{"before.backup", "after.backup"}, actions(),
		"a restore that fails leaves the install as it was")

	// Whole: the install is what it was at the backup.
	require.NoError(t, backup.RestoreDatabase(ctx, secret.New(ownerURL), bytes.NewReader(dump.Bytes())))
	require.Equal(t, []string{"before.backup"}, actions(), "the install is back at its backed-up state")

	var kind string
	require.NoError(t, owner.QueryRow(ctx,
		`SELECT relkind::text FROM pg_class WHERE oid = 'public.audit_events'::regclass`).Scan(&kind))
	require.Equal(t, "p", kind, "the audit log is still partitioned")
	require.Equal(t, partitionsBefore, partitions(), "with every partition it had")

	// And a second restore over the restored install works too.
	require.NoError(t, backup.RestoreDatabase(ctx, secret.New(ownerURL), bytes.NewReader(dump.Bytes())))
}

// useServerTools puts pg_restore and psql on PATH as the ones inside the test
// database's container, and returns the daemon and that container's ID.
//
// The restore runs the client tools a server image ships, which match its
// Postgres major version; a developer's or a CI runner's own are whatever
// version the machine happens to have, and pg_restore refuses a dump from a
// newer pg_dump. Run inside the container, they connect over its local socket
// to the same server the test holds a connection to.
func useServerTools(t *testing.T, ownerURL string) (host, container string) {
	t.Helper()
	u, err := url.Parse(ownerURL)
	require.NoError(t, err)

	// The daemon testcontainers started the database on, which is not
	// necessarily the docker CLI's current context.
	cli, err := testcontainers.NewDockerClientWithOpts(context.Background())
	require.NoError(t, err)
	host = cli.DaemonHost()
	_ = cli.Close()
	out, err := exec.Command("docker", "--host", host, "ps", "--format", "{{.ID}} {{.Ports}}").Output()
	require.NoError(t, err)
	var ids []string
	for _, line := range strings.Split(string(out), "\n") {
		if id, ports, ok := strings.Cut(line, " "); ok && strings.Contains(ports, ":"+u.Port()+"->5432/") {
			ids = append(ids, id)
		}
	}
	require.Len(t, ids, 1, "exactly one container publishes the test database's port %s", u.Port())

	dir := t.TempDir()
	for _, tool := range []string{"pg_restore", "psql"} {
		script := "#!/bin/sh\nexec docker --host '" + host + "' exec -i -e PGUSER -e PGPASSWORD -e PGDATABASE " +
			ids[0] + " " + tool + " \"$@\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, tool), []byte(script), 0o755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return host, ids[0]
}
