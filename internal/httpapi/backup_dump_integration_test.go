//go:build integration

package httpapi_test

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// dumpRuntime holds volumes in memory and one running database, which Run
// dumps and loads.
type dumpRuntime struct {
	adapterapi.RuntimeAdapter
	volumes  map[string][]byte
	database []byte
}

func (*dumpRuntime) Kind() string                                     { return "fake" }
func (*dumpRuntime) Category() adapterapi.Category                    { return adapterapi.CategoryRuntime }
func (*dumpRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (*dumpRuntime) HealthCheck(context.Context) error                { return nil }
func (r *dumpRuntime) SnapshotVolume(_ context.Context, h adapterapi.VolumeHandle, dst io.Writer) error {
	_, err := dst.Write(r.volumes[h.Handle])
	return err
}
func (r *dumpRuntime) RestoreVolume(_ context.Context, h adapterapi.VolumeHandle, src io.Reader) error {
	b, err := io.ReadAll(src)
	r.volumes[h.Handle] = b
	return err
}
func (*dumpRuntime) Observe(context.Context, adapterapi.BundleRef) (adapterapi.ObservedBundle, error) {
	return adapterapi.ObservedBundle{Exists: true, Workloads: []adapterapi.ObservedWorkload{{Name: "web", Running: true}}}, nil
}
func (r *dumpRuntime) Run(_ context.Context, ref adapterapi.WorkloadRef, req adapterapi.RunRequest) error {
	if ref.Workload != "svc-db" {
		return errs.New(errs.NotFound, "nothing running")
	}
	if req.Stdin != nil {
		b, err := io.ReadAll(req.Stdin)
		r.database = b
		return err
	}
	_, err := req.Stdout.Write(r.database)
	return err
}

type dumpServices struct{ adapterapi.ServicesAdapter }

func (dumpServices) Kind() string                                     { return "fake" }
func (dumpServices) Category() adapterapi.Category                    { return adapterapi.CategoryServices }
func (dumpServices) Configure(context.Context, json.RawMessage) error { return nil }
func (dumpServices) HealthCheck(context.Context) error                { return nil }
func (dumpServices) Dump(h adapterapi.ServiceHandle, _ spec.SlotType) (adapterapi.DumpPlan, bool) {
	return adapterapi.DumpPlan{Workload: h.Handle, Dump: []string{"dump"}, Load: []string{"load"}, Volumes: []string{"vol_db"}}, true
}

// TestR423_AnAppBackupTakenThroughTheAPIDumpsItsDatabase asserts R-423
// through the endpoints a person uses: a backup of a running app dumps its
// provisioned database and marks its other volume live, and restoring it
// loads the dump and says how many databases it loaded.
func TestR423_AnAppBackupTakenThroughTheAPIDumpsItsDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	admin := i.admin()
	id := i.appWithSpec(admin, "notes")

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	i.Server.Backup.SecretsKeyPath = filepath.Join(t.TempDir(), "secrets.key")
	require.NoError(t, os.WriteFile(i.Server.Backup.SecretsKeyPath, key, 0o600))

	rt := &dumpRuntime{volumes: map[string][]byte{"h-files": []byte("uploads")}, database: []byte("rows")}
	registry := i.Server.Backup.Registry
	require.NoError(t, registry.Register("rt_dump", rt))
	require.NoError(t, registry.Register("svc_dump", dumpServices{}))
	require.NoError(t, i.Volumes.RecordFromRuntime(ctx, id, "rt_dump", []state.VolumeRecord{
		{VolumeID: "vol_files", Name: "files", Handle: "h-files"},
		{VolumeID: "vol_db", Name: "db", Handle: "h-db"},
	}))
	require.NoError(t, state.NewServices(i.db).Record(ctx, state.ServiceInstance{
		ID: "svc_db", AppID: id, SlotKey: "db", SlotType: spec.SlotPostgres,
		AdapterRef: "svc_dump", Handle: "svc-db", ConnectionRef: "DATABASE_URL",
	}))

	got := i.do(admin, http.MethodPost, "/backups", map[string]any{"kind": "rolling", "app_id": id})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	var rec state.Backup
	got.JSON(t, &rec)
	require.Equal(t, []string{"vol_files"}, rec.Manifest.LiveVolumes())
	require.Equal(t, 1, rec.Manifest.Counts["services"])

	rt.database = []byte("dropped")
	got = i.do(admin, http.MethodPost, "/apps/"+id+"/restore", map[string]any{"backup_id": rec.ID, "confirm": true})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var restored struct {
		Volumes   int `json:"volumes"`
		Databases int `json:"databases"`
	}
	got.JSON(t, &restored)
	require.Equal(t, 1, restored.Databases)
	require.Equal(t, 1, restored.Volumes)
	require.Equal(t, "rows", string(rt.database))
}
