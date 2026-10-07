//go:build integration

package imageregistry_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/secret"
)

func ptr[T any](v T) *T { return &v }

// TestR190_TheRegistryPasswordIsStoredAsCiphertextOnly asserts R-190 for the
// install registry's password set from the console: the row holds what the
// secrets adapter sealed and never the password, the settings table has
// nowhere to put one, and a URL carrying a credential is refused by the
// database itself.
func TestR190_TheRegistryPasswordIsStoredAsCiphertextOnly(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, _ := statetest.Connect(t)
	sa := secretslocal.New()
	require.NoError(t, sa.Configure(ctx, json.RawMessage(`{"key_path":"`+filepath.Join(t.TempDir(), "k")+`"}`)))

	svc := &imageregistry.Service{Fixed: map[string]imageregistry.Source{}, Store: state.NewInstallRegistry(db, sa, "sec_local")}
	_, err := svc.Update(ctx, imageregistry.Change{URL: ptr("https://registry.internal:5000"), Username: ptr("pando"),
		Password: ptr(secret.New("hunter2-registry-password"))}, "usr_1")
	require.NoError(t, err)

	var ciphertext []byte
	require.NoError(t, db.QueryRow(ctx,
		`SELECT ciphertext FROM install_registry_credentials WHERE field = 'password'`).Scan(&ciphertext))
	require.NotEmpty(t, ciphertext)
	require.NotContains(t, string(ciphertext), "hunter2-registry-password")

	var columns []string
	rows, err := db.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_name = 'install_registry'`)
	require.NoError(t, err)
	for rows.Next() {
		var c string
		require.NoError(t, rows.Scan(&c))
		columns = append(columns, c)
	}
	rows.Close()
	require.NotContains(t, columns, "password")

	_, err = db.Exec(ctx, `UPDATE install_registry SET url = 'https://pando:pw@registry.internal'`)
	require.ErrorContains(t, err, "install_registry_url_no_credential")
	_, err = db.Exec(ctx, `INSERT INTO install_registry_credentials (registry_id, field, adapter_ref) VALUES ('install', 'other', 'x')`)
	require.Error(t, err, "no row without ciphertext, and no other field")

	reg, err := svc.Current(ctx)
	require.NoError(t, err)
	auth, err := reg.Auth(ctx)
	require.NoError(t, err)
	require.Equal(t, "hunter2-registry-password", auth.Password.Reveal())
}

// TestR256_AReplicaUsesARotatedRegistryPasswordWithoutARestart asserts the
// rotation contract (issue #72): a password changed through one replica is
// the one another replica pushes and pulls with next, with no restart, since
// each reads the stored credential when it needs it.
func TestR256_AReplicaUsesARotatedRegistryPasswordWithoutARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, ownerURL := statetest.Connect(t)
	_, password := statetest.Database(t)
	b, err := state.ConnectCopy(ctx, ownerURL, password)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	keyPath := filepath.Join(t.TempDir(), "k")
	adapter := func() *secretslocal.Adapter {
		sa := secretslocal.New()
		require.NoError(t, sa.Configure(ctx, json.RawMessage(`{"key_path":"`+keyPath+`"}`)))
		return sa
	}
	first := &imageregistry.Service{Fixed: map[string]imageregistry.Source{}, Store: state.NewInstallRegistry(a, adapter(), "sec_local")}
	second := &imageregistry.Service{Fixed: map[string]imageregistry.Source{}, Store: state.NewInstallRegistry(b, adapter(), "sec_local")}

	_, err = first.Update(ctx, imageregistry.Change{URL: ptr("https://registry.internal:5000"), Username: ptr("pando"),
		Password: ptr(secret.New("before"))}, "usr_1")
	require.NoError(t, err)
	authOn := func(s *imageregistry.Service) string {
		reg, err := s.Current(ctx)
		require.NoError(t, err)
		auth, err := reg.Auth(ctx)
		require.NoError(t, err)
		return auth.Password.Reveal()
	}
	require.Equal(t, "before", authOn(second))

	_, err = first.Update(ctx, imageregistry.Change{Password: ptr(secret.New("after"))}, "usr_1")
	require.NoError(t, err)
	require.Equal(t, "after", authOn(second), "the other replica's next pull uses the new password")
}
