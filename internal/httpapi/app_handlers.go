package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// requireControl resolves the app and checks a control-plane verb.
//
// Resolution comes first so a caller without access cannot tell a missing app
// from one they may not see — both return the same not-found.
func (s *Server) requireControl(w http.ResponseWriter, r *http.Request, verb authz.Verb) (state.App, bool) {
	return s.requireControlOn(w, r, chi.URLParam(r, "appID"), verb)
}

// requireControlOn is requireControl for an app named in the body rather than
// the path.
//
// One endpoint takes it that way — POST /backups with kind "rolling", where the
// app is a field because design 04 puts both backup kinds on one route. Sharing
// the check rather than writing a second one is the point: this is where the
// not-found-rather-than-forbidden rule lives, and a second copy would be the
// one that eventually forgets it.
func (s *Server) requireControlOn(w http.ResponseWriter, r *http.Request, appID string, verb authz.Verb) (state.App, bool) {
	if !id.Is(id.App, appID) {
		Error(w, r, errs.New(errs.NotFound, "There is no app with that ID."))
		return state.App{}, false
	}

	app, found, err := s.Apps.ByID(r.Context(), appID)
	if err != nil {
		Error(w, r, err)
		return state.App{}, false
	}

	p := PrincipalFrom(r.Context())
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no app with that ID."))
		return state.App{}, false
	}
	if err := s.Authz.CheckControl(r.Context(), p, appID, verb); err != nil {
		if errs.CodeOf(err) == errs.PermDenied || errs.CodeOf(err) == errs.PermVerbRequired {
			// Not found rather than forbidden when the caller cannot even view
			// the app: confirming existence is itself a disclosure.
			if viewErr := s.Authz.CheckControl(r.Context(), p, appID, authz.AppView); viewErr != nil {
				Error(w, r, errs.New(errs.NotFound, "There is no app with that ID."))
				return state.App{}, false
			}
		}
		Error(w, r, err)
		return state.App{}, false
	}
	return app, true
}

type createAppRequest struct {
	Name   string `json:"name"`
	Source struct {
		Type   string `json:"type"`
		URL    string `json:"url"`
		Ref    string `json:"ref"`
		Image  string `json:"image"`
		Subdir string `json:"subdir"`

		// Credential pulls a private image (issue #41). Stored before
		// detection starts, so the first read of the registry already uses it.
		Credential *oci.Credential `json:"credential"`
	} `json:"source"`
}

var slugPattern = regexp.MustCompile(`[^a-z0-9-]+`)

// validSourceType refuses a source type Pando cannot fetch, at creation
// rather than at the first detection. Empty is an app written by hand.
func validSourceType(t spec.SourceType) error {
	switch t {
	case "", spec.SourceGit, spec.SourceImage, spec.SourceUpload:
		return nil
	}
	return errs.Newf(errs.ValidInvalid,
		"%q is not a kind of source Pando knows. Valid answers: git (a repository URL), image (a prebuilt image), or upload (files sent from your computer).", t)
}

