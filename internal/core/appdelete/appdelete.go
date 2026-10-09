// Package appdelete deletes an app: settles its storage (R-204), archives it
// and starts its teardown.
//
// One path for a person's delete through the API and Pando's own deletion of
// an idle app (R-398), so host policy's require_backup_before_destroy (R-284)
// holds for both. It used to be handler code, which is how the setting came to
// be read nowhere.
package appdelete

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// Storage is what to do with an app's volumes.
type Storage string

const (
	// StorageUndecided: the caller has not said. Refused for an app with
	// volumes, so nobody discards data by leaving a parameter out.
	StorageUndecided Storage = ""
	// StorageBackUp keeps a final backup, until somebody discards it.
	StorageBackUp Storage = "backup"
	// StorageDiscard deletes the volumes with no backup.
	StorageDiscard Storage = "discard"
	// StoragePolicy lets host policy answer: back up when it requires a
	// backup, discard otherwise. Pando's own deletions, with nobody present
	// to be asked (R-398).
	StoragePolicy Storage = "policy"
)

// Request is one deletion.
type Request struct {
	Storage Storage

	// Actor is who is deleting, for the audit events: the principal fields
	// and request ID. Action and detail are filled in here.
	Actor audit.Event

	// Reason, when set, goes into the app.delete event's detail — "idle" for
	// the idle pass — so the log says why an app went with nobody's name on it.
	Reason string
}

// Result is what a deletion did.
type Result struct {
	VolumesDiscarded int
	// BackupID is the final backup holding the app's data, when one was kept.
	BackupID string
}

// Apps is the app store's part.
type Apps interface {
	VolumeCount(ctx context.Context, appID string) (int, error)
	RevisionByID(ctx context.Context, specID string) (state.Revision, bool, error)
	DiscardStorage(ctx context.Context, appID string) error
	Archive(ctx context.Context, appID string) error
}

// VolumeDeleter removes an app's volume rows once its storage is settled.
type VolumeDeleter interface {
	DeleteForApp(ctx context.Context, appID string) error
}

// Backups takes and records the final backup.
type Backups interface {
	NewID() string
	Record(ctx context.Context, rec state.Backup) error
}

// BackupCreator takes the backup itself.
type BackupCreator interface {
	CreateForApp(ctx context.Context, id string, req backup.AppCreateRequest) (backup.Created, error)
}

// VolumeResolver resolves an app's volumes to their runtime handles.
type VolumeResolver interface {
	VolumesForApp(ctx context.Context, appID string) ([]backup.VolumeRef, error)
}

// PolicyLoader reads host policy.
type PolicyLoader interface {
	Load(ctx context.Context) (policy.Document, error)
}

// Auditor writes audit events.
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Service deletes apps.
type Service struct {
	Apps    Apps
	Volumes VolumeDeleter

	// Backups, Backup and BundleSource take the final backup. Nil leaves
	// backups unavailable, and a delete that needs one is refused.
	Backups      Backups
	Backup       BackupCreator
	BundleSource VolumeResolver

	// Policy is host policy. Nil requires no backup.
	Policy PolicyLoader

	Auditor Auditor

	// TeardownNow asks for the deleted app's bundle to be torn down now
	// rather than at the next pass. Optional.
	TeardownNow func()
}

// Delete deletes app, settling its storage as req says.
//
// Refused, with nothing changed, when the app has volumes and req does not say
// what to do with them (STATE_BACKUP_DECISION_REQUIRED), when it says to
// discard them and host policy requires a backup (POLICY_BACKUP_REQUIRED), and
// when the backup fails: a failed backup means the data is not safe, so
// deleting is the one thing not to do.
func (s *Service) Delete(ctx context.Context, app state.App, req Request) (Result, error) {
	volumes, err := s.Apps.VolumeCount(ctx, app.ID)
	if err != nil {
		return Result{}, err
	}

	required, err := s.backupRequired(ctx)
	if err != nil {
		return Result{}, err
	}

	storage, err := decide(req.Storage, volumes, required)
	if err != nil {
		return Result{}, err
	}

	// Volumes are ON DELETE RESTRICT (R-204), so they are resolved explicitly
	// rather than cascading.
	var result Result
	if volumes > 0 {
		if result, err = s.settleStorage(ctx, app, req.Actor, storage, volumes, required); err != nil {
			return Result{}, err
		}
	}

	if err := s.Apps.Archive(ctx, app.ID); err != nil {
		return Result{}, err
	}
	if s.TeardownNow != nil {
		s.TeardownNow()
	}

	detail := map[string]any{
		"forced":            req.Storage == StorageDiscard,
		"volumes_discarded": volumes,
		// Which backup holds this app's data, if any. The question after a
		// deletion is always "can we get it back", and this is the answer.
		"backup_id": result.BackupID,
	}
	if req.Reason != "" {
		detail["reason"] = req.Reason
	}
	s.audit(ctx, req.Actor, "app.delete", app.ID, detail)
	return result, nil
}

