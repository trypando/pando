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
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR091_ASourceConnectionIsAnAdapterWhoseTokenIsSealed asserts R-091 and
// R-190 at the database: a source connection is an adapter_configs row of
// category source, its token is ciphertext, an OAuth flow in progress is
// sealed under its own scope, and disconnecting it removes both.
func TestR091_ASourceConnectionIsAnAdapterWhoseTokenIsSealed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	adapters := state.NewAdapters(db)
	require.NoError(t, adapters.Upsert(ctx, state.AdapterConfig{
		ID: "src_github", Category: "source", Kind: "github", Name: "GitHub (acme)",
		Config: json.RawMessage(`{"method":"oauth","host":"github.com","scope":"acme"}`), Enabled: true,
	}), "the source category is accepted")

	sa := secretslocal.New()
	cfg, err := json.Marshal(map[string]string{"key_path": filepath.Join(t.TempDir(), "secrets.key")})
	require.NoError(t, err)
	require.NoError(t, sa.Configure(ctx, cfg))
	creds := state.NewAdapterCredentials(db, sa, "sek_local")
	pending := state.NewSourceAuthorizations(db, sa, "sek_local")

	const token = "gho_must-never-be-stored-in-the-clear"
	require.NoError(t, creds.Put(ctx, "src_github", "access_token", secret.New(token)))
	require.NoError(t, pending.Put(ctx, "src_github", "flow", secret.New("device-code-in-flight")))

	var sealed, flow []byte
	require.NoError(t, db.QueryRow(ctx,
		`SELECT ciphertext FROM adapter_credentials WHERE adapter_id = 'src_github'`).Scan(&sealed))
	require.NoError(t, db.QueryRow(ctx,
		`SELECT ciphertext FROM source_authorizations WHERE adapter_id = 'src_github'`).Scan(&flow))
	require.False(t, bytes.Contains(sealed, []byte(token)))
	require.False(t, bytes.Contains(flow, []byte("device-code-in-flight")))

	got, err := pending.Resolve(ctx, "src_github")
	require.NoError(t, err)
	require.Equal(t, "device-code-in-flight", got["flow"].Reveal())

	// A token's ciphertext cannot be read back as a pending flow's: the scope
	// is part of what was sealed.
	_, err = db.Exec(ctx, `INSERT INTO source_authorizations (adapter_id, field, adapter_ref, ciphertext)
		VALUES ('src_github', 'stolen', 'sek_local', $1)`, sealed)
	require.NoError(t, err)
	_, err = pending.Resolve(ctx, "src_github")
	require.Error(t, err, "a credential sealed under another scope does not open here")

	// Disconnecting is confined to the category: the route cannot remove
	// another kind of adapter, and removes the connection's secrets with it.
	require.NoError(t, adapters.Upsert(ctx, state.AdapterConfig{
		ID: "rt_docker_conn_test", Category: "runtime", Kind: "docker", Name: "Docker", Enabled: true,
	}))
	err = adapters.DeleteInCategory(ctx, "rt_docker_conn_test", "source")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.NoError(t, adapters.DeleteInCategory(ctx, "src_github", "source"))
	var left int
	require.NoError(t, db.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM adapter_credentials WHERE adapter_id = 'src_github') +
		(SELECT count(*) FROM source_authorizations WHERE adapter_id = 'src_github')`).Scan(&left))
	require.Zero(t, left)
}
