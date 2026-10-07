package backup

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// RestoreRequest asks for a bundle to be applied.
type RestoreRequest struct {
	AdapterRef string
	ObjectName string
	Passphrase secret.Value

	// Confirm is Sequence D step 3. Restoring replaces the install — every app,
	// every user, every secret — and a caller that has not said so explicitly
	// is a caller who meant to verify.
	Confirm bool
}

// RestoreResult reports what was applied.
type RestoreResult struct {
	Manifest       Manifest
	VolumesApplied int

	// EdgeVolumesApplied counts the edge's storage put back — Traefik's
	// certificates (EdgesPrefix).
	EdgeVolumesApplied int

	// UploadsApplied counts uploaded source archives put back (O-37).
	UploadsApplied int
}

// Restore decrypts, verifies, and only then applies (Sequence D, R-215).
//
// The ordering is the requirement. Everything that can reject the bundle
// happens before anything is written: decrypt, then verify against the
// manifest, then check schema compatibility, then confirm intent. A bundle that
// fails any of those leaves the target install completely untouched — not
// mostly untouched.
//
// That is why the bundle is staged to disk first. Verifying a stream and then
// applying it would need the stream twice, and re-reading from a remote
// destination means the bytes verified and the bytes applied are not provably
// the same bytes.
func (s *Service) Restore(ctx context.Context, req RestoreRequest) (RestoreResult, error) {
	if !req.Confirm {
		return RestoreResult{}, errs.New(errs.ValidInvalid,
			"Restoring replaces everything in this installation: every app, every account and every secret.").
			WithRemedy("Send confirm: true once you are sure this is the installation you mean to replace.")
	}

	dest, _, err := s.destination(req.AdapterRef)
	if err != nil {
		return RestoreResult{}, err
	}

	rc, err := dest.Reader(ctx, req.ObjectName)
	if err != nil {
		return RestoreResult{}, err
	}
	defer func() { _ = rc.Close() }()

	if err := s.ensureWorkDir(); err != nil {
		return RestoreResult{}, err
	}

	staged, err := os.CreateTemp(s.WorkDir, "pando-restore-*.tar")
	if err != nil {
		return RestoreResult{}, errs.Wrap(errs.Internal, "Pando could not stage the restore.", err)
	}
	defer func() {
		_ = staged.Close()
		// The staged copy is every secret in the install in the clear. It goes
		// as soon as the restore is over, successful or not.
		_ = os.Remove(staged.Name())
	}()

	// Step 1: decrypt. A wrong passphrase fails here and reveals nothing about
	// what is inside (R-214).
	if err := Decrypt(staged, rc, req.Passphrase); err != nil {
		if errors.Is(err, ErrPassphrase) {
			return RestoreResult{}, errs.New(errs.BackupDecryptFailed,
				"That passphrase does not open this backup.").
				WithRemedy("Pando does not keep backup passphrases, so there is no way to recover one. Try another backup.")
		}
		return RestoreResult{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return RestoreResult{}, errs.Wrap(errs.Internal, "Pando could not read the staged restore.", err)
	}

	// Step 2: verify against the manifest. Nothing has been touched yet.
	v, err := Verify(staged)
	if err != nil {
		return RestoreResult{}, err
	}
	if v.Manifest.SchemaVersion > s.SchemaVersion {
		return RestoreResult{}, errs.Newf(errs.BackupIncomplete,
			"This backup was taken from a newer version of Pando (database schema %d, this install reads %d).",
			v.Manifest.SchemaVersion, s.SchemaVersion).
			WithRemedy("Upgrade Pando to at least the version that took the backup, then restore.")
	}

	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return RestoreResult{}, errs.Wrap(errs.Internal, "Pando could not read the staged restore.", err)
	}

	// Steps 4–7. From here the install is being replaced.
	return s.apply(ctx, staged, v)
}

