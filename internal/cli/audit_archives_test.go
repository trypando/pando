package cli_test

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// archiveBytes is a small archive and its SHA-256.
func archiveBytes(t *testing.T) (string, string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	_, err := gz.Write([]byte(`{"id":1,"action":"app.create"}` + "\n"))
	require.NoError(t, err)
	require.NoError(t, gz.Close())
	sum := sha256.Sum256(buf.Bytes())
	return buf.String(), hex.EncodeToString(sum[:])
}

func archivesAPI(t *testing.T, body, digest string) *fakeAPI {
	return newAPI(t).
		reply("GET /audit/archives", map[string]any{"archives": []map[string]any{
			{"id": "aar_01", "month": "2025-01", "row_count": 1, "size_bytes": len(body), "sha256": digest},
			{"id": "aar_02", "month": "2025-02", "adapter_ref": "bk_s3", "row_count": 1200, "size_bytes": 4 << 20, "sha256": digest},
		}}).
		reply("GET /audit/archives/aar_01", body)
}

// TestR347_TheCLIListsArchivedMonths asserts R-347 and R-261: the archives
// the API lists, each with where it is kept.
func TestR347_TheCLIListsArchivedMonths(t *testing.T) {
	body, digest := archiveBytes(t)
	got := run(t, archivesAPI(t, body, digest), "", "audit", "archives")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, got.out, "aar_01")
	require.Contains(t, got.out, "2025-02")
	require.Contains(t, got.out, "Pando", "an archive with no destination is kept by Pando")
	require.Contains(t, got.out, "bk_s3")
	require.Contains(t, got.out, "4.0 MB")

	empty := run(t, newAPI(t).reply("GET /audit/archives", map[string]any{"archives": []any{}}), "", "audit", "archives")
	require.NoError(t, empty.err)
	require.Contains(t, empty.out, "No months have been archived.")
}

// TestR347_TheCLIDownloadsAnArchiveAndChecksItsDigest asserts the download
// saves the archive under its month's name, and that bytes which do not match
// the manifest's digest are removed rather than kept.
func TestR347_TheCLIDownloadsAnArchiveAndChecksItsDigest(t *testing.T) {
	body, digest := archiveBytes(t)
	t.Chdir(t.TempDir())

	got := run(t, archivesAPI(t, body, digest), "", "audit", "archives", "download", "aar_01")
	require.NoError(t, got.err, got.errOut)
	saved, err := os.ReadFile("audit-2025-01.jsonl.gz")
	require.NoError(t, err)
	require.Equal(t, body, string(saved))
	require.Contains(t, got.errOut, "Saved audit-2025-01.jsonl.gz")

	// To standard output.
	got = run(t, archivesAPI(t, body, digest), "", "audit", "archives", "download", "aar_01", "-o", "-")
	require.NoError(t, got.err, got.errOut)
	require.Equal(t, body, got.out)

	// A file already there is not overwritten.
	got = run(t, archivesAPI(t, body, digest), "", "audit", "archives", "download", "aar_01")
	require.Error(t, got.err)

	// The wrong bytes are refused, and nothing is left behind.
	out := filepath.Join(t.TempDir(), "bad.gz")
	got = run(t, archivesAPI(t, "not the archive", digest), "", "audit", "archives", "download", "aar_01", "-o", out)
	require.ErrorContains(t, got.err, "does not match")
	require.NoFileExists(t, out)

	// An archive that is not listed.
	got = run(t, archivesAPI(t, body, digest), "", "audit", "archives", "download", "aar_99")
	require.ErrorContains(t, got.err, "no audit archive")

	// A server that refuses says so.
	refused := newAPI(t).fail("GET /audit/archives", 403, map[string]any{"message": "You need install.audit.read."})
	got = run(t, refused, "", "audit", "archives", "download", "aar_01")
	require.ErrorContains(t, got.err, "install.audit.read")
	got = run(t, refused, "", "audit", "archives")
	require.ErrorContains(t, got.err, "install.audit.read")
}
