package docker

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR417_AnEdgeIsGivenTheCertificatesPandoIssuedAsFiles asserts that each
// control host's runtime writes the leader's certificates where its edge
// reads them: a certificate and a key per name, the key unreadable to others,
// an unchanged file left alone, and a certificate no longer issued removed
// (R-417, O-68).
func TestR417_AnEdgeIsGivenTheCertificatesPandoIssuedAsFiles(t *testing.T) {
	dir := t.TempDir()
	certs := []api.EdgeCertificate{
		{Name: "pando-tls-a.example.com", CertPEM: []byte("CERT A"), KeyPEM: secret.New("KEY A")},
		{Name: "pando-tls-wildcard", CertPEM: []byte("CERT W"), KeyPEM: secret.New("KEY W")},
	}
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pando-console.yml"), []byte("routes"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pando-tls-gone.crt"), []byte("old"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "pando-tls-gone.key"), []byte("old"), 0o600))

	require.NoError(t, writeEdgeCertificates(dir, certs))
	got, err := os.ReadFile(filepath.Join(dir, "pando-tls-a.example.com.crt"))
	require.NoError(t, err)
	require.Equal(t, "CERT A", string(got))
	got, err = os.ReadFile(filepath.Join(dir, "pando-tls-wildcard.key"))
	require.NoError(t, err)
	require.Equal(t, "KEY W", string(got))
	info, err := os.Stat(filepath.Join(dir, "pando-tls-wildcard.key"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	_, err = os.Stat(filepath.Join(dir, "pando-tls-gone.crt"))
	require.True(t, os.IsNotExist(err), "a certificate no longer issued is removed")
	_, err = os.Stat(filepath.Join(dir, "pando-console.yml"))
	require.NoError(t, err, "only certificate files are pruned")

	// Unchanged: not rewritten, so the edge does not reload for nothing.
	path := filepath.Join(dir, "pando-tls-a.example.com.crt")
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))
	require.NoError(t, writeEdgeCertificates(dir, certs))
	info, err = os.Stat(path)
	require.NoError(t, err)
	require.WithinDuration(t, old, info.ModTime(), time.Second)

	// Renewed: rewritten in place.
	certs[0].CertPEM = []byte("CERT A2")
	require.NoError(t, writeEdgeCertificates(dir, certs))
	got, _ = os.ReadFile(path)
	require.Equal(t, "CERT A2", string(got))

	// None issued yet: nothing left to serve stale.
	require.NoError(t, writeEdgeCertificates(dir, nil))
	matches, _ := filepath.Glob(filepath.Join(dir, "*.crt"))
	require.Empty(t, matches)

	require.NoError(t, writeEdgeCertificates("", certs), "a plan with no directory writes nothing")
}

// TestAnEdgeCertificateThatCannotBeWrittenSaysWhich asserts a directory the
// runtime cannot write to fails naming the file, not silently.
func TestAnEdgeCertificateThatCannotBeWrittenSaysWhich(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.Chmod(dir, 0o500))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	err := writeEdgeCertificates(dir, []api.EdgeCertificate{{Name: "pando-tls-a", CertPEM: []byte("x"), KeyPEM: secret.New("y")}})
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "pando-tls-a.crt")
}

// TestAnEdgeCertificateNameCannotLeaveItsDirectory asserts a name that is not
// a plain file name is refused, and a directory that cannot be made fails
// with a message rather than a panic.
func TestAnEdgeCertificateNameCannotLeaveItsDirectory(t *testing.T) {
	dir := t.TempDir()
	err := writeEdgeCertificates(dir, []api.EdgeCertificate{{Name: "../escape", CertPEM: []byte("x"), KeyPEM: secret.New("y")}})
	require.Equal(t, errs.Internal, errs.CodeOf(err))

	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	err = writeEdgeCertificates(filepath.Join(file, "sub"), nil)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
}
