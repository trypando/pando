//go:build integration

package imageregistry_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	registryoci "github.com/trypando/pando/internal/adapter/imageregistry/oci"
	secretslocal "github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/state/statetest"
	"github.com/trypando/pando/internal/secret"
)

func newOCI(kind string) api.ImageRegistryAdapter {
	if kind == registryoci.Kind {
		return registryoci.New()
	}
	return nil
}

// TestR256_AReplicaUsesARotatedRegistryPasswordWithoutARestart asserts the
// rotation contract (issue #72), kept when the registry became an adapter
// (issue #153): a password changed through one replica is the one another
// replica pushes and pulls with next, with no restart, since each builds the
// adapter from its row and its sealed credential when it needs it.
func TestR256_AReplicaUsesARotatedRegistryPasswordWithoutARestart(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a, ownerURL := statetest.Connect(t)
	_, password := statetest.Database(t)
	b, err := state.ConnectCopy(ctx, ownerURL, password)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	keyPath := filepath.Join(t.TempDir(), "k")
	sealer := func() *secretslocal.Adapter {
		sa := secretslocal.New()
		require.NoError(t, sa.Configure(ctx, json.RawMessage(`{"key_path":"`+keyPath+`"}`)))
		return sa
	}
	replica := func(db *state.DB) (*imageregistry.Service, *state.AdapterCredentials) {
		creds := state.NewAdapterCredentials(db, sealer(), "sec_local")
		return &imageregistry.Service{Configs: state.NewAdapters(db), Credentials: creds, New: newOCI}, creds
	}
	_, firstCreds := replica(a)
	second, _ := replica(b)

	require.NoError(t, state.NewAdapters(a).Upsert(ctx, state.AdapterConfig{
		ID: "reg_main", Category: "image_registry", Kind: "oci", Name: "Registry", Enabled: true,
		Config: json.RawMessage(`{"url":"https://registry.internal:5000","username":"pando"}`),
	}))
	require.NoError(t, firstCreds.Put(ctx, "reg_main", "password", secret.New("before")))

	authOn := func(s *imageregistry.Service) string {
		reg, err := s.Current(ctx)
		require.NoError(t, err)
		require.Equal(t, "reg_main", reg.ID())
		auth, err := reg.Auth(ctx)
		require.NoError(t, err)
		return auth.Password.Reveal()
	}
	require.Equal(t, "before", authOn(second))

	require.NoError(t, firstCreds.Put(ctx, "reg_main", "password", secret.New("after")))
	require.Equal(t, "after", authOn(second), "the other replica's next pull uses the new password")

	var ciphertext []byte
	require.NoError(t, a.QueryRow(ctx,
		`SELECT ciphertext FROM adapter_credentials WHERE adapter_id = 'reg_main' AND field = 'password'`).Scan(&ciphertext))
	require.NotContains(t, string(ciphertext), "after", "stored as ciphertext only (R-190)")
}
