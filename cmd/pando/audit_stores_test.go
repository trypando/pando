package main

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	backuplocal "github.com/trypando/pando/internal/adapter/backup/local"
)

// TestR347_ArchivesGoWherePolicySays asserts R-347's two destinations: kept
// under Pando's own directory, or exported to a backup destination by name or
// to the default one — and that a destination that is not there is refused
// with what to do, rather than looked for elsewhere.
func TestR347_ArchivesGoWherePolicySays(t *testing.T) {
	ctx := context.Background()
	registry := adapterapi.NewRegistry()

	stores, err := auditArchiveStores(ctx, t.TempDir(), registry)
	require.NoError(t, err)

	w, err := stores.Kept.Writer(ctx, "audit-2025-01.jsonl.gz")
	require.NoError(t, err)
	_, err = io.WriteString(w, "kept")
	require.NoError(t, err)
	require.NoError(t, w.Close())
	r, err := stores.Kept.Reader(ctx, "audit-2025-01.jsonl.gz")
	require.NoError(t, err)
	got, err := io.ReadAll(r)
	require.NoError(t, err)
	require.NoError(t, r.Close())
	require.Equal(t, "kept", string(got))

	_, _, err = stores.Export("")
	require.ErrorContains(t, err, "has none")
	_, _, err = stores.Export("bk_absent")
	require.ErrorContains(t, err, "not running")

	dest := backuplocal.New()
	require.NoError(t, dest.Configure(ctx, []byte(`{"path":"`+t.TempDir()+`"}`)))
	require.NoError(t, registry.Register("bk_local", dest))
	require.NoError(t, registry.SetDefault(adapterapi.CategoryBackup, "bk_local"))
	_, ref, err := stores.Export("")
	require.NoError(t, err)
	require.Equal(t, "bk_local", ref, "empty is the default destination")
	_, ref, err = stores.Export("bk_local")
	require.NoError(t, err)
	require.Equal(t, "bk_local", ref)

	_, err = auditArchiveStores(ctx, "", registry)
	require.Error(t, err, "a kept store needs a directory")
}
