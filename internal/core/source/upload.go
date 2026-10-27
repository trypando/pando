package source

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
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
// An upload is a gzipped tar, kept in Pando's database (Uploads, R-417) so
// every control host reads what was uploaded to any of them. It is the source
// of record for that app's next deploy, so it is kept rather than streamed:
// R-020 says nothing is read from the repository at deploy time, and the same
// logic applies here — the deploy reads what was uploaded, not whatever is on
// somebody's laptop now.
//
// Before R-417 an upload was a file under UploadDir. With Uploads set, such a
// file is still read, and moved into the database the first time it is
// (migrateUpload) or at start (MigrateUploads). Without Uploads, files are the
// store, which is what a test or an embedding with no database uses.

// DefaultUploadDir is where uploaded sources were kept before they moved into
// the database, and where a file not yet moved is still found.
const DefaultUploadDir = "/var/lib/pando/uploads"

// UploadStore keeps uploaded archives (state.Uploads).
type UploadStore interface {
	PutUpload(ctx context.Context, appID string, r io.Reader) error
	// AddUpload stores r only if the app has none, and reports whether it
	// did. An app that no longer exists is errs.NotFound.
	AddUpload(ctx context.Context, appID string, r io.Reader) (bool, error)
	// OpenUpload answers ok false when the app has no upload stored.
	OpenUpload(ctx context.Context, appID string) (io.ReadCloser, bool, error)
	DeleteUpload(ctx context.Context, appID string) error
}

// isAppID reports whether an app ID can name a file in the upload
// directory: not empty, not hidden, and no path separator.

// uploadSuffix ends an uploaded source's file name under UploadDir.
const uploadSuffix = ".tar.gz"

