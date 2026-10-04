//go:build integration

package httpapi_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	backuplocal "github.com/trypando/pando/internal/adapter/backup/local"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state/statetest"
)

// TestR347_AnArchivedMonthIsListedAndDownloadedThroughTheAPI asserts issue
// #60's last acceptance condition: once a month is archived, the API lists it
// with its manifest and serves the archive itself, whose bytes match the
// digest the manifest and the response both carry. Behind install.audit.read,
// like the log.
func TestR347_AnArchivedMonthIsListedAndDownloadedThroughTheAPI(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	admin := i.admin()

	// Before anything is archived, or on an install wired without archives,
	// the list is empty rather than an error, and no ID finds anything.
	var none struct {
		Archives []audit.ArchiveRecord `json:"archives"`
	}
	empty := i.do(admin, http.MethodGet, "/audit/archives", nil)
	require.Equal(t, http.StatusOK, empty.Code, empty.String())
	empty.JSON(t, &none)
	require.NotNil(t, none.Archives)
	require.Empty(t, none.Archives)
	require.Equal(t, http.StatusNotFound, i.do(admin, http.MethodGet, "/audit/archives/aar_01HQ8ZZZZZZZZZZZZZZZZZZZZZ", nil).Code)

	owner, err := pgx.Connect(ctx, i.ownerURL)
	require.NoError(t, err)
	t.Cleanup(func() { _ = owner.Close(ctx) })
	for _, at := range []time.Time{
		time.Date(2025, 3, 2, 8, 0, 0, 0, time.UTC),
		time.Date(2025, 3, 9, 8, 0, 0, 0, time.UTC),
		time.Date(2025, 3, 30, 8, 0, 0, 0, time.UTC),
	} {
		_, err := owner.Exec(ctx, `INSERT INTO audit_events (occurred_at, principal_kind, principal_id, action)
			VALUES ($1, 'user', $2, 'app.deploy')`, at, i.AdminID)
		require.NoError(t, err)
	}

	kept := backuplocal.New()
	require.NoError(t, kept.Configure(ctx, json.RawMessage(`{"path":`+jsonString(t.TempDir())+`}`)))
	stores := audit.Stores{Kept: kept}
	_, err = (&audit.Archiver{
		Pool:      statetest.Archiver(t, i.ownerURL),
		Stores:    stores,
		Retention: func(context.Context) (audit.Retention, error) { return audit.Retention{}, nil },
		Clock:     clock.System{},
		Logger:    zap.NewNop(),
	}).Pass(ctx)
	require.NoError(t, err)
	i.Server.AuditArchives = &audit.Archives{Pool: i.db.Pool, Stores: stores}

	var listed struct {
		Archives []audit.ArchiveRecord `json:"archives"`
	}
	got := i.do(admin, http.MethodGet, "/audit/archives", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &listed)
	var march audit.ArchiveRecord
	for _, a := range listed.Archives {
		if a.Month == "2025-03" {
			march = a
		}
	}
	require.NotEmpty(t, march.ID, "March 2025 is listed: %s", got.String())
	require.EqualValues(t, 3, march.RowCount)

	// The live log no longer has them; the archive does.
	var events struct {
		Events []audit.Record `json:"events"`
	}
	i.do(admin, http.MethodGet, "/audit?until=2025-04-01T00:00:00Z", nil).JSON(t, &events)
	require.Empty(t, events.Events)

	download := i.do(admin, http.MethodGet, "/audit/archives/"+march.ID, nil)
	require.Equal(t, http.StatusOK, download.Code, download.String())
	require.Equal(t, "application/gzip", download.Hdr.Get("Content-Type"))
	require.Contains(t, download.Hdr.Get("Content-Disposition"), "audit-2025-03.jsonl.gz")
	sum := sha256.Sum256(download.Body)
	require.Equal(t, march.SHA256, hex.EncodeToString(sum[:]))
	require.Equal(t, "sha-256=:"+base64.StdEncoding.EncodeToString(sum[:])+":", download.Hdr.Get("Repr-Digest"))

	gz, err := gzip.NewReader(bytes.NewReader(download.Body))
	require.NoError(t, err)
	lines := 0
	for scan := bufio.NewScanner(gz); scan.Scan(); lines++ {
		var row map[string]any
		require.NoError(t, json.Unmarshal(scan.Bytes(), &row))
		require.Equal(t, "app.deploy", row["action"])
	}
	require.Equal(t, 3, lines)

	// Somebody without install.audit.read sees neither the list nor an archive.
	user := i.user("ordinary")
	require.Equal(t, http.StatusForbidden, i.do(user, http.MethodGet, "/audit/archives", nil).Code)
	require.Equal(t, http.StatusForbidden, i.do(user, http.MethodGet, "/audit/archives/"+march.ID, nil).Code)

	missing := i.do(admin, http.MethodGet, "/audit/archives/aar_01HQ8ZZZZZZZZZZZZZZZZZZZZZ", nil)
	require.Equal(t, http.StatusNotFound, missing.Code, missing.String())
	malformed := i.do(admin, http.MethodGet, "/audit/archives/not-an-archive", nil)
	require.Equal(t, http.StatusNotFound, malformed.Code, malformed.String())
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
