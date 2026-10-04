package audit_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/errs"
)

// memStore is a Store in memory, for checking what Verify accepts.
type memStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newMemStore() *memStore { return &memStore{objects: map[string][]byte{}} }

func (m *memStore) Writer(_ context.Context, name string) (io.WriteCloser, error) {
	return &memWriter{store: m, name: name}, nil
}

func (m *memStore) Reader(_ context.Context, name string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.objects[name]
	if !ok {
		return nil, errs.Newf(errs.NotFound, "no object %q", name)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (m *memStore) put(name string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[name] = b
}

type memWriter struct {
	store *memStore
	name  string
	buf   bytes.Buffer
}

func (w *memWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *memWriter) Close() error {
	w.store.put(w.name, append([]byte(nil), w.buf.Bytes()...))
	return nil
}

// archiveOf gzips lines and returns the bytes and the record that describes
// them.
func archiveOf(t *testing.T, lines ...string) ([]byte, audit.ArchiveRecord) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	for _, l := range lines {
		_, err := io.WriteString(gz, l+"\n")
		require.NoError(t, err)
	}
	require.NoError(t, gz.Close())
	sum := sha256.Sum256(buf.Bytes())
	return buf.Bytes(), audit.ArchiveRecord{
		ID: "aar_test", Month: "2025-01", ObjectName: "audit-2025-01.jsonl.gz",
		RowCount: int64(len(lines)), FirstID: 1, LastID: int64(len(lines)),
		SizeBytes: int64(buf.Len()), SHA256: hex.EncodeToString(sum[:]),
	}
}

// TestR347_AnArchiveIsCheckedAgainstItsManifest asserts the read-back that
// comes before anything leaves the live log: an archive that is whole passes,
// and one that is truncated, altered, short of an event, or out of order does
// not — and says which.
func TestR347_AnArchiveIsCheckedAgainstItsManifest(t *testing.T) {
	ctx := context.Background()
	lines := []string{`{"id":1,"action":"app.create"}`, `{"id":2,"action":"app.deploy"}`, `{"id":3,"action":"app.delete"}`}
	good, rec := archiveOf(t, lines...)

	st := newMemStore()
	st.put(rec.ObjectName, good)
	require.NoError(t, audit.Verify(ctx, st, rec), "a whole archive verifies")

	err := audit.Verify(ctx, newMemStore(), rec)
	require.Equal(t, errs.NotFound, errs.CodeOf(err), "an archive that is not there is not there")

	for name, tc := range map[string]struct {
		bytes []byte
		rec   audit.ArchiveRecord
		says  string
	}{
		"truncated": {bytes: good[:len(good)-5], rec: rec, says: "does not"},
		"one byte changed": {
			bytes: func() []byte { b := append([]byte(nil), good...); b[len(b)-1] ^= 0xff; return b }(),
			rec:   rec, says: "",
		},
		"an event fewer": {
			bytes: func() []byte { b, _ := archiveOf(t, lines[:2]...); return b }(),
			rec:   rec, says: "",
		},
		"out of order": {
			bytes: func() []byte { b, _ := archiveOf(t, lines[1], lines[0], lines[2]); return b }(),
			rec:   rec, says: "out of order",
		},
		"not gzip": {bytes: []byte("plain text\n"), rec: rec, says: "not a gzip file"},
		"not audit events": {
			bytes: func() []byte { b, _ := archiveOf(t, "plain text", "more", "text"); return b }(),
			rec:   rec, says: "line 1 is not an audit event",
		},
		"a different digest recorded": {
			bytes: good,
			rec:   func() audit.ArchiveRecord { r := rec; r.SHA256 = "00" + r.SHA256[2:]; return r }(),
			says:  "SHA-256",
		},
	} {
		t.Run(name, func(t *testing.T) {
			st := newMemStore()
			st.put(tc.rec.ObjectName, tc.bytes)
			err := audit.Verify(ctx, st, tc.rec)
			require.Error(t, err)
			require.Equal(t, errs.BackupIncomplete, errs.CodeOf(err))
			require.Contains(t, errs.As(err).Message, "2025-01")
			require.Contains(t, errs.As(err).Message, tc.says)
		})
	}
}

// TestR348_RetentionHasAFloorOfThreeMonths asserts the defaults and the
// floor as the archiver reads them: unset is three months and keep, and
// asking for less than the floor still keeps the floor.
func TestR348_RetentionHasAFloorOfThreeMonths(t *testing.T) {
	require.Equal(t, 3, audit.Retention{}.MonthsKept())
	require.Equal(t, audit.ArchiveKeep, audit.Retention{}.Mode())
	require.Equal(t, 3, audit.Retention{Months: 1}.MonthsKept(), "below the floor is the floor")
	require.Equal(t, 12, audit.Retention{Months: 12}.MonthsKept())

	for _, ok := range []audit.ArchiveMode{"", audit.ArchiveKeep, audit.ArchiveExport, audit.ArchiveOff} {
		require.NoError(t, ok.Valid())
	}
	require.ErrorContains(t, audit.ArchiveMode("delete").Valid(), "keep, export or off")
}
