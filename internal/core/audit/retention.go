package audit

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Audit retention (R-347, R-348, design 02 §2.6).
//
// The live log keeps a bounded number of months. A month past retention is
// written out as gzipped JSON lines, read back and checked, recorded, and only
// then removed — by a database function that refuses a month younger than the
// floor or one without an archive of every row it holds. The application role
// still cannot remove anything (R-027); this runs as a separate role whose
// only power over the log is that function.

// MinRetentionMonths is the floor (R-348): no month leaves the live log until
// it ended at least this long ago, whatever policy says. audit_drop_month holds
// the same number in the database, so lowering this alone changes nothing.
const MinRetentionMonths = 3

// DefaultRetentionMonths is retention when policy does not say (R-348).
const DefaultRetentionMonths = 3

// ArchiveMode is what happens to a month past retention.
type ArchiveMode string

const (
	// ArchiveKeep writes the archive under Pando's own directory. The default.
	ArchiveKeep ArchiveMode = "keep"
	// ArchiveExport writes it to a backup destination (R-217).
	ArchiveExport ArchiveMode = "export"
	// ArchiveOff archives nothing, so nothing leaves the live log: the log
	// grows as it did before retention existed.
	ArchiveOff ArchiveMode = "off"
)

// Valid refuses anything but the three modes. Empty is unset, which is keep.
func (m ArchiveMode) Valid() error {
	switch m {
	case "", ArchiveKeep, ArchiveExport, ArchiveOff:
		return nil
	}
	return fmt.Errorf("%q is not an audit_archive setting; use keep, export or off", string(m))
}

// Retention is host policy's word on the audit log.
type Retention struct {
	// Months the live log keeps. Zero is DefaultRetentionMonths.
	Months int
	// Archive is the mode. Empty is ArchiveKeep.
	Archive ArchiveMode
	// Destination is the backup adapter an export goes to. Empty is the
	// default backup destination.
	Destination string
}

// MonthsKept is retention with the default applied and the floor held.
func (r Retention) MonthsKept() int {
	if r.Months <= 0 {
		return DefaultRetentionMonths
	}
	return max(r.Months, MinRetentionMonths)
}

// Mode is the archive mode with the default applied.
func (r Retention) Mode() ArchiveMode {
	if r.Archive == "" {
		return ArchiveKeep
	}
	return r.Archive
}

// Store is somewhere an archive can be written and read back. A backup
// adapter is one (R-217); so is the directory Pando keeps archives in.
type Store interface {
	Writer(ctx context.Context, name string) (io.WriteCloser, error)
	Reader(ctx context.Context, name string) (io.ReadCloser, error)
}

// Stores resolves where an archive lives.
type Stores struct {
	// Kept is Pando's own directory, for ArchiveKeep.
	Kept Store

	// Export resolves a backup adapter by reference, empty meaning the
	// default, and returns the reference it resolved to.
	Export func(ref string) (Store, string, error)
}

// open is the store an archive was written to: Kept when it has no adapter
// reference, otherwise that adapter. Resolved from what was recorded and
// never looked for elsewhere, like a backup (design 02 §2.8).
func (s Stores) open(adapterRef string) (Store, error) {
	if adapterRef == "" {
		if s.Kept == nil {
			return nil, errs.New(errs.Internal, "This installation has no directory for audit archives.")
		}
		return s.Kept, nil
	}
	if s.Export == nil {
		return nil, errs.Newf(errs.Internal, "There is no backup destination called %q.", adapterRef)
	}
	st, _, err := s.Export(adapterRef)
	return st, err
}