// handleCreateApp starts Sequence A.
//
// app.create is install-scoped: there is no app yet to hold a verb on, which
// is exactly why it cannot go through requireControl. Sequence A step 1 has
// always called this an install-level check.
func (s *Server) handleCreateApp(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireInstall(w, r, authz.AppCreate)
	if !ok {
		return
	}

	var req createAppRequest
	idempotencyKey, err := decodeWithKey(r, &req)
	if err != nil {
		Error(w, r, err)
		return
	}

	// Creating an app is infrastructure-creating, so a retry must not produce
	// two apps with the same name — which would fail the second time on the
	// unique constraint and look to an agent like the first attempt failed.
	if s.replayed(w, r, idempotencyKey, "POST /apps") {
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "An app needs a name.").
			WithRemedy("Give the app a short name, for example \"team-notes\"."))
		return
	}

	src := spec.Source{
		Type:   spec.SourceType(req.Source.Type),
		URL:    req.Source.URL,
		Ref:    req.Source.Ref,
		Image:  req.Source.Image,
		Subdir: req.Source.Subdir,
	}
	if err := validSourceType(src.Type); err != nil {
		Error(w, r, err)
		return
	}

	// A credential is checked before the app exists, so a bad one is a
	// refused request rather than an app created without it.
	if req.Source.Credential != nil {
		if src.Type != spec.SourceImage {
			Error(w, r, errs.New(errs.ValidInvalid,
				"A registry credential is for an app that runs a prebuilt image, and this app is built from source.").
				WithRemedy("Leave the credential out, or create the app from an image."))
			return
		}
		if err := req.Source.Credential.Validate(); err != nil {
			Error(w, r, err)
			return
		}
		src.CredentialRef = registryCredentialRef
	}

	// The source allowlist (R-092) is evaluated before anything touches disk:
	// the clone, the pull, or the upload that follows creation. Given the whole
	// source, not its URL — an image or an upload has none, and an empty URL
	// used to be read as nothing to check (issue #41).
	if s.Policy != nil {
		if err := s.Policy.AllowsSource(r.Context(), src); err != nil {
			s.audit(r, audit.Event{
				PrincipalKind: audit.PrincipalKind(p.Kind),
				PrincipalID:   p.ID,
				OnBehalfOf:    p.UserID,
				Action:        "app.create.denied",
				Detail:        map[string]any{"source_type": req.Source.Type, "source_url": req.Source.URL, "source_image": req.Source.Image},
			})
			Error(w, r, err)
			return
		}
	}

	app, err := s.Apps.Create(r.Context(), req.Name, slugify(req.Name), p.UserID, p.ID, src)
	if err != nil {
		Error(w, r, err)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "app.create",
		AppID:         app.ID,
		TargetKind:    "app",
		TargetID:      app.ID,
	})

	if req.Source.Credential != nil {
		if !s.saveRegistryCredential(w, r, app.ID, app.Source, *req.Source.Credential) {
			return
		}
	}

	// Sequence A step 5: enqueue detection.
	//
	// Queued, not run in this request: cloning a repository and trial-running
	// it takes longer than any reasonable HTTP timeout, and the detection
	// queue runs it on whichever replica has room (issue #72, O-32). The
	// result is written to the detections table either way, which is what GET
	// /detection reads.
	// Not for an upload that has not arrived yet: `pando deploy ./` creates the
	// app first and sends the directory second, so there is nothing to look at
	// here and detecting now produces a failure that is purely an artifact of
	// the ordering. The CLI re-runs detection explicitly once the source is up,
	// which is R-022's explicit re-detection doing exactly what it is for.
	pending := app.Source.Type == spec.SourceUpload && app.Source.UploadID == ""
	if s.Detector != nil && s.DetectionQueue != nil && app.Source.Type != "" && !pending {
		if err := s.DetectionQueue.Enqueue(r.Context(), app.ID); err != nil {
			// The app exists; a detection that could not be queued is one
			// the person can start again from the app's page.
			s.Logger.Warn("could not queue detection", zap.String("app_id", app.ID), zap.Error(err))
		}
	}

	s.remember(r, idempotencyKey, "POST /apps", http.StatusAccepted, app)

	// 202, not 201: the app exists but is in draft. Detection has been queued,
	// and nothing is deployed.
	JSON(w, http.StatusAccepted, app)
}

func (s *Server) handleListApps(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	every, err := s.seesEveryApp(r, p)
	if err != nil {
		Error(w, r, err)
		return
	}
	page, err := pageFrom(r)
	if err != nil {
		Error(w, r, err)
		return
	}
	var (
		apps  []state.App
		next  string
		total int
	)
	if every {
		apps, next, total, err = s.Apps.ListAllPage(r.Context(), page)
	} else {
		apps, next, total, err = s.Apps.ListForPrincipalPage(r.Context(), p, page)
	}
	if err != nil {
		Error(w, r, err)
		return
	}
	if apps, err = s.withDetections(r.Context(), apps); err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{
		"apps":        s.withVerdicts(r.Context(), withAddresses(r, apps)),
		"next_cursor": next,
		"total":       total,
	})
}

