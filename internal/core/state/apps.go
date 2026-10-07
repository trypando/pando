package state

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// App lifecycle states (design 05 §1.1).
const (
	StateDraft     = "draft"
	StateProposed  = "proposed"
	StateDeploying = "deploying"
	StateRunning   = "running"
	StateDegraded  = "degraded"
	StateStopped   = "stopped"
	StateFailed    = "failed"
	StateArchived  = "archived"
)

// App is a deployable unit.
type App struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Slug         string `json:"slug"`
	OwnerUserID  string `json:"owner_user_id"`
	State        string `json:"state"`
	DesiredState string `json:"desired_state"`
	PinnedSpecID string `json:"pinned_spec_id,omitempty"`

	// Source is where the app comes from, recorded at creation so detection has
	// something to clone before any spec exists to carry it.
	Source spec.Source `json:"source"`

	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`

	// Routing is the pinned spec's routing block, so a caller can say where
	// the app is without fetching a whole spec revision to find out.
	//
	// Zero until an app has a pinned spec: before that it has no address at
	// all, which is a different thing from having one nobody can reach.
	Routing spec.Routing `json:"routing,omitzero"`

	// Address is where to open this app, rendered for the request that asked.
	// Filled in by the API layer, which is the only place that knows what host
	// the caller reached Pando on. See spec.Address.
	Address string `json:"address,omitempty"`

	// SecurityScore is the newest score of the revision this app is running
	// (R-310). Nil for an app that has never been scanned, which is not a score
	// of zero (R-318) — the list shows the difference.
	//
	// Which of the two stored numbers this is — every finding, or only the ones
	// with a fix — is host policy's choice, applied by the API layer.
	SecurityScore *int `json:"security_score,omitempty"`

	// SecurityScoreFixable is the same scan counting only fixable findings, so
	// the API layer can answer either question without a second query.
	SecurityScoreFixable *int `json:"-"`

	// SecurityVerdict is that score placed against host policy: ok, insecure,
	// unscanned or inert. Filled in by the API layer, because it depends on a
	// policy document and on whether a scanner is configured, and neither is
	// the store's to know.
	SecurityVerdict string `json:"security_verdict,omitempty"`

	// SecurityScanning says a scan of this app is running now, whoever started
	// it. Filled in by the API layer from the security service, which is the
	// one place that knows (R-310, R-261).
	SecurityScanning bool `json:"security_scanning,omitempty"`

	// IconUpdatedAt is when the app's tile image last changed, and nil when it
	// has none (R-340). The time rather than a flag so a client can put it in
	// the image's URL and a browser never shows yesterday's picture from cache.
	// The bytes are at GET /apps/{id}/icon; no list carries them.
	IconUpdatedAt *time.Time `json:"icon_updated_at,omitempty"`

	// Favorite is whether the caller has marked this app as a favorite
	// (R-341). Only GET /me/apps fills it in: it is a fact about the person
	// asking, and no other list is answering a question about them.
	Favorite bool `json:"favorite,omitempty"`

	// SectionID is the caller's launcher section this app is filed under, and
	// empty when it is under "Your apps" (R-342). Like Favorite, only GET
	// /me/apps fills it in.
	SectionID string `json:"section_id,omitempty"`

	// Detection is where the app's detection has got to — running and at
	// which stage, or how it finished — so a draft says why it is still a
	// draft (issue #80). Filled in by the API layer on GET /apps and GET
	// /apps/{id}; nil for an app that has never been through detection.
	Detection *DetectionSummary `json:"detection,omitempty"`
}

// Apps stores apps and their spec revisions.
type Apps struct{ db *DB }

func NewApps(db *DB) *Apps { return &Apps{db: db} }

// Create inserts an app and its two grants in one transaction.
//
// R-073: app creation writes two grant rows, one per plane, independently
// revocable. They are written here rather than by the handler so that an app
// cannot exist without them — a control grant without a data grant would leave
// the owner able to manage an app they cannot open.
func (a *Apps) Create(ctx context.Context, name, slug, ownerUserID, createdBy string, src spec.Source) (App, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return App{}, errs.Wrap(errs.Internal, "Could not create the app.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	app := App{
		ID:           id.New(id.App),
		Name:         name,
		Slug:         slug,
		OwnerUserID:  ownerUserID,
		State:        StateDraft,
		DesiredState: "stopped",
		Source:       src,
	}

	encodedSource, err := json.Marshal(src)
	if err != nil {
		return App{}, errs.Wrap(errs.Internal, "Could not record where the app comes from.", err)
	}

	err = tx.QueryRow(ctx, `
		INSERT INTO apps (id, name, slug, owner_user_id, state, desired_state, source)
		VALUES ($1, $2, $3, $4, 'draft', 'stopped', $5)
		RETURNING created_at, updated_at`,
		app.ID, name, slug, ownerUserID, encodedSource).Scan(&app.CreatedAt, &app.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return App{}, errs.Newf(errs.ValidInvalid, "An app named %q already exists.", name).
				WithRemedy("Choose a different name.")
		}
		return App{}, errs.Wrap(errs.Internal, "Could not create the app.", err)
	}

	for _, g := range []struct {
		plane  string
		roleID any
	}{
		{"control", authz.RoleOwner},
		{"data", nil},
	} {
		_, err = tx.Exec(ctx, `
			INSERT INTO grants (id, app_id, plane, principal_kind, principal_id, role_id, created_by)
			VALUES ($1, $2, $3, 'user', $4, $5, $6)`,
			id.New(id.Grant), app.ID, g.plane, ownerUserID, g.roleID, createdBy)
		if err != nil {
			return App{}, errs.Wrap(errs.Internal, "Could not set up access for the app.", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return App{}, errs.Wrap(errs.Internal, "Could not create the app.", err)
	}
	return app, nil
}

// ByID returns an app.
func (a *Apps) ByID(ctx context.Context, appID string) (App, bool, error) {
	var app App
	var owner, pinned *string
	var source []byte
	var routing []byte
	err := a.db.QueryRow(ctx, `
		SELECT a.id, a.name, a.slug, a.owner_user_id, a.state, a.desired_state, a.pinned_spec_id,
		       a.source, a.created_at, a.updated_at, a.deleted_at, r.body->'routing',
		       s.score, s.score_fixable, i.updated_at
		FROM apps a
		LEFT JOIN spec_revisions r ON r.id = a.pinned_spec_id
		LEFT JOIN app_icons i ON i.app_id = a.id
		LEFT JOIN LATERAL (
		    SELECT sc.score, sc.score_fixable
		    FROM app_scans sc
		    WHERE sc.app_id = a.id AND sc.score IS NOT NULL
		      AND (sc.spec_id = a.pinned_spec_id OR sc.spec_id IS NULL)
		    ORDER BY (sc.spec_id IS NOT NULL) DESC, sc.ran_at DESC
		    LIMIT 1
		) s ON true
		WHERE a.id = $1 AND a.deleted_at IS NULL`, appID).
		Scan(&app.ID, &app.Name, &app.Slug, &owner, &app.State, &app.DesiredState, &pinned,
			&source, &app.CreatedAt, &app.UpdatedAt, &app.DeletedAt, &routing,
			&app.SecurityScore, &app.SecurityScoreFixable, &app.IconUpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, false, nil
	}
	if err != nil {
		return App{}, false, errs.Wrap(errs.Internal, "Could not read the app.", err)
	}
	if owner != nil {
		app.OwnerUserID = *owner
	}
	if pinned != nil {
		app.PinnedSpecID = *pinned
	}
	if len(source) > 0 {
		_ = json.Unmarshal(source, &app.Source)
	}
	if len(routing) > 0 {
		_ = json.Unmarshal(routing, &app.Routing)
	}
	return app, true, nil
}

// ListForPrincipal returns apps the principal can see on the control plane.
//
// Scoped by grant rather than returning everything and filtering afterward: a
// list endpoint that leaks the existence of apps is a smaller problem than one
// that leaks their contents, but it is still a leak.
func (a *Apps) ListForPrincipal(ctx context.Context, p authz.Principal) ([]App, error) {
	return a.listControl(ctx, `
		JOIN grants g ON g.app_id = a.id AND g.plane = 'control'
		WHERE a.deleted_at IS NULL
		  AND (
		        (g.principal_kind = 'user'  AND g.principal_id = $1)
		     OR (g.principal_kind = 'token' AND g.principal_id = $2)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $1))
		  )`, nullable(p.UserID), nullable(accountTokenID(p)))
}

// ListAll returns every app, for a principal whose install role reaches every
// app with app.view (install.apps.view, R-081). The caller decides
// that; this only reads.
func (a *Apps) ListAll(ctx context.Context) ([]App, error) {
	return a.listControl(ctx, `WHERE a.deleted_at IS NULL`)
}

// listControl is the admin console's app list, narrowed by `where`.
func (a *Apps) listControl(ctx context.Context, where string, args ...any) ([]App, error) {
	rows, err := a.db.Query(ctx, `
		SELECT DISTINCT a.id, a.name, a.slug, a.owner_user_id, a.state, a.desired_state,
		       a.pinned_spec_id, a.created_at, a.updated_at, r.body->'routing', s.score, s.score_fixable,
		       i.updated_at
		FROM apps a
		LEFT JOIN spec_revisions r ON r.id = a.pinned_spec_id
		LEFT JOIN app_icons i ON i.app_id = a.id
		-- The newest scan of the revision this app is running, and failing that
		-- the newest that belongs to no revision — which is what detection
		-- produces, from the source, before a revision exists. Never another
		-- revision's: a score for a spec the app is not running is not a score
		-- of what is deployed (design 09 §3).
		LEFT JOIN LATERAL (
		    SELECT sc.score, sc.score_fixable
		    FROM app_scans sc
		    WHERE sc.app_id = a.id AND sc.score IS NOT NULL
		      AND (sc.spec_id = a.pinned_spec_id OR sc.spec_id IS NULL)
		    ORDER BY (sc.spec_id IS NOT NULL) DESC, sc.ran_at DESC
		    LIMIT 1
		) s ON true
		`+where+`
		ORDER BY a.created_at DESC`, args...)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list apps.", err)
	}
	defer rows.Close()

	var out []App
	for rows.Next() {
		var app App
		var owner, pinned *string
		var routing []byte
		if err := rows.Scan(&app.ID, &app.Name, &app.Slug, &owner, &app.State, &app.DesiredState,
			&pinned, &app.CreatedAt, &app.UpdatedAt, &routing,
			&app.SecurityScore, &app.SecurityScoreFixable, &app.IconUpdatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list apps.", err)
		}
		if len(routing) > 0 {
			_ = json.Unmarshal(routing, &app.Routing)
		}
		if owner != nil {
			app.OwnerUserID = *owner
		}
		if pinned != nil {
			app.PinnedSpecID = *pinned
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// ListForUse returns apps the principal holds a DATA-plane grant on (R-264).
//
// Deliberately a different query from ListForPrincipal. Two planes, two
// endpoints: the launcher shows what you can open, not what you can manage.
func (a *Apps) ListForUse(ctx context.Context, p authz.Principal) ([]App, error) {
	rows, err := a.db.Query(ctx, `
		SELECT DISTINCT a.id, a.name, a.slug, a.state, r.body->'routing', i.updated_at,
		       f.app_id IS NOT NULL, lp.section_id
		FROM apps a
		LEFT JOIN spec_revisions r ON r.id = a.pinned_spec_id
		LEFT JOIN app_icons i ON i.app_id = a.id
		LEFT JOIN app_favorites f ON f.app_id = a.id AND f.user_id = $1
		LEFT JOIN launcher_placements lp ON lp.app_id = a.id AND lp.user_id = $1
		LEFT JOIN grants g ON g.app_id = a.id AND g.plane = 'data'
		WHERE a.deleted_at IS NULL
		  AND (
		        a.owner_user_id = $1
		     OR g.principal_kind = 'anonymous'
		     OR (g.principal_kind = 'user'  AND g.principal_id = $1)
		     OR (g.principal_kind = 'token' AND g.principal_id = $2)
		     OR (g.principal_kind = 'group' AND g.principal_id IN (
		            SELECT group_id FROM effective_group_members WHERE user_id = $1))
		  )
		ORDER BY a.name`,
		nullable(p.UserID), nullable(accountTokenID(p)))
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list apps.", err)
	}
	defer rows.Close()

	var out []App
	for rows.Next() {
		var app App
		var routing []byte
		var sectionID *string
		if err := rows.Scan(&app.ID, &app.Name, &app.Slug, &app.State, &routing, &app.IconUpdatedAt,
			&app.Favorite, &sectionID); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list apps.", err)
		}
		if sectionID != nil {
			app.SectionID = *sectionID
		}
		if len(routing) > 0 {
			// A routing block that will not parse is not a reason to refuse
			// somebody their list of apps. They lose the address, not the app.
			_ = json.Unmarshal(routing, &app.Routing)
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// Rename changes an app's display name.
func (a *Apps) Rename(ctx context.Context, appID, name string) error {
	_, err := a.db.Exec(ctx,
		`UPDATE apps SET name = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, appID, name)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not rename the app.", err)
	}
	return nil
}

