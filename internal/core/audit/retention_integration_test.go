//go:build integration

package audit_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// retention is one database with the three roles a server holds: the
// application role, the archiver, and the owner a test plants old events with.
type retention struct {
	app, archiver *pgxpool.Pool
	owner         *pgx.Conn
	kept          *memStore
}

func newRetention(t *testing.T) *retention {
	t.Helper()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	return &retention{app: db.Pool, archiver: statetest.Archiver(t, ownerURL), owner: owner, kept: newMemStore()}
}

// plant writes an event at a given time, as nothing in Pando does: the writer
// lets the database stamp it. A month in the past is the only way a test can
// have one.
func (r *retention) plant(t *testing.T, at time.Time, action string) int64 {
	t.Helper()
	var id int64
	require.NoError(t, r.owner.QueryRow(context.Background(), `
		INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action, detail)
		VALUES ($1, 'user', 'usr_planted', $2, '{"why":"test"}') RETURNING id`, at, action).Scan(&id))
	return id
}

// get is what a store holds under name.
func (m *memStore) get(name string) []byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.objects[name]
}

func (r *retention) archiverWith(ret audit.Retention) *audit.Archiver {
	return &audit.Archiver{
		Pool:      r.archiver,
		Stores:    audit.Stores{Kept: r.kept},
		Retention: func(context.Context) (audit.Retention, error) { return ret, nil },
		Clock:     clock.System{},
		Logger:    zap.NewNop(),
	}
}