// withDetections fills in where each app's detection has got to, so a draft
// in the list and on its own page says why it is still a draft — detection
// running and at which stage, or waiting on answers — instead of only that it
// is one (issue #80). One query for the whole list.
func (s *Server) withDetections(ctx context.Context, apps []state.App) ([]state.App, error) {
	if s.Detections == nil || len(apps) == 0 {
		return apps, nil
	}
	ids := make([]string, len(apps))
	for i := range apps {
		ids[i] = apps[i].ID
	}
	summaries, err := s.Detections.Summaries(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range apps {
		if d, ok := summaries[apps[i].ID]; ok {
			apps[i].Detection = &d
		}
	}
	return apps, nil
}

// seesEveryApp reports whether the caller's install role reaches every app
// with app.view (install.apps.view, R-081), so the list shows them all — the
// same answer CheckControl gives for app.view on each. Holding another
// install.apps.* verb without it is not seeing every app: there is no
// implication graph (R-082), and app.delete does not imply app.view.
func (s *Server) seesEveryApp(r *http.Request, p authz.Principal) (bool, error) {
	if s.Verbs == nil || p.Kind == authz.KindAnonymous {
		return false, nil
	}
	held, err := s.Verbs.InstallVerbsFor(r.Context(), p)
	if err != nil {
		return false, err
	}
	for _, v := range held {
		if v == string(authz.InstallAppsView) {
			return true, nil
		}
	}
	return false, nil
}

// withAddresses fills in where each app is reached.
//
// Here rather than in the state store because a port-mode address needs the
// host the caller used, which only a request knows (R-261 — the API answers
// "where is this app", and no client re-derives it).
func withAddresses(r *http.Request, apps []state.App) []state.App {
	for i := range apps {
		apps[i].Address = spec.Address(r.Host, apps[i].Slug, apps[i].Routing)
	}
	return apps
}

// handleMyApps returns the launcher tiles (R-264).
//
// A different list from handleListApps, deliberately: that one is control-plane
// scoped, this one is data-plane. Two planes, two endpoints.
func (s *Server) handleMyApps(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous {
		Error(w, r, errs.New(errs.AuthRequired, "You need to sign in."))
		return
	}
	apps, err := s.Apps.ListForUse(r.Context(), p)
	if err != nil {
		Error(w, r, err)
		return
	}

	// The person's own launcher sections (R-342), in the same response as the
	// apps filed under them, so the two cannot disagree. A service token has
	// no person and so no sections.
	sections := []state.Section{}
	if p.UserID != "" {
		if sections, err = s.Apps.Sections(r.Context(), p.UserID); err != nil {
			Error(w, r, err)
			return
		}
	}
	JSON(w, http.StatusOK, map[string]any{"apps": withAddresses(r, apps), "sections": sections})
}

func (s *Server) handleGetApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	app.Address = spec.Address(r.Host, app.Slug, app.Routing)
	withDetection, err := s.withDetections(r.Context(), []state.App{app})
	if err != nil {
		Error(w, r, err)
		return
	}
	app = withDetection[0]

	// What the caller may do on this app, by the authorizer's own answer
	// verb by verb, so the console shows what it cannot do as read-only
	// rather than as a control that fails when used (R-261).
	verbs, err := s.Authz.AppVerbs(r.Context(), PrincipalFrom(r.Context()), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	// The app's last scheduled backup and what came of it (R-211, issue #87).
	// On the app, for its owner: a backup that did not happen is the app's
	// problem, and the Backups screen is only for whoever manages the install.
	var lastBackup *state.BackupAttempt
	if s.Backups != nil {
		attempts, err := s.Backups.Attempts(r.Context(), app.ID)
		if err != nil {
			Error(w, r, err)
			return
		}
		if len(attempts) > 0 {
			lastBackup = &attempts[0]
		}
	}

	JSON(w, http.StatusOK, struct {
		state.App
		Verbs      []authz.Verb         `json:"verbs"`
		LastBackup *state.BackupAttempt `json:"last_backup,omitempty"`
	}{app, verbs, lastBackup})
}

func (s *Server) handlePatchApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}

	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "An app needs a name."))
		return
	}

	if err := s.Apps.Rename(r.Context(), app.ID, req.Name); err != nil {
		Error(w, r, err)
		return
	}
	s.auditApp(r, app.ID, "app.update")

	updated, _, err := s.Apps.ByID(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, updated)
}