// Icon is an app's tile image (R-340).
type Icon struct {
	ContentType string
	Data        []byte
	UpdatedAt   time.Time
}

// SetIcon replaces an app's tile image. The caller has already checked the
// bytes are an image of an allowed type; the table's CHECK is the backstop.
func (a *Apps) SetIcon(ctx context.Context, appID, contentType string, data []byte) error {
	_, err := a.db.Exec(ctx, `
		INSERT INTO app_icons (app_id, content_type, data)
		VALUES ($1, $2, $3)
		ON CONFLICT (app_id) DO UPDATE
		SET content_type = EXCLUDED.content_type, data = EXCLUDED.data, updated_at = now()`,
		appID, contentType, data)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not save the app's image.", err)
	}
	return nil
}

// ClearIcon removes an app's tile image. Clearing one that is not there is
// not an error: the outcome asked for is the outcome that holds.
func (a *Apps) ClearIcon(ctx context.Context, appID string) error {
	if _, err := a.db.Exec(ctx, `DELETE FROM app_icons WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the app's image.", err)
	}
	return nil
}

// IconOf returns an app's tile image, and false when it has none.
func (a *Apps) IconOf(ctx context.Context, appID string) (Icon, bool, error) {
	var icon Icon
	err := a.db.QueryRow(ctx,
		`SELECT content_type, data, updated_at FROM app_icons WHERE app_id = $1`, appID).
		Scan(&icon.ContentType, &icon.Data, &icon.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Icon{}, false, nil
	}
	if err != nil {
		return Icon{}, false, errs.Wrap(errs.Internal, "Could not read the app's image.", err)
	}
	return icon, true, nil
}

// SetFavorite marks or unmarks an app as one of a user's favorites (R-341).
// Both directions are idempotent: the outcome asked for is the one that holds.
func (a *Apps) SetFavorite(ctx context.Context, userID, appID string, favorite bool) error {
	var err error
	if favorite {
		_, err = a.db.Exec(ctx, `
			INSERT INTO app_favorites (user_id, app_id) VALUES ($1, $2)
			ON CONFLICT (user_id, app_id) DO NOTHING`, userID, appID)
	} else {
		_, err = a.db.Exec(ctx,
			`DELETE FROM app_favorites WHERE user_id = $1 AND app_id = $2`, userID, appID)
	}
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update your favorites.", err)
	}
	return nil
}

// Archive soft-deletes an app.
//
// Soft delete because the audit log references it, and an audit trail pointing
// at a row that no longer exists answers fewer questions than one that does.
// Volumes are ON DELETE RESTRICT and must be resolved first (R-204) — the caller
// handles the keep-or-discard decision.
// Known reports whether this installation ever created the app, deleted ones
// included. It is how startup tells this install's app networks from another
// install's on the same Docker host (issue #55).
func (a *Apps) Known(ctx context.Context, appID string) (bool, error) {
	var known bool
	if err := a.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM apps WHERE id = $1)`, appID).Scan(&known); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not look the app up.", err)
	}
	return known, nil
}

