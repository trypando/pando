package backup

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// dumpingServices dumps a Postgres service whose data is in vol_b, and has no
// dump for anything else.
type dumpingServices struct{ api.ServicesAdapter }

func (dumpingServices) Kind() string                                     { return "fake" }
func (dumpingServices) Category() api.Category                           { return api.CategoryServices }
func (dumpingServices) Configure(context.Context, json.RawMessage) error { return nil }
func (dumpingServices) HealthCheck(context.Context) error                { return nil }
func (dumpingServices) Dump(h api.ServiceHandle, t spec.SlotType) (api.DumpPlan, bool) {
	if t != spec.SlotPostgres {
		return api.DumpPlan{}, false
	}
	return api.DumpPlan{Workload: h.Handle, Dump: []string{"dump"}, Load: []string{"load"}, Volumes: []string{"vol_b"}}, true
}

var appServices = []ServiceRef{{ServiceID: "svc_db", AppID: "app1", AdapterRef: "svc_docker", Handle: "svc-db", Type: spec.SlotPostgres}}

// dumpingBackups is appBackups with a running app whose Postgres is dumped.
func dumpingBackups(t *testing.T) (*Service, *volumeRuntime) {
	t.Helper()
	s, rt := appBackups(t)
	rt.running = true
	rt.databases = map[string][]byte{"svc-db": []byte("rows as of one transaction")}
	require.NoError(t, s.Registry.Register("svc_docker", dumpingServices{}))
	return s, rt
}

func entryNamed(m Manifest, name string) (Entry, bool) {
	for _, e := range m.Entries {
		if e.Name == name {
			return e, true
		}
	}
	return Entry{}, false
}

// TestR423_AProvisionedDatabaseIsDumpedRatherThanCopied asserts R-423: a
// running database is dumped by its engine and its volume left out, the other
// volume is copied and marked live, and a restore loads the dump back.
func TestR423_AProvisionedDatabaseIsDumpedRatherThanCopied(t *testing.T) {
	ctx := context.Background()
	s, rt := dumpingBackups(t)

	created, err := s.CreateForApp(ctx, "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: appServices,
	})
	require.NoError(t, err)

	dump, ok := entryNamed(created.Manifest, ServicesPrefix+"svc_db.dump")
	require.True(t, ok, "the database is dumped")
	require.False(t, dump.Live, "a dump is one transaction, never live")
	_, ok = entryNamed(created.Manifest, VolumesPrefix+"vol_b.tar")
	require.False(t, ok, "the database's files are not also copied")
	uploads, ok := entryNamed(created.Manifest, VolumesPrefix+"vol_a.tar")
	require.True(t, ok)
	require.True(t, uploads.Live, "copied while the app ran")
	require.Equal(t, []string{"vol_a"}, created.Manifest.LiveVolumes())
	require.Equal(t, 1, created.Manifest.Counts["volumes"])
	require.Equal(t, 1, created.Manifest.Counts["services"])

	rt.databases["svc-db"] = []byte("dropped by mistake")
	rt.volumes["pando-app1-vol_a"] = []byte("overwritten")
	got, err := s.RestoreApp(ctx, AppRestoreRequest{
		AppID: "app1", AdapterRef: "bk_local", ObjectName: "bkp_1",
		Volumes: appVolumes, Services: appServices, Confirm: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, got.ServicesApplied)
	require.Equal(t, 1, got.VolumesApplied)
	require.Equal(t, "rows as of one transaction", string(rt.databases["svc-db"]))
	require.Equal(t, "uploads", string(rt.volumes["pando-app1-vol_a"]))
}

// TestR423_AStoppedAppIsCopiedAndNotMarkedLive asserts R-423's other half: a
// database that is not running is copied with its volume, and nothing copied
// while nothing ran is marked live.
func TestR423_AStoppedAppIsCopiedAndNotMarkedLive(t *testing.T) {
	s, rt := dumpingBackups(t)
	rt.running = false
	delete(rt.databases, "svc-db")

	created, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: appServices,
	})
	require.NoError(t, err)
	_, ok := entryNamed(created.Manifest, ServicesPrefix+"svc_db.dump")
	require.False(t, ok)
	require.Equal(t, 2, created.Manifest.Counts["volumes"])
	require.Empty(t, created.Manifest.LiveVolumes())
}

// TestR423_AnAppWhoseStateCannotBeReadIsTakenToBeRunning: marking a copy that
// may be fine as live is the safe mistake.
func TestR423_AnAppWhoseStateCannotBeReadIsTakenToBeRunning(t *testing.T) {
	s, rt := appBackups(t)
	rt.observeErr = errors.New("daemon gone")
	created, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{AppID: "app1", Kind: "rolling", Volumes: appVolumes})
	require.NoError(t, err)
	require.Equal(t, []string{"vol_a", "vol_b"}, created.Manifest.LiveVolumes())
}