// ArchiveRecord is one archived month: where it is and what is in it.
//
// The manifest R-347 asks for. A copy is written beside the archive, so an
// exported archive says what it holds without Pando's database.
type ArchiveRecord struct {
	ID         string    `json:"id"`
	Month      string    `json:"month"` // 2026-06
	AdapterRef string    `json:"adapter_ref,omitempty"`
	ObjectName string    `json:"object_name"`
	RowCount   int64     `json:"row_count"`
	FirstID    int64     `json:"first_id"`
	LastID     int64     `json:"last_id"`
	FirstAt    time.Time `json:"first_at"`
	LastAt     time.Time `json:"last_at"`
	SizeBytes  int64     `json:"size_bytes"`
	SHA256     string    `json:"sha256"`
	CreatedAt  time.Time `json:"created_at"`
}

// ManifestName is the manifest's object name beside an archive.
func ManifestName(objectName string) string { return objectName + ".manifest.json" }

// DefaultArchiveInterval is how often retention runs. Months turn over once a
// month; a daily pass finds each one within a day and costs one small query
// on the days there is nothing to do.
const DefaultArchiveInterval = 24 * time.Hour

// Archiver runs retention. Pool must be held as the archiver role
// (state.ArchiverRole); the application role cannot do any of this, by design.
type Archiver struct {
	Pool      *pgxpool.Pool
	Stores    Stores
	Retention func(ctx context.Context) (Retention, error)
	Clock     clock.Clock
	Logger    *zap.Logger
	Interval  time.Duration

	// Holds names the enabled audit sinks that have not been sent every event
	// in [lo, hi) (R-386). A month any of them holds is neither archived nor
	// removed this pass: archiving it now would record an archive for rows
	// still in the live log. Nil holds nothing.
	Holds func(ctx context.Context, lo, hi time.Time) ([]string, error)

	// Audit records that a month was held, once per month per process. The
	// archiver's own role cannot write the log, so this is the application's
	// writer. Nil records nothing.
	Audit func(ctx context.Context, e Event)

	held map[string]bool
}

// Run passes at startup and then every Interval until ctx ends.
func (a *Archiver) Run(ctx context.Context) {
	interval := a.Interval
	if interval <= 0 {
		interval = DefaultArchiveInterval
	}
	for {
		if _, err := a.Pass(ctx); err != nil && ctx.Err() == nil {
			// Logged, and tried again next pass. Nothing has left the log:
			// every step before the drop is one a failure leaves undone.
			a.Logger.Error("audit retention did not finish", zap.Error(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-a.Clock.After(interval):
		}
	}
}

// Pass makes the partitions the next months need, then archives and removes
// every month past retention. It returns the archives that months left the
// live log under.
func (a *Archiver) Pass(ctx context.Context) ([]ArchiveRecord, error) {
	now := a.Clock.Now().UTC()
	this := monthOf(now)

	// This month and the next two, so writes land in a partition of their own
	// rather than the default one. Done whatever retention says: it is how
	// the table is laid out, not a retention decision.
	for n := 0; n < 3; n++ {
		m := this.AddDate(0, n, 0)
		if _, err := a.Pool.Exec(ctx, `SELECT audit_ensure_partition($1::date)`, dateOf(m)); err != nil {
			return nil, errs.Wrap(errs.Internal, "Pando could not prepare the audit log for the coming months.", err)
		}
	}

	r, err := a.Retention(ctx)
	if err != nil {
		return nil, err
	}
	if r.Mode() == ArchiveOff {
		return nil, nil
	}

	// A month is past retention when it ended at least MonthsKept ago. That
	// is every month before the one the cutoff falls in.
	before := monthOf(now.AddDate(0, -r.MonthsKept(), 0))
	months, err := a.monthsBefore(ctx, before)
	if err != nil {
		return nil, err
	}

	var out []ArchiveRecord
	for _, m := range months {
		rec, err := a.retire(ctx, m, r)
		if err != nil {
			return out, err
		}
		if rec.ID != "" {
			out = append(out, rec)
			a.Logger.Info("audit month archived",
				zap.String("month", rec.Month), zap.Int64("rows", rec.RowCount),
				zap.String("archive_id", rec.ID), zap.String("sha256", rec.SHA256))
		}
	}
	return out, nil
}

// monthsBefore lists every month before `before` that holds events or has a
// partition: an emptied partition is removed too, so the table does not
// collect empty months forever.
func (a *Archiver) monthsBefore(ctx context.Context, before time.Time) ([]time.Time, error) {
	rows, err := a.Pool.Query(ctx, `
		SELECT m FROM (
			SELECT date_trunc('month', occurred_at AT TIME ZONE 'UTC')::date AS m
			FROM audit_events WHERE occurred_at < $1 GROUP BY 1
			UNION
			SELECT to_date(substring(c.relname from 14), 'YYYY_MM')
			FROM pg_inherits i JOIN pg_class c ON c.oid = i.inhrelid
			WHERE i.inhparent = 'public.audit_events'::regclass
			  AND c.relname ~ '^audit_events_[0-9]{4}_[0-9]{2}$'
		) AS months
		WHERE m < $2::date
		ORDER BY m`, before, dateOf(before))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read which audit months are past retention.", err)
	}
	months, err := pgx.CollectRows(rows, pgx.RowTo[time.Time])
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read which audit months are past retention.", err)
	}
	return months, nil
}

