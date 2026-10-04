package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/update"
)

func tarball(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name string
		body []byte
	}{{"LICENSE", []byte("license")}, {name, content}} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write(f.body)
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

type release struct {
	files    map[string][]byte
	fetched  []string
	verified bool
}

func newRelease(t *testing.T, version string, binary []byte) (*release, string) {
	t.Helper()
	archive := fmt.Sprintf("pando_%s_linux_amd64.tar.gz", version)
	tb := tarball(t, "pando", binary)
	sum := sha256.Sum256(tb)
	dir := "https://example.test/v" + version + "/"
	return &release{files: map[string][]byte{
		dir + archive:                       tb,
		dir + "checksums.txt":               []byte(hex.EncodeToString(sum[:]) + "  " + archive + "\n"),
		dir + "checksums.txt.sigstore.json": []byte(`{}`),
	}}, dir + archive
}

func updater(t *testing.T, r *release, verifyErr error) (*selfUpdater, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "pando")
	require.NoError(t, os.WriteFile(exe, []byte("old binary"), 0o755))
	return &selfUpdater{
		exe: exe,
		releases: func(context.Context) ([]update.Release, error) {
			return []update.Release{{Version: "0.5.0-rc.1", Prerelease: true}, {Version: "0.4.0"}, {Version: "0.3.1"}}, nil
		},
		download: func(_ context.Context, url string) ([]byte, error) {
			r.fetched = append(r.fetched, url)
			b, ok := r.files[url]
			if !ok {
				return nil, errors.New("404 " + url)
			}
			return b, nil
		},
		verify: func(context.Context, []byte, []byte) error {
			r.verified = verifyErr == nil
			return verifyErr
		},
		base: "https://example.test/", goos: "linux", goarch: "amd64",
	}, exe
}

func runSelfUpdate(t *testing.T, u *selfUpdater, version string, prerelease bool) (string, error) {
	t.Helper()
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetContext(context.Background())
	err := u.run(cmd, version, prerelease)
	return out.String(), err
}

// TestR363_SelfUpdateReplacesTheBinaryOnlyAfterTheSignatureAndChecksumHold
// asserts R-363.
func TestR363_SelfUpdateReplacesTheBinaryOnlyAfterTheSignatureAndChecksumHold(t *testing.T) {
	old := Version
	t.Cleanup(func() { Version = old })
	Version = "0.3.1"

	t.Run("the latest stable release", func(t *testing.T) {
		r, _ := newRelease(t, "0.4.0", []byte("new binary"))
		u, exe := updater(t, r, nil)
		out, err := runSelfUpdate(t, u, "", false)
		require.NoError(t, err)
		require.Contains(t, out, "Pando 0.4.0, after checking its signature")
		require.True(t, r.verified)
		got, err := os.ReadFile(exe)
		require.NoError(t, err)
		require.Equal(t, "new binary", string(got))
		info, err := os.Stat(exe)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o755), info.Mode().Perm())
	})

	t.Run("a signature that does not verify replaces nothing", func(t *testing.T) {
		r, archive := newRelease(t, "0.4.0", []byte("new binary"))
		u, exe := updater(t, r, errors.New("wrong identity"))
		_, err := runSelfUpdate(t, u, "0.4.0", false)
		require.ErrorContains(t, err, "did not verify")
		require.NotContains(t, r.fetched, archive, "the archive is not even downloaded")
		got, _ := os.ReadFile(exe)
		require.Equal(t, "old binary", string(got))
	})

	t.Run("an archive that does not match the signed checksum replaces nothing", func(t *testing.T) {
		r, archive := newRelease(t, "0.4.0", []byte("new binary"))
		r.files[archive] = tarball(t, "pando", []byte("tampered"))
		u, exe := updater(t, r, nil)
		_, err := runSelfUpdate(t, u, "0.4.0", false)
		require.ErrorContains(t, err, "does not match its checksum")
		got, _ := os.ReadFile(exe)
		require.Equal(t, "old binary", string(got))
	})

	t.Run("a release candidate only when asked for", func(t *testing.T) {
		r, _ := newRelease(t, "0.5.0-rc.1", []byte("rc binary"))
		u, exe := updater(t, r, nil)
		_, err := runSelfUpdate(t, u, "", true)
		require.NoError(t, err)
		got, _ := os.ReadFile(exe)
		require.Equal(t, "rc binary", string(got))
	})

	t.Run("no build for this platform", func(t *testing.T) {
		r, _ := newRelease(t, "0.4.0", []byte("new binary"))
		u, _ := updater(t, r, nil)
		u.goarch = "riscv64"
		_, err := runSelfUpdate(t, u, "0.4.0", false)
		require.ErrorContains(t, err, "no pando_0.4.0_linux_riscv64.tar.gz for this platform")
	})

	t.Run("already the latest", func(t *testing.T) {
		Version = "0.4.0"
		t.Cleanup(func() { Version = "0.3.1" })
		r, _ := newRelease(t, "0.4.0", nil)
		u, _ := updater(t, r, nil)
		out, err := runSelfUpdate(t, u, "", false)
		require.NoError(t, err)
		require.Contains(t, out, "Nothing to do.")
		require.Empty(t, r.fetched)
	})

	t.Run("a package manager's CLI is left to it", func(t *testing.T) {
		r, _ := newRelease(t, "0.4.0", nil)
		u, _ := updater(t, r, nil)
		u.exe = "/opt/homebrew/Caskroom/pando/0.3.1/pando"
		out, err := runSelfUpdate(t, u, "", false)
		require.NoError(t, err)
		require.Contains(t, out, "brew upgrade --cask trypando/tap/pando")
		require.Empty(t, r.fetched)
	})
}

