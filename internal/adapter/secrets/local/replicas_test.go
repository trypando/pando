package local_test

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/secrets/local"
	"github.com/trypando/pando/internal/secret"
)

// TestR190_ReplicasStartingOnOneEmptyVolumeShareOneKey asserts that several
// replicas generating the key at the same moment on one shared volume end
// with one key between them, so a secret any of them seals every other can
// open (R-190, issue #72).
func TestR190_ReplicasStartingOnOneEmptyVolumeShareOneKey(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "secrets.key")
	cfg := keyConfig(t, path)

	const replicas = 32
	adapters := make([]*local.Adapter, replicas)
	results := make([]error, replicas)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range adapters {
		adapters[i] = local.New()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			results[i] = adapters[i].Configure(ctx, cfg)
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range results {
		require.NoError(t, err)
	}

	ref := api.SecretRef{AppID: "app_1", Key: "DATABASE_URL"}
	for i, sealer := range adapters {
		stored, err := sealer.Put(ctx, ref, secret.New("postgres://x"))
		require.NoError(t, err)
		opener := adapters[(i+1)%replicas]
		got, err := opener.Get(ctx, stored)
		require.NoError(t, err, "replica %d's secret opens on replica %d", i, (i+1)%replicas)
		require.Equal(t, "postgres://x", got.Reveal())
	}

	// Nothing is left beside the key: the losers' temporary files are gone.
	entries, err := os.ReadDir(filepath.Dir(path))
	require.NoError(t, err)
	require.Len(t, entries, 1)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

// TestR190_AKeyThatCannotBeWrittenIsReported asserts that a key directory
// Pando cannot write to stops the adapter with where, rather than running
// with a key that is lost at the next restart.
func TestR190_AKeyThatCannotBeWrittenIsReported(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root writes to a read-only directory")
	}
	dir := filepath.Join(t.TempDir(), "readonly")
	require.NoError(t, os.Mkdir(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	path := filepath.Join(dir, "secrets.key")

	err := local.New().Configure(context.Background(), keyConfig(t, path))
	require.Error(t, err)
	require.Contains(t, err.Error(), "Could not write the key to "+path)
	_, statErr := os.Stat(path)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
