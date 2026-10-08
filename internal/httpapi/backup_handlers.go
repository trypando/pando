package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Backup endpoints (design 04 §2.8, Sequence D).
//
// All four behind install.backup.manage. Listing is included rather than given
// install.view, because the list names what exists to be restored and when the
// install was last protected — which is reconnaissance for anyone deciding
// whether it is worth attacking.

func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallBackupManage); !ok {
		return
	}
	if s.Backups == nil {
		Error(w, r, errs.New(errs.Internal, "Backups are not set up on this installation."))
		return
	}

	page, err := pageFrom(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	appID := r.URL.Query().Get("app_id")
	rows, next, err := s.Backups.List(r.Context(), appID, page)
	if err != nil {
		Error(w, r, err)
		return
	}

	// The last scheduled attempt for each app, taken or not (issue #87). The
	// list above says what exists; this says what was tried and did not
	// happen, which the list cannot — an app missing from it looks the same
	// whether it was never due or failed every hour for a week.
	//
	// The newest of them, a page's worth, with the first page only: one per
	// app is as many rows as apps (issue #72).
	body := map[string]any{"backups": rows, "next_cursor": next}
	if page.Cursor == "" {
		attempts, err := s.Backups.Attempts(r.Context(), appID, page.Size())
		if err != nil {
			Error(w, r, err)
			return
		}
		body["attempts"] = attempts
	}
	JSON(w, http.StatusOK, body)
}

type createBackupRequest struct {
	// Kind is "dr_bundle" (the default) or "rolling" (design 04 §2.8).
	//
	// Two very different objects behind one endpoint, and the difference is
	// scope, not size: a DR bundle is the whole installation and is encrypted
	// under a passphrase Pando never keeps (R-213), while a rolling backup is
	// one app's data and is encrypted under the install's own secrets key.
	// R-213 governs the bundle that has to survive the machine; an app backup
	// that needed a typed passphrase would be an app backup nobody schedules.
	Kind string `json:"kind"`

	// AppID is required for a rolling backup and meaningless for a DR bundle.
	AppID string `json:"app_id"`

	// Passphrase is never stored (R-213). Losing it makes the bundle unusable
	// (R-214) — the console says so at the point of creation, which is the only
	// place saying it does any good.
	Passphrase string `json:"passphrase"`

	DestinationRef string `json:"destination_ref"`

	// RetainDays of 0 means keep until explicitly discarded.
	RetainDays int `json:"retain_days"`
}

// handleCreateBackup takes a full-host DR bundle (R-212).
//
// Synchronous, and deliberately: a backup that returns 202 and fails in the
// background is a backup an operator believes they have. The cost is a long
// request, which is the right cost for this — nobody takes a DR bundle in a
// loop.
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	if s.Backups == nil || s.Backup == nil {
		Error(w, r, errs.New(errs.Internal, "Backups are not set up on this installation."))
		return
	}

	var req createBackupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	// A rolling backup is one app's data and is authorized on that app.
	//
	// The same verb as restoring it: taking a copy and putting it back are two
	// halves of one operation, and an owner who may do the destructive half
	// should not need an administrator for the safe one. An administrator holds
	// no app verb (R-087), so this is not install.backup.manage with a filter —
	// it is a different question.
	if req.Kind == "rolling" {
		s.createRollingBackup(w, r, req)
		return
	}

	p, ok := s.requireInstall(w, r, authz.InstallBackupManage)
	if !ok {
		return
	}
	if req.Kind != "" && req.Kind != "dr_bundle" {
		Error(w, r, errs.Newf(errs.ValidInvalid, "There is no backup kind called %q.", req.Kind).
			WithRemedy(`Use "dr_bundle" for the whole installation, or "rolling" for one app's data.`))
		return
	}
	if len(req.Passphrase) < minPassphraseLength {
		Error(w, r, errs.Newf(errs.ValidInvalid,
			"A backup passphrase needs at least %d characters.", minPassphraseLength).
			WithRemedy("Pando never stores this passphrase, so choose something you can find again after the machine is gone."))
		return
	}

	id := s.Backups.NewID()
	created, err := s.Backup.Create(r.Context(), id, backup.CreateRequest{
		Passphrase:     secret.New(req.Passphrase),
		DestinationRef: req.DestinationRef,
		RetainFor:      time.Duration(req.RetainDays) * 24 * time.Hour,
	})
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "backup.failed", TargetKind: "backup", TargetID: id,
		})
		Error(w, r, err)
		return
	}

	// Recorded only once the object is written. A row written first would claim
	// a bundle exists during the window where it does not.
	rec := state.Backup{
		ID: id, Kind: "dr_bundle", AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
		SizeBytes: created.SizeBytes, Manifest: created.Manifest,
		RetainUntil: created.RetainUntil, CreatedBy: p.ID,
	}
	if err := s.Backups.Record(r.Context(), rec); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.create", TargetKind: "backup", TargetID: id,
		Detail: map[string]any{
			"destination": created.AdapterRef,
			"size_bytes":  created.SizeBytes,
			"counts":      created.Manifest.Counts,
		},
	})
	JSON(w, http.StatusCreated, rec)
}

