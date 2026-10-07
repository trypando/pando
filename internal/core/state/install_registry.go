package state

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// InstallRegistry stores the install's image registry as set from the console
// and the API (issue #72, PR 5; migration 000051). Settings in one row, the
// password sealed by the install's secrets adapter in another table, never in
// the clear (R-190).
type InstallRegistry struct {
	db    *DB
	creds *AdapterCredentials
}

// installRegistryID keys the one credential row.
const installRegistryID = "install"

// NewInstallRegistry reads and writes the stored registry. adapter seals the
// password; nil leaves the settings writable and refuses a password.
func NewInstallRegistry(db *DB, adapter api.SecretsAdapter, adapterRef string) *InstallRegistry {
	return &InstallRegistry{db: db, creds: &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "install_registry_credentials", column: "registry_id", scope: "install-registry:"}}
}

// StoredRegistry is the stored settings. No password: that is Password.
type StoredRegistry struct {
	URL       string
	Username  string
	Kind      string
	Layout    string
	Insecure  bool
	Always    bool
	UpdatedBy string
	UpdatedAt time.Time
}

// Load returns the stored settings, and false when none were ever saved.
func (s *InstallRegistry) Load(ctx context.Context) (StoredRegistry, bool, error) {
	var r StoredRegistry
	var by *string
	err := s.db.QueryRow(ctx, `
		SELECT url, username, kind, layout, insecure, always, updated_by, updated_at
		FROM install_registry`).Scan(&r.URL, &r.Username, &r.Kind, &r.Layout, &r.Insecure, &r.Always, &by, &r.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return StoredRegistry{}, false, nil
	}
	if err != nil {
		return StoredRegistry{}, false, errs.Wrap(errs.Internal, "Could not read the install registry's settings.", err)
	}
	if by != nil {
		r.UpdatedBy = *by
	}
	return r, true, nil
}

// Save replaces the stored settings.
func (s *InstallRegistry) Save(ctx context.Context, r StoredRegistry, by string) error {
	_, err := s.db.Exec(ctx, `
		INSERT INTO install_registry (id, url, username, kind, layout, insecure, always, updated_by, updated_at)
		VALUES (true, $1, $2, $3, $4, $5, $6, NULLIF($7, ''), now())
		ON CONFLICT (id) DO UPDATE SET
			url = EXCLUDED.url, username = EXCLUDED.username, kind = EXCLUDED.kind,
			layout = EXCLUDED.layout, insecure = EXCLUDED.insecure, always = EXCLUDED.always,
			updated_by = EXCLUDED.updated_by, updated_at = now()`,
		r.URL, r.Username, orDefault(r.Kind, "basic"), orDefault(r.Layout, "per_app"), r.Insecure, r.Always, by)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the install registry's settings.", err)
	}
	return nil
}

// Clear removes the stored settings and password.
func (s *InstallRegistry) Clear(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM install_registry`); err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the install registry's settings.", err)
	}
	return s.creds.Delete(ctx, installRegistryID, "password")
}

// SetPassword seals and stores the password; a zero value removes it.
func (s *InstallRegistry) SetPassword(ctx context.Context, v secret.Value) error {
	return s.creds.Put(ctx, installRegistryID, "password", v)
}

// Password opens the stored password, and false when there is none. Read on
// every push and pull, so a password changed on one replica is the one every
// replica uses next, without a restart.
func (s *InstallRegistry) Password(ctx context.Context) (secret.Value, bool, error) {
	all, err := s.creds.Resolve(ctx, installRegistryID)
	if err != nil {
		return secret.Value{}, false, err
	}
	v, ok := all["password"]
	return v, ok && !v.IsZero(), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}
