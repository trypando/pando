package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Service builds, verifies and restores DR bundles (Sequence D, R-212–R-216).
type Service struct {
	Registry *api.Registry

	// DatabaseURL is dumped and restored. Held as a secret.Value because it
	// carries the password and would otherwise reach a log line the first time
	// someone logged the config (R-194).
	DatabaseURL secret.Value

	// SecretsKeyPath is the local secrets adapter's key file. Without it in the
	// bundle, a restored install has every app's ciphertext and nothing to
	// decrypt it with — which looks like a successful restore until an app
	// starts (R-212).
	SecretsKeyPath string

	// TokenKeyPath is core's API token key file (core/tokenkey). API tokens
	// are stored as HMAC-SHA-256 under it, so a restore without it holds every
	// token's digest and can check none of them (R-063, R-212). Empty leaves it
	// out, which only a test does: every install has one.
	TokenKeyPath string

	// State supplies everything the bundle records about the install.
	State StateSource

	// Version and SchemaVersion go into the manifest so a restore can refuse a
	// bundle it cannot make sense of, rather than half-applying it.
	Version       string
	SchemaVersion uint

	// WorkDir is where the bundle is assembled before being encrypted and
	// streamed out. A DR bundle does not fit in memory.
	WorkDir string

	// Regrant re-applies the database's grant policy after a restore. Nil
	// skips it. pg_restore recreates every table and function with the
	// owner's default privileges, which hand the application role UPDATE
	// and DELETE on every month of the audit log until something revokes
	// them (R-027).
	Regrant func(ctx context.Context) error
}

// StateSource is what the service needs from the state store.
//
// An interface rather than the concrete store so the service can be tested
// without Postgres, and so this package does not import state — which would
// make the dependency run both ways once state records the backup.
type StateSource interface {
	// Counts are object counts for the manifest: apps, users, grants, specs,
	// volumes. Checksums prove the bytes arrived; counts prove the bytes
	// describe the install the operator thinks they are restoring.
	Counts(ctx context.Context) (map[string]int, error)

	// AdapterConfigs and HostPolicy are exported verbatim.
	AdapterConfigs(ctx context.Context) ([]byte, error)
	HostPolicy(ctx context.Context) ([]byte, error)

	// VolumesToSnapshot lists every live volume with the runtime that holds it.
	VolumesToSnapshot(ctx context.Context) ([]VolumeRef, error)

	// ServicesToSnapshot lists every provisioned service with the adapter that
	// holds it (R-131).
	ServicesToSnapshot(ctx context.Context) ([]ServiceRef, error)
}

// ServiceRef names one provisioned service and the adapter that owns it.
type ServiceRef struct {
	ServiceID  string
	AppID      string
	AdapterRef string
	Handle     string
}

// VolumeRef names one volume and the adapter that can snapshot it.
type VolumeRef struct {
	// AppID names the app the volume belongs to. A volume ID is unique only
	// within its app — two apps may both keep one called "data" — so a
	// bundle holding every app's volumes names each entry by both.
	AppID      string
	VolumeID   string
	AdapterRef string
	Handle     string
}

// MinPassphraseLength is the only rule, and it is longer than a password's.
//
// A password protects an account somebody can regain by other means; this
// protects every secret in the installation and there is no other means. No
// composition requirements, for the same reason as elsewhere: they produce
// shorter, more guessable secrets and a note on a monitor.
const MinPassphraseLength = 16

// CreateRequest asks for a bundle.
type CreateRequest struct {
	// Passphrase is supplied at backup time and never stored (R-213). Losing it
	// makes the bundle unusable (R-214), which is an accepted cost and one the
	// console states at creation rather than in documentation.
	Passphrase secret.Value

	// DestinationRef names the backup adapter to write to. Empty uses the
	// configured default.
	DestinationRef string

	// RetainFor is how long to keep it. Zero means keep until discarded.
	RetainFor time.Duration
}

// Created reports what a backup produced.
type Created struct {
	ObjectName  string
	AdapterRef  string
	SizeBytes   int64
	Manifest    Manifest
	RetainUntil *time.Time
}