// handleDeleteApp implements R-204/R-205.
//
// The 409-on-ambiguity shape is what lets the interactive prompt and the
// non-interactive default coexist without two code paths: a client that has not
// decided about backups is told to decide, and one that has says so in the
// query string.
func (s *Server) handleDeleteApp(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppDelete)
	if !ok {
		return
	}

	backup := r.URL.Query().Get("backup")
	force := r.URL.Query().Get("force") == "true"

	volumes, err := s.Apps.VolumeCount(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}

	if backup == "" && !force && volumes > 0 {
		Error(w, r, errs.New(errs.StateBackupDecisionRequired,
			"This app has storage attached. Deleting it will remove that storage unless you back it up first.").
			WithRemedy("Delete with backup=true to keep a copy, or force=true to delete without one. A backup made this way is kept until you discard it.").
			WithDetail("volume_count", volumes))
		return
	}

	// Volumes are ON DELETE RESTRICT (R-204), so they are resolved explicitly
	// rather than cascading.
	var keptBackup string
	if volumes > 0 {
		if backup == "true" {
			id, err := s.backUpBeforeDelete(r, app)
			if err != nil {
				// The delete stops here. A failed backup means the data is not
				// safe, so deleting is the one thing not to do — and the caller
				// is told exactly how to go ahead anyway if they have decided
				// the data is not worth keeping.
				s.audit(r, audit.Event{
					PrincipalKind: audit.PrincipalKind(PrincipalFrom(r.Context()).Kind),
					PrincipalID:   PrincipalFrom(r.Context()).ID,
					OnBehalfOf:    PrincipalFrom(r.Context()).UserID,
					Action:        "app.delete.backup_failed",
					AppID:         app.ID, TargetKind: "app", TargetID: app.ID,
					Detail: map[string]any{"reason": reasonOf(err)},
				})
				Error(w, r, errs.Wrap(errs.StateInvalid,
					"Pando could not back up this app's storage, so it has not been deleted.", err).
					WithRemedy("Fix the problem and try again, or delete with force=true to remove the app and discard its storage.").
					WithDetail("backup_error", reasonOf(err)))
				return
			}
			keptBackup = id
		}

		if err := s.Volumes.DeleteForApp(r.Context(), app.ID); err != nil {
			Error(w, r, err)
			return
		}

		// The storage itself goes when the bundle is torn down. The delete has
		// settled it: discarded, or backed up first (R-204). It used to stay on
		// disk with no row left to reach it by (issue #55).
		if err := s.Apps.DiscardStorage(r.Context(), app.ID); err != nil {
			Error(w, r, err)
			return
		}
	}

	if err := s.Apps.Archive(r.Context(), app.ID); err != nil {
		Error(w, r, err)
		return
	}
	if s.TeardownNow != nil {
		s.TeardownNow()
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(PrincipalFrom(r.Context()).Kind),
		PrincipalID:   PrincipalFrom(r.Context()).ID,
		OnBehalfOf:    PrincipalFrom(r.Context()).UserID,
		Action:        "app.delete",
		AppID:         app.ID,
		TargetKind:    "app",
		TargetID:      app.ID,
		Detail: map[string]any{
			"forced":            force,
			"volumes_discarded": volumes,
			// Which backup holds this app's data, if any. The question after a
			// deletion is always "can we get it back", and this is the answer.
			"backup_id": keptBackup,
		},
	})
	JSON(w, http.StatusNoContent, nil)
}