// retire archives one month, verifies the archive, records it, and drops the
// month. Each step is one a failure leaves the month intact behind.
func (a *Archiver) retire(ctx context.Context, month time.Time, r Retention) (ArchiveRecord, error) {
	month = monthOf(month)
	if a.Holds != nil {
		holders, err := a.Holds(ctx, month, month.AddDate(0, 1, 0))
		if err != nil {
			return ArchiveRecord{}, err
		}
		if len(holders) > 0 {
			a.hold(ctx, month, holders)
			return ArchiveRecord{}, nil
		}
	}
	rec, err := a.archive(ctx, month, r)
	if err != nil {
		return ArchiveRecord{}, err
	}
	if _, err := a.Pool.Exec(ctx, `SELECT audit_drop_month($1::date, $2, $3)`,
		dateOf(month), rec.RowCount, rec.SHA256); err != nil {
		return ArchiveRecord{}, errs.Wrap(errs.Internal,
			fmt.Sprintf("Pando archived the audit events from %s but could not remove them from the live log.", label(month)), err).
			WithRemedy("Nothing was lost: the events are in the live log and in the archive. Pando tries again at its next daily pass.")
	}
	return rec, nil
}

// hold leaves a month in the live log because an audit sink has not been
// sent all of it, and says so once (R-386). A sink that keeps failing is
// turned off (R-383) and stops holding, so a dead collector cannot keep the
// live log growing (R-224).
func (a *Archiver) hold(ctx context.Context, month time.Time, holders []string) {
	a.Logger.Warn("audit month kept in the live log until every audit sink has been sent it",
		zap.String("month", label(month)), zap.Strings("audit_sinks", holders))
	if a.held == nil {
		a.held = map[string]bool{}
	}
	if a.held[label(month)] || a.Audit == nil {
		return
	}
	a.held[label(month)] = true
	a.Audit(ctx, Event{
		PrincipalKind: KindSystem, PrincipalID: "system",
		Action: "audit.archive.held", TargetKind: "audit_month", TargetID: label(month),
		Detail: map[string]any{"month": label(month), "audit_sinks": holders},
	})
}