// Create assembles, encrypts and stores a DR bundle.
//
// Order matters and is Sequence D's: everything is gathered and written to the
// destination first, and only then does the caller record the row. A row
// written before the object exists claims a bundle during exactly the window
// when a disaster is most likely to interrupt.
func (s *Service) Create(ctx context.Context, id string, req CreateRequest) (Created, error) {
	if req.Passphrase.Reveal() == "" {
		return Created{}, errs.New(errs.ValidInvalid,
			"A backup needs a passphrase, and Pando does not keep it.").
			WithRemedy("Choose a passphrase and store it somewhere you will still have it after the machine is gone.")
	}

	dest, ref, err := s.destination(req.DestinationRef)
	if err != nil {
		return Created{}, err
	}

	if err := s.ensureWorkDir(); err != nil {
		return Created{}, err
	}

	// Assembled on disk, not in memory: a bundle is a database dump plus every
	// app volume.
	staging, err := os.CreateTemp(s.WorkDir, "pando-bundle-*.tar")
	if err != nil {
		return Created{}, errs.Wrap(errs.Internal, "Pando could not start the backup.", err)
	}
	defer func() {
		_ = staging.Close()
		_ = os.Remove(staging.Name())
	}()

	manifest, err := s.assemble(ctx, staging)
	if err != nil {
		return Created{}, err
	}
	if _, err := staging.Seek(0, io.SeekStart); err != nil {
		return Created{}, errs.Wrap(errs.Internal, "Pando could not read back the backup.", err)
	}

	// Encrypt on the way out. The destination never sees plaintext, so a
	// compromised destination yields ciphertext (R-213).
	w, err := dest.Writer(ctx, id)
	if err != nil {
		return Created{}, err
	}
	counter := &countingWriter{w: w}
	if err := Encrypt(counter, staging, req.Passphrase); err != nil {
		_ = w.Close()
		return Created{}, err
	}
	if err := w.Close(); err != nil {
		return Created{}, err
	}

	out := Created{
		ObjectName: id, AdapterRef: ref, SizeBytes: counter.n, Manifest: manifest,
	}

	// Retention belongs to whoever can enforce it. If the destination expires
	// objects itself, Pando records no expiry and never prunes: pruning what
	// the store has already locked fails every time, and the failure looks like
	// a Pando bug (R-217).
	if req.RetainFor > 0 && !dest.Capabilities().OwnsRetention {
		until := time.Now().UTC().Add(req.RetainFor)
		out.RetainUntil = &until
	}
	return out, nil
}

// assemble writes the tar into w and returns the manifest.
func (s *Service) assemble(ctx context.Context, w io.Writer) (Manifest, error) {
	b := NewWriter(w, "dr_bundle", s.Version, s.SchemaVersion)

	dump, err := s.dumpDatabase(ctx)
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = os.Remove(dump) }()

	if err := addFile(b, PostgresName, dump); err != nil {
		return Manifest{}, err
	}

	if err := s.addKeys(b); err != nil {
		return Manifest{}, err
	}

	adapters, err := s.State.AdapterConfigs(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if err := b.Add(AdaptersName, int64(len(adapters)), bytesReader(adapters)); err != nil {
		return Manifest{}, err
	}

	policy, err := s.State.HostPolicy(ctx)
	if err != nil {
		return Manifest{}, err
	}
	if err := b.Add(PolicyName, int64(len(policy)), bytesReader(policy)); err != nil {
		return Manifest{}, err
	}

	volumes, err := s.State.VolumesToSnapshot(ctx)
	if err != nil {
		return Manifest{}, err
	}
	for _, v := range volumes {
		if err := s.addVolume(ctx, b, v, installVolumeEntry(v)); err != nil {
			return Manifest{}, err
		}
	}

	edgeVolumes, err := s.addEdgeVolumes(ctx, b)
	if err != nil {
		return Manifest{}, err
	}
	b.Count("edge_volumes", edgeVolumes)

	// Provisioned services (R-212).
	//
	// This was the last thing a bundle promised and did not contain. A restore
	// used to bring back every app, every secret and every volume, and then the
	// app's own database — the one Pando stood up for it — would come back
	// empty, because nothing ever asked the services adapter for it.
	services, captured, err := s.addServices(ctx, b)
	if err != nil {
		return Manifest{}, err
	}

	counts, err := s.State.Counts(ctx)
	if err != nil {
		return Manifest{}, err
	}
	for object, n := range counts {
		b.Count(object, n)
	}
	b.Count("volumes", len(volumes))
	b.Count("services", services)

	// Counted separately from services, and both go in the manifest, because
	// "5 services, 0 snapshotted" is the state an operator needs to be able to
	// read off a bundle: it is correct for the in-bundle provisioner, whose
	// data is under volumes/, and a disaster for anything else.
	b.Count("services_snapshotted", captured)

	return b.Finish()
}