func TestSelfUpdateDownloadsSayWhatFailed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		require.Contains(t, r.Header.Get("User-Agent"), "pando/")
		_, _ = w.Write([]byte("release bytes"))
	}))
	t.Cleanup(srv.Close)

	b, err := httpDownload(context.Background(), srv.URL+"/checksums.txt")
	require.NoError(t, err)
	require.Equal(t, "release bytes", string(b))

	_, err = httpDownload(context.Background(), srv.URL+"/missing")
	require.ErrorContains(t, err, "404")

	_, err = httpDownload(context.Background(), "http://127.0.0.1:1/unreachable")
	require.Error(t, err)

	_, err = checksumOf([]byte("abc  pando_0.4.0_linux_amd64.tar.gz\n"), "pando_0.4.0_darwin_arm64.tar.gz")
	require.ErrorContains(t, err, "for this platform")

	require.Error(t, replaceExecutable(filepath.Join(t.TempDir(), "no-such-dir", "pando"), []byte("x")),
		"a directory that cannot be written to replaces nothing")

	cmd := SelfUpdateCmd()
	require.Equal(t, "self-update [version]", cmd.Use)
	require.NotNil(t, cmd.Flag("prerelease"))
}

func TestAReleaseListThatCannotBeReadIsSaid(t *testing.T) {
	r, _ := newRelease(t, "0.4.0", nil)
	u, _ := updater(t, r, nil)
	u.releases = func(context.Context) ([]update.Release, error) { return nil, errors.New("rate limited") }
	_, err := runSelfUpdate(t, u, "", false)
	require.ErrorContains(t, err, "could not read Pando's releases")

	u.releases = func(context.Context) ([]update.Release, error) { return nil, nil }
	_, err = runSelfUpdate(t, u, "", false)
	require.ErrorContains(t, err, "no Pando release is published")
}

func TestAnArchiveWithoutPandoInItIsRefused(t *testing.T) {
	_, err := binaryFrom(tarball(t, "README.md", []byte("hi")))
	require.ErrorContains(t, err, "no pando binary")
	_, err = binaryFrom([]byte("not gzip"))
	require.Error(t, err)
}
