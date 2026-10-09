package state

import (
	"context"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// Launcher sections (R-342): groupings a person makes in their own launcher.
//
// Everything here is keyed on the user, and every query says so. A section or
// placement belonging to someone else is indistinguishable from one that does
// not exist.

// Section is one of a person's launcher groupings.
type Section struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// maxSectionName matches the table's CHECK.
const maxSectionName = 80

func sectionName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errs.New(errs.ValidInvalid, "A section needs a name.")
	}
	if len([]rune(name)) > maxSectionName {
		return "", errs.Newf(errs.ValidInvalid, "A section's name can be at most %d characters.", maxSectionName)
	}
	return name, nil
}

// Sections returns a person's sections, oldest first — the order they made
// them, which is the order they appear in.
func (a *Apps) Sections(ctx context.Context, userID string) ([]Section, error) {
	return queryAll(ctx, a.db, "Could not read your sections.",
		func(r pgx.CollectableRow) (s Section, err error) {
			return s, r.Scan(&s.ID, &s.Name, &s.CreatedAt)
		}, `
		SELECT id, name, created_at FROM launcher_sections
		WHERE user_id = $1 ORDER BY created_at, id`, userID)
}

// CreateSection makes a new section for a person.
func (a *Apps) CreateSection(ctx context.Context, userID, name string) (Section, error) {
	name, err := sectionName(name)
	if err != nil {
		return Section{}, err
	}
	s := Section{ID: id.New(id.Section), Name: name}
	err = a.db.QueryRow(ctx, `
		INSERT INTO launcher_sections (id, user_id, name) VALUES ($1, $2, $3)
		RETURNING created_at`, s.ID, userID, name).Scan(&s.CreatedAt)
	if isUniqueViolation(err) {
		return Section{}, duplicateSection(name)
	}
	if err != nil {
		return Section{}, errs.Wrap(errs.Internal, "Could not create the section.", err)
	}
	return s, nil
}

// RenameSection renames one of a person's sections. False when they have no
// section with that ID.
func (a *Apps) RenameSection(ctx context.Context, userID, sectionID, name string) (Section, bool, error) {
	name, err := sectionName(name)
	if err != nil {
		return Section{}, false, err
	}
	s := Section{ID: sectionID, Name: name}
	tag, err := a.db.Exec(ctx, `
		UPDATE launcher_sections SET name = $3, updated_at = now()
		WHERE id = $1 AND user_id = $2`, sectionID, userID, name)
	if isUniqueViolation(err) {
		return Section{}, false, duplicateSection(name)
	}
	if err != nil {
		return Section{}, false, errs.Wrap(errs.Internal, "Could not rename the section.", err)
	}
	if tag.RowsAffected() == 0 {
		return Section{}, false, nil
	}
	err = a.db.QueryRow(ctx, `SELECT created_at FROM launcher_sections WHERE id = $1`, sectionID).Scan(&s.CreatedAt)
	if err != nil {
		return Section{}, false, errs.Wrap(errs.Internal, "Could not rename the section.", err)
	}
	return s, true, nil
}

// DeleteSection removes one of a person's sections. Its apps go back to "Your
// apps" by the placements' cascade. False when they have no such section.
func (a *Apps) DeleteSection(ctx context.Context, userID, sectionID string) (bool, error) {
	tag, err := a.db.Exec(ctx,
		`DELETE FROM launcher_sections WHERE id = $1 AND user_id = $2`, sectionID, userID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not delete the section.", err)
	}
	return tag.RowsAffected() > 0, nil
}

// PlaceApp puts an app into one of a person's sections, moving it out of any
// other. False when they have no such section.
func (a *Apps) PlaceApp(ctx context.Context, userID, sectionID, appID string) (bool, error) {
	tag, err := a.db.Exec(ctx, `
		INSERT INTO launcher_placements (user_id, app_id, section_id)
		SELECT $1, $2, s.id FROM launcher_sections s WHERE s.id = $3 AND s.user_id = $1
		ON CONFLICT (user_id, app_id) DO UPDATE SET section_id = EXCLUDED.section_id`,
		userID, appID, sectionID)
	if err != nil {
		return false, errs.Wrap(errs.Internal, "Could not move the app.", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UnplaceApp takes an app out of a section, back to "Your apps". Taking out an
// app that is not in it is not an error.
func (a *Apps) UnplaceApp(ctx context.Context, userID, sectionID, appID string) error {
	_, err := a.db.Exec(ctx, `
		DELETE FROM launcher_placements WHERE user_id = $1 AND app_id = $2 AND section_id = $3`,
		userID, appID, sectionID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not move the app.", err)
	}
	return nil
}

func duplicateSection(name string) error {
	return errs.Newf(errs.ValidInvalid, "You already have a section called %q.", name).
		WithRemedy("Choose a different name, or move the app into the section you already have.")
}