func isAppID(appID string) bool {
	return appID != "" && !strings.ContainsAny(appID, `/\`) && !strings.HasPrefix(appID, ".")
}

func notAnApp(appID string) error {
	return errs.Newf(errs.ValidInvalid, "%q does not name an app.", appID)
}

// uploadPath is where an app's uploaded archive is kept as a file.
//
// An empty UploadDir is refused rather than joined, which would put every
// upload in whatever the working directory happens to be.
func (s Sources) uploadPath(appID string) (string, error) {
	if s.UploadDir == "" {
		return "", errs.New(errs.Internal, "Pando has no directory configured for uploaded source.")
	}
	if !isAppID(appID) {
		return "", notAnApp(appID)
	}
	return filepath.Join(s.UploadDir, appID+uploadSuffix), nil
}

// StoreUpload keeps an uploaded archive for an app.
func (s Sources) StoreUpload(ctx context.Context, appID string, r io.Reader) error {
	if s.Uploads == nil {
		return s.storeUploadFile(appID, r)
	}
	if !isAppID(appID) {
		return notAnApp(appID)
	}
	spooled, err := s.spool(r)
	if err != nil {
		return err
	}
	defer spooled.discard()
	if err := s.Uploads.PutUpload(ctx, appID, spooled.f); err != nil {
		return err
	}
	// A file from before the move is now older than what the database holds.
	return s.removeUploadFiles(appID)
}

// bodyReader keeps the error reading r failed with, so a failure of the body
// is told from a failure of the disk.
type bodyReader struct {
	r   io.Reader
	err error
}

func (b *bodyReader) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) {
		b.err = err
	}
	return n, err
}

// spoolFile is a request body copied to disk, removed by discard.
type spoolFile struct{ f *os.File }

func (s spoolFile) discard() {
	_ = s.f.Close()
	_ = os.Remove(s.f.Name())
}

// spool copies an upload to a file under WorkDir and returns it rewound.
//
// The store writes an upload in one transaction, which holds a database
// connection until its reader ends. Read from the request body, that was as
// long as the client took to send it, and a few dozen slow uploads held every
// connection Pando has (R-417). Read from this file it is as long as the disk
// takes. The caller bounds r (maxUploadBytes in httpapi), and a read error is
// returned as it came, so the caller can tell a body over its limit.
func (s Sources) spool(r io.Reader) (spoolFile, error) {
	if s.WorkDir != "" {
		if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
			return spoolFile{}, errs.Wrap(errs.Internal, couldNotStore, err)
		}
	}
	f, err := os.CreateTemp(s.WorkDir, ".upload-*")
	if err != nil {
		return spoolFile{}, errs.Wrap(errs.Internal, couldNotStore, err)
	}
	spooled := spoolFile{f: f}
	body := &bodyReader{r: r}
	if _, err := io.Copy(f, body); err != nil {
		spooled.discard()
		if body.err != nil {
			return spoolFile{}, body.err
		}
		return spoolFile{}, errs.Wrap(errs.Internal, couldNotStore, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		spooled.discard()
		return spoolFile{}, errs.Wrap(errs.Internal, couldNotStore, err)
	}
	return spooled, nil
}

// storeUploadFile writes an uploaded archive under UploadDir.
func (s Sources) storeUploadFile(appID string, r io.Reader) error {
	final, err := s.uploadPath(appID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.UploadDir, 0o700); err != nil {
		return errs.Wrap(errs.Internal, couldNotStore, err)
	}

	// Written to a temporary name and renamed, so a deploy that runs while an
	// upload is in flight reads the previous archive rather than half of the
	// new one.
	tmp := final + ".partial"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errs.Wrap(errs.Internal, couldNotStore, err)
	}
	_, err = io.Copy(f, r)
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(tmp, final)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(errs.Internal, couldNotStore, err)
	}
	return nil
}

const (
	couldNotStore = "Pando could not store the upload."
	couldNotRead  = "Pando could not read the uploaded source."
)

// DiscardUpload removes a deleted app's stored upload.
//
// The archive is kept for the app's next deploy, and a deleted app has none:
// a restore goes to an app created again, with an upload of its own (R-206).
// It had been kept forever (issue #55). A missing archive is not an error — an
// app built from git never had one.
func (s Sources) DiscardUpload(ctx context.Context, appID string) error {
	if !isAppID(appID) {
		return notAnApp(appID)
	}
	if s.Uploads == nil {
		if _, err := s.uploadPath(appID); err != nil {
			return err
		}
	} else if err := s.Uploads.DeleteUpload(ctx, appID); err != nil {
		return err
	}
	return s.removeUploadFiles(appID)
}

// removeUploadFiles removes an app's archive file and any half-written one.
// No UploadDir is nothing to remove.
func (s Sources) removeUploadFiles(appID string) error {
	if s.UploadDir == "" {
		return nil
	}
	final := filepath.Join(s.UploadDir, appID+uploadSuffix)
	for _, p := range []string{final, final + ".partial"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return errs.Wrap(errs.Internal, "Pando could not remove the app's uploaded source.", err)
		}
	}
	return nil
}

// migrateUpload moves an app's archive file into the database (R-417), and
// reports whether it stored one.
//
// Only into an app with no upload there (AddUpload): StoreUpload removes the
// file when it writes the row, so a row beside a file is the newer, whether
// it was there before or written by an upload while this ran, and the file
// is removed without being stored. A file whose app was deleted is removed
// too: it has no deploy to serve (R-206), and kept it would be tried again at
// every start.
func (s Sources) migrateUpload(ctx context.Context, appID string) (bool, error) {
	if s.UploadDir == "" {
		return false, nil
	}
	f, err := os.Open(filepath.Join(s.UploadDir, appID+uploadSuffix))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, errs.Wrap(errs.Internal, couldNotRead, err)
	}
	stored, err := s.Uploads.AddUpload(ctx, appID, f)
	_ = f.Close()
	if err != nil && errs.CodeOf(err) != errs.NotFound {
		return false, err
	}
	return stored, s.removeUploadFiles(appID)
}

// MigrateUploads moves every archive file under UploadDir that the database
// has no upload for into it (R-417), and returns how many it moved. Every file
// is tried; the first error is returned after the rest.
func (s Sources) MigrateUploads(ctx context.Context) (int, error) {
	if s.Uploads == nil || s.UploadDir == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(s.UploadDir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, errs.Wrap(errs.Internal, "Pando could not list the uploaded source to move it into the database.", err)
	}
	moved := 0
	var first error
	for _, e := range entries {
		appID, ok := strings.CutSuffix(e.Name(), uploadSuffix)
		if !ok || !e.Type().IsRegular() || !isAppID(appID) {
			continue
		}
		did, err := s.migrateUpload(ctx, appID)
		if err != nil && first == nil {
			first = err
		}
		if did {
			moved++
		}
	}
	return moved, first
}

// openUpload returns the app's stored archive: from the database, or from its
// file, moved into the database first, if it has not been yet.
func (s Sources) openUpload(ctx context.Context, appID string) (io.ReadCloser, error) {
	if s.Uploads == nil {
		return s.openUploadFile(appID)
	}
	rc, ok, err := s.Uploads.OpenUpload(ctx, appID)
	if err != nil || ok {
		return rc, err
	}
	// Opened again whether or not the file was the one stored: an upload
	// stored while this ran is kept over the file, and is the one to read.
	if _, err := s.migrateUpload(ctx, appID); err != nil {
		return nil, err
	}
	if rc, ok, err = s.Uploads.OpenUpload(ctx, appID); err != nil || ok {
		return rc, err
	}
	return nil, uploadGone()
}

func (s Sources) openUploadFile(appID string) (io.ReadCloser, error) {
	archive, err := s.uploadPath(appID)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(archive)
	if errors.Is(err, os.ErrNotExist) {
		return nil, uploadGone()
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, couldNotRead, err)
	}
	return f, nil
}

func uploadGone() error {
	return errs.New(errs.ValidInvalid,
		"This app's uploaded source is no longer on the server.").
		WithRemedy("Run `pando deploy .` from the app's directory again.")
}

// fetchUpload expands a stored upload into a checkout.
//
// The archive read is the one stored for the app the fetch is for (ForApp),
// never one the spec names: an app deploys only what was uploaded to it
// (R-025). A spec naming another is refused here as well as where it is
// written (spec.CheckUploadOwner), since a revision written before that check
// may still be deployed.
func (s Sources) fetchUpload(ctx context.Context, src spec.Source) (*Checkout, error) {
	appID := AppFrom(ctx)
	if !isAppID(appID) {
		return nil, errs.New(errs.Internal, "Pando could not tell which app this uploaded source is for.")
	}
	if err := spec.CheckUploadOwner(src, appID); err != nil {
		return nil, err
	}
	f, err := s.openUpload(ctx, appID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	dir, err := s.tempDir("pando-upload-")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not unpack the uploaded source.", err)
	}

	// Hashed while it is read rather than in a second pass: the digest is of
	// the bytes that were unpacked, which a second read of an archive the next
	// upload may replace could not promise.
	sum := sha256.New()
	if err := extract(io.TeeReader(f, sum), dir); err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	// The rest of the archive, if the tar ended before it did: a digest of
	// part of an archive names nothing.
	if _, err := io.Copy(sum, f); err != nil {
		_ = os.RemoveAll(dir)
		return nil, readFailure(err)
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

// readFailure keeps an error the store already said, such as a digest that
// does not match, and otherwise says the archive could not be read.
func readFailure(err error) error {
	if errs.As(err) != nil {
		return err
	}
	return errs.Wrap(errs.Internal, couldNotRead, err)
}

// extract unpacks a gzipped tar, refusing anything that escapes the directory.
//
// Path traversal in a tar is the oldest trick there is, and this archive comes
// from whatever a user or an agent chose to send. The check is on the resolved
// path rather than the name, because ../ is not the only way to leave a
// directory — a symlink pointing out of it and a later entry written through
// that symlink is the other, which is why symlinks are dropped entirely.
func extract(r io.Reader, dir string) error {
	return extractWithin(r, dir, uploadLimits)
}

// unpackLimits bound what an archive may unpack to.
type unpackLimits struct {
	file, total int64
	entries     int
	// archive bounds the whole tar stream once decompressed: headers,
	// extension records and the data of entries that are skipped, which the
	// tar reader reads past without them counting toward total.
	archive int64
}

var uploadLimits = unpackLimits{
	file: maxUploadFileBytes, total: maxUnpackedBytes, entries: maxUploadEntries, archive: maxArchiveBytes,
}

// errArchiveTooLarge is what the capped stream answers past its limit, so the
// refusal says why rather than that the archive could not be read.
var errArchiveTooLarge = errors.New("archive larger than its limit once decompressed")

// cappedReader fails, rather than ending cleanly, once more than left bytes
// have been read: a clean end would make a cut-off archive look complete.
type cappedReader struct {
	r    io.Reader
	left int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, errArchiveTooLarge
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

func extractWithin(r io.Reader, dir string, limits unpackLimits) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return errs.Wrap(errs.ValidInvalid, "That upload is not a gzipped tar archive.", err)
	}
	defer func() { _ = gz.Close() }()

	tr := tar.NewReader(&cappedReader{r: gz, left: limits.archive})
	var unpacked int64
	for entries := 1; ; entries++ {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return readFailed(err, limits)
		}
		target, err := checkEntry(header, dir, entries, limits)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return errs.Wrap(errs.Internal, couldNotUnpack, err)
			}

		case tar.TypeReg:
			n, err := extractFile(tr, header, target, limits, limits.total-unpacked)
			if err != nil {
				return readFailed(err, limits)
			}
			unpacked += n

		default:
			// Symlinks, devices, fifos: skipped, not an error. A source tree
			// with a symlink in it is normal and should still deploy; a symlink
			// that Pando followed while unpacking is how an archive writes
			// outside its directory.
		}
	}
}

// readFailed says why reading the archive stopped: past the decompressed
// limit, or an error already said for the upload, or unreadable.
func readFailed(err error, limits unpackLimits) error {
	if errors.Is(err, errArchiveTooLarge) {
		return archiveTooLarge(limits)
	}
	if errs.As(err) != nil {
		return err
	}
	return errs.Wrap(errs.ValidInvalid, "That upload could not be read.", err)
}

// checkEntry refuses an entry past the entry limit, outside dir, or sparse,
// and otherwise answers where it unpacks to.
func checkEntry(header *tar.Header, dir string, entries int, limits unpackLimits) (string, error) {
	if entries > limits.entries {
		return "", errs.Newf(errs.ValidInvalid,
			"That upload holds more than %d files and directories, which is more than Pando unpacks.", limits.entries).
			WithRemedy(uploadTooBigRemedy)
	}

	target := filepath.Join(dir, filepath.Clean("/"+header.Name))
	if !strings.HasPrefix(target, filepath.Clean(dir)+string(os.PathSeparator)) && target != dir {
		return "", errs.Newf(errs.ValidInvalid,
			"That upload contains a path that would write outside the app's directory: %s", header.Name)
	}

	// A sparse entry is a few bytes in the archive standing for a file of
	// any size, mostly zeros. No source tree needs one, and pando deploy
	// never writes one.
	if isSparse(header) {
		return "", errs.Newf(errs.ValidInvalid,
			"That upload contains %s as a sparse file, which Pando does not unpack.", header.Name).
			WithRemedy("Upload the directory with pando deploy, which archives every file whole.")
	}
	return target, nil
}

// Limits on what an upload may unpack to (R-410): far above a large source tree
// (the deploy QA's largest upload is under 1 MB in 23 files; Pando's own
// tree, without .git and node_modules as pando deploy sends it, is 18 MB in
// about 2,100 files), and low enough that an archive compressed to almost
// nothing cannot fill the disk or keep the unpacking busy. Each is refused
// when reached, never truncated: a source tree with part of a file missing
// builds into something nobody wrote.
const (
	maxUploadFileBytes = 1 << 30 // 1 GiB per file
	maxUnpackedBytes   = 4 << 30 // 4 GiB in all
	maxUploadEntries   = 200_000
	// The tar stream itself: everything above plus 1 GiB for headers and
	// extension records, several times what 200,000 entries' headers take.
	maxArchiveBytes = maxUnpackedBytes + 1<<30
)

const couldNotUnpack = "Pando could not unpack the upload."

func archiveTooLarge(limits unpackLimits) error {
	return errs.Newf(errs.ValidInvalid,
		"That upload is larger than %s once decompressed, which is more than Pando unpacks.", sizeWords(limits.archive)).
		WithRemedy(uploadTooBigRemedy)
}

const uploadTooBigRemedy = "Check for build output, dependencies or a .git directory being included, or deploy from a repository instead."

// extractFile writes one regular file of an upload, refusing it when it is
// larger than one file may be or than what is left of the whole upload's
// allowance. It returns how many bytes it wrote.
func extractFile(tr io.Reader, header *tar.Header, target string, limits unpackLimits, left int64) (int64, error) {
	limit := min(limits.file, left)
	tooBig := func() error {
		if limit < limits.file {
			return errs.Newf(errs.ValidInvalid,
				"That upload unpacks to more than %s, which is more than Pando unpacks.", sizeWords(limits.total)).
				WithRemedy(uploadTooBigRemedy)
		}
		return errs.Newf(errs.ValidInvalid,
			"That upload contains %s, which is larger than %s, the most Pando unpacks for one file.",
			header.Name, sizeWords(limits.file)).
			WithRemedy(uploadTooBigRemedy)
	}
	if header.Size > limit {
		return 0, tooBig()
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, errs.Wrap(errs.Internal, couldNotUnpack, err)
	}
	out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, fileMode(header.Mode))
	if err != nil {
		return 0, errs.Wrap(errs.Internal, couldNotUnpack, err)
	}
	// Read one byte past the limit, so a file over it is refused rather than
	// cut short: the header's size is the archive's word, not a promise.
	n, err := io.Copy(out, io.LimitReader(tr, limit+1))
	if err != nil {
		_ = out.Close()
		return n, errs.Wrap(errs.Internal, couldNotUnpack, err)
	}
	if err := out.Close(); err != nil {
		return n, errs.Wrap(errs.Internal, couldNotUnpack, err)
	}
	if n > limit {
		return n, tooBig()
	}
	return n, nil
}

// sizeWords says a limit in the largest whole unit that fits it.
func sizeWords(n int64) string {
	switch {
	case n >= 1<<30 && n%(1<<30) == 0:
		return fmt.Sprintf("%d GB", n>>30)
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MB", n>>20)
	}
	return fmt.Sprintf("%d bytes", n)
}

// isSparse reports whether an entry is a sparse file, in the old GNU format or
// the PAX one.
func isSparse(header *tar.Header) bool {
	if header.Typeflag == tar.TypeGNUSparse {
		return true
	}
	for key := range header.PAXRecords {
		if strings.HasPrefix(key, "GNU.sparse.") {
			return true
		}
	}
	return false
}

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