// backUpBeforeDelete takes the final copy R-204 promises.
//
// Kept until explicitly discarded, never aged out — which is the whole point of
// it, and why the row carries no retention.
func (s *Server) backUpBeforeDelete(r *http.Request, app state.App) (string, error) {
	if s.Backup == nil || s.Backups == nil || s.BundleSource == nil {
		return "", errs.New(errs.Internal, "Backups are not set up on this installation.")
	}

	volumes, err := s.BundleSource.VolumesForApp(r.Context(), app.ID)
	if err != nil {
		return "", err
	}

	var pinned json.RawMessage
	if app.PinnedSpecID != "" {
		rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
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
	created, err := s.Backup.CreateForApp(r.Context(), id, backup.AppCreateRequest{
		AppID:   app.ID,
		Kind:    "on_delete",
		Spec:    pinned,
		Volumes: volumes,
	})
	if err != nil {
		return "", err
	}

	p := PrincipalFrom(r.Context())
	if err := s.Backups.Record(r.Context(), state.Backup{
		ID: id, AppID: app.ID, Kind: "on_delete",
		AdapterRef: created.AdapterRef, ObjectName: created.ObjectName,
		SizeBytes: created.SizeBytes, Manifest: created.Manifest,
		// RetainUntil deliberately nil: R-204 keeps this until somebody
		// discards it, and the schema refuses to let it carry an expiry.
		CreatedBy: p.ID,
	}); err != nil {
		return "", err
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "backup.create", AppID: app.ID, TargetKind: "backup", TargetID: id,
		Detail: map[string]any{"kind": "on_delete", "volumes": len(volumes)},
	})
	return id, nil
}

// reasonOf is the message of an error, for an audit detail.
func reasonOf(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// --- specs -----------------------------------------------------------------

func (s *Server) handleListSpecs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	revs, err := s.Apps.ListRevisions(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, map[string]any{"revisions": revs, "pinned_spec_id": app.PinnedSpecID})
}

func (s *Server) handleGetSpec(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	rev, found, err := s.revisionFromPath(r, app.ID, "rev")
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no such revision of this app's spec."))
		return
	}
	JSON(w, http.StatusOK, rev)
}

// handleCreateSpec writes a new revision. Editing produces a revision; it never
// modifies one (R-152).
func (s *Server) handleCreateSpec(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}

	var body spec.AppSpec
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The spec could not be read.").
			WithRemedy("Check that the body is valid JSON matching the spec schema."))
		return
	}

	// An imported spec is untrusted input like any other and lands as a
	// proposal requiring review, never as a live deployment (design 01 §5).
	origin := spec.OriginEdited
	if body.Origin == spec.OriginImported || body.Origin == spec.OriginManual {
		origin = body.Origin
	}

	body.SchemaVersion = spec.SchemaVersion
	body.AppID = app.ID

	// ?dry_run=true answers exactly as a save would and writes nothing: the
	// same defaults, validation and gates, refusals with the same codes and
	// words, but no revision, no audit event, and no audited denial — nothing
	// was attempted. What the console's editors ask on every edit, so they
	// show the server's decision instead of guessing it.
	dryRun := r.URL.Query().Get("dry_run") == "true"
	var gate specgate.Authorizer = s.Authz
	if dryRun {
		gate = specgate.Quiet(s.Authz)
	}

	// Defaults, before validation.
	//
	// These used to be applied only on the detection path, so a hand-written
	// spec — the API's own documented way to configure an app — silently got
	// none of them. R-223's 100 MB log cap, R-211's backup retention and the
	// spec-revision limit were all absent from every hand-written spec, which
	// includes every spec the acceptance suite writes. That is why nothing
	// noticed: the tests exercised exactly the path that skipped it.
	//
	// Non-destructive by construction: Apply fills empty fields and leaves
	// anything the author set alone.
	if s.Defaults != nil {
		s.Defaults.Defaults(r.Context()).Apply(&body, app.Slug)
	}

	// R-163 holds for a spec written whole, not only for PUT /routing: a
	// mode the adapter does not default to needs app.routing.override, unless
	// the app already had it. Checked against the pinned spec, and ModeSource
	// set from the adapter rather than taken on the author's word.
	p := PrincipalFrom(r.Context())
	pinned, err := s.pinnedSpec(r.Context(), app)
	if err != nil {
		Error(w, r, err)
		return
	}
	if s.Address != nil {
		var current *spec.Routing
		if pinned != nil {
			current = &pinned.Routing
		}
		override, err := s.Address.Overrides(r.Context(), current, &body.Routing)
		if err != nil {
			Error(w, r, err)
			return
		}
		if override {
			if err := gate.CheckControl(r.Context(), p, app.ID, authz.AppRoutingOverride); err != nil {
				Error(w, r, err)
				return
			}
		}
	}

	if err := spec.Validate(&body); err != nil {
		Error(w, r, err)
		return
	}

	// Egress (R-182 – R-184) and auto-deploy under approval (R-158), judged
	// against the pinned spec. After validation, so an entry that does not
	// parse is reported as such rather than resolved around.
	gated, err := s.gateSpecWith(r, gate, app, pinned, &body)
	if err != nil {
		Error(w, r, err)
		return
	}

	if dryRun {
		s.writeDryRun(w, r, app, &body, gated)
		return
	}

	rev, err := s.Apps.CreateRevision(r.Context(), app.ID, &body, origin, p.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	s.auditApp(r, app.ID, "spec.create")

	JSON(w, http.StatusCreated, rev)
}