// Live reports whether an app still exists and has not been deleted. A
// question worth asking again just before acting on an app read a while ago.
func (a *Apps) Live(ctx context.Context, appID string) (bool, error) {
	var live bool
	if err := a.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM apps WHERE id = $1 AND deleted_at IS NULL AND state <> 'archived')`,
		appID).Scan(&live); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not look the app up.", err)
	}
	return live, nil
}

func (a *Apps) Archive(ctx context.Context, appID string) error {
	_, err := a.db.Exec(ctx, `
		UPDATE apps SET state = 'archived', desired_state = 'stopped', deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the app.", err)
	}

	// Its host port goes back in the pool (O-15). The row is kept, so the
	// table's ON DELETE CASCADE never fires and the port would be held by an
	// app that no longer exists.
	if _, err := a.db.Exec(ctx, `DELETE FROM port_allocations WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not release the app's port.", err)
	}
	return nil
}

// VolumeCount returns how many volumes an app holds, for the delete decision.
func (a *Apps) VolumeCount(ctx context.Context, appID string) (int, error) {
	var n int
	if err := a.db.QueryRow(ctx, `SELECT count(*) FROM volumes WHERE app_id = $1`, appID).Scan(&n); err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not read the app's storage.", err)
	}
	return n, nil
}

// --- spec revisions --------------------------------------------------------

// Revision is a stored spec revision.
type Revision struct {
	ID        string        `json:"id"`
	AppID     string        `json:"app_id"`
	Revision  int           `json:"revision"`
	Origin    spec.Origin   `json:"origin"`
	Body      *spec.AppSpec `json:"body"`
	CreatedBy string        `json:"created_by"`
	CreatedAt time.Time     `json:"created_at"`

	// EverPinned is derived from spec_pins, not stored on the revision —
	// spec_revisions is append-only and a mutable column on it would be a hole
	// in that guarantee.
	EverPinned bool `json:"ever_pinned"`
}

// CreateRevision appends a spec revision.
//
// The revision number is assigned inside the transaction from the current
// maximum, so two concurrent edits cannot both claim the same number — the
// UNIQUE (app_id, revision) constraint is the backstop, and this is what keeps
// it from being hit in normal operation.
func (a *Apps) CreateRevision(ctx context.Context, appID string, s *spec.AppSpec, origin spec.Origin, createdBy string) (Revision, error) {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return Revision{}, errs.Wrap(errs.Internal, "Could not save the spec.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var next int
	if err := tx.QueryRow(ctx,
		`SELECT coalesce(max(revision), 0) + 1 FROM spec_revisions WHERE app_id = $1`, appID).Scan(&next); err != nil {
		return Revision{}, errs.Wrap(errs.Internal, "Could not save the spec.", err)
	}

	// An edited image keeps no digest that was resolved from the old one
	// (issue #41). Here, where every revision is written, so no path that
	// creates one can skip it.
	if next > 1 {
		var prevBody []byte
		if err := tx.QueryRow(ctx,
			`SELECT body FROM spec_revisions WHERE app_id = $1 AND revision = $2`, appID, next-1).Scan(&prevBody); err == nil {
			var prev spec.AppSpec
			if json.Unmarshal(prevBody, &prev) == nil {
				spec.DropStalePin(&prev, s)
			}
		}
	}

	s.SchemaVersion = spec.SchemaVersion
	s.AppID = appID
	s.Revision = next
	s.Origin = origin
	s.CreatedBy = createdBy
	s.CreatedAt = time.Now().UTC()

	body, err := json.Marshal(s)
	if err != nil {
		return Revision{}, errs.Wrap(errs.Internal, "Could not encode the spec.", err)
	}

	rev := Revision{
		ID:        id.New(id.Spec),
		AppID:     appID,
		Revision:  next,
		Origin:    origin,
		Body:      s,
		CreatedBy: createdBy,
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO spec_revisions (id, app_id, revision, origin, body, created_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING created_at`,
		rev.ID, appID, next, string(origin), body, createdBy).Scan(&rev.CreatedAt); err != nil {
		return Revision{}, errs.Wrap(errs.Internal, "Could not save the spec.", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return Revision{}, errs.Wrap(errs.Internal, "Could not save the spec.", err)
	}
	return rev, nil
}