func TestR423_ADumpThatFailsFailsTheBackupAndSaysWhich(t *testing.T) {
	s, rt := dumpingBackups(t)
	rt.runErr = errs.New(errs.AdapterFailed, `A command Pando ran in svc-db exited with status 1 and said "pg_dump: error".`)
	_, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: appServices,
	})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "PostgreSQL database")
	require.Contains(t, e.Message, "pg_dump: error")
	require.NotEmpty(t, e.Remedy)
}

// A service with no dump, no provisioner, or storage the backup does not hold
// is copied with its volumes, as before R-423.
func TestR423_AServiceWithNoDumpIsCopiedWithItsVolume(t *testing.T) {
	s, _ := dumpingBackups(t)
	for name, svc := range map[string]ServiceRef{
		"no dump for the type": {ServiceID: "svc_r", AdapterRef: "svc_docker", Handle: "svc-db", Type: spec.SlotRedis},
		"no provisioner":       {ServiceID: "svc_db", AdapterRef: "svc_gone", Handle: "svc-db", Type: spec.SlotPostgres},
	} {
		created, err := s.CreateForApp(context.Background(), "bkp_"+name, AppCreateRequest{
			AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: []ServiceRef{svc},
		})
		require.NoError(t, err, name)
		require.Equal(t, 2, created.Manifest.Counts["volumes"], name)
	}
	created, err := s.CreateForApp(context.Background(), "bkp_x", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes[:1], Services: appServices,
	})
	require.NoError(t, err)
	require.Equal(t, 0, created.Manifest.Counts["services"], "its volume is not one the backup holds")
}

// TestR423_ARestoreThatCannotLoadTheDumpTouchesNothing: every dump is matched
// to its service before any volume is written.
func TestR423_ARestoreThatCannotLoadTheDumpTouchesNothing(t *testing.T) {
	ctx := context.Background()
	s, rt := dumpingBackups(t)
	_, err := s.CreateForApp(ctx, "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: appServices,
	})
	require.NoError(t, err)
	rt.volumes["pando-app1-vol_a"] = []byte("newer")

	restore := func(services []ServiceRef, volumes []VolumeRef) error {
		_, err := s.RestoreApp(ctx, AppRestoreRequest{
			AppID: "app1", AdapterRef: "bk_local", ObjectName: "bkp_1",
			Volumes: volumes, Services: services, Confirm: true,
		})
		return err
	}
	require.ErrorContains(t, restore(nil, appVolumes), "svc_db")
	gone := []ServiceRef{{ServiceID: "svc_db", AdapterRef: "svc_gone", Handle: "svc-db", Type: spec.SlotPostgres}}
	require.ErrorContains(t, restore(gone, appVolumes), "not configured")
	redis := []ServiceRef{{ServiceID: "svc_db", AdapterRef: "svc_docker", Handle: "svc-db", Type: spec.SlotRedis}}
	require.ErrorContains(t, restore(redis, appVolumes), "can no longer be loaded")
	require.ErrorContains(t, restore(appServices, appVolumes[:1]), "can no longer be loaded")
	elsewhere := []VolumeRef{appVolumes[0], {VolumeID: "vol_b", AdapterRef: "rt_gone", Handle: "h"}}
	require.ErrorContains(t, restore(appServices, elsewhere), "not configured")
	require.Equal(t, "newer", string(rt.volumes["pando-app1-vol_a"]))
}

func TestR423_LoadingADumpNeedsTheDatabaseRunning(t *testing.T) {
	ctx := context.Background()
	s, rt := dumpingBackups(t)
	_, err := s.CreateForApp(ctx, "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Volumes: appVolumes, Services: appServices,
	})
	require.NoError(t, err)

	delete(rt.databases, "svc-db")
	req := AppRestoreRequest{AppID: "app1", AdapterRef: "bk_local", ObjectName: "bkp_1",
		Volumes: appVolumes, Services: appServices, Confirm: true}
	_, err = s.RestoreApp(ctx, req)
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.StateInvalid, e.Code)
	require.Equal(t, "Start the app, then restore again.", e.Remedy)

	rt.databases["svc-db"] = nil
	rt.runErr = errs.New(errs.AdapterFailed, "pg_restore: error")
	_, err = s.RestoreApp(ctx, req)
	require.ErrorContains(t, err, "pg_restore: error")
}

func TestTheManifestNamesTheVolumesCopiedLive(t *testing.T) {
	m := Manifest{Entries: []Entry{
		{Name: VolumesPrefix + "app1/vol_a.tar", Live: true},
		{Name: VolumesPrefix + "vol_b.tar"},
		{Name: ServicesPrefix + "svc.dump"},
	}}
	require.Equal(t, []string{"vol_a"}, m.LiveVolumes())
}
