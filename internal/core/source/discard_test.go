package source

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR224_ADeletedAppsUploadIsRemoved asserts R-224. The archive an app was
// uploaded as stayed on disk after the app was deleted (issue #55).
func TestR224_ADeletedAppsUploadIsRemoved(t *testing.T) {
	s := Sources{UploadDir: t.TempDir()}

	err := s.StoreUpload(context.Background(), "app_01GONE", strings.NewReader("archive"))
	require.NoError(t, err)
	err = s.StoreUpload(context.Background(), "app_01KEEP", strings.NewReader("archive"))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(s.UploadDir, "app_01GONE.tar.gz.partial"), []byte("x"), 0o600))

	require.NoError(t, s.DiscardUpload(context.Background(), "app_01GONE"))
	require.NoFileExists(t, filepath.Join(s.UploadDir, "app_01GONE.tar.gz"))
	require.NoFileExists(t, filepath.Join(s.UploadDir, "app_01GONE.tar.gz.partial"))
	require.FileExists(t, filepath.Join(s.UploadDir, "app_01KEEP.tar.gz"))

	require.NoError(t, s.DiscardUpload(context.Background(), "app_01GONE"), "an app with no upload is not an error")
	for _, bad := range []string{"", "../x", ".hidden", `a\b`} {
		require.Error(t, s.DiscardUpload(context.Background(), bad), bad)
	}
}
