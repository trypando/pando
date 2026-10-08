//go:build integration

package state_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/secret"
)

func credentialStore(t *testing.T, db *state.DB) *state.AdapterCredentials {
	t.Helper()
	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(context.Background(), cfg))
	return state.NewAdapterCredentials(db, sa, "sek_local")
}

// TestR190_AnAdapterCredentialIsStoredOnlyAsCiphertext asserts R-190 for
// adapter credentials (O-20): no column anywhere holds the plaintext.
func TestR190_AnAdapterCredentialIsStoredOnlyAsCiphertext(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	require.NoError(t, state.NewAdapters(db).Upsert(ctx, state.AdapterConfig{
		ID: "ai_anthropic", Category: "ai", Kind: "anthropic", Name: "Anthropic", Enabled: true,
	}), "the ai category is accepted (R-258)")

	creds := credentialStore(t, db)
	const key = "sk-ant-must-never-be-stored-in-the-clear"
	require.NoError(t, creds.Put(ctx, "ai_anthropic", "api_key", secret.New(key)))

	var ciphertext []byte
	var config string
	require.NoError(t, db.QueryRow(ctx,
		`SELECT ciphertext FROM adapter_credentials WHERE adapter_id = 'ai_anthropic'`).Scan(&ciphertext))
	require.NoError(t, db.QueryRow(ctx,
		`SELECT config::text FROM adapter_configs WHERE id = 'ai_anthropic'`).Scan(&config))
	require.False(t, bytes.Contains(ciphertext, []byte(key)))
	require.NotContains(t, config, key)

	got, err := creds.Resolve(ctx, "ai_anthropic")
	require.NoError(t, err)
	require.Equal(t, key, got["api_key"].Reveal())

	fields, err := creds.Fields(ctx)
	require.NoError(t, err)
	require.Equal(t, []string{"api_key"}, fields["ai_anthropic"], "names are listable; values are not")

	require.NoError(t, creds.Put(ctx, "ai_anthropic", "api_key", secret.Value{}), "empty removes")
	got, err = creds.Resolve(ctx, "ai_anthropic")
	require.NoError(t, err)
	require.Empty(t, got)
}

// TestR190_TheDatabaseRefusesCredentialsInPlainConfiguration asserts O-20's
// mechanism: a check constraint, not only the handler.
func TestR190_TheDatabaseRefusesCredentialsInPlainConfiguration(t *testing.T) {
	t.Parallel()
	db := connected(t)
	err := state.NewAdapters(db).Upsert(context.Background(), state.AdapterConfig{
		ID: "ai_sneaky", Category: "ai", Kind: "anthropic", Name: "x", Enabled: true,
		Config: json.RawMessage(`{"credentials":{"api_key":"sk-ant-plain"}}`),
	})
	require.Error(t, err)
}

// TestR190_TheDatabaseRefusesARegistryAddressCarryingAPassword asserts the
// mechanism 000051 had for the install registry, kept when it became an
// adapter (issue #153): an image registry whose address carries a username
// and password is refused by a check constraint, not only by the adapter.
func TestR190_TheDatabaseRefusesARegistryAddressCarryingAPassword(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	adapters := state.NewAdapters(connected(t))
	err := adapters.Upsert(ctx, state.AdapterConfig{
		ID: "reg_sneaky", Category: "image_registry", Kind: "oci", Name: "x", Enabled: true,
		Config: json.RawMessage(`{"url":"https://pando:hunter2@registry.internal"}`),
	})
	require.Error(t, err)

	require.NoError(t, adapters.Upsert(ctx, state.AdapterConfig{
		ID: "reg_plain", Category: "image_registry", Kind: "oci", Name: "x", Enabled: true,
		Config: json.RawMessage(`{"url":"https://registry.internal/pando"}`),
	}), "the category is one the database accepts")
}

// TestO20_ACredentialIsBoundToItsAdapter asserts that ciphertext sealed for one
// adapter cannot be opened as another's: the adapter ID is authenticated data.
func TestO20_ACredentialIsBoundToItsAdapter(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	adapters := state.NewAdapters(db)
	// Two providers: an install has one AI adapter per provider (R-259).
	for id, kind := range map[string]string{"ai_one": "anthropic", "ai_two": "openai"} {
		require.NoError(t, adapters.Upsert(ctx, state.AdapterConfig{
			ID: id, Category: "ai", Kind: kind, Name: id, Enabled: true,
		}))
	}
	creds := credentialStore(t, db)
	require.NoError(t, creds.Put(ctx, "ai_one", "api_key", secret.New("sk-ant-one")))

	_, err := db.Exec(ctx, `
		INSERT INTO adapter_credentials (adapter_id, field, adapter_ref, ciphertext)
		SELECT 'ai_two', field, adapter_ref, ciphertext FROM adapter_credentials WHERE adapter_id = 'ai_one'`)
	require.NoError(t, err)

	_, err = creds.Resolve(ctx, "ai_two")
	require.Error(t, err, "a copied row does not decrypt under a different adapter")
}