// addServices snapshots provisioned services whose data is not already in an
// app volume, and returns how many services there were and how many were
// captured here.
func (s *Service) addServices(ctx context.Context, b *Writer) (total, captured int, err error) {
	if s.State == nil {
		return 0, 0, nil
	}
	services, err := s.State.ServicesToSnapshot(ctx)
	if err != nil {
		return 0, 0, err
	}

	for _, sv := range services {
		total++

		adapter, ok := s.Registry.Services(sv.AdapterRef)
		if !ok {
			return 0, 0, errs.Newf(errs.AdapterFailed,
				"The provisioner holding %s is not configured, so its data cannot be backed up.", sv.ServiceID).
				WithRemedy("Configure that provisioner, or delete the app that uses it, then back up again.")
		}

		// The capability, not a type assertion (R-254). An adapter whose data
		// is in app volumes has already had it backed up by the loop above;
		// calling Snapshot would copy the same bytes into the bundle twice, and
		// double the size of the one file an operator has to store offsite.
		if adapter.Capabilities().DataInAppVolumes {
			continue
		}

		if err := s.addServiceSnapshot(ctx, b, adapter, sv); err != nil {
			return 0, 0, err
		}
		captured++
	}
	return total, captured, nil
}

// addServiceSnapshot stages one service's snapshot and adds it to the bundle.
//
// Staged for the same reason volumes are: tar needs a size in the header and a
// database dump streams.
func (s *Service) addServiceSnapshot(ctx context.Context, b *Writer, adapter api.ServicesAdapter, sv ServiceRef) error {
	staged, err := os.CreateTemp(s.WorkDir, "pando-svc-*.dump")
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	if err := adapter.Snapshot(ctx, api.ServiceHandle{ServiceID: sv.ServiceID, Handle: sv.Handle}, staged); err != nil {
		return err
	}
	info, err := staged.Stat()
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	return b.Add(ServicesPrefix+sv.ServiceID+".dump", info.Size(), staged)
}

// addVolume snapshots one volume through its runtime adapter.
//
// Staged to a file first because tar needs the size in the header and a volume
// snapshot streams. The alternative is buffering a volume in memory, which for
// the volumes this exists to protect is not an alternative.
func (s *Service) addVolume(ctx context.Context, b *Writer, v VolumeRef, entry string) error {
	rt, ok := s.Registry.Runtime(v.AdapterRef)
	if !ok {
		return errs.Newf(errs.AdapterFailed,
			"The runtime holding %s is not configured, so its data cannot be backed up.", v.VolumeID)
	}
	return s.addSnapshot(ctx, b, rt, api.VolumeHandle{VolumeID: v.VolumeID, Handle: v.Handle}, entry)
}

// installVolumeEntry names a volume inside a whole-installation bundle:
// volumes/<app>/<volume>.tar.
//
// By app as well as volume. Named by volume alone, two apps that each keep a
// volume called "data" wrote two entries with one name, and a restore put the
// second app's data into the first (issue #87). A per-app backup holds one
// app, so it keeps volumes/<volume>.tar.
func installVolumeEntry(v VolumeRef) string {
	if v.AppID == "" {
		return VolumesPrefix + v.VolumeID + ".tar"
	}
	return VolumesPrefix + v.AppID + "/" + v.VolumeID + ".tar"
}

// parseVolumeEntry reads a volume entry's name back into its app and volume.
// An entry from a bundle taken before entries were named by app carries no
// app, and the app is returned empty.
func parseVolumeEntry(name string) (appID, volumeID string) {
	rest := strings.TrimSuffix(strings.TrimPrefix(name, VolumesPrefix), ".tar")
	if i := strings.LastIndex(rest, "/"); i >= 0 {
		return rest[:i], rest[i+1:]
	}
	return "", rest
}

// addEdgeVolumes adds the storage of every edge the default runtime runs, and
// returns how many. None on a runtime that runs no edges.
func (s *Service) addEdgeVolumes(ctx context.Context, b *Writer) (int, error) {
	rt, ok := s.edgeRuntime(ctx)
	if !ok {
		return 0, nil
	}
	handles, err := rt.EdgeVolumes(ctx)
	if err != nil {
		return 0, err
	}
	for _, h := range handles {
		if !safeHandle(h.Handle) {
			// The name becomes a path in the bundle and on restore; one
			// that could climb out of edges/ is not written.
			return 0, errs.Newf(errs.Internal, "The runtime named an edge's storage %q, which Pando will not write into a backup.", h.Handle)
		}
		if err := s.addSnapshot(ctx, b, rt, h, EdgesPrefix+h.Handle+".tar"); err != nil {
			return 0, err
		}
	}
	return len(handles), nil
}