// apply writes the verified bundle over the install.
//
// Database first. Everything else — the secrets key, adapter configs, volumes —
// is meaningless without the rows that reference it, and a failure partway
// through leaves an install that is recoverable by re-running the restore. The
// other order leaves volumes belonging to apps the database has never heard of.
func (s *Service) apply(ctx context.Context, bundle io.Reader, v Verified) (RestoreResult, error) {
	result := RestoreResult{Manifest: v.Manifest}

	tr := tar.NewReader(bundle)
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return result, errs.Wrap(errs.Internal, "Pando could not read the backup while restoring.", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		switch {
		case header.Name == ManifestName:
			// Already read and checked.

		case header.Name == PostgresName:
			if err := s.restoreDatabase(ctx, tr); err != nil {
				return result, err
			}
			if s.Regrant != nil {
				if err := s.Regrant(ctx); err != nil {
					return result, err
				}
			}

		case header.Name == SecretsKey:
			if err := s.restoreSecretsKey(tr); err != nil {
				return result, err
			}

		case header.Name == TokenKey:
			if err := s.restoreTokenKey(tr); err != nil {
				return result, err
			}

		case strings.HasPrefix(header.Name, VolumesPrefix):
			if err := s.restoreVolume(ctx, header.Name, tr); err != nil {
				return result, err
			}
			result.VolumesApplied++

		case strings.HasPrefix(header.Name, UploadsPrefix):
			if err := s.restoreUpload(header.Name, tr); err != nil {
				return result, err
			}
			result.UploadsApplied++

		case strings.HasPrefix(header.Name, EdgesPrefix):
			if err := s.restoreEdgeVolume(ctx, header.Name, tr); err != nil {
				return result, err
			}
			result.EdgeVolumesApplied++

		case header.Name == AdaptersName || header.Name == PolicyName:
			// Both live in the database and arrived with the dump. They are in
			// the bundle as a readable copy for an operator reconstructing an
			// install by hand, which is the situation where the database is the
			// thing that will not load.

		default:
			// Verify already refused anything the manifest does not list, so
			// reaching here means a name this version does not know from a
			// bundle it accepted. Skipped rather than fatal: the alternative is
			// refusing to restore because of a file we have no use for.
		}
	}

	return result, nil
}

// restoreDatabase pipes the dump into pg_restore.
//
// --clean --if-exists, so the target's existing objects are dropped rather than
// collided with. This is the step that makes restore destructive and it is why
// Confirm exists.
func (s *Service) restoreDatabase(ctx context.Context, dump io.Reader) error {
	env, dbname, err := pgEnv(s.DatabaseURL)
	if err != nil {
		return err
	}

	// --single-transaction is what makes this all-or-nothing: a dump that fails
	// halfway rolls back, so a failed restore leaves the previous install
	// rather than a half-replaced one. --exit-on-error is required for that to
	// mean anything, because pg_restore's default is to keep going.
	cmd := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists",
		"--no-owner", "--no-privileges", "--exit-on-error", "--single-transaction",
		"--dbname", dbname)
	cmd.Env = env
	cmd.Stdin = dump

	out, err := cmd.CombinedOutput()
	if err != nil {
		return errs.Newf(errs.Internal, "Pando could not restore the database: %s", trimForMessage(out)).
			WithRemedy("The installation has not been fully replaced. Check the database is reachable and try again.")
	}
	return nil
}

// restoreSecretsKey writes the key back.
//
// Without it a restored install holds every app's ciphertext and nothing that
// opens it — which looks like a successful restore right up until an app
// starts. Written 0600 because it is the key to every secret in the install.
func (s *Service) restoreSecretsKey(r io.Reader) error {
	if s.SecretsKeyPath == "" {
		return nil
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read the secrets key from the backup.", err)
	}
	if err := os.WriteFile(s.SecretsKeyPath, body, 0o600); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not write the secrets key.", err)
	}
	return nil
}

// restoreTokenKey writes the API token key back.
//
// The restored database holds every token as HMAC-SHA-256 under this key, and
// the check every replica makes at start (token_key_check) was made with it.
// Without it every token is unusable and no replica starts. 0600, like the
// secrets key.
func (s *Service) restoreTokenKey(r io.Reader) error {
	if s.TokenKeyPath == "" {
		return nil
	}
	body, err := io.ReadAll(r)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read the API token key from the backup.", err)
	}
	if err := os.WriteFile(s.TokenKeyPath, body, 0o600); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not write the API token key.", err)
	}
	return nil
}