// decide is what to do with an app's storage, or the refusal: host policy
// answers StoragePolicy, an app with volumes must be told what to do with
// them, and policy's backup rule refuses discarding them (R-204, R-284).
func decide(asked Storage, volumes int, required bool) (Storage, error) {
	storage := asked
	if storage == StoragePolicy {
		storage = StorageDiscard
		if required {
			storage = StorageBackUp
		}
	}
	if volumes == 0 {
		return storage, nil
	}
	switch {
	case storage == StorageUndecided:
		remedy := "Delete with backup=true to keep a copy, or force=true to delete without one. A backup made this way is kept until you discard it."
		if required {
			remedy = "Delete with backup=true. This installation keeps a final backup of every app with storage it deletes; the backup is kept until you discard it."
		}
		return "", errs.New(errs.StateBackupDecisionRequired,
			"This app has storage attached. Deleting it will remove that storage unless you back it up first.").
			WithRemedy(remedy).
			WithDetail("volume_count", volumes)
	case storage == StorageDiscard && required:
		return "", errs.New(errs.PolicyBackupRequired,
			"This installation keeps a final backup of every app with storage before deleting it, so this app cannot be deleted without one.").
			WithRemedy("Delete with backup=true. An administrator can change this with require_backup_before_destroy in host policy.").
			WithDetail("volume_count", volumes)
	}
	return storage, nil
}

// settleStorage backs up the app's storage when it is to be kept, then
// releases its volumes for the teardown to destroy. A failed backup stops
// here with nothing removed.
func (s *Service) settleStorage(ctx context.Context, app state.App, actor audit.Event, storage Storage, volumes int, required bool) (Result, error) {
	var result Result
	if storage == StorageBackUp {
		id, err := s.backUp(ctx, app, actor)
		if err != nil {
			s.audit(ctx, actor, "app.delete.backup_failed", app.ID, map[string]any{"reason": err.Error()})
			return Result{}, errs.Wrap(errs.StateInvalid,
				"Pando could not back up this app's storage, so it has not been deleted.", err).
				WithRemedy(backupFailedRemedy(required)).
				WithDetail("backup_error", err.Error())
		}
		result.BackupID = id
	}

	if err := s.Volumes.DeleteForApp(ctx, app.ID); err != nil {
		return Result{}, err
	}

	// The storage itself goes when the bundle is torn down. The delete has
	// settled it: discarded, or backed up first (R-204). It used to stay on
	// disk with no row left to reach it by (issue #55).
	if err := s.Apps.DiscardStorage(ctx, app.ID); err != nil {
		return Result{}, err
	}
	result.VolumesDiscarded = volumes
	return result, nil
}

func backupFailedRemedy(required bool) string {
	if required {
		return "Fix the problem and try again. This installation requires a backup before an app with storage is deleted."
	}
	return "Fix the problem and try again, or delete with force=true to remove the app and discard its storage."
}

func (s *Service) backupRequired(ctx context.Context) (bool, error) {
	if s.Policy == nil {
		return false, nil
	}
	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return false, err
	}
	return doc.RequireBackupBeforeDestroy, nil
}

// backUp takes the final copy R-204 promises.
//
// Kept until explicitly discarded, never aged out — which is the whole point of
// it, and why the row carries no retention.
func (s *Service) backUp(ctx context.Context, app state.App, actor audit.Event) (string, error) {
	if s.Backup == nil || s.Backups == nil || s.BundleSource == nil {
		return "", errs.New(errs.Internal, "Backups are not set up on this installation.")
	}

	volumes, err := s.BundleSource.VolumesForApp(ctx, app.ID)
	if err != nil {
		return "", err
	}

	var pinned json.RawMessage
	if app.PinnedSpecID != "" {
		rev, found, err := s.Apps.RevisionByID(ctx, app.PinnedSpecID)
		if err != nil {
			return "", err
		}
		if found && rev.Body != nil {
			// Marshalled here rather than stored raw: the bundle holds the spec
			// as it is served, so a person reading the backup by hand sees the
			// same document the API would have given them.
			encoded, err := json.Marshal(rev.Body)
			if err != nil {
				return "", errs.Wrap(errs.Internal, "Could not record the app's setup in the backup.", err)
			}
			pinned = encoded
		}
	}

	id := s.Backups.NewID()
	created, err := s.Backup.CreateForApp(ctx, id, backup.AppCreateRequest{
		AppID:   app.ID,
		Kind:    "on_delete",
		Spec:    pinned,
		Volumes: volumes,
	})
	if err != nil {
		return "", err
	}

	if err := s.Backups.Record(ctx, state.Backup{
		ID: id, AppID: app.ID, Kind: "on_delete",
		AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
		SizeBytes: created.SizeBytes, Manifest: created.Manifest,
		// RetainUntil deliberately nil: R-204 keeps this until somebody
		// discards it, and the schema refuses to let it carry an expiry.
		CreatedBy: actor.PrincipalID,
	}); err != nil {
		return "", err
	}

	e := actor
	e.TargetKind, e.TargetID = "backup", id
	s.write(ctx, e, "backup.create", app.ID, map[string]any{"kind": "on_delete", "volumes": len(volumes)})
	return id, nil
}

func (s *Service) audit(ctx context.Context, actor audit.Event, action, appID string, detail map[string]any) {
	e := actor
	e.TargetKind, e.TargetID = "app", appID
	s.write(ctx, e, action, appID, detail)
}

// write records an event, logging rather than failing the delete when the
// write does not land: an audit failure must not undo a deletion already
// made, but it must never pass silently either.
func (s *Service) write(ctx context.Context, e audit.Event, action, appID string, detail map[string]any) {
	if s.Auditor == nil {
		return
	}
	e.Action, e.AppID, e.Detail = action, appID, detail
	if err := s.Auditor.Write(ctx, e); err != nil {
		log.From(ctx).Error("audit write failed", zap.String("action", action), zap.Error(err))
	}
}
