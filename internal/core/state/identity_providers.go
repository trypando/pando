package state

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
)

// IdentityProvider is a configured identity adapter instance (R-040, R-045):
// the local one seeded on every install, and any an administrator adds.
type IdentityProvider struct {
	ID      string          `json:"id"`
	Kind    string          `json:"kind"`
	Name    string          `json:"name"`
	Config  json.RawMessage `json:"config"`
	Enabled bool            `json:"enabled"`

	// JITProvisioning makes an account at a first sign-in. Off unless an
	// administrator turns it on; host policy can refuse it everywhere.
	JITProvisioning bool `json:"jit_provisioning"`

	// LinkByEmail matches a first sign-in to an existing account by an email
	// the provider vouches for (O-1). Off unless turned on.
	LinkByEmail bool `json:"link_by_email"`

	// SCIM is on when a token is set. The token itself is shown once and
	// kept only as a digest.
	SCIMEnabled           bool       `json:"scim_enabled"`
	SCIMTokenCreatedAt    *time.Time `json:"scim_token_created_at,omitempty"`
	SCIMIdentityAttribute string     `json:"scim_identity_attribute"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// DefaultSCIMIdentityAttribute is which SCIM attribute a provider of this
// kind signs people in with, when the administrator has not said.
func DefaultSCIMIdentityAttribute(kind string) string {
	if kind == "saml" {
		return "userName"
	}
	return "externalId"
}

// IdentityProviders reads and writes identity_adapters.
type IdentityProviders struct{ db *DB }

func NewIdentityProviders(db *DB) *IdentityProviders { return &IdentityProviders{db: db} }

const providerColumns = `id, kind, name, config, enabled, jit_provisioning, link_by_email,
	scim_token_hash IS NOT NULL, scim_token_created_at, coalesce(scim_identity_attribute, ''), created_at, updated_at`

func scanProvider(row pgx.Row) (IdentityProvider, error) {
	var p IdentityProvider
	err := row.Scan(&p.ID, &p.Kind, &p.Name, &p.Config, &p.Enabled, &p.JITProvisioning, &p.LinkByEmail,
		&p.SCIMEnabled, &p.SCIMTokenCreatedAt, &p.SCIMIdentityAttribute, &p.CreatedAt, &p.UpdatedAt)
	if p.SCIMIdentityAttribute == "" {
		p.SCIMIdentityAttribute = DefaultSCIMIdentityAttribute(p.Kind)
	}
	return p, err
}

// List returns every provider, the local one first.
func (s *IdentityProviders) List(ctx context.Context) ([]IdentityProvider, error) {
	rows, err := s.db.Query(ctx, `SELECT `+providerColumns+` FROM identity_adapters
		ORDER BY kind <> 'local', lower(name)`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the identity providers.", err)
	}
	defer rows.Close()
	out := []IdentityProvider{}
	for rows.Next() {
		p, err := scanProvider(rows)
		if err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the identity providers.", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ByID returns one provider.
func (s *IdentityProviders) ByID(ctx context.Context, providerID string) (IdentityProvider, bool, error) {
	p, err := scanProvider(s.db.QueryRow(ctx, `SELECT `+providerColumns+` FROM identity_adapters WHERE id = $1`, providerID))
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityProvider{}, false, nil
	}
	if err != nil {
		return IdentityProvider{}, false, errs.Wrap(errs.Internal, "Could not read the identity provider.", err)
	}
	return p, true, nil
}

// BySCIMToken finds the provider a SCIM bearer token belongs to, by digest.
func (s *IdentityProviders) BySCIMToken(ctx context.Context, digest string) (IdentityProvider, bool, error) {
	p, err := scanProvider(s.db.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM identity_adapters WHERE scim_token_hash = $1`, digest))
	if errors.Is(err, pgx.ErrNoRows) {
		return IdentityProvider{}, false, nil
	}
	if err != nil {
		return IdentityProvider{}, false, errs.Wrap(errs.Internal, "Could not read the identity provider.", err)
	}
	return p, true, nil
}

// ProviderSettings are the fields an administrator sets. A nil field is left
// as it is on update.
type ProviderSettings struct {
	// ID is a new provider's ID when the caller chose it, so the addresses
	// built from it could be registered with the provider first. Empty is a
	// fresh one. Not used by Update.
	ID                    string
	Name                  *string
	Config                json.RawMessage
	Enabled               *bool
	JITProvisioning       *bool
	LinkByEmail           *bool
	SCIMIdentityAttribute *string
}

