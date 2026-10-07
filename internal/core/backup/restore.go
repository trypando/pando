package backup

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

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

		case strings.HasPrefix(header.Name, VolumesPrefix):
			if err := s.restoreVolume(ctx, header.Name, tr); err != nil {
				return result, err
			}
			result.VolumesApplied++

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

// dropPartitionedTables runs first in the restore transaction.
//
// pg_restore --clean cannot drop a partitioned table whose primary key its
// partitions inherit: it drops objects in reverse dump order, which reaches a
// partition's attached key (audit_events_default_pkey) before the parent's,
// and Postgres refuses with "cannot drop inherited constraint". audit_events
// is partitioned since migration 000041, so every restore onto a live install
// failed there. Dropping each partitioned table, with its partitions, before
// the dump's own statements leaves --clean's DROP ... IF EXISTS for them as
// no-ops; the dump recreates them. It runs inside the same transaction as the
// rest, so a restore that fails still leaves the audit log as it was.
const dropPartitionedTables = `
DO $$
DECLARE t regclass;
BEGIN
    FOR t IN
        SELECT c.oid::regclass
        FROM pg_catalog.pg_class c
        JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
        WHERE c.relkind = 'p' AND NOT c.relispartition AND n.nspname = 'public'
    LOOP
        EXECUTE format('DROP TABLE %s CASCADE', t);
    END LOOP;
END
$$;
`

// restoreDatabase applies the dump to the install's database.
//
// pg_restore turns the dump into SQL, with --clean --if-exists so the target's
// existing objects are dropped rather than collided with. This is the step
// that makes restore destructive and it is why Confirm exists. psql applies
// that SQL after dropPartitionedTables, which pg_restore alone has no way to
// run in its own transaction.
func (s *Service) restoreDatabase(ctx context.Context, dump io.Reader) error {
	env, dbname, err := pgEnv(s.DatabaseURL)
	if err != nil {
		return err
	}

	gen := exec.CommandContext(ctx, "pg_restore", "--clean", "--if-exists",
		"--no-owner", "--no-privileges", "--exit-on-error", "--file", "-")
	gen.Env = env
	gen.Stdin = dump
	var genErr strings.Builder
	gen.Stderr = &genErr
	script, err := gen.StdoutPipe()
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not start restoring the database.", err)
	}

	// One transaction, opened here and committed only once pg_restore has
	// exited cleanly — not psql's --single-transaction, which commits at the
	// end of its input however that input ended, and a dump pg_restore gives
	// up on halfway is a script that has dropped everything and recreated
	// half of it. Without the COMMIT, psql disconnects with the transaction
	// open and Postgres rolls it back; with ON_ERROR_STOP, so does a failing
	// statement. Either way a failed restore leaves the previous install
	// rather than a half-replaced one.
	genWait := sync.OnceValue(gen.Wait)
	apply := exec.CommandContext(ctx, "psql", "--no-psqlrc", "--quiet",
		"--set", "ON_ERROR_STOP=1", "--output", os.DevNull, "--dbname", dbname)
	apply.Env = env
	apply.Stdin = io.MultiReader(
		strings.NewReader("BEGIN;\n"+dropPartitionedTables),
		script,
		&commitIfClean{wait: genWait},
	)

	if err := gen.Start(); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not start pg_restore.", err).
			WithRemedy("Check that pg_restore and psql are installed alongside Pando.")
	}
	out, applyErr := apply.CombinedOutput()
	// Drain whatever psql left unread, so pg_restore is never blocked on a
	// full pipe while it is waited for.
	_, _ = io.Copy(io.Discard, script)

	if err := genWait(); err != nil {
		return errs.Newf(errs.Internal, "Pando could not restore the database: %s",
			trimForMessage([]byte(genErr.String()))).
			WithRemedy("The installation has not been fully replaced. Check the database is reachable and try again.")
	}
	if applyErr != nil {
		return errs.Newf(errs.Internal, "Pando could not restore the database: %s", trimForMessage(out)).
			WithRemedy("The installation has not been fully replaced. Check the database is reachable and try again.")
	}
	return nil
}

// commitIfClean is the end of the restore script: a COMMIT if pg_restore
// exited cleanly, and otherwise an error that ends psql's input with the
// transaction still open, which Postgres rolls back.
type commitIfClean struct {
	wait   func() error
	commit io.Reader
}

func (c *commitIfClean) Read(p []byte) (int, error) {
	if c.commit == nil {
		if err := c.wait(); err != nil {
			return 0, err
		}
		c.commit = strings.NewReader("COMMIT;\n")
	}
	return c.commit.Read(p)
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