// archive returns a verified, recorded archive of every event in month — one
// already written if it still holds exactly the month's events, or a new one.
// A month with no events gets a zero record, which audit_drop_month accepts.
func (a *Archiver) archive(ctx context.Context, month time.Time, r Retention) (ArchiveRecord, error) {
	lo, hi := month, month.AddDate(0, 1, 0)

	// One snapshot for the count and the rows, so the archive and the
	// manifest describe the same events. audit_drop_month counts again under
	// a lock, so an event arriving later in an old month — a restore, a
	// backdated insert — is a mismatch it refuses, not a row it drops.
	tx, err := a.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return ArchiveRecord{}, errs.Wrap(errs.Internal, "Pando could not read the audit log to archive it.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = 'UTC'`); err != nil {
		return ArchiveRecord{}, errs.Wrap(errs.Internal, "Pando could not read the audit log to archive it.", err)
	}

	want := ArchiveRecord{Month: label(month)}
	var firstAt, lastAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT count(*), coalesce(min(id), 0), coalesce(max(id), 0), min(occurred_at), max(occurred_at)
		FROM audit_events WHERE occurred_at >= $1 AND occurred_at < $2`, lo, hi).
		Scan(&want.RowCount, &want.FirstID, &want.LastID, &firstAt, &lastAt); err != nil {
		return ArchiveRecord{}, errs.Wrap(errs.Internal, "Pando could not read the audit log to archive it.", err)
	}
	if want.RowCount == 0 {
		return want, nil
	}
	want.FirstAt, want.LastAt = firstAt.UTC(), lastAt.UTC()

	// A pass that wrote an archive and then failed to drop the month — or a
	// drop refused — finds that archive here, checks it again, and uses it.
	if prior, ok, err := a.recorded(ctx, tx, want); err != nil {
		return ArchiveRecord{}, err
	} else if ok {
		st, err := a.Stores.open(prior.AdapterRef)
		if err == nil {
			err = Verify(ctx, st, prior)
		}
		if err == nil {
			return prior, nil
		}
		a.Logger.Warn("an earlier audit archive no longer verifies; writing a new one",
			zap.String("archive_id", prior.ID), zap.Error(err))
	}

	st, ref, err := a.destination(r)
	if err != nil {
		return ArchiveRecord{}, err
	}
	rec := want
	rec.ID = id.New(id.AuditArchive)
	rec.AdapterRef = ref
	rec.ObjectName = fmt.Sprintf("audit-%s-%s.jsonl.gz", rec.Month, rec.ID)

	if err := a.write(ctx, tx, st, &rec, lo, hi); err != nil {
		return ArchiveRecord{}, err
	}
	if err := Verify(ctx, st, rec); err != nil {
		return ArchiveRecord{}, err
	}

	if err := a.Pool.QueryRow(ctx, `
		INSERT INTO audit_archives (id, month, adapter_ref, object_name, row_count, first_id, last_id,
		                            first_at, last_at, size_bytes, sha256)
		VALUES ($1, $2::date, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING created_at`,
		rec.ID, dateOf(month), nullable(rec.AdapterRef), rec.ObjectName, rec.RowCount, rec.FirstID, rec.LastID,
		rec.FirstAt, rec.LastAt, rec.SizeBytes, rec.SHA256).Scan(&rec.CreatedAt); err != nil {
		return ArchiveRecord{}, errs.Wrap(errs.Internal, "Pando wrote an audit archive but could not record it.", err)
	}
	rec.CreatedAt = rec.CreatedAt.UTC()
	return rec, nil
}

// recorded finds an archive already holding exactly want's events.
func (a *Archiver) recorded(ctx context.Context, tx pgx.Tx, want ArchiveRecord) (ArchiveRecord, bool, error) {
	rows, err := tx.Query(ctx, archiveSelect+`
		WHERE month = $1::date AND row_count = $2 AND first_id = $3 AND last_id = $4
		ORDER BY created_at DESC LIMIT 1`, want.Month+"-01", want.RowCount, want.FirstID, want.LastID)
	if err != nil {
		return ArchiveRecord{}, false, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	found, err := pgx.CollectRows(rows, scanArchive)
	if err != nil {
		return ArchiveRecord{}, false, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	if len(found) == 0 {
		return ArchiveRecord{}, false, nil
	}
	return found[0], true, nil
}

// destination is where a new archive goes under r.
func (a *Archiver) destination(r Retention) (Store, string, error) {
	if r.Mode() == ArchiveExport {
		if a.Stores.Export == nil {
			return nil, "", errs.New(errs.Internal, "This installation has no backup destination to export audit archives to.")
		}
		st, ref, err := a.Stores.Export(r.Destination)
		if err != nil {
			return nil, "", err
		}
		return st, ref, nil
	}
	if a.Stores.Kept == nil {
		return nil, "", errs.New(errs.Internal, "This installation has no directory for audit archives.")
	}
	return a.Stores.Kept, "", nil
}

// write streams the month's events into the archive and fills in its size and
// digest, then writes the manifest beside it.
//
// One JSON object per line, as Postgres renders the row: every column, nulls
// included, times in UTC. The archive holds what the rows hold and nothing
// else — no secret value was ever in a row to carry (R-194).
func (a *Archiver) write(ctx context.Context, tx pgx.Tx, st Store, rec *ArchiveRecord, lo, hi time.Time) error {
	failed := func(err error) error {
		return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the audit archive for %s.", rec.Month), err)
	}

	digest := sha256.New()
	size := &counter{}
	var written int64
	err := writeObject(ctx, st, rec.ObjectName, func(w io.Writer) error {
		gz := gzip.NewWriter(io.MultiWriter(w, digest, size))
		rows, err := tx.Query(ctx, `
			SELECT row_to_json(e)::text FROM audit_events e
			WHERE occurred_at >= $1 AND occurred_at < $2 ORDER BY id`, lo, hi)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				return err
			}
			if _, err := io.WriteString(gz, line+"\n"); err != nil {
				return err
			}
			written++
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return gz.Close()
	})
	if err != nil {
		return failed(err)
	}
	if written != rec.RowCount {
		return failed(fmt.Errorf("wrote %d events, expected %d", written, rec.RowCount))
	}
	rec.SizeBytes = size.n
	rec.SHA256 = hex.EncodeToString(digest.Sum(nil))

	manifest, err := json.MarshalIndent(rec, "", "  ")
	if err == nil {
		err = writeObject(ctx, st, ManifestName(rec.ObjectName), func(w io.Writer) error {
			_, err := w.Write(append(manifest, '\n'))
			return err
		})
	}
	if err != nil {
		return failed(err)
	}
	return nil
}

// writeObject writes one object with fill, and closes it whether or not fill
// succeeded: a store that writes atomically keeps nothing that was not closed
// cleanly after a whole write.
func writeObject(ctx context.Context, st Store, name string, fill func(io.Writer) error) error {
	w, err := st.Writer(ctx, name)
	if err != nil {
		return err
	}
	if err := fill(w); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

// Verify reads an archive back and checks it against its record: the stored
// bytes' size and digest, that it decompresses, and that it holds RowCount
// events in id order from FirstID to LastID. Nothing is removed from the live
// log until this passes (R-347).
func Verify(ctx context.Context, st Store, rec ArchiveRecord) error {
	bad := func(why string) error {
		return errs.Newf(errs.BackupIncomplete,
			"The audit archive for %s (%s) does not hold what was recorded: %s.", rec.Month, rec.ID, why).
			WithRemedy("Nothing is removed from the live audit log while its archive does not verify. Check the archive's destination; Pando writes a new archive at its next pass.")
	}

	rc, err := st.Reader(ctx, rec.ObjectName)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	digest := sha256.New()
	size := &counter{}
	raw := io.TeeReader(rc, io.MultiWriter(digest, size))
	gz, err := gzip.NewReader(raw)
	if err != nil {
		return bad("it is not a gzip file")
	}
	lines := bufio.NewReader(gz)
	var count, prev, first int64
	for {
		line, err := lines.ReadBytes('\n')
		if len(line) > 0 {
			var row struct {
				ID int64 `json:"id"`
			}
			if jerr := json.Unmarshal(line, &row); jerr != nil {
				return bad(fmt.Sprintf("line %d is not an audit event", count+1))
			}
			if count > 0 && row.ID <= prev {
				return bad(fmt.Sprintf("line %d is out of order", count+1))
			}
			if count == 0 {
				first = row.ID
			}
			prev = row.ID
			count++
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return bad("it does not decompress")
		}
	}
	// Whatever follows the gzip stream counts toward the digest too.
	if _, err := io.Copy(io.Discard, raw); err != nil {
		return bad("it could not be read to the end")
	}

	switch {
	case count != rec.RowCount:
		return bad(fmt.Sprintf("it holds %d events, not %d", count, rec.RowCount))
	case first != rec.FirstID || prev != rec.LastID:
		return bad(fmt.Sprintf("it runs from event %d to %d, not %d to %d", first, prev, rec.FirstID, rec.LastID))
	case size.n != rec.SizeBytes:
		return bad(fmt.Sprintf("it is %d bytes, not %d", size.n, rec.SizeBytes))
	case hex.EncodeToString(digest.Sum(nil)) != rec.SHA256:
		return bad("its SHA-256 digest differs")
	}
	return nil
}

// Archives lists and opens archives for the API (R-261). It reads with the
// application role, which may read the record and write none of it.
type Archives struct {
	Pool   *pgxpool.Pool
	Stores Stores
}

// List is every archive, newest month first.
func (a *Archives) List(ctx context.Context) ([]ArchiveRecord, error) {
	rows, err := a.Pool.Query(ctx, archiveSelect+` ORDER BY month DESC, created_at DESC`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	out, err := pgx.CollectRows(rows, scanArchive)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	if out == nil {
		out = []ArchiveRecord{}
	}
	return out, nil
}

// Open returns an archive's record and its bytes, from wherever it was
// written. The caller closes the reader.
func (a *Archives) Open(ctx context.Context, archiveID string) (ArchiveRecord, io.ReadCloser, error) {
	rows, err := a.Pool.Query(ctx, archiveSelect+` WHERE id = $1`, archiveID)
	if err != nil {
		return ArchiveRecord{}, nil, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	found, err := pgx.CollectRows(rows, scanArchive)
	if err != nil {
		return ArchiveRecord{}, nil, errs.Wrap(errs.Internal, "Pando could not read its audit archives.", err)
	}
	if len(found) == 0 {
		return ArchiveRecord{}, nil, errs.Newf(errs.NotFound, "There is no audit archive %q.", archiveID).
			WithRemedy("GET /api/v1/audit/archives lists the archives this installation holds.")
	}
	rec := found[0]
	st, err := a.Stores.open(rec.AdapterRef)
	if err != nil {
		return ArchiveRecord{}, nil, err
	}
	rc, err := st.Reader(ctx, rec.ObjectName)
	if err != nil {
		return ArchiveRecord{}, nil, err
	}
	return rec, rc, nil
}

const archiveSelect = `
	SELECT id, to_char(month, 'YYYY-MM'), coalesce(adapter_ref, ''), object_name, row_count,
	       first_id, last_id, first_at, last_at, size_bytes, sha256, created_at
	FROM audit_archives`

func scanArchive(row pgx.CollectableRow) (ArchiveRecord, error) {
	var r ArchiveRecord
	err := row.Scan(&r.ID, &r.Month, &r.AdapterRef, &r.ObjectName, &r.RowCount,
		&r.FirstID, &r.LastID, &r.FirstAt, &r.LastAt, &r.SizeBytes, &r.SHA256, &r.CreatedAt)
	r.FirstAt, r.LastAt, r.CreatedAt = r.FirstAt.UTC(), r.LastAt.UTC(), r.CreatedAt.UTC()
	return r, err
}

// monthOf is the first instant of t's month, in UTC.
func monthOf(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func dateOf(month time.Time) string { return month.UTC().Format(time.DateOnly) }

func label(month time.Time) string { return month.UTC().Format("2006-01") }

type counter struct{ n int64 }

func (c *counter) Write(p []byte) (int, error) {
	c.n += int64(len(p))
	return len(p), nil
}