func (r *retention) count(t *testing.T, where string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, r.owner.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE `+where, args...).Scan(&n))
	return n
}

func (r *retention) partitionExists(t *testing.T, name string) bool {
	t.Helper()
	var exists bool
	require.NoError(t, r.owner.QueryRow(context.Background(),
		`SELECT to_regclass('public.' || $1) IS NOT NULL`, name).Scan(&exists))
	return exists
}

// TestR347_AMonthPastRetentionIsArchivedVerifiedAndRemoved asserts issue #60's
// first acceptance condition: past the retention period a month's events are
// archived, the archive verifies, and the month's partition is gone — and
// nothing younger is touched.
func TestR347_AMonthPastRetentionIsArchivedVerifiedAndRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)

	// January 2025 has a partition of its own; February 2025's events are in
	// the default partition, the way a restore or a long-stopped archiver
	// leaves them. Both are well past the floor whenever this runs.
	_, err := r.archiver.Exec(ctx, `SELECT audit_ensure_partition('2025-01-01')`)
	require.NoError(t, err)
	jan := []int64{
		r.plant(t, time.Date(2025, 1, 3, 9, 0, 0, 0, time.UTC), "app.create"),
		r.plant(t, time.Date(2025, 1, 31, 23, 59, 59, 0, time.UTC), "app.deploy"),
	}
	r.plant(t, time.Date(2025, 2, 14, 12, 0, 0, 0, time.UTC), "grant.create")
	recent := r.plant(t, time.Now().UTC().AddDate(0, -1, 0), "app.restart")

	archived, err := r.archiverWith(audit.Retention{}).Pass(ctx)
	require.NoError(t, err)
	require.Len(t, archived, 2, "January and February, nothing younger")
	byMonth := map[string]audit.ArchiveRecord{}
	for _, a := range archived {
		byMonth[a.Month] = a
	}
	got := byMonth["2025-01"]
	require.EqualValues(t, 2, got.RowCount)
	require.Equal(t, jan[0], got.FirstID)
	require.Equal(t, jan[1], got.LastID)
	require.Empty(t, got.AdapterRef, "kept by Pando by default")
	require.Len(t, got.SHA256, 64)

	// The archive verifies, and holds the events as the log held them.
	require.NoError(t, audit.Verify(ctx, r.kept, got))
	gz, err := gzip.NewReader(bytes.NewReader(r.kept.get(got.ObjectName)))
	require.NoError(t, err)
	var rows []map[string]any
	scan := bufio.NewScanner(gz)
	for scan.Scan() {
		var row map[string]any
		require.NoError(t, json.Unmarshal(scan.Bytes(), &row))
		rows = append(rows, row)
	}
	require.Len(t, rows, 2)
	require.Equal(t, "app.create", rows[0]["action"])
	require.Equal(t, "usr_planted", rows[0]["principal_id"])
	require.Equal(t, map[string]any{"why": "test"}, rows[0]["detail"])
	require.Equal(t, "2025-01-03T09:00:00+00:00", rows[0]["occurred_at"])

	// The manifest is beside it.
	var manifest audit.ArchiveRecord
	require.NoError(t, json.Unmarshal(r.kept.get(audit.ManifestName(got.ObjectName)), &manifest))
	require.Equal(t, got.SHA256, manifest.SHA256)
	require.Equal(t, got.RowCount, manifest.RowCount)

	// The months are gone from the live log, partition and all.
	require.False(t, r.partitionExists(t, "audit_events_2025_01"), "the month's partition is dropped")
	require.Zero(t, r.count(t, `occurred_at < '2025-03-01'`))
	require.Equal(t, 1, r.count(t, `id = $1`, recent), "a recent event stays")

	// Leaving is recorded, in the log, by the database.
	var detail map[string]any
	require.NoError(t, r.owner.QueryRow(ctx, `
		SELECT detail FROM audit_events WHERE action = 'audit.archive' AND target_id = $1`, got.ID).Scan(&detail))
	require.Equal(t, "2025-01", detail["month"])
	require.EqualValues(t, 2, detail["rows"])
	require.Equal(t, got.SHA256, detail["sha256"])

	// The application role lists and opens what was archived.
	list, err := (&audit.Archives{Pool: r.app, Stores: audit.Stores{Kept: r.kept}}).List(ctx)
	require.NoError(t, err)
	require.Len(t, list, 2)
	require.Equal(t, "2025-02", list[0].Month, "newest month first")

	// A second pass finds nothing left to do.
	again, err := r.archiverWith(audit.Retention{}).Pass(ctx)
	require.NoError(t, err)
	require.Empty(t, again)
}

// TestR347_TurningArchivingOffKeepsEverything asserts the way back to before
// retention: with audit_archive off nothing is archived, so nothing leaves.
func TestR347_TurningArchivingOffKeepsEverything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), "app.create")

	archived, err := r.archiverWith(audit.Retention{Archive: audit.ArchiveOff}).Pass(ctx)
	require.NoError(t, err)
	require.Empty(t, archived)
	require.Equal(t, 1, r.count(t, `occurred_at < '2024-07-01'`))
}

// TestR347_LongerRetentionKeepsMore asserts that policy lengthens retention: a
// month past the default but inside the configured period stays.
func TestR347_LongerRetentionKeepsMore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	fiveMonths := time.Now().UTC().AddDate(0, -5, 0)
	r.plant(t, fiveMonths, "app.create")

	archived, err := r.archiverWith(audit.Retention{Months: 12}).Pass(ctx)
	require.NoError(t, err)
	require.Empty(t, archived)
	require.Equal(t, 1, r.count(t, `action = 'app.create'`))
}

// TestR348_TheApplicationRoleStillCannotRemoveAuditEvents asserts issue #60's
// second acceptance condition, and R-027 with retention in place: the role
// serving traffic cannot UPDATE, DELETE or TRUNCATE the log — not through the
// table, not through a month's partition, not through the default partition —
// cannot call the functions that make or drop a month, and cannot vouch for an
// archive that was never written.
func TestR348_TheApplicationRoleStillCannotRemoveAuditEvents(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	_, err := r.archiver.Exec(ctx, `SELECT audit_ensure_partition('2025-01-01')`)
	require.NoError(t, err)
	r.plant(t, time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), "app.create")
	r.plant(t, time.Date(2024, 1, 3, 0, 0, 0, 0, time.UTC), "app.create") // in the default partition

	this := "audit_events_" + time.Now().UTC().Format("2006_01")
	for _, table := range []string{"audit_events", "audit_events_2025_01", "audit_events_default", this} {
		for _, stmt := range []string{
			`UPDATE %s SET action = 'tampered'`,
			`DELETE FROM %s`,
			`TRUNCATE %s`,
		} {
			sql := fmt.Sprintf(stmt, table)
			_, err := r.app.Exec(ctx, sql)
			require.Error(t, err, "the application role must not be able to run %q", sql)
			require.Contains(t, err.Error(), "permission denied", sql)
		}
	}

	for _, sql := range []string{
		`SELECT audit_drop_month('2025-01-01', 1, repeat('0', 64))`,
		`SELECT audit_ensure_partition('2030-01-01')`,
		`INSERT INTO audit_archives (id, month, object_name, row_count, first_id, last_id, first_at, last_at, size_bytes, sha256)
		 VALUES ('aar_forged', '2025-01-01', 'forged', 1, 1, 1, now(), now(), 1, repeat('0', 64))`,
		`DELETE FROM audit_archives`,
		`SET ROLE ` + state.ArchiverRole,
	} {
		_, err := r.app.Exec(ctx, sql)
		require.Error(t, err, "the application role must not be able to run %q", sql)
	}

	require.Equal(t, 2, r.count(t, `occurred_at < '2025-02-01'`), "nothing was removed")
}

// TestR348_NoMonthLeavesBeforeTheFloorOrWithoutItsArchive asserts what the
// database refuses even the archiver: a month that ended less than three
// months ago, a month with no archive, an archive that does not hold every
// event — including one that arrived in the month after it was archived — and
// anything but the first of a month. The archiver holds no UPDATE or DELETE of
// its own either.
func TestR348_NoMonthLeavesBeforeTheFloorOrWithoutItsArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)

	lastMonth := time.Now().UTC().AddDate(0, -1, 0)
	r.plant(t, lastMonth, "app.create")
	first := lastMonth.Format("2006-01") + "-01"
	_, err := r.archiver.Exec(ctx, `SELECT audit_drop_month($1::date, 1, repeat('0', 64))`, first)
	require.ErrorContains(t, err, "less than three months old")

	old := r.plant(t, time.Date(2025, 1, 3, 0, 0, 0, 0, time.UTC), "app.create")
	_, err = r.archiver.Exec(ctx, `SELECT audit_drop_month('2025-01-01', 1, repeat('0', 64))`)
	require.ErrorContains(t, err, "no archive")

	_, err = r.archiver.Exec(ctx, `SELECT audit_drop_month('2025-01-15', 1, repeat('0', 64))`)
	require.ErrorContains(t, err, "first day of a month")

	// Archived — and then another event lands in the month before the drop.
	_, err = r.archiver.Exec(ctx, `
		INSERT INTO audit_archives (id, month, object_name, row_count, first_id, last_id, first_at, last_at, size_bytes, sha256)
		VALUES ('aar_01', '2025-01-01', 'x', 1, $1, $1, now(), now(), 1, repeat('a', 64))`, old)
	require.NoError(t, err)
	r.plant(t, time.Date(2025, 1, 4, 0, 0, 0, 0, time.UTC), "app.delete")
	_, err = r.archiver.Exec(ctx, `SELECT audit_drop_month('2025-01-01', 1, repeat('a', 64))`)
	require.ErrorContains(t, err, "not the 1 archived")
	_, err = r.archiver.Exec(ctx, `SELECT audit_drop_month('2025-01-01', 2, repeat('a', 64))`)
	require.ErrorContains(t, err, "no archive holding all 2")
	require.Equal(t, 2, r.count(t, `occurred_at < '2025-02-01'`), "nothing was removed")

	for _, sql := range []string{
		`DELETE FROM audit_events`,
		`UPDATE audit_events SET action = 'tampered'`,
		`DELETE FROM audit_archives`,
	} {
		_, err := r.archiver.Exec(ctx, sql)
		require.Error(t, err, "the archiver must not be able to run %q", sql)
	}

	// The archiver itself writes a new archive holding both and then drops.
	archived, err := r.archiverWith(audit.Retention{}).Pass(ctx)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.EqualValues(t, 2, archived[0].RowCount)
	require.Zero(t, r.count(t, `occurred_at < '2025-02-01'`))
}

// TestR348_StartupRefusesAnAuditPartitionTheAppRoleCanDeleteFrom asserts the
// preflight covers partitions: a month of the log is a table, and DELETE on it
// is DELETE on the audit log. A copy whose partition the application role can
// delete from is not served.
func TestR348_StartupRefusesAnAuditPartitionTheAppRoleCanDeleteFrom(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	ownerURL, password := statetest.Database(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })

	_, err = owner.Exec(ctx, `GRANT DELETE ON audit_events_default TO `+state.AppRole)
	require.NoError(t, err)
	_, err = state.ConnectCopy(ctx, ownerURL, password)
	require.ErrorContains(t, err, "a month of audit records")

	_, err = owner.Exec(ctx, `REVOKE DELETE ON audit_events_default FROM `+state.AppRole)
	require.NoError(t, err)
	_, err = owner.Exec(ctx, `GRANT EXECUTE ON FUNCTION audit_drop_month(date, bigint, text) TO `+state.AppRole)
	require.NoError(t, err)
	_, err = state.ConnectCopy(ctx, ownerURL, password)
	require.ErrorContains(t, err, "may call the functions")
	_, err = owner.Exec(ctx, `REVOKE EXECUTE ON FUNCTION audit_drop_month(date, bigint, text) FROM `+state.AppRole)
	require.NoError(t, err)

	_, err = owner.Exec(ctx, `GRANT INSERT ON audit_archives TO `+state.AppRole)
	require.NoError(t, err)
	_, err = state.ConnectCopy(ctx, ownerURL, password)
	require.ErrorContains(t, err, "record of audit archives")
	_, err = owner.Exec(ctx, `REVOKE INSERT ON audit_archives FROM `+state.AppRole)
	require.NoError(t, err)

	_, err = owner.Exec(ctx, `ALTER FUNCTION audit_drop_month(date, bigint, text) OWNER TO `+state.AppRole)
	require.NoError(t, err)
	_, err = state.ConnectCopy(ctx, ownerURL, password)
	require.ErrorContains(t, err, "owns the functions")
}

// TestR348_ARestoreIsReprotectedAtOnce asserts what a DR restore calls once
// pg_restore has recreated every table: the grants are applied again, so a
// month that arrived with the owner's default privileges stops being
// deletable without waiting for a restart.
func TestR348_ARestoreIsReprotectedAtOnce(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, ownerURL := statetest.Connect(t)
	owner, err := pgx.Connect(ctx, ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })

	_, err = owner.Exec(ctx, `GRANT DELETE ON audit_events_default TO `+state.AppRole)
	require.NoError(t, err)
	_, err = db.Exec(ctx, `DELETE FROM audit_events_default WHERE false`)
	require.NoError(t, err, "the drift took effect")

	require.NoError(t, state.Regrant(ctx, ownerURL))
	_, err = db.Exec(ctx, `DELETE FROM audit_events_default WHERE false`)
	require.Error(t, err, "and is revoked again")

	_, err = state.ConnectArchiver(ctx, ownerURL, secret.New("not-the-password"))
	require.Error(t, err, "the archiver connects with its own password or not at all")
}

// TestR347_AnExportedArchiveIsOpenedFromWhereItWasWritten asserts the export
// mode: the archive goes to the backup destination policy names, is recorded
// with it, and is served back from there — never looked for anywhere else.
func TestR347_AnExportedArchiveIsOpenedFromWhereItWasWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 4, 2, 0, 0, 0, 0, time.UTC), "app.create")

	remote := newMemStore()
	stores := audit.Stores{
		Kept: r.kept,
		Export: func(ref string) (audit.Store, string, error) {
			if ref == "" || ref == "bk_remote" {
				return remote, "bk_remote", nil
			}
			return nil, "", errs.Newf(errs.Internal, "no destination %q", ref)
		},
	}
	a := r.archiverWith(audit.Retention{Archive: audit.ArchiveExport})
	a.Stores = stores
	archived, err := a.Pass(ctx)
	require.NoError(t, err)
	require.Len(t, archived, 1)
	require.Equal(t, "bk_remote", archived[0].AdapterRef)
	require.NotEmpty(t, remote.get(archived[0].ObjectName))
	require.Empty(t, r.kept.get(archived[0].ObjectName), "nothing is kept locally when exporting")

	archives := &audit.Archives{Pool: r.app, Stores: stores}
	rec, body, err := archives.Open(ctx, archived[0].ID)
	require.NoError(t, err)
	require.NoError(t, body.Close())
	require.Equal(t, archived[0].SHA256, rec.SHA256)

	_, _, err = archives.Open(ctx, "aar_01HQ8ZZZZZZZZZZZZZZZZZZZZZ")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))

	// Recorded against a destination that is no longer configured: refused,
	// not looked for in Pando's own directory.
	_, _, err = (&audit.Archives{Pool: r.app, Stores: audit.Stores{Kept: r.kept}}).Open(ctx, archived[0].ID)
	require.ErrorContains(t, err, "bk_remote")
}

// TestR347_AnArchiveAlreadyWrittenIsUsedIfItStillVerifies asserts the
// recovery path: a pass that wrote and recorded an archive but did not drop the
// month leaves one the next pass checks again and uses. One that no longer
// verifies is replaced, not trusted.
func TestR347_AnArchiveAlreadyWrittenIsUsedIfItStillVerifies(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	first := r.plant(t, time.Date(2025, 5, 2, 0, 0, 0, 0, time.UTC), "app.create")
	last := r.plant(t, time.Date(2025, 5, 3, 0, 0, 0, 0, time.UTC), "app.deploy")
	june := r.plant(t, time.Date(2025, 6, 3, 0, 0, 0, 0, time.UTC), "app.deploy")

	record := func(id, month string, from, to int64, rows int, name string) {
		t.Helper()
		var lines []string
		for i := 0; i < rows; i++ {
			lines = append(lines, fmt.Sprintf(`{"id":%d}`, from+int64(i)*(to-from)))
		}
		body, rec := archiveOf(t, lines...)
		r.kept.put(name, body)
		_, err := r.archiver.Exec(ctx, `
			INSERT INTO audit_archives (id, month, object_name, row_count, first_id, last_id, first_at, last_at, size_bytes, sha256)
			VALUES ($1, $2::date, $3, $4, $5, $6, now(), now(), $7, $8)`,
			id, month, name, rows, from, to, rec.SizeBytes, rec.SHA256)
		require.NoError(t, err)
	}
	record("aar_01HQ8AAAAAAAAAAAAAAAAAAAAA", "2025-05-01", first, last, 2, "may-earlier.jsonl.gz")
	record("aar_01HQ8BBBBBBBBBBBBBBBBBBBBB", "2025-06-01", june, june, 1, "june-earlier.jsonl.gz")
	r.kept.put("june-earlier.jsonl.gz", []byte("damaged since"))

	archived, err := r.archiverWith(audit.Retention{}).Pass(ctx)
	require.NoError(t, err)
	require.Len(t, archived, 2)
	require.Equal(t, "aar_01HQ8AAAAAAAAAAAAAAAAAAAAA", archived[0].ID, "May's earlier archive still verifies and is used")
	require.NotEqual(t, "aar_01HQ8BBBBBBBBBBBBBBBBBBBBB", archived[1].ID, "June's no longer does and is replaced")
	require.NoError(t, audit.Verify(ctx, r.kept, archived[1]))
	require.Zero(t, r.count(t, `occurred_at < '2025-07-01'`))
}

// TestR347_NothingLeavesWhenTheArchiveCannotBeWritten asserts the order of
// the steps: when the archive cannot be written, or there is nowhere to write
// it, the pass fails and the month stays in the live log.
func TestR347_NothingLeavesWhenTheArchiveCannotBeWritten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newRetention(t)
	r.plant(t, time.Date(2025, 7, 2, 0, 0, 0, 0, time.UTC), "app.create")

	for name, a := range map[string]*audit.Archiver{
		"the store refuses": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Stores = audit.Stores{Kept: failingStore{}}
			return a
		}(),
		"writing fails partway": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Stores = audit.Stores{Kept: &brokenStore{memStore: newMemStore(), failWrite: true}}
			return a
		}(),
		"the manifest cannot be written": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Stores = audit.Stores{Kept: &brokenStore{memStore: newMemStore(), refuseManifest: true}}
			return a
		}(),
		"the archive does not read back": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Stores = audit.Stores{Kept: &brokenStore{memStore: newMemStore(), readsGarbage: true}}
			return a
		}(),
		"no directory": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Stores = audit.Stores{}
			return a
		}(),
		"export with no destinations": r.archiverWith(audit.Retention{Archive: audit.ArchiveExport}),
		"export to one that is not there": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{Archive: audit.ArchiveExport, Destination: "bk_gone"})
			a.Stores.Export = func(ref string) (audit.Store, string, error) {
				return nil, "", errs.Newf(errs.Internal, "no destination %q", ref)
			}
			return a
		}(),
		"policy cannot be read": func() *audit.Archiver {
			a := r.archiverWith(audit.Retention{})
			a.Retention = func(context.Context) (audit.Retention, error) {
				return audit.Retention{}, errs.New(errs.Internal, "policy is unreadable")
			}
			return a
		}(),
	} {
		_, err := a.Pass(ctx)
		require.Error(t, err, name)
		require.Equal(t, 1, r.count(t, `occurred_at < '2025-08-01'`), name)
	}
}

// brokenStore is a memStore that fails in one chosen way.
type brokenStore struct {
	*memStore
	failWrite, refuseManifest, readsGarbage bool
}

func (b *brokenStore) Writer(ctx context.Context, name string) (io.WriteCloser, error) {
	if b.refuseManifest && strings.HasSuffix(name, ".manifest.json") {
		return nil, errs.New(errs.Internal, "the disk is full")
	}
	w, err := b.memStore.Writer(ctx, name)
	if b.failWrite {
		return failingWriter{w}, err
	}
	return w, err
}

func (b *brokenStore) Reader(ctx context.Context, name string) (io.ReadCloser, error) {
	if b.readsGarbage {
		return io.NopCloser(strings.NewReader("not what was written")), nil
	}
	return b.memStore.Reader(ctx, name)
}

type failingWriter struct{ io.WriteCloser }

func (failingWriter) Write([]byte) (int, error) {
	return 0, errs.New(errs.Internal, "the disk is full")
}

// failingStore refuses every write and read.
type failingStore struct{}

func (failingStore) Writer(context.Context, string) (io.WriteCloser, error) {
	return nil, errs.New(errs.Internal, "the disk is full")
}

func (failingStore) Reader(context.Context, string) (io.ReadCloser, error) {
	return nil, errs.New(errs.Internal, "the disk is full")
}

// TestR347_RetentionRunsAtStartAndThenOnItsInterval asserts the loop: a pass
// at once, a failed pass logged rather than fatal, another after the interval,
// and an end when the context does.
func TestR347_RetentionRunsAtStartAndThenOnItsInterval(t *testing.T) {
	t.Parallel()
	r := newRetention(t)
	core, logs := observer.New(zapcore.InfoLevel)
	fake := clock.NewFake(time.Now())

	passes := make(chan struct{}, 4)
	a := r.archiverWith(audit.Retention{})
	a.Clock = fake
	a.Logger = zap.New(core)
	a.Interval = time.Hour
	a.Retention = func(context.Context) (audit.Retention, error) {
		passes <- struct{}{}
		return audit.Retention{}, errs.New(errs.Internal, "policy is unreadable")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { a.Run(ctx); close(done) }()

	<-passes
	require.Eventually(t, func() bool { return logs.FilterMessage("audit retention did not finish").Len() == 1 },
		5*time.Second, 10*time.Millisecond)
	require.Eventually(t, func() bool { fake.Advance(time.Hour); return len(passes) > 0 },
		5*time.Second, 10*time.Millisecond, "another pass after the interval")
	cancel()
	<-done
}