// RevisionByID returns one revision.
func (a *Apps) RevisionByID(ctx context.Context, specID string) (Revision, bool, error) {
	return a.scanRevision(ctx, `WHERE id = $1`, specID)
}

// RevisionByNumber returns an app's revision by its number.
func (a *Apps) RevisionByNumber(ctx context.Context, appID string, number int) (Revision, bool, error) {
	return a.scanRevision(ctx, `WHERE app_id = $1 AND revision = $2`, appID, number)
}

func (a *Apps) scanRevision(ctx context.Context, where string, args ...any) (Revision, bool, error) {
	var rev Revision
	var body []byte
	var origin string
	err := a.db.QueryRow(ctx, `
		SELECT r.id, r.app_id, r.revision, r.origin, r.body, r.created_by, r.created_at,
		       EXISTS (SELECT 1 FROM spec_pins p WHERE p.spec_id = r.id)
		FROM spec_revisions r `+where,
		args...).
		Scan(&rev.ID, &rev.AppID, &rev.Revision, &origin, &body, &rev.CreatedBy, &rev.CreatedAt, &rev.EverPinned)
	if errors.Is(err, pgx.ErrNoRows) {
		return Revision{}, false, nil
	}
	if err != nil {
		return Revision{}, false, errs.Wrap(errs.Internal, "Could not read the spec.", err)
	}

	rev.Origin = spec.Origin(origin)
	var s spec.AppSpec
	if err := json.Unmarshal(body, &s); err != nil {
		return Revision{}, false, errs.Wrap(errs.Internal, "Could not read the stored spec.", err)
	}
	rev.Body = &s
	return rev, true, nil
}

// ListRevisions returns an app's revisions, newest first.
func (a *Apps) ListRevisions(ctx context.Context, appID string) ([]Revision, error) {
	rows, err := a.db.Query(ctx, `
		SELECT r.id, r.app_id, r.revision, r.origin, r.created_by, r.created_at,
		       EXISTS (SELECT 1 FROM spec_pins p WHERE p.spec_id = r.id)
		FROM spec_revisions r WHERE r.app_id = $1 ORDER BY r.revision DESC`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list the app's specs.", err)
	}
	defer rows.Close()

	var out []Revision
	for rows.Next() {
		var rev Revision
		var origin string
		if err := rows.Scan(&rev.ID, &rev.AppID, &rev.Revision, &origin, &rev.CreatedBy, &rev.CreatedAt, &rev.EverPinned); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list the app's specs.", err)
		}
		rev.Origin = spec.Origin(origin)
		out = append(out, rev)
	}
	return out, rows.Err()
}