// handlePinSpec points the app at a revision. It does not deploy.
func (s *Server) handlePinSpec(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppSpecEdit)
	if !ok {
		return
	}
	rev, found, err := s.revisionFromPath(r, app.ID, "rev")
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.NotFound, "There is no such revision of this app's spec."))
		return
	}

	// Validate again at pin time. A revision written by an older Pando, or
	// imported from elsewhere, can be well-formed on arrival and invalid now.
	if err := spec.Validate(rev.Body); err != nil {
		Error(w, r, err)
		return
	}

	state := app.State
	if state == "draft" {
		state = "proposed"
	}
	if err := s.Apps.Pin(r.Context(), app.ID, rev.ID, state, PrincipalFrom(r.Context()).ID); err != nil {
		Error(w, r, err)
		return
	}
	s.auditApp(r, app.ID, "spec.pin")

	updated, _, err := s.Apps.ByID(r.Context(), app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, updated)
}

// handleDiffSpecs returns the classified diff between two revisions.
func (s *Server) handleDiffSpecs(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}

	from, found, err := s.revisionFromPath(r, app.ID, "a")
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}
	to, found, err := s.revisionFromPath(r, app.ID, "b")
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}

	d := spec.Compare(from.Body, to.Body)
	JSON(w, http.StatusOK, map[string]any{
		"from":                  from.Revision,
		"to":                    to.Revision,
		"class":                 d.Class(),
		"requires_confirmation": d.RequiresConfirmation(),
		"changes":               d.Changes,
	})
}

// handleExportSpec emits the spec (R-020).
//
// Safe to hand to someone: the spec holds no secret values by construction, only
// references. Nothing needs stripping here, which is the point of that design.
func (s *Server) handleExportSpec(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app has no pinned spec to export yet."))
		return
	}
	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}

	s.auditApp(r, app.ID, "spec.export")
	JSON(w, http.StatusOK, rev.Body)
}

func (s *Server) revisionFromPath(r *http.Request, appID, param string) (state.Revision, bool, error) {
	raw := chi.URLParam(r, param)
	if n, err := strconv.Atoi(raw); err == nil {
		return s.Apps.RevisionByNumber(r.Context(), appID, n)
	}
	rev, found, err := s.Apps.RevisionByID(r.Context(), raw)
	if err != nil || !found {
		return rev, found, err
	}
	if rev.AppID != appID {
		// A revision ID from another app must not resolve here.
		return state.Revision{}, false, nil
	}
	return rev, true, nil
}

func orNotFound(err error) error {
	if err != nil {
		return err
	}
	return errs.New(errs.NotFound, "There is no such revision of this app's spec.")
}

func (s *Server) auditApp(r *http.Request, appID, action string) {
	p := PrincipalFrom(r.Context())
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		AppID:         appID,
		TargetKind:    "app",
		TargetID:      appID,
	})
}

func slugify(name string) string {
	s := slugPattern.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if s == "" {
		s = "app"
	}
	// Slugs are used in routing and must be unique, so a suffix keeps two apps
	// with the same name from colliding on the URL as well as on the name.
	return s + "-" + strings.ToLower(id.New(id.App)[len(id.App)+1:])[:6]
}