// edgeRuntime is the default runtime, when it runs edges.
func (s *Service) edgeRuntime(ctx context.Context) (api.RuntimeAdapter, bool) {
	if s.Registry == nil {
		return nil, false
	}
	ref, ok := s.Registry.Default(api.CategoryRuntime)
	if !ok {
		return nil, false
	}
	rt, ok := s.Registry.Runtime(ref)
	if !ok {
		return nil, false
	}
	caps, err := rt.Capabilities(ctx)
	if err != nil || !caps.SupportsEdge {
		return nil, false
	}
	return rt, true
}

var safeHandlePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func safeHandle(h string) bool {
	return safeHandlePattern.MatchString(h) && !strings.Contains(h, "..")
}

// addSnapshot stages one volume's snapshot and adds it to the bundle as name.
func (s *Service) addSnapshot(ctx context.Context, b *Writer, rt api.RuntimeAdapter, h api.VolumeHandle, name string) error {
	staged, err := os.CreateTemp(s.WorkDir, "pando-vol-*.tar")
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the app's data.", err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	if err := rt.SnapshotVolume(ctx, h, staged); err != nil {
		return err
	}
	info, err := staged.Stat()
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the app's data.", err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not stage the app's data.", err)
	}
	return b.Add(name, info.Size(), staged)
}

// dumpDatabase runs pg_dump into a temporary file and returns its path.
//
// The custom format, because it restores with pg_restore --clean and does not
// depend on psql parsing whatever the dump contains. The password reaches the
// child through the environment and never through argv, which is world-readable
// in /proc — the database name is the only part of the URL on the command line,
// and it is not a secret (R-194).
func (s *Service) dumpDatabase(ctx context.Context) (string, error) {
	f, err := os.CreateTemp(s.WorkDir, "pando-pg-*.dump")
	if err != nil {
		return "", errs.Wrap(errs.Internal, "Pando could not start the database backup.", err)
	}
	path := f.Name()
	_ = f.Close()

	env, dbname, err := pgEnv(s.DatabaseURL)
	if err != nil {
		_ = os.Remove(path)
		return "", err
	}

	// pando_private holds the restricted roles' passwords and is left out: a
	// bundle carries no database password (R-194), and a restore onto a new
	// server provisions new ones at its first start (issue #72).
	cmd := exec.CommandContext(ctx, "pg_dump", "--format=custom", "--no-owner", "--no-privileges",
		"--exclude-schema=pando_private", "--dbname", dbname, "--file", path)
	cmd.Env = env

	out, err := cmd.CombinedOutput()
	if err != nil {
		_ = os.Remove(path)
		// pg_dump's own message, which names the table or permission at fault.
		// Replacing it with something generic would delete the only useful
		// detail an operator has.
		return "", errs.Newf(errs.Internal,
			"Pando could not back up the database: %s", trimForMessage(out)).
			WithRemedy("Check that the database is reachable and that pg_dump is installed alongside Pando.")
	}
	return path, nil
}

// Verify decrypts and checks a stored bundle without applying it (R-216).
//
// Touches nothing. That is the property Sequence D asserts and the reason this
// is a separate call from Restore rather than a flag on it: a flag is a thing
// someone passes wrongly, and the wrong value here overwrites an install.
func (s *Service) Verify(ctx context.Context, adapterRef, objectName string, passphrase secret.Value) (Verified, error) {
	dest, _, err := s.destination(adapterRef)
	if err != nil {
		return Verified{}, err
	}

	rc, err := dest.Reader(ctx, objectName)
	if err != nil {
		return Verified{}, err
	}
	defer func() { _ = rc.Close() }()

	// Decrypt through a pipe rather than to a file: verification reads the
	// whole stream once and keeps nothing, so there is no reason for a
	// plaintext copy of every secret in the install to touch the disk.
	pr, pw := io.Pipe()
	decrypted := make(chan error, 1)
	go func() {
		err := Decrypt(pw, rc, passphrase)
		_ = pw.CloseWithError(err)
		decrypted <- err
	}()

	v, verifyErr := Verify(pr)
	_ = pr.Close()
	decryptErr := <-decrypted

	// Decrypt is step 1 and verify is step 2, so a decrypt failure is reported
	// as one even though the verify is what noticed. Without this ordering a
	// mistyped passphrase reads as "this backup is damaged" — which sends an
	// operator to restore an older bundle when the one in front of them is
	// perfectly good.
	if decryptErr != nil {
		if errors.Is(decryptErr, ErrPassphrase) {
			return Verified{}, errs.New(errs.BackupDecryptFailed,
				"That passphrase does not open this backup.").
				WithRemedy("Pando never stores backup passphrases, so there is no way to recover one.")
		}
		return Verified{}, decryptErr
	}
	if verifyErr != nil {
		return Verified{}, verifyErr
	}
	return v, nil
}

