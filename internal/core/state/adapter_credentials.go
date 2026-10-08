package state

import (
	"context"
	"fmt"
	"regexp"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// AdapterCredentials stores the credentials an adapter needs, encrypted
// (R-190, R-194; O-20).
//
// The same arrangement as Secrets, one scope up: the row holds ciphertext or an
// external reference that the install's secrets adapter produced, and there is
// no plaintext column. Core never encrypts. adapter_configs.config stays what it
// always was — non-secret settings — and the database refuses a `credentials`
// key in it, so this is the only place a credential can be kept.
type AdapterCredentials struct {
	db         *DB
	adapter    api.SecretsAdapter
	adapterRef string

	// table, column and scope say whose credentials these are: adapter_configs',
	// identity providers' (identity_adapter_credentials), or apps' registry
	// credentials. Fixed at construction from the constructors below, never
	// from input.
	table  string
	column string
	scope  string
}

func NewAdapterCredentials(db *DB, adapter api.SecretsAdapter, adapterRef string) *AdapterCredentials {
	return &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "adapter_credentials", column: "adapter_id", scope: "adapter:"}
}

// NewRegistryCredentials stores the credential an app's image is pulled with
// (issue #41), keyed by the app. Its own table and scope rather than the app's
// secrets: an app secret can be named by an env entry and so reach the app,
// and the credential that pulls the app's image is never the app's to read.
// App-owned (O-30). A source credential is not: since issue #127 a source
// connection is the install's (O-3), stored in adapter_credentials.
func NewRegistryCredentials(db *DB, adapter api.SecretsAdapter, adapterRef string) *AdapterCredentials {
	return &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "registry_credentials", column: "app_id", scope: "registry:"}
}

// NewSourceAuthorizations stores an OAuth authorization of a source connection
// while it is in progress (issue #127): the device code or PKCE verifier, and
// the state a web callback presents. Sealed like a credential and under its own
// scope, so a pending flow cannot be replayed as the connection's token.
func NewSourceAuthorizations(db *DB, adapter api.SecretsAdapter, adapterRef string) *AdapterCredentials {
	return &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "source_authorizations", column: "adapter_id", scope: "source-authorization:"}
}

// NewIdentityCredentials stores identity providers' secrets — an OIDC client
// secret — the same way, in their own table (identity_adapters is not
// adapter_configs). A different scope in the sealed reference as well, so a
// provider's ciphertext cannot be replayed as an adapter's even if two IDs
// matched.
func NewIdentityCredentials(db *DB, adapter api.SecretsAdapter, adapterRef string) *AdapterCredentials {
	return &AdapterCredentials{db: db, adapter: adapter, adapterRef: adapterRef,
		table: "identity_adapter_credentials", column: "adapter_id", scope: "identity:"}
}

var credentialField = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// credentialRef scopes a credential to its adapter.
//
// SecretRef.AppID carries the scope, and the local secrets adapter binds it into
// the ciphertext as authenticated data. "adapter:" cannot collide with an app ID,
// which is always prefixed app_, so a credential's ciphertext cannot be replayed
// as an app secret or the other way round.
func credentialRef(scope, adapterID, field string) api.SecretRef {
	return api.SecretRef{AppID: scope + adapterID, Key: field}
}

// Put stores or rotates one credential. An empty value removes it.
func (c *AdapterCredentials) Put(ctx context.Context, adapterID, field string, v secret.Value) error {
	if c == nil || c.adapter == nil {
		return errs.New(errs.StateInvalid,
			"Pando has no secrets adapter configured, so it cannot store an adapter's credentials.").
			WithRemedy("Configure a secrets adapter, restart Pando, and try again.")
	}
	if !credentialField.MatchString(field) {
		return errs.Newf(errs.ValidInvalid,
			"%q is not a usable credential name. Use lowercase letters, digits and underscores, such as api_key.", field)
	}
	if v.IsZero() {
		return c.Delete(ctx, adapterID, field)
	}

	stored, err := c.adapter.Put(ctx, credentialRef(c.scope, adapterID, field), v)
	if err != nil {
		return err
	}
	_, err = c.db.Exec(ctx, `
		INSERT INTO `+c.table+` AS t (`+c.column+`, field, adapter_ref, ciphertext, external_ref)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (`+c.column+`, field) DO UPDATE SET
			adapter_ref = EXCLUDED.adapter_ref,
			ciphertext = EXCLUDED.ciphertext,
			external_ref = EXCLUDED.external_ref,
			version = t.version + 1,
			updated_at = now()`,
		adapterID, field, c.adapterRef, stored.Ciphertext, nullable(stored.Handle))
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not store the adapter's credential.", err)
	}
	return nil
}

// Delete removes one credential.
func (c *AdapterCredentials) Delete(ctx context.Context, adapterID, field string) error {
	_, err := c.db.Exec(ctx,
		`DELETE FROM `+c.table+` WHERE `+c.column+` = $1 AND field = $2`, adapterID, field)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not remove the adapter's credential.", err)
	}
	return nil
}

// Fields names the credentials each adapter has, never their values. It is what
// GET /adapters reports, so the console can say "an API key is set" without
// anything being able to read it back.
func (c *AdapterCredentials) Fields(ctx context.Context) (map[string][]string, error) {
	rows, err := c.db.Query(ctx,
		`SELECT `+c.column+`, field FROM `+c.table+` ORDER BY `+c.column+`, field`)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not list adapter credentials.", err)
	}
	defer rows.Close()

	out := map[string][]string{}
	for rows.Next() {
		var adapterID, field string
		if err := rows.Scan(&adapterID, &field); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not list adapter credentials.", err)
		}
		out[adapterID] = append(out[adapterID], field)
	}
	return out, rows.Err()
}

// Resolve decrypts one adapter's credentials, for Configure at startup.
//
// The only place they leave storage. The values stay secret.Value until the
// adapter reads them, so none of them can be logged on the way.
func (c *AdapterCredentials) Resolve(ctx context.Context, adapterID string) (map[string]secret.Value, error) {
	rows, err := c.db.Query(ctx,
		`SELECT field, ciphertext, external_ref FROM `+c.table+` WHERE `+c.column+` = $1`, adapterID)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the adapter's credentials.", err)
	}
	defer rows.Close()

	type sealed struct {
		field      string
		ciphertext []byte
		handle     *string
	}
	var all []sealed
	for rows.Next() {
		var s sealed
		if err := rows.Scan(&s.field, &s.ciphertext, &s.handle); err != nil {
			return nil, errs.Wrap(errs.Internal, "Could not read the adapter's credentials.", err)
		}
		all = append(all, s)
	}
	if err := rows.Err(); err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the adapter's credentials.", err)
	}
	if len(all) == 0 {
		return nil, nil
	}
	if c.adapter == nil {
		return nil, fmt.Errorf("adapter %s has stored credentials and no secrets adapter is configured to open them", adapterID)
	}

	out := make(map[string]secret.Value, len(all))
	for _, s := range all {
		ref := credentialRef(c.scope, adapterID, s.field)
		stored := api.StoredRef{AppID: ref.AppID, Key: ref.Key, Ciphertext: s.ciphertext}
		if s.handle != nil {
			stored.Handle = *s.handle
		}
		v, err := c.adapter.Get(ctx, stored)
		if err != nil {
			return nil, err
		}
		out[s.field] = v
	}
	return out, nil
}