// Create adds a provider.
func (s *IdentityProviders) Create(ctx context.Context, kind string, set ProviderSettings) (IdentityProvider, error) {
	providerID := set.ID
	if providerID == "" {
		providerID = id.New(id.IdentityAdpt)
	}
	cfg := set.Config
	if len(cfg) == 0 {
		cfg = json.RawMessage(`{}`)
	}
	name := ""
	if set.Name != nil {
		name = strings.TrimSpace(*set.Name)
	}
	enabled := set.Enabled == nil || *set.Enabled
	jit := set.JITProvisioning != nil && *set.JITProvisioning
	link := set.LinkByEmail != nil && *set.LinkByEmail
	_, err := s.db.Exec(ctx, `
		INSERT INTO identity_adapters (id, kind, name, config, enabled, jit_provisioning, link_by_email, scim_identity_attribute)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		providerID, kind, name, cfg, enabled, jit, link, nullableStr(set.SCIMIdentityAttribute))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.ConstraintName == "identity_adapters_pkey" {
		// Two creates with one ID, both past the service's check (R-105).
		return IdentityProvider{}, ProviderIDTaken(providerID)
	}
	if err != nil {
		return IdentityProvider{}, providerWriteError(err, name)
	}
	p, _, err := s.ByID(ctx, providerID)
	return p, err
}

// Update changes a provider's settings.
func (s *IdentityProviders) Update(ctx context.Context, providerID string, set ProviderSettings) (IdentityProvider, error) {
	var cfg any
	if len(set.Config) > 0 {
		cfg = set.Config
	}
	var name any
	if set.Name != nil {
		name = strings.TrimSpace(*set.Name)
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE identity_adapters SET
			name = coalesce($2, name),
			config = coalesce($3::jsonb, config),
			enabled = coalesce($4, enabled),
			jit_provisioning = coalesce($5, jit_provisioning),
			link_by_email = coalesce($6, link_by_email),
			scim_identity_attribute = CASE WHEN $7::text IS NULL THEN scim_identity_attribute ELSE nullif($7, '') END,
			updated_at = now()
		WHERE id = $1`,
		providerID, name, cfg, set.Enabled, set.JITProvisioning, set.LinkByEmail, set.SCIMIdentityAttribute)
	if err != nil {
		n := ""
		if set.Name != nil {
			n = *set.Name
		}
		return IdentityProvider{}, providerWriteError(err, n)
	}
	if tag.RowsAffected() == 0 {
		return IdentityProvider{}, errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	p, _, err := s.ByID(ctx, providerID)
	return p, err
}

// ProviderIDTaken refuses a new provider an ID another already has. A create
// sent twice, its first answer lost, lands here too, and the provider it
// added is the one wanted: its addresses are already registered upstream.
func ProviderIDTaken(providerID string) error {
	return errs.Newf(errs.ValidInvalid, "Another identity provider has the ID %s.", providerID).
		WithRemedy("If this provider was just added, it was saved: find it in the list of identity providers. " +
			"Otherwise leave id out and Pando chooses one.")
}

func providerWriteError(err error, name string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.ConstraintName {
		case "identity_adapters_name_unique":
			return errs.Newf(errs.ValidInvalid, "There is already an identity provider called %q.", name).
				WithRemedy("Choose a different name. People see it on the sign-in page, so make it one they recognize.")
		case "identity_adapters_no_inline_credentials":
			return errs.New(errs.ValidInvalid, "A secret cannot be stored in a provider's settings.").
				WithRemedy("Send it in \"credentials\" instead, where it is stored encrypted.")
		case "identity_adapters_kind_check":
			return errs.New(errs.ValidInvalid, "Pando has no identity provider of that kind.").
				WithRemedy("Use oidc or saml.")
		case "identity_adapters_scim_identity_attribute_check":
			return errs.New(errs.ValidInvalid, "The SCIM identity attribute is externalId or userName.")
		}
	}
	return errs.Wrap(errs.Internal, "Could not save the identity provider.", err)
}

// Delete removes a provider nobody has used. One that has signed anyone in
// is referenced by their account, their sessions and the audit log, and is
// disabled instead — deleting it would free its identities for reuse.
func (s *IdentityProviders) Delete(ctx context.Context, providerID string) error {
	if providerID == LocalAdapterID {
		return errs.New(errs.ValidInvalid, "Local accounts cannot be removed.").
			WithRemedy("To stop password sign-in, turn on disable_password_sign_in in host policy.")
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var used bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM users WHERE adapter_id = $1)
		    OR EXISTS (SELECT 1 FROM user_identities WHERE adapter_id = $1)
		    OR EXISTS (SELECT 1 FROM sessions WHERE adapter_id = $1)`, providerID).Scan(&used); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	if used {
		return errs.New(errs.ValidInvalid,
			"Someone has signed in through this identity provider, so Pando keeps it and its accounts.").
			WithRemedy("Turn it off instead: nobody can sign in through a provider that is off.")
	}
	// Groups it synced go with it, and their grants with them, as when an
	// administrator deletes a group.
	if _, err := tx.Exec(ctx, `
		DELETE FROM grants WHERE principal_kind = 'group'
		  AND principal_id IN (SELECT id FROM groups WHERE adapter_id = $1)`, providerID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM groups WHERE adapter_id = $1`, providerID); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	tag, err := tx.Exec(ctx, `DELETE FROM identity_adapters WHERE id = $1`, providerID)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	if err := tx.Commit(ctx); err != nil {
		return errs.Wrap(errs.Internal, "Could not delete the identity provider.", err)
	}
	return nil
}

// SetSCIMToken turns SCIM on, or rotates its token, storing only the digest.
// An empty digest turns SCIM off.
func (s *IdentityProviders) SetSCIMToken(ctx context.Context, providerID, digest string) error {
	tag, err := s.db.Exec(ctx, `
		UPDATE identity_adapters SET
			scim_token_hash = nullif($2, ''),
			scim_token_created_at = CASE WHEN $2 = '' THEN NULL ELSE now() END,
			updated_at = now()
		WHERE id = $1 AND kind <> 'local'`, providerID, digest)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not change the provider's SCIM token.", err)
	}
	if tag.RowsAffected() == 0 {
		return errs.New(errs.NotFound, "There is no external identity provider with that ID.")
	}
	return nil
}

// AnyExternalEnabled reports whether an external provider is on — the
// condition for turning password sign-in off.
func (s *IdentityProviders) AnyExternalEnabled(ctx context.Context) (bool, error) {
	var ok bool
	if err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM identity_adapters WHERE kind <> 'local' AND enabled)`).Scan(&ok); err != nil {
		return false, errs.Wrap(errs.Internal, "Could not read the identity providers.", err)
	}
	return ok, nil
}

func nullableStr(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}
