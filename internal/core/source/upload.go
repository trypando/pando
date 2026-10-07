package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Uploaded source (R-262).
//
// `pando deploy ./` is a [D] in design 04 §4, and the reason is specific: an
// agent that just generated an app cannot commit it and push, but it can run a
// command. Without this, the agent workflow R-262 describes starts by asking a
// person to make a repository.
//
// An upload is stored as a gzipped tar under a directory Pando owns, keyed by
// the app. It is the source of record for that app's next deploy, so it is kept
// rather than streamed: R-020 says nothing is read from the repository at
// deploy time, and the same logic applies here — the deploy reads what was
// uploaded, not whatever is on somebody's laptop now.

// DefaultUploadDir is where a server keeps uploaded sources.
const DefaultUploadDir = "/var/lib/pando/uploads"

// uploadPath is where an app's uploaded archive is kept.
//
// An empty UploadDir is refused rather than joined, which would put every
// upload in whatever the working directory happens to be.
func (s Sources) uploadPath(appID string) (string, error) {
	if s.UploadDir == "" {
		return "", errs.New(errs.Internal, "Pando has no directory configured for uploaded source.")
	}
	return filepath.Join(s.UploadDir, appID+".tar.gz"), nil
}

// StoreUpload writes an uploaded archive for an app and returns its path.
func (s Sources) StoreUpload(appID string, r io.Reader) (string, error) {
	final, err := s.uploadPath(appID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(s.UploadDir, 0o700); err != nil {
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}

	// Written to a temporary name and renamed, so a deploy that runs while an
	// upload is in flight reads the previous archive rather than half of the
	// new one.
	tmp := final + ".partial"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return "", errs.Wrap(errs.Internal, "Pando could not store the upload.", err)
	}
	return final, nil
}

// DiscardUpload removes a deleted app's stored upload.
//
// The archive is kept for the app's next deploy, and a deleted app has none:
// a restore goes to an app created again, with an upload of its own (R-206).
// It had been kept forever (issue #55). A missing archive is not an error — an
// app built from git never had one.
func (s Sources) DiscardUpload(appID string) error {
	if appID == "" || strings.ContainsAny(appID, `/\`) || strings.HasPrefix(appID, ".") {
		return errs.Newf(errs.ValidInvalid, "%q does not name an app.", appID)
	}
	final, err := s.uploadPath(appID)
	if err != nil {
		return err
	}
	for _, p := range []string{final, final + ".partial"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return errs.Wrap(errs.Internal, "Pando could not remove the app's uploaded source.", err)
		}
	}
	return nil
}

// fetchUpload expands a stored upload into a checkout.
func (s Sources) fetchUpload(_ context.Context, src spec.Source) (*Checkout, error) {
	archive, err := s.uploadPath(src.UploadID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(archive)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errs.New(errs.ValidInvalid,
			"This app's uploaded source is no longer on the server.").
			WithRemedy("Run `pando deploy .` from the app's directory again.")
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read the uploaded source.", err)
	}
	defer func() { _ = f.Close() }()

	dir, err := s.tempDir("pando-upload-")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not unpack the uploaded source.", err)
	}

	// Hashed while it is read rather than in a second pass: the digest is of
	// the bytes that were unpacked, which a second read of a file the next
	// upload may replace could not promise.
	sum := sha256.New()
	if err := extract(io.TeeReader(f, sum), dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// The rest of the file, if the archive ended before it did: a digest of
	// part of an archive names nothing.
	if _, err := io.Copy(sum, f); err != nil {
		_ = os.RemoveAll(dir)
		return nil, errs.Wrap(errs.Internal, "Pando could not read the uploaded source.", err)
	}
	// No commit: an upload has no revision. The deploy records the archive it
	// came from instead, which is the honest answer to "what was deployed",
	// and the archive's digest is what a scan of it is known by.
	//
	// With the same cleanup a clone has. Without it every detection and every
	// deploy of an uploaded app left a full copy of its source in the
	// temporary directory for as long as the server ran (issue #55).
	return &Checkout{
		Dir: dir, Commit: "", Digest: "sha256:" + hex.EncodeToString(sum.Sum(nil)),
		cleanup: func() { _ = os.RemoveAll(dir) },
	}, nil
}

// extract unpacks a gzipped tar, refusing anything that escapes the directory.
//
// Path traversal in a tar is the oldest trick there is, and this archive comes
// from whatever a user or an agent chose to send. The check is on the resolved
// path rather than the name, because ../ is not the only way to leave a
// directory — a symlink pointing out of it and a later entry written through
// that symlink is the other, which is why symlinks are dropped entirely.
func extract(r io.Reader, dir string) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, "That upload is not a gzipped tar archive.", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(gz)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return errs.Wrap(errs.ValidInvalid, "That upload could not be read.", err)
		}

		target := filepath.Join(dir, filepath.Clean("/"+header.Name))
		if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != dir {
			return errs.Newf(errs.ValidInvalid,
				"That upload contains a path that would write outside the app's directory: %s", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return errs.Wrap(errs.Internal, "Pando could not unpack the upload.", err)
			}

		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return errs.Wrap(errs.Internal, "Pando could not unpack the upload.", err)
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(header.Mode))
			if err != nil {
				return errs.Wrap(errs.Internal, "Pando could not unpack the upload.", err)
			}
			// Bounded: an archive claiming a petabyte should fail on the limit
			// rather than on the disk.
			if _, err := io.Copy(out, io.LimitReader(tr, maxUploadFileBytes)); err != nil {
				_ = out.Close()
				return errs.Wrap(errs.Internal, "Pando could not unpack the upload.", err)
			}
			if err := out.Close(); err != nil {
				return errs.Wrap(errs.Internal, "Pando could not unpack the upload.", err)
			}

		default:
			// Symlinks, devices, fifos: skipped, not an error. A source tree
			// with a symlink in it is normal and should still deploy; a symlink
			// that Pando followed while unpacking is how an archive writes
			// outside its directory.
		}
	}
}

const maxUploadFileBytes = 1 << 30 // 1 GiB per file

// fileMode takes the archive's permissions, within limits.
//
// Honoring the mode matters more than it looks: extracting everything 0600
// produces a source tree only root can read, and a build that then runs as a
// different user — nginx serving static files, say — answers 403 for every file
// in the app. That failure appears at runtime, in the app, looking like the
// app's fault.
//
// Only the permission bits, so setuid and setgid cannot arrive in an upload.
// An executable bit is kept, because a repository with a build script in it
// needs one.
func fileMode(mode int64) os.FileMode {
	// G115: Perm masks to the low nine bits after the conversion, so a mode
	// that overflows uint32 cannot produce a permission this function did not
	// intend. The mask is also what drops setuid and setgid.
	perm := os.FileMode(mode).Perm() //nolint:gosec
	if perm == 0 {
		return 0o644
	}
	// Always readable and writable by the owner: Pando has to be able to read
	// back what it just wrote, whatever the archive claimed.
	return perm | 0o600
}