// createRollingBackup takes one app's data, on demand (R-206, R-210).
//
// The same object the reconciler takes on a schedule (R-211), so "restore from
// this morning's" and "restore from the one I took before the migration" are
// the same path and not two. A recovery path that only exists on a schedule is
// one nobody can reach at the moment they need it.
func (s *Server) createRollingBackup(w http.ResponseWriter, r *http.Request, req createBackupRequest) {
	if req.AppID == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "A rolling backup needs to know which app it is for.").
			WithRemedy(`Send app_id, or use kind "dr_bundle" to back up the whole installation.`))
		return
	}

	app, ok := s.requireControlOn(w, r, req.AppID, authz.AppDeploy)
	if !ok {
		return
	}
	if s.BundleSource == nil || s.Apps == nil {
		Error(w, r, errs.New(errs.Internal, "Backups are not set up on this installation."))
		return
	}

	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app has never been deployed, so there is nothing to back up.").
			WithRemedy("Deploy it first."))
		return
	}
	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}

	// The spec as stored, not as re-marshalled. The bundle keeps the exact
	// bytes the app was deployed from, so a restore has the configuration and
	// not Pando's later idea of it.
	appSpec, err := json.Marshal(rev.Body)
	if err != nil {
		Error(w, r, errs.Wrap(errs.Internal, "Could not read this app's configuration.", err))
		return
	}

	volumes, err := s.BundleSource.VolumesForApp(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if len(volumes) == 0 {
		// Not an empty backup. An app with no storage has nothing a copy would
		// hold that its spec revisions do not, and a bundle that restores
		// nothing is worse than a refusal: it is a recovery somebody believes
		// in (R-105).
		Error(w, r, errs.New(errs.StateInvalid, "This app keeps no data, so there is nothing to back up.").
			WithRemedy("Its configuration is already kept in full, and every deploy can be rolled back to an earlier revision."))
		return
	}

	p := PrincipalFrom(r.Context())
	id := s.Backups.NewID()
	created, err := s.Backup.CreateForApp(r.Context(), id, backup.AppCreateRequest{
		AppID: app.ID, Kind: "rolling", Spec: appSpec, Volumes: volumes,
		DestinationRef: req.DestinationRef,
		RetainFor:      time.Duration(req.RetainDays) * 24 * time.Hour,
	})
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "backup.failed", AppID: app.ID, TargetKind: "backup", TargetID: id,
		})
		Error(w, r, err)
		return
	}

	rec := state.Backup{
		ID: id, AppID: app.ID, Kind: "rolling",
		AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
		SizeBytes: created.SizeBytes, Manifest: created.Manifest,
		RetainUntil: created.RetainUntil, CreatedBy: p.ID,
	}
	if err := s.Backups.Record(r.Context(), rec); err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.create", AppID: app.ID, TargetKind: "backup", TargetID: id,
		Detail: map[string]any{"kind": "rolling", "destination": created.AdapterRef, "size_bytes": created.SizeBytes},
	})
	JSON(w, http.StatusCreated, rec)
}

