package cli

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/update"
)

// selfUpdater replaces this CLI with a release (R-363). Its parts are fields so
// a test can serve a release without GitHub; what checks the signature is the
// same update.VerifyChecksums the server's tests run on real release files.
type selfUpdater struct {
	exe      string
	releases func(ctx context.Context) ([]update.Release, error)
	download func(ctx context.Context, url string) ([]byte, error)
	verify   func(ctx context.Context, checksums, bundle []byte) error
	base     string // where a version's files are: base + "v" + version + "/"
	goos     string
	goarch   string
}

// SelfUpdateCmd is `pando self-update`. Beside `version` rather than among the
// API's commands: it acts on this binary, not on an installation (R-261).
func SelfUpdateCmd() *cobra.Command {
	var prerelease bool
	cmd := &cobra.Command{
		Use:   "self-update [version]",
		Short: "Replace this pando CLI with the latest release, after checking its signature",
		Long: "Downloads the release for this platform, checks the release's checksums.txt against its\n" +
			"signature from Pando's release workflow and the archive against checksums.txt, and replaces\n" +
			"this binary. A CLI installed with Homebrew or a Linux package is upgraded by that package\n" +
			"manager instead, and this prints its command.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			exe, err := os.Executable()
			if err != nil {
				return err
			}
			if resolved, err := filepath.EvalSymlinks(exe); err == nil {
				exe = resolved
			}
			cache, _ := os.UserCacheDir()
			u := &selfUpdater{
				exe:      exe,
				releases: update.NewGitHub(Version).Releases,
				download: httpDownload,
				verify: func(ctx context.Context, sums, bundle []byte) error {
					tm, err := update.TrustedRoot(ctx, filepath.Join(cache, "pando", "sigstore"))
					if err != nil {
						return err
					}
					return update.VerifyChecksums(tm, sums, bundle)
				},
				base:   "https://github.com/trypando/pando/releases/download/",
				goos:   runtime.GOOS,
				goarch: runtime.GOARCH,
			}
			version := ""
			if len(args) == 1 {
				version = args[0]
			}
			return u.run(cmd, version, prerelease)
		},
	}
	cmd.Flags().BoolVar(&prerelease, "prerelease", false, "consider release candidates when choosing the latest")
	return cmd
}

func (u *selfUpdater) run(cmd *cobra.Command, version string, prerelease bool) error {
	ctx := cmd.Context()
	w := cmd.OutOrStdout()

	chosen := version == ""
	if chosen {
		all, err := u.releases(ctx)
		if err != nil {
			return fmt.Errorf("could not read Pando's releases: %w", err)
		}
		channel := policy.UpdateChannelStable
		if prerelease {
			channel = policy.UpdateChannelPrerelease
		}
		offered := update.Offered(all, channel)
		if len(offered) == 0 {
			return fmt.Errorf("no Pando release is published")
		}
		version = offered[0].Version
	}
	version = strings.TrimPrefix(version, "v")
	if strings.TrimPrefix(Version, "v") == version || (chosen && !update.Newer(Version, version) && Version != "dev") {
		fmt.Fprintf(w, "This pando is %s, and the latest release is %s. Nothing to do.\n", strings.TrimPrefix(Version, "v"), version)
		return nil
	}

	// A package manager owns its files; replacing them under it breaks its
	// next upgrade and its record of what is installed.
	if how := upgradeFor(u.exe, version, fileExists, fileContains); !strings.HasPrefix(how, "download ") && !strings.HasPrefix(how, "go install ") {
		fmt.Fprintf(w, "This pando was installed by a package manager. Upgrade it with:\n\n  %s\n", how)
		return nil
	}

	dir := u.base + "v" + version + "/"
	archive := fmt.Sprintf("pando_%s_%s_%s.tar.gz", version, u.goos, u.goarch)
	sums, err := u.download(ctx, dir+"checksums.txt")
	if err != nil {
		return err
	}
	bundle, err := u.download(ctx, dir+"checksums.txt.sigstore.json")
	if err != nil {
		return err
	}
	if err := u.verify(ctx, sums, bundle); err != nil {
		return fmt.Errorf("Pando %s's checksums.txt did not verify as signed by Pando's release workflow, so nothing was replaced: %w", version, err)
	}
	want, err := checksumOf(sums, archive)
	if err != nil {
		return err
	}
	tarball, err := u.download(ctx, dir+archive)
	if err != nil {
		return err
	}
	if got := sha256.Sum256(tarball); hex.EncodeToString(got[:]) != want {
		return fmt.Errorf("%s does not match its checksum in the signed checksums.txt, so nothing was replaced", archive)
	}
	bin, err := binaryFrom(tarball)
	if err != nil {
		return err
	}
	if err := replaceExecutable(u.exe, bin); err != nil {
		return fmt.Errorf("could not replace %s: %w", u.exe, err)
	}
	fmt.Fprintf(w, "Replaced %s with Pando %s, after checking its signature.\n", u.exe, version)
	return nil
}

func checksumOf(sums []byte, name string) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("the release has no %s for this platform", name)
}

// binaryFrom finds the pando executable in a release archive.
func binaryFrom(tarball []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarball))
	if err != nil {
		return nil, err
	}
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil, fmt.Errorf("the release archive has no pando binary in it")
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && filepath.Base(h.Name) == "pando" {
			return io.ReadAll(io.LimitReader(tr, 512<<20))
		}
	}
}

// replaceExecutable writes the new binary beside the old one and renames it
// over it, so an interrupted update leaves the old one working.
func replaceExecutable(exe string, bin []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(exe), ".pando-update-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(bin); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o755); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), exe)
}

func httpDownload(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pando/"+strings.TrimPrefix(Version, "v"))
	resp, err := (&http.Client{Timeout: 5 * time.Minute}).Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 512<<20))
}