// Discard removes a stored bundle, for retention (R-211).
//
// Refused when the destination owns retention or holds the object immutably:
// deleting what the store has already locked fails every time, and the failure
// looks like a Pando bug rather than the policy it is.
func (s *Service) Discard(ctx context.Context, adapterRef, objectName string) error {
	dest, _, err := s.destination(adapterRef)
	if err != nil {
		return err
	}

	caps := dest.Capabilities()
	if caps.OwnsRetention || caps.Immutable {
		return nil
	}
	return dest.Delete(ctx, objectName)
}

// ensureWorkDir creates the staging directory.
//
// Created on demand rather than at install time: a directory that has to exist
// before the first backup is a step somebody skips, and they find out during
// the backup they are taking because something has already gone wrong. 0700
// because what passes through here is every secret in the install in the clear.
func (s *Service) ensureWorkDir() error {
	if s.WorkDir == "" {
		return nil // os.CreateTemp falls back to the system temporary directory.
	}
	if err := os.MkdirAll(s.WorkDir, 0o700); err != nil {
		return errs.Wrap(errs.Internal,
			fmt.Sprintf("Pando could not create its working directory at %s.", s.WorkDir), err)
	}
	return nil
}

func (s *Service) destination(ref string) (api.BackupAdapter, string, error) {
	if ref == "" {
		var ok bool
		ref, ok = s.Registry.Default(api.CategoryBackup)
		if !ok {
			return nil, "", errs.New(errs.Internal,
				"This installation has nowhere to put a backup.").
				WithRemedy("Configure a backup destination before taking a backup.")
		}
	}
	dest, ok := s.Registry.Backup(ref)
	if !ok {
		return nil, "", errs.Newf(errs.ValidInvalid, "There is no backup destination called %q.", ref)
	}
	return dest, ref, nil
}

// pgEnv splits the database URL into libpq environment variables.
//
// The password must not appear in argv. Splitting here rather than passing the
// whole URL as PGDATABASE also means a malformed URL is reported as a
// configuration problem now, rather than as a pg_dump error later that names
// nothing useful.
func pgEnv(dsn secret.Value) (env []string, dbname string, err error) {
	u, parseErr := url.Parse(dsn.Reveal())
	if parseErr != nil || u.Host == "" {
		return nil, "", errs.New(errs.Internal,
			"This installation's database address could not be read, so Pando cannot back it up.")
	}

	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	dbname = strings.TrimPrefix(u.Path, "/")
	if dbname == "" {
		return nil, "", errs.New(errs.Internal, "This installation's database address names no database.")
	}

	env = append(os.Environ(),
		"PGHOST="+host,
		"PGPORT="+port,
		"PGDATABASE="+dbname,
	)
	if user := u.User.Username(); user != "" {
		env = append(env, "PGUSER="+user)
	}
	if pw, ok := u.User.Password(); ok {
		env = append(env, "PGPASSWORD="+pw)
	}
	// sslmode and anything else the URL carried.
	if mode := u.Query().Get("sslmode"); mode != "" {
		env = append(env, "PGSSLMODE="+mode)
	}
	return env, dbname, nil
}

// addKeys puts the install's key files in the bundle (R-212).
//
// A missing secrets key is normal — an install using an external secrets
// adapter has no local key to export — but it is checked explicitly rather
// than by swallowing every error from addFile, because an unreadable key and
// an absent one are very different and only one of them should produce a
// bundle. A missing token key is not normal: Pando creates it at start, and a
// bundle without it restores tokens nobody can use.
func (s *Service) addKeys(b *Writer) error {
	if s.SecretsKeyPath != "" {
		switch _, statErr := os.Stat(s.SecretsKeyPath); {
		case statErr == nil:
			if err := addFile(b, SecretsKey, s.SecretsKeyPath); err != nil {
				return err
			}
		case !os.IsNotExist(statErr):
			return errs.Wrap(errs.Internal,
				"Pando could not read the secrets key for the backup.", statErr)
		}
	}
	if s.TokenKeyPath != "" {
		if err := addFile(b, TokenKey, s.TokenKeyPath); err != nil {
			return err
		}
	}
	return nil
}

func addFile(b *Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not read %s for the backup.", filepath.Base(path)), err)
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read part of the backup.", err)
	}
	return b.Add(name, info.Size(), f)
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

func bytesReader(b []byte) io.Reader { return &sliceReader{b: b} }

type sliceReader struct {
	b []byte
	i int
}

func (r *sliceReader) Read(p []byte) (int, error) {
	if r.i >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.i:])
	r.i += n
	return n, nil
}

func trimForMessage(out []byte) string {
	const max = 400
	s := string(out)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}
