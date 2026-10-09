package appdelete_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/appdelete"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

type fakeApps struct {
	volumes   int
	discarded bool
	archived  bool
}

func (a *fakeApps) VolumeCount(context.Context, string) (int, error) { return a.volumes, nil }
func (a *fakeApps) RevisionByID(context.Context, string) (state.Revision, bool, error) {
	return state.Revision{}, false, nil
}
func (a *fakeApps) DiscardStorage(context.Context, string) error { a.discarded = true; return nil }
func (a *fakeApps) Archive(context.Context, string) error        { a.archived = true; return nil }

type fakeVolumes struct{ deleted bool }

func (v *fakeVolumes) DeleteForApp(context.Context, string) error { v.deleted = true; return nil }

type fakeBackups struct{ recorded []state.Backup }

func (b *fakeBackups) NewID() string { return "bak_1" }
func (b *fakeBackups) Record(_ context.Context, rec state.Backup) error {
	b.recorded = append(b.recorded, rec)
	return nil
}

type fakeBackup struct{ err error }

func (b fakeBackup) CreateForApp(context.Context, string, backup.AppCreateRequest) (backup.Created, error) {
	return backup.Created{}, b.err
}

type fakeBundles struct{}

func (fakeBundles) VolumesForApp(context.Context, string) ([]backup.VolumeRef, error) {
	return nil, nil
}

type fakePolicy struct{ doc policy.Document }

func (p fakePolicy) Load(context.Context) (policy.Document, error) { return p.doc, nil }

type auditLog struct{ events []audit.Event }

func (a *auditLog) Write(_ context.Context, e audit.Event) error {
	a.events = append(a.events, e)
	return nil
}

type rig struct {
	apps    *fakeApps
	volumes *fakeVolumes
	backups *fakeBackups
	audit   *auditLog
	svc     *appdelete.Service
}

func newRig(volumes int, requireBackup bool, backupErr error) *rig {
	r := &rig{apps: &fakeApps{volumes: volumes}, volumes: &fakeVolumes{}, backups: &fakeBackups{}, audit: &auditLog{}}
	r.svc = &appdelete.Service{
		Apps: r.apps, Volumes: r.volumes,
		Backups: r.backups, Backup: fakeBackup{err: backupErr}, BundleSource: fakeBundles{},
		Policy:  fakePolicy{policy.Document{RequireBackupBeforeDestroy: requireBackup}},
		Auditor: r.audit,
	}
	return r
}

var app = state.App{ID: "app_1", Name: "Expenses"}

// TestR284_PolicyRefusesADeleteWithoutABackup asserts R-284: with
// require_backup_before_destroy, discarding an app's storage is refused, and
// nothing is changed.
func TestR284_PolicyRefusesADeleteWithoutABackup(t *testing.T) {
	r := newRig(2, true, nil)

	_, err := r.svc.Delete(context.Background(), app, appdelete.Request{Storage: appdelete.StorageDiscard})
	require.Error(t, err)
	assert.Equal(t, errs.PolicyBackupRequired, errs.CodeOf(err))
	assert.False(t, r.apps.archived)
	assert.False(t, r.volumes.deleted)

	// Undecided is told the only answer that will do.
	_, err = r.svc.Delete(context.Background(), app, appdelete.Request{})
	var e *errs.Error
	require.ErrorAs(t, err, &e)
	assert.Equal(t, errs.StateBackupDecisionRequired, e.Code)
	assert.Contains(t, e.Remedy, "backup=true")
	assert.NotContains(t, e.Remedy, "force=true")

	// An app with no storage has nothing to back up.
	r = newRig(0, true, nil)
	_, err = r.svc.Delete(context.Background(), app, appdelete.Request{Storage: appdelete.StorageDiscard})
	require.NoError(t, err)
	assert.True(t, r.apps.archived)
}

// TestR398_PolicyAnswersForPandosOwnDelete asserts R-398: with nobody to
// ask, host policy decides — a backup kept when it requires one, the storage
// discarded when it does not.
func TestR398_PolicyAnswersForPandosOwnDelete(t *testing.T) {
	idle := appdelete.Request{
		Storage: appdelete.StoragePolicy, Reason: "idle",
		Actor: audit.Event{PrincipalKind: audit.KindSystem, PrincipalID: "idle"},
	}

	r := newRig(1, true, nil)
	result, err := r.svc.Delete(context.Background(), app, idle)
	require.NoError(t, err)
	assert.Equal(t, "bak_1", result.BackupID)
	require.Len(t, r.backups.recorded, 1)
	assert.Equal(t, "on_delete", r.backups.recorded[0].Kind)
	assert.Nil(t, r.backups.recorded[0].RetainUntil, "kept until discarded (R-204)")
	last := r.audit.events[len(r.audit.events)-1]
	assert.Equal(t, "app.delete", last.Action)
	assert.Equal(t, "idle", last.Detail["reason"])
	assert.Equal(t, "bak_1", last.Detail["backup_id"])

	r = newRig(1, false, nil)
	result, err = r.svc.Delete(context.Background(), app, idle)
	require.NoError(t, err)
	assert.Empty(t, result.BackupID)
	assert.Equal(t, 1, result.VolumesDiscarded)
	assert.True(t, r.apps.discarded)
}

// TestR204_AFailedBackupDeletesNothing asserts R-204 and R-398: when the
// backup fails, the app and its storage stay where they were.
func TestR204_AFailedBackupDeletesNothing(t *testing.T) {
	r := newRig(1, true, errors.New("the backup destination is full"))

	_, err := r.svc.Delete(context.Background(), app, appdelete.Request{Storage: appdelete.StoragePolicy})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not back up")
	assert.False(t, r.apps.archived)
	assert.False(t, r.volumes.deleted)
	assert.Equal(t, "app.delete.backup_failed", r.audit.events[0].Action)
}