// handleVerifyBackup checks a bundle without applying it (R-216).
//
// Its own route rather than a flag on restore. A flag is a thing somebody
// passes wrongly, and the wrong value here replaces an installation.
func (s *Server) handleVerifyBackup(w http.ResponseWriter, r *http.Request) {
	p, rec, ok := s.requireBackup(w, r)
	if !ok {
		return
	}

	var req struct {
		Passphrase string `json:"passphrase"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	// An app backup is opened with the install's own key, a DR bundle with the
	// passphrase (R-213).
	//
	// Chosen from the record rather than from whether a passphrase was sent: a
	// caller who supplies one for a rolling backup is confused, and the useful
	// answer is the verification, not a lecture. Getting this wrong is how
	// R-216 stops being reachable for the backups people actually take — a
	// rolling backup verified against a passphrase fails with
	// BACKUP_DECRYPT_FAILED, which says the passphrase is wrong about a bundle
	// that has none.
	var (
		v   backup.Verified
		err error
	)
	if rec.AppID != "" {
		v, err = s.Backup.VerifyApp(r.Context(), rec.AdapterRef, rec.ObjectName)
	} else {
		v, err = s.Backup.Verify(r.Context(), rec.AdapterRef, rec.ObjectName, secret.New(req.Passphrase))
	}
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "backup.verify.failed", TargetKind: "backup", TargetID: rec.ID,
			Detail: map[string]any{"code": string(errs.CodeOf(err))},
		})
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.verify", TargetKind: "backup", TargetID: rec.ID,
	})
	JSON(w, http.StatusOK, map[string]any{
		"verified": true,
		"manifest": v.Manifest,
	})
}

// handleRestoreBackup replaces this installation from a bundle.
//
// The most destructive action the API has. Verification happens first and the
// target is untouched if it fails (R-215); confirmation is explicit; and the
// audit event is written *before* the restore, because a restore that
// half-succeeds must still be recorded as attempted — the same ordering exec
// uses for the same reason.
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	p, rec, ok := s.requireBackup(w, r)
	if !ok {
		return
	}

	// An app's backup is not an installation.
	//
	// It would fail anyway — a rolling backup is sealed with the install key
	// and this path opens with a passphrase — but it would fail as
	// BACKUP_DECRYPT_FAILED, which tells someone their passphrase is wrong
	// about a bundle that never had one. On the most destructive route in the
	// API that is the wrong sentence to answer with, and it sends them looking
	// for a passphrase instead of for the right endpoint (R-105).
	if rec.AppID != "" {
		Error(w, r, errs.New(errs.ValidInvalid,
			"That backup holds one app's data, not this installation.").
			WithDetail("app_id", rec.AppID).
			WithRemedy("Restore it from that app instead. This route replaces the whole installation from a full backup."))
		return
	}

	var req struct {
		Passphrase string `json:"passphrase"`
		Confirm    bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.restore.start", TargetKind: "backup", TargetID: rec.ID,
	})

	result, err := s.Backup.Restore(r.Context(), backup.RestoreRequest{
		AdapterRef: rec.AdapterRef, ObjectName: rec.ObjectName,
		Passphrase: secret.New(req.Passphrase), Confirm: req.Confirm,
	})
	if err != nil {
		s.audit(r, audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "backup.restore.refused", TargetKind: "backup", TargetID: rec.ID,
			Detail: map[string]any{"code": string(errs.CodeOf(err))},
		})
		Error(w, r, err)
		return
	}

	// Put the bundle's own row back.
	//
	// It was not there when the bundle was taken — a backup cannot contain the
	// record of itself — so the restore just erased it, and without this the
	// bundle an operator is standing on becomes unlistable and unrestorable the
	// moment they use it once. Found by restoring twice.
	if err := s.Backups.Record(r.Context(), rec); err != nil {
		// Reported, not fatal. The install is restored; this is bookkeeping,
		// and failing the request here would say the restore failed when it
		// did not.
		s.Logger.Warn("restored install has no record of the bundle it came from",
			zap.String("backup_id", rec.ID), zap.Error(err))
	}

	// This event lands in the restored database, which is the point: the
	// restore is part of the history of the install it produced.
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.restore", TargetKind: "backup", TargetID: rec.ID,
		Detail: map[string]any{"volumes": result.VolumesApplied},
	})
	JSON(w, http.StatusOK, map[string]any{
		"restored": true,
		"volumes":  result.VolumesApplied,
		"manifest": result.Manifest,
	})
}

// handleRestoreAppBackup puts one app's data back (R-206, R-210).
//
// Distinct from restoring a DR bundle, which replaces the installation. This
// replaces one app's data, in place, and is gated on the app rather than the
// install: it is an app operation, so an app's owner can do it without holding
// install.backup.manage.
func (s *Server) handleRestoreAppBackup(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppDeploy)
	if !ok {
		return
	}
	if s.Backups == nil || s.Backup == nil || s.BundleSource == nil {
		Error(w, r, errs.New(errs.Internal, "Backups are not set up on this installation."))
		return
	}

	var req struct {
		BackupID string `json:"backup_id"`
		Confirm  bool   `json:"confirm"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	rec, found, err := s.Backups.ByID(r.Context(), req.BackupID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no backup with that ID."))
		return
	}
	if rec.AppID != app.ID {
		// R-206: in place only, matched by Pando's own identity for the app.
		// Restoring one app's data into another is a promise Pando cannot keep
		// — it cannot know what is inside a volume — so it is refused rather
		// than attempted.
		Error(w, r, errs.New(errs.ValidInvalid, "That backup belongs to a different app.").
			WithRemedy("A backup restores only to the app it came from."))
		return
	}

	volumes, err := s.BundleSource.VolumesForApp(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.restore.start", AppID: app.ID, TargetKind: "backup", TargetID: rec.ID,
	})

	result, err := s.Backup.RestoreApp(r.Context(), backup.AppRestoreRequest{
		AppID: app.ID, AdapterRef: rec.AdapterRef, ObjectName: rec.ObjectName,
		Volumes: volumes, Confirm: req.Confirm,
	})
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "app.restore", AppID: app.ID, TargetKind: "backup", TargetID: rec.ID,
		Detail: map[string]any{"volumes": result.VolumesApplied},
	})
	JSON(w, http.StatusOK, map[string]any{
		"restored": true, "volumes": result.VolumesApplied,
		"note": "Restart the app so it reads the restored data.",
	})
}

// requireBackup resolves the backup and checks the verb.
func (s *Server) requireBackup(w http.ResponseWriter, r *http.Request) (authz.Principal, state.Backup, bool) {
	p, ok := s.requireInstall(w, r, authz.InstallBackupManage)
	if !ok {
		return p, state.Backup{}, false
	}
	if s.Backups == nil || s.Backup == nil {
		Error(w, r, errs.New(errs.Internal, "Backups are not set up on this installation."))
		return p, state.Backup{}, false
	}

	rec, found, err := s.Backups.ByID(r.Context(), chi.URLParam(r, "backupID"))
	if err != nil {
		Error(w, r, err)
		return p, state.Backup{}, false
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no backup with that ID."))
		return p, state.Backup{}, false
	}
	return p, rec, true
}

// minPassphraseLength is backup.MinPassphraseLength, which says why.
const minPassphraseLength = backup.MinPassphraseLength