// Pin points the app at a revision.
//
// Records the pinning as an event in the same transaction, because retention
// pruning must never remove a revision that was once live (R-152) and that fact
// has to survive the pointer moving on during a rollback. The event log doubles
// as the rollback history.
func (a *Apps) Pin(ctx context.Context, appID, specID, newState, pinnedBy string) error {
	tx, err := a.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not pin the spec.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Where the revision says the app is reached, claimed in the same
	// transaction as the pin, so the address and the pinned spec cannot
	// disagree (migration 35).
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT body FROM spec_revisions WHERE id = $1`, specID).Scan(&body); err != nil {
		return errs.Wrap(errs.Internal, "Could not pin the spec.", err)
	}
	var pinned spec.AppSpec
	if err := json.Unmarshal(body, &pinned); err != nil {
		return errs.Wrap(errs.Internal, "Could not read the spec being pinned.", err)
	}
	if err := checkAddress(ctx, tx, appID, pinned.Routing); err != nil {
		return err
	}
	hostname, path := addressOf(pinned.Routing)

	if _, err := tx.Exec(ctx,
		`INSERT INTO spec_pins (app_id, spec_id, pinned_by) VALUES ($1, $2, $3)`,
		appID, specID, pinnedBy); err != nil {
		return errs.Wrap(errs.Internal, "Could not pin the spec.", err)
	}
	if _, err := tx.Exec(ctx,
		`UPDATE apps SET pinned_spec_id = $2, state = $3, updated_at = now(),
		        address_hostname = NULLIF($4, ''), address_path = NULLIF($5, '')
		 WHERE id = $1`,
		appID, specID, newState, hostname, path); err != nil {
		if isUniqueViolation(err) {
			// Two pins of the same address raced past checkAddress; the
			// index decided.
			return errs.New(errs.StateAddressTaken, "Another app took this address a moment ago.").
				WithRemedy("Choose another address for this app.")
		}
		return errs.Wrap(errs.Internal, "Could not pin the spec.", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not pin the spec.", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23505"
}

// isForeignKeyViolation reports a 23503. A foreign key refusing a write is
// usually a caller passing an ID that names the wrong kind of thing, which is
// worth a sentence rather than an internal error.
func isForeignKeyViolation(err error) bool {
	var pgErr interface{ SQLState() string }
	return errors.As(err, &pgErr) && pgErr.SQLState() == "23503"
}

// AwaitingTeardown returns archived apps whose bundle is still standing.
//
// The leak this closes: nothing ever called Destroy, so a deleted app's
// containers and its private network stayed up. Docker's default pool holds
// about thirty networks and Pando takes one per app, so an install that adds
// and removes apps eventually cannot start one.
//
// Returns the routing adapter ref too, because the route outlives the app the
// same way — a Traefik file per deleted app, accumulating.
func (a *Apps) AwaitingTeardown(ctx context.Context, limit int) ([]TeardownTarget, error) {
	rows, err := a.db.Query(ctx, `
		SELECT a.id, coalesce(r.body->'runtime'->>'adapter_ref', ''),
		       coalesce(r.body->'routing'->>'adapter_ref', ''),
		       coalesce(r.body->'build'->>'adapter_ref', ''),
		       a.discard_storage
		FROM apps a
		LEFT JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NOT NULL AND a.bundle_destroyed_at IS NULL
		ORDER BY a.deleted_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not find apps waiting to be torn down.", err)
	}
	defer rows.Close()

	out := make([]TeardownTarget, 0)
	for rows.Next() {
		var t TeardownTarget
		if err := rows.Scan(&t.AppID, &t.RuntimeRef, &t.RoutingRef, &t.BuilderRef, &t.DiscardStorage); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not find apps waiting to be torn down.", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TeardownTarget is a deleted app whose bundle still exists.
type TeardownTarget struct {
	AppID      string
	RuntimeRef string
	RoutingRef string

	// BuilderRef is the builder that holds the app's build cache, so the
	// teardown can have it forgotten.
	BuilderRef string

	// DiscardStorage says the delete settled the app's storage — discarded,
	// or backed up first — so its volumes go with the bundle.
	DiscardStorage bool
}

// DiscardStorage records that a delete settled what becomes of the app's
// storage — discarded, or backed up first — so the teardown destroys the
// volumes with the bundle rather than leaving them on disk (R-204, R-224).
func (a *Apps) DiscardStorage(ctx context.Context, appID string) error {
	if _, err := a.db.Exec(ctx,
		`UPDATE apps SET discard_storage = true, updated_at = now() WHERE id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the app's storage is to be removed.", err)
	}
	return nil
}

// MarkBundleDestroyed records that the runtime confirmed the bundle is gone.
func (a *Apps) MarkBundleDestroyed(ctx context.Context, appID string) error {
	_, err := a.db.Exec(ctx,
		`UPDATE apps SET bundle_destroyed_at = now() WHERE id = $1`, appID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record that the app's bundle was removed.", err)
	}
	return nil
}

// AppWithStorage is an app that has data worth backing up (R-210).
type AppWithStorage struct {
	AppID string
	Spec  []byte

	// Retain is how many daily copies to keep (R-211).
	Retain int
}

// WithStorage lists live apps that declare volumes, for rolling backups.
//
// Only apps with storage. An app with none has nothing a rolling backup would
// hold that its spec revisions do not already, and taking one anyway would fill
// the destination with empty bundles nobody wants to page through.
//
// The volumes test is jsonb_typeof and not a coalesce. `AppSpec.Volumes` is
// tagged without omitempty, so an app that declares no storage stores
// `"volumes": null` — and `->` on a key holding JSON null answers with that
// null, not SQL NULL, so the coalesce never fired and jsonb_array_length was
// handed a scalar. Postgres raises 22023, the reconciler logged "could not list
// apps for rolling backups", and no app on the installation was backed up
// (R-210).
//
// A typeof guard beside it is not enough on its own: AND does not promise an
// evaluation order, and the planner is free to reach jsonb_array_length first —
// it did. So the emptiness test is a comparison, which is defined for every
// input this column can hold.
func (a *Apps) WithStorage(ctx context.Context) ([]AppWithStorage, error) {
	rows, err := a.db.Query(ctx, `
		SELECT a.id, r.body,
		       coalesce((r.body->'retention'->>'backup_daily_count')::int, 0)
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL
		  AND a.state IN ('running', 'degraded')
		  AND jsonb_typeof(r.body->'volumes') = 'array'
		  AND r.body->'volumes' <> '[]'::jsonb
		ORDER BY a.id`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list apps with storage.", err)
	}
	defer rows.Close()

	out := make([]AppWithStorage, 0)
	for rows.Next() {
		var app AppWithStorage
		if err := rows.Scan(&app.AppID, &app.Spec, &app.Retain); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list apps with storage.", err)
		}
		// Absent means the default, not "keep none" (R-211, issue #87).
		//
		// spec.Defaults.Apply turns a zero into the default before a spec is
		// stored, so no stored spec can ask for zero on purpose. A zero here is
		// a revision stored before defaults were applied on every path — every
		// hand-written spec, for a while — and reading it as "keep none"
		// skipped that app on every pass, silently, for as long as it ran.
		if app.Retain == 0 {
			app.Retain = spec.StandardDefaults().Retention.BackupDailyCount
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// LiveApps lists every app a candidate policy could block, for design 05 §3's
// preview.
//
// Pinned revision, not newest: a preview answers "what would break", and what
// would break is what the app's next deploy would actually try to do. Draft and
// archived apps are excluded — a draft has never deployed and an archived one
// is not going to.
//
// The anonymous grant comes along because a policy that forbids it (R-076) has
// no spec to detect it in, and an app that anyone on the internet can reach
// under a policy that says they may not is precisely the case an admin needs
// named before they save.
func (a *Apps) LiveApps(ctx context.Context) ([]planner.InventoryApp, error) {
	rows, err := a.db.Query(ctx, `
		SELECT a.id, a.name, r.body,
		       EXISTS (SELECT 1 FROM grants g
		               WHERE g.app_id = a.id AND g.plane = 'data'
		                 AND g.principal_kind = 'anonymous'),
		       EXISTS (SELECT 1 FROM grants g
		               WHERE g.app_id = a.id AND g.plane = 'data'
		                 AND g.principal_kind = 'anonymous' AND g.passcode_hash IS NOT NULL)
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL
		  AND a.state NOT IN ('draft', 'proposed', 'archived')
		ORDER BY a.name`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list this installation's apps.", err)
	}
	defer rows.Close()

	out := make([]planner.InventoryApp, 0)
	for rows.Next() {
		var app planner.InventoryApp
		if err := rows.Scan(&app.AppID, &app.Name, &app.Spec, &app.AnonymousGrant, &app.AnonymousPasscode); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list this installation's apps.", err)
		}
		out = append(out, app)
	}
	return out, rows.Err()
}

// OrphanedVolume is storage whose app is gone and whose data is in a backup.
type OrphanedVolume struct {
	VolumeID   string
	AppID      string
	AdapterRef string
	Handle     string
}

// OrphanedVolumes lists storage that is safe to reclaim.
//
// Narrow on purpose. A volume qualifies only when its app is deleted *and* a
// backup of that app exists — R-204 says volumes outlive the apps that mount
// them, and this does not weaken that. It stops the storage outliving the last
// thing that could ever want it, which is a different claim.
//
// A delete made since apps.discard_storage exists removes the volume rows and
// records the decision on the app instead, and the teardown destroys the
// volumes with the bundle (Apps.DiscardStorage). This pass covers volumes whose
// rows a delete left behind before that.
func (a *Apps) OrphanedVolumes(ctx context.Context, limit int) ([]OrphanedVolume, error) {
	rows, err := a.db.Query(ctx, `
		SELECT v.id, v.app_id, v.adapter_ref, coalesce(v.handle, '')
		FROM volumes v
		JOIN apps a ON a.id = v.app_id
		WHERE a.deleted_at IS NOT NULL
		  AND v.handle IS NOT NULL
		  AND EXISTS (
		        SELECT 1 FROM backups b
		        WHERE b.app_id = v.app_id AND b.kind = 'on_delete')
		ORDER BY a.deleted_at
		LIMIT $1`, limit)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list orphaned storage.", err)
	}
	defer rows.Close()

	out := make([]OrphanedVolume, 0)
	for rows.Next() {
		var o OrphanedVolume
		if err := rows.Scan(&o.VolumeID, &o.AppID, &o.AdapterRef, &o.Handle); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list orphaned storage.", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ForgetVolume removes a volume row whose storage has been destroyed.
//
// By app and ID: a volume ID is unique only within its app.
func (a *Apps) ForgetVolume(ctx context.Context, appID, volumeID string) error {
	if _, err := a.db.Exec(ctx, `DELETE FROM volumes WHERE app_id = $1 AND id = $2`, appID, volumeID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the storage record.", err)
	}
	return nil
}

// SetSource replaces an app's source.
//
// For `pando deploy ./`, which turns an app into one fed by uploads. Recorded
// on the app rather than inferred at deploy time: R-020 makes the spec the sole
// record of how an app runs, and a source nobody wrote down is a deploy nobody
// can explain afterwards.
func (a *Apps) SetSource(ctx context.Context, appID string, src spec.Source) error {
	body, err := json.Marshal(src)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's source.", err)
	}
	_, err = a.db.Exec(ctx,
		`UPDATE apps SET source = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`,
		appID, body)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not record the app's source.", err)
	}
	return nil
}

// Volumes records storage attached to apps.
type Volumes struct{ db *DB }

func NewVolumes(db *DB) *Volumes { return &Volumes{db: db} }

// DeleteForApp removes an app's volume rows.
//
// Only reached once the caller has resolved the keep-or-discard decision
// (R-204). The ON DELETE RESTRICT on volumes.app_id means an app cannot be
// archived while rows remain, so this is the explicit step that constraint
// forces — which is exactly why the constraint is there.
func (v *Volumes) DeleteForApp(ctx context.Context, appID string) error {
	if _, err := v.db.Exec(ctx, `DELETE FROM volumes WHERE app_id = $1`, appID); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the app's storage.", err)
	}
	return nil
}

// Create records a volume.
func (v *Volumes) Create(ctx context.Context, appID, name, adapterRef string) (string, error) {
	volumeID := id.New(id.Volume)
	_, err := v.db.Exec(ctx,
		`INSERT INTO volumes (id, app_id, name, adapter_ref) VALUES ($1, $2, $3, $4)`,
		volumeID, appID, name, adapterRef)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Could not add the storage.", err)
	}
	return volumeID, nil
}

// RecordFromRuntime writes the volume rows for an app's spec volumes, using the
// handles the runtime reports.
//
// Nothing used to do this. The spec declared volumes, the planner planned them,
// the runtime created them — and Pando's own `volumes` table stayed empty. Three
// things silently did nothing as a result: `ON DELETE RESTRICT` protected no
// rows, so R-204's "volumes survive app deletion" was a constraint on an empty
// table; deleting an app never offered to keep a backup, because it counted
// zero volumes; and the DR bundle contained no app data at all, because the
// query that finds volumes to snapshot found none.
//
// The ID is the spec's volume ID, not a generated one. A spec that says
// `vol_data` and a row that says `vol_01HQ8…` are two names for one thing, and
// a restore matching them up is a join nobody wrote.
//
// Idempotent: every deploy re-records, and the handle is refreshed in case the
// runtime's own naming changed under it.
//
// Keyed by app and ID together (migration 000036). The ID is only unique
// within an app — two apps may both keep a volume called "data" — and keyed by
// ID alone, the second app's deploy overwrote the first app's row and recorded
// nothing for its own, so it was never backed up (issue #87).
func (v *Volumes) RecordFromRuntime(ctx context.Context, appID, adapterRef string, observed []VolumeRecord) error {
	for _, o := range observed {
		if o.VolumeID == "" {
			continue
		}
		_, err := v.db.Exec(ctx, `
			INSERT INTO volumes (id, app_id, name, adapter_ref, handle)
			VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (app_id, id) DO UPDATE
			  SET handle = excluded.handle, adapter_ref = excluded.adapter_ref`,
			o.VolumeID, appID, o.Name, adapterRef, nullable(o.Handle))
		if err != nil {
			return errs.Wrap(errs.Internal, "Could not record the app's storage.", err)
		}
	}
	return nil
}

// VolumeRecord is one volume as the runtime reports it.
type VolumeRecord struct {
	VolumeID string
	Name     string
	Handle   string
}

// SetDesiredState records what a human asked for.
//
// Separate from state, which is what is true. The reconciler reads both: the
// gap between them is the work it has to do.
func (a *Apps) SetDesiredState(ctx context.Context, appID, desired string) error {
	switch desired {
	case "running", "stopped":
	default:
		return errs.New(errs.ValidInvalid, "An app is either running or stopped.")
	}
	_, err := a.db.Exec(ctx,
		`UPDATE apps SET desired_state = $2, updated_at = now() WHERE id = $1`, appID, desired)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the app.", err)
	}
	return nil
}

// SetState records what is observed to be true.
func (a *Apps) SetState(ctx context.Context, appID, appState string) error {
	_, err := a.db.Exec(ctx,
		`UPDATE apps SET state = $2, updated_at = now() WHERE id = $1 AND deleted_at IS NULL`, appID, appState)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the app.", err)
	}
	return nil
}

// SetStateIf moves an app to `to` only if it is in `from`, so a caller
// settling its own transition never overwrites one somebody else made since.
func (a *Apps) SetStateIf(ctx context.Context, appID, from, to string) error {
	_, err := a.db.Exec(ctx,
		`UPDATE apps SET state = $3, updated_at = now() WHERE id = $1 AND state = $2 AND deleted_at IS NULL`,
		appID, from, to)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not update the app.", err)
	}
	return nil
}

// ByRouting resolves a running app from how it is addressed.
//
// Used by the proxy on every request, so it reads the pinned spec in the same
// query rather than making a second round trip per request.
//
// `by` is "hostname" or "slug". A hostname lookup reads the pinned spec's
// routing block; a slug lookup reads the app's own column.
func (a *Apps) ByRouting(ctx context.Context, by, value string) (App, *spec.AppSpec, bool, error) {
	var where string
	switch by {
	case "hostname":
		where = `r.body->'routing'->>'hostname' = $1`
	case "slug":
		where = `a.slug = $1`
	case "id":
		where = `a.id = $1`
	case "port":
		// The mode is part of the match, not just the number. A port-mode app's
		// routing block is the only place a port means "this app's address";
		// leaving the mode out would let a path-mode app whose routing happened
		// to record a port answer on a listener that is not its own.
		where = `r.body->'routing'->>'mode' = 'port' AND r.body->'routing'->>'port' = $1`
	default:
		return App{}, nil, false, errs.Newf(errs.Internal, "Unknown routing lookup %q.", by)
	}

	var app App
	var owner *string
	var body []byte
	err := a.db.QueryRow(ctx, `
		SELECT a.id, a.name, a.slug, a.owner_user_id, a.state, a.desired_state, a.pinned_spec_id,
		       a.created_at, a.updated_at, r.body
		FROM apps a
		JOIN spec_revisions r ON r.id = a.pinned_spec_id
		WHERE a.deleted_at IS NULL AND `+where, value).
		Scan(&app.ID, &app.Name, &app.Slug, &owner, &app.State, &app.DesiredState, &app.PinnedSpecID,
			&app.CreatedAt, &app.UpdatedAt, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return App{}, nil, false, nil
	}
	if err != nil {
		return App{}, nil, false, errs.Wrap(errs.Internal, "Could not look up the app.", err)
	}
	if owner != nil {
		app.OwnerUserID = *owner
	}

	var s spec.AppSpec
	if err := json.Unmarshal(body, &s); err != nil {
		return App{}, nil, false, errs.Wrap(errs.Internal, "Could not read the app's spec.", err)
	}
	return app, &s, true, nil
}

// Handles reports each of an app's recorded volumes and the runtime handle it
// was last seen under — empty for a volume that was never materialized.
//
// A non-empty handle is the distinction R-203 turns on. A volume row with a
// handle was created in the runtime and may hold data; recreating it after it
// disappears produces an empty replacement and an app that comes up healthy
// having lost everything — the failure that looks exactly like success. A row
// without a handle has never existed anywhere, so creating it loses nothing.
func (v *Volumes) Handles(ctx context.Context, appID string) (map[string]string, error) {
	rows, err := v.db.Query(ctx,
		`SELECT id, coalesce(handle, '') FROM volumes WHERE app_id = $1`, appID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the app's storage.", err)
	}
	defer rows.Close()

	out := map[string]string{}
	for rows.Next() {
		var volumeID, handle string
		if err := rows.Scan(&volumeID, &handle); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the app's storage.", err)
		}
		out[volumeID] = handle
	}
	return out, rows.Err()
}

// PruneSpecRevisions trims revision history to each app's retention setting.
//
// R-152 keeps the last ten pinned specs so a rollback has something to roll
// back to. Two exemptions, and both matter more than the saving:
//
//   - **A revision that was ever pinned is never pruned.** Rollback is
//     repointing at a revision that provably existed, and spec_pins is the
//     append-only record of which those are. Pruning one would make a rollback
//     target vanish from under a person who is looking at it.
//   - **The currently pinned revision is never pruned**, which the first rule
//     already covers, but a deployment also references it by foreign key and
//     would refuse.
//   - **A revision a scan describes is never pruned** (R-319). The scan is
//     append-only and names its revision; the foreign key refuses the delete.
//
// Returns how many rows went.
func (a *Apps) PruneSpecRevisions(ctx context.Context) (int, error) {
	tag, err := a.db.Exec(ctx, `
		WITH keep AS (
		    SELECT r.id
		    FROM spec_revisions r
		    JOIN apps app ON app.id = r.app_id
		    WHERE r.revision > (
		        SELECT coalesce(max(r2.revision), 0) - coalesce(
		            (SELECT (pinned.body->'retention'->>'spec_revisions')::int
		             FROM spec_revisions pinned WHERE pinned.id = app.pinned_spec_id),
		            10)
		        FROM spec_revisions r2 WHERE r2.app_id = r.app_id
		    )
		)
		DELETE FROM spec_revisions r
		WHERE r.id NOT IN (SELECT id FROM keep)
		  -- Never a revision that was ever pinned: rollback is repointing at
		  -- something that provably existed, and this is that proof.
		  AND NOT EXISTS (SELECT 1 FROM spec_pins p WHERE p.spec_id = r.id)
		  -- Never one a deployment refers to.
		  AND NOT EXISTS (SELECT 1 FROM deployments d WHERE d.spec_id = r.id)
		  -- Never one a scan describes. A scan is an append-only fact about
		  -- a revision (R-319); deleting the revision would leave it naming
		  -- nothing, and the foreign key refuses that.
		  AND NOT EXISTS (SELECT 1 FROM app_scans s WHERE s.spec_id = r.id)
	`)
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Could not prune old spec revisions.", err)
	}
	return int(tag.RowsAffected()), nil
}

// WithAutoDeploy returns apps that track a ref (R-141).
//
// Off by default, so this is normally empty and the poll costs one indexed
// query. The filter is on the pinned spec rather than a column on the app,
// because auto-deploy is a property of the spec someone reviewed and pinned —
// putting it on the app row would let it be changed without a revision.
func (a *Apps) WithAutoDeploy(ctx context.Context) ([]App, error) {
	rows, err := a.db.Query(ctx, `
		SELECT app.id, app.name, app.slug, app.owner_user_id, app.state,
		       app.desired_state, app.pinned_spec_id, app.created_at, app.updated_at
		FROM apps app
		JOIN spec_revisions r ON r.id = app.pinned_spec_id
		WHERE app.deleted_at IS NULL
		  AND app.state NOT IN ('failed', 'archived', 'deploying')
		  AND (r.body->'deploy'->'auto_deploy'->>'enabled')::boolean IS TRUE
	`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list apps tracking a branch.", err)
	}
	defer rows.Close()

	var out []App
	for rows.Next() {
		var app App
		var owner, pinned *string
		if err := rows.Scan(&app.ID, &app.Name, &app.Slug, &owner, &app.State,
			&app.DesiredState, &pinned, &app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list apps tracking a branch.", err)
		}
		if owner != nil {
			app.OwnerUserID = *owner
		}
		if pinned != nil {
			app.PinnedSpecID = *pinned
		}
		out = append(out, app)
	}
	return out, rows.Err()
}
