package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// volumeRuntime keeps volumes in memory, by handle, and runs a database's
// dump and load against an in-memory database per workload. Methods an app
// backup never calls are the embedded interface's, and panic if reached.
type volumeRuntime struct {
	api.RuntimeAdapter
	volumes map[string][]byte

	// running is whether the app's workloads are running; observeErr is
	// what Observe fails with, when set.
	running    bool
	observeErr error

	// databases is each running database workload's contents; one missing is
	// not running. runErr is what Run fails with, when set.
	databases map[string][]byte
	runErr    error
}

func (r *volumeRuntime) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	if r.observeErr != nil {
		return api.ObservedBundle{}, r.observeErr
	}
	return api.ObservedBundle{Exists: true, Workloads: []api.ObservedWorkload{{Name: "web", Present: true, Running: r.running}}}, nil
}

func (r *volumeRuntime) Run(_ context.Context, ref api.WorkloadRef, req api.RunRequest) error {
	data, ok := r.databases[ref.Workload]
	if !ok {
		return errs.Newf(errs.NotFound, "There is nothing running called %q.", ref.Workload)
	}
	if r.runErr != nil {
		return r.runErr
	}
	switch req.Command[0] {
	case "dump":
		_, err := req.Stdout.Write(data)
		return err
	case "load":
		b, err := io.ReadAll(req.Stdin)
		r.databases[ref.Workload] = b
		return err
	}
	return errs.New(errs.Internal, "unknown command")
}

func (*volumeRuntime) Kind() string                                     { return "fake" }
func (*volumeRuntime) Category() api.Category                           { return api.CategoryRuntime }
func (*volumeRuntime) Configure(context.Context, json.RawMessage) error { return nil }
func (*volumeRuntime) HealthCheck(context.Context) error                { return nil }
func (r *volumeRuntime) SnapshotVolume(_ context.Context, h api.VolumeHandle, dst io.Writer) error {
	_, err := dst.Write(r.volumes[h.Handle])
	return err
}
func (r *volumeRuntime) RestoreVolume(_ context.Context, h api.VolumeHandle, src io.Reader) error {
	b, err := io.ReadAll(src)
	r.volumes[h.Handle] = b
	return err
}

// appBackups is a service with a local destination, an install key, and a
// runtime holding two volumes.
func appBackups(t *testing.T) (*Service, *volumeRuntime) {
	t.Helper()
	s, reg, _ := withDestination(t)
	rt := &volumeRuntime{volumes: map[string][]byte{
		"pando-app1-vol_a": []byte("uploads"),
		"pando-app1-vol_b": []byte("database"),
	}}
	require.NoError(t, reg.Register("rt_docker", rt))

	key := make([]byte, 32)
	_, err := rand.Read(key)
	require.NoError(t, err)
	s.SecretsKeyPath = filepath.Join(t.TempDir(), "secrets.key")
	require.NoError(t, os.WriteFile(s.SecretsKeyPath, key, 0o600))
	s.Version, s.SchemaVersion = "test", 1
	return s, rt
}

var appVolumes = []VolumeRef{
	{VolumeID: "vol_a", AdapterRef: "rt_docker", Handle: "pando-app1-vol_a"},
	{VolumeID: "vol_b", AdapterRef: "rt_docker", Handle: "pando-app1-vol_b"},
}

// TestR206_AnAppBackupRestoresInPlace asserts R-206 end to end: an app's data
// and spec go out, encrypted under the install's key, and come back into the
// same app's storage, replacing what was written since.
func TestR206_AnAppBackupRestoresInPlace(t *testing.T) {
	ctx := context.Background()
	s, rt := appBackups(t)

	created, err := s.CreateForApp(ctx, "bkp_1", AppCreateRequest{
		AppID: "app1", Kind: "rolling", Spec: json.RawMessage(`{"app_id":"app1"}`),
		Volumes: appVolumes, RetainFor: 7 * 24 * time.Hour,
	})
	require.NoError(t, err)
	require.Equal(t, "bk_local", created.AdapterRef)
	require.Positive(t, created.SizeBytes)
	require.NotNil(t, created.RetainUntil, "R-211: a rolling backup ages out")
	require.Equal(t, 2, created.Manifest.Counts["volumes"])

	v, err := s.VerifyApp(ctx, "bk_local", "bkp_1")
	require.NoError(t, err)
	require.Equal(t, created.Manifest.Entries, v.Manifest.Entries)

	rt.volumes["pando-app1-vol_a"] = []byte("overwritten by mistake")
	got, err := s.RestoreApp(ctx, AppRestoreRequest{
		AppID: "app1", AdapterRef: "bk_local", ObjectName: "bkp_1", Volumes: appVolumes, Confirm: true,
	})
	require.NoError(t, err)
	require.Equal(t, 2, got.VolumesApplied)
	require.Equal(t, "uploads", string(rt.volumes["pando-app1-vol_a"]))
	require.Equal(t, "database", string(rt.volumes["pando-app1-vol_b"]))
}