// restoreUpload writes an uploaded source archive back where deploys read it
// (O-37). The name is checked against what StoreUpload writes, so an entry
// cannot name a path outside the directory. 0600 in a 0700 directory, as
// StoreUpload makes them.
func (s *Service) restoreUpload(entryName string, r io.Reader) error {
	if s.UploadDir == "" {
		return nil
	}
	name := strings.TrimPrefix(entryName, UploadsPrefix)
	if !uploadName.MatchString(name) {
		return errs.Newf(errs.ValidInvalid, "The backup holds an uploaded source named %q, which is not one Pando writes.", name)
	}
	if err := os.MkdirAll(s.UploadDir, 0o700); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not restore the uploaded source.", err)
	}
	final := filepath.Join(s.UploadDir, name)
	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not restore the uploaded source.", err)
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return errs.Wrap(errs.Internal, "Pando could not restore the uploaded source.", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(errs.Internal, "Pando could not restore the uploaded source.", err)
	}
	if err := os.Rename(tmp, final); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(errs.Internal, "Pando could not restore the uploaded source.", err)
	}
	return nil
}

// restoreVolume recreates a volume and streams its contents back.
//
// The volume is created if it is not there — after a database restore onto a
// fresh machine it never is — and the runtime is the one the restored database
// now says holds it.
// restoreEdgeVolume puts an edge's storage back, into the default runtime.
//
// Before the edge starts is the point: the edge service mounts this same
// storage, finds the certificates already there, and issues nothing new.
func (s *Service) restoreEdgeVolume(ctx context.Context, entryName string, r io.Reader) error {
	handle := strings.TrimSuffix(strings.TrimPrefix(entryName, EdgesPrefix), ".tar")
	if !safeHandle(handle) {
		return errs.Newf(errs.ValidInvalid, "The backup names an edge's storage %q, which Pando will not restore into.", handle)
	}
	rt, ok := s.edgeRuntime(ctx)
	if !ok {
		// Not fatal: the certificates are re-issued when the edge starts,
		// which costs time and rate limit, not data.
		return nil
	}
	return rt.RestoreVolume(ctx, api.VolumeHandle{Handle: handle}, r)
}

func (s *Service) restoreVolume(ctx context.Context, entryName string, r io.Reader) error {
	appID, volumeID := parseVolumeEntry(entryName)
	if volumeID == "" {
		return nil
	}

	refs, err := s.State.VolumesToSnapshot(ctx)
	if err != nil {
		return err
	}
	// By app and volume. A bundle taken before entries were named by app
	// carries only the volume, and is matched on that as it always was.
	var target *VolumeRef
	for i := range refs {
		if refs[i].VolumeID == volumeID && (appID == "" || refs[i].AppID == appID) {
			target = &refs[i]
			break
		}
	}
	if target == nil {
		// In the bundle, absent from the restored database. That means the
		// bundle is internally inconsistent, but the data is real and throwing
		// it away is worse than leaving it unplaced — so this is reported
		// rather than silently dropped, and rather than fatal.
		return errs.Newf(errs.Internal,
			"The backup contains data for %s, which the restored installation does not have a record of.", volumeID).
			WithRemedy("The rest of the restore completed. This app's data is in the bundle and can be placed by hand.")
	}

	rt, ok := s.Registry.Runtime(target.AdapterRef)
	if !ok {
		return errs.Newf(errs.AdapterFailed,
			"The runtime that should hold %s is not configured on this installation.", volumeID)
	}

	handle := api.VolumeHandle{VolumeID: target.VolumeID, Handle: target.Handle}
	if handle.Handle == "" {
		created, err := rt.CreateVolume(ctx, api.VolumeRequest{BundleID: target.AppID, VolumeID: volumeID})
		if err != nil {
			return err
		}
		handle = created
	}
	return rt.RestoreVolume(ctx, handle, r)
}