// TestR204_TheBackupTakenOnDeleteNeverAgesOut asserts R-204.
func TestR204_TheBackupTakenOnDeleteNeverAgesOut(t *testing.T) {
	s, _ := appBackups(t)
	created, err := s.CreateForApp(context.Background(), "bkp_final", AppCreateRequest{
		AppID: "app1", Kind: "on_delete", Volumes: appVolumes, RetainFor: time.Hour,
	})
	require.NoError(t, err)
	require.Nil(t, created.RetainUntil)
}

func TestRestoringNeedsConfirmationAndTouchesNothingWithout(t *testing.T) {
	s, rt := appBackups(t)
	_, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{AppID: "app1", Kind: "rolling", Volumes: appVolumes})
	require.NoError(t, err)
	rt.volumes["pando-app1-vol_a"] = []byte("newer")

	_, err = s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_local", ObjectName: "bkp_1", Volumes: appVolumes})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Remedy, "confirm")
	require.Equal(t, "newer", string(rt.volumes["pando-app1-vol_a"]))
}

func TestABackupFromAnotherKeySaysWhatThatMeans(t *testing.T) {
	s, _ := appBackups(t)
	_, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{AppID: "app1", Kind: "rolling", Volumes: appVolumes})
	require.NoError(t, err)

	other := make([]byte, 32)
	_, _ = rand.Read(other)
	require.NoError(t, os.WriteFile(s.SecretsKeyPath, other, 0o600))

	_, err = s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_local", ObjectName: "bkp_1", Volumes: appVolumes, Confirm: true})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.BackupDecryptFailed, e.Code)
	require.Contains(t, e.Message, "secrets key", "not a passphrase error the operator cannot act on")
}

func TestStorageTheAppNoLongerHasIsReportedNotDropped(t *testing.T) {
	s, _ := appBackups(t)
	_, err := s.CreateForApp(context.Background(), "bkp_1", AppCreateRequest{AppID: "app1", Kind: "rolling", Volumes: appVolumes})
	require.NoError(t, err)

	_, err = s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_local", ObjectName: "bkp_1",
		Volumes: appVolumes[:1], Confirm: true})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Equal(t, errs.StateInvalid, e.Code)
	require.Contains(t, e.Message, "vol_b")

	gone := []VolumeRef{appVolumes[0], {VolumeID: "vol_b", AdapterRef: "rt_gone", Handle: "h"}}
	_, err = s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_local", ObjectName: "bkp_1",
		Volumes: gone, Confirm: true})
	require.ErrorContains(t, err, "not configured")
}

func TestWithoutAnInstallKeyThereIsNoAppBackup(t *testing.T) {
	ctx := context.Background()
	s, _ := appBackups(t)

	path := s.SecretsKeyPath
	s.SecretsKeyPath = ""
	_, err := s.CreateForApp(ctx, "b", AppCreateRequest{})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Remedy, "whole installation")
	_, err = s.VerifyApp(ctx, "bk_local", "b")
	require.Error(t, err)
	_, err = s.RestoreApp(ctx, AppRestoreRequest{Confirm: true})
	require.Error(t, err)

	s.SecretsKeyPath = filepath.Join(t.TempDir(), "missing.key")
	_, err = s.CreateForApp(ctx, "b", AppCreateRequest{})
	require.ErrorContains(t, err, "could not read the key")

	require.NoError(t, os.WriteFile(path, nil, 0o600))
	s.SecretsKeyPath = path
	_, err = s.CreateForApp(ctx, "b", AppCreateRequest{})
	require.ErrorContains(t, err, "empty")
}

func TestAnAppBackupWithAVolumeOnNoRuntimeIsRefused(t *testing.T) {
	s, _ := appBackups(t)
	_, err := s.CreateForApp(context.Background(), "b", AppCreateRequest{AppID: "app1", Kind: "rolling",
		Volumes: []VolumeRef{{VolumeID: "vol_x", AdapterRef: "rt_gone", Handle: "h"}}})
	require.ErrorContains(t, err, "vol_x")

	_, err = s.CreateForApp(context.Background(), "b", AppCreateRequest{DestinationRef: "bk_gone"})
	require.Error(t, err)
}

func TestARestoreFromABackupThatIsNotThereFails(t *testing.T) {
	s, _ := appBackups(t)
	_, err := s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_local", ObjectName: "bkp_nothing", Confirm: true})
	require.Error(t, err)
	_, err = s.RestoreApp(context.Background(), AppRestoreRequest{AdapterRef: "bk_gone", ObjectName: "x", Confirm: true})
	require.Error(t, err)
}
