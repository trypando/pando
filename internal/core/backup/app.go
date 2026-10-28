package backup

import (
	"archive/tar"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Per-app backups (R-204, R-205, R-210).
//
// Scope is "recover from a recent mistake", not disaster recovery (R-013). The
// one taken when an app is deleted is the important case: R-204 says Pando asks
// whether to keep a final copy, and that copy is kept until explicitly
// discarded rather than aged out.
//
// **Encrypted under the install's own secrets key, not a supplied passphrase.**
// That is a deliberate departure from R-213, which governs the DR bundle, and
// the reasoning there does not carry over: R-213 exists because a restore onto
// a *fresh machine* cannot unwrap keys held by the machine that died, and
// because shipping the key inside the encrypted bundle makes it plaintext for
// anyone holding the file. An app backup is restored in place, onto this
// install, by R-206 — so the machine that can read it is the machine that wrote
// it, and there is no key to ship anywhere.
//
// The alternative would be prompting for a passphrase every time someone
// deletes an app. That is a prompt people learn to type "password" into, which
// is worse than the key already protecting every secret in the install.

// AppBundleName is the fixed name of the spec inside a per-app bundle.
const AppSpecName = "spec.json"

// AppCreateRequest asks for a backup of one app.
type AppCreateRequest struct {
	AppID string

	// Kind is rolling or on_delete. An on_delete backup is kept until somebody
	// discards it (R-204), so it carries no retention at all.
	Kind string

	// Spec is the app's pinned spec, stored beside the data. Without it a
	// restore has bytes and no idea what ran them.
	Spec json.RawMessage

	// Volumes are the app's volumes, resolved to their runtime and handle.
	Volumes []VolumeRef

	// Services are the app's provisioned services. One its adapter can dump
	// is dumped, and the volumes the dump stands for are not copied (R-423).
	Services []ServiceRef

	DestinationRef string
	RetainFor      time.Duration
}

// CreateForApp backs up one app's data and spec.
func (s *Service) CreateForApp(ctx context.Context, id string, req AppCreateRequest) (Created, error) {
	key, err := s.installKey()
	if err != nil {
		return Created{}, err
	}

	dest, ref, err := s.destination(req.DestinationRef)
	if err != nil {
		return Created{}, err
	}
	if err := s.ensureWorkDir(); err != nil {
		return Created{}, err
	}

	staging, err := os.CreateTemp(s.WorkDir, "pando-app-backup-*.tar")
	if err != nil {
		return Created{}, errs.Wrap(errs.Internal, "Pando could not start the backup.", err)
	}
	defer func() {
		_ = staging.Close()
		_ = os.Remove(staging.Name())
	}()

	b := NewWriter(staging, req.Kind, s.Version, s.SchemaVersion)

	spec := req.Spec
	if len(spec) == 0 {
		spec = json.RawMessage("null")
	}
	if err := b.Add(AppSpecName, int64(len(spec)), bytesReader(spec)); err != nil {
		return Created{}, err
	}

	volumes := make(map[string]VolumeRef, len(req.Volumes))
	for _, v := range req.Volumes {
		volumes[v.VolumeID] = v
	}
	dumped := map[string]bool{}
	services := 0
	for _, sv := range req.Services {
		stands, err := s.addDump(ctx, b, req.AppID, sv, volumes)
		if err != nil {
			return Created{}, err
		}
		for _, id := range stands {
			dumped[id] = true
		}
		if stands != nil {
			services++
		}
	}

	live := s.appRunning(ctx, req.AppID, req.Volumes)
	copied := 0
	for _, v := range req.Volumes {
		if dumped[v.VolumeID] {
			continue
		}
		entry := VolumesPrefix + v.VolumeID + ".tar"
		if err := s.addVolume(ctx, b, v, entry); err != nil {
			return Created{}, err
		}
		if live {
			b.MarkLive(entry)
		}
		copied++
	}
	b.Count("volumes", copied)
	b.Count("services", services)

	manifest, err := b.Finish()
	if err != nil {
		return Created{}, err
	}
	if _, err := staging.Seek(0, io.SeekStart); err != nil {
		return Created{}, errs.Wrap(errs.Internal, "Pando could not read back the backup.", err)
	}

	w, err := dest.Writer(ctx, id)
	if err != nil {
		return Created{}, err
	}
	counter := &countingWriter{w: w}
	if err := Encrypt(counter, staging, key); err != nil {
		_ = w.Close()
		return Created{}, err
	}
	if err := w.Close(); err != nil {
		return Created{}, err
	}

	out := Created{ObjectName: id, AdapterRef: ref, SizeBytes: counter.n, Manifest: manifest}

	// R-204: an on_delete backup is kept until discarded, never aged out. The
	// schema refuses to give one an expiry, so this must not try.
	if req.Kind != "on_delete" && req.RetainFor > 0 && !dest.Capabilities().OwnsRetention {
		until := time.Now().UTC().Add(req.RetainFor)
		out.RetainUntil = &until
	}
	return out, nil
}

// addDump dumps one provisioned service with its engine's own tool, run in
// its workload (R-423), and returns the volumes the dump stands for. Nil, and
// nothing added, when there is no dump to take: the adapter has none for the
// type, or the service is not running — and then its files are not being
// written, and copying them is consistent.
func (s *Service) addDump(ctx context.Context, b *Writer, appID string, sv ServiceRef, volumes map[string]VolumeRef) ([]string, error) {
	adapter, ok := s.Registry.Services(sv.AdapterRef)
	if !ok {
		return nil, nil
	}
	plan, ok := adapter.Dump(api.ServiceHandle{ServiceID: sv.ServiceID, Handle: sv.Handle}, sv.Type)
	if !ok || len(plan.Volumes) == 0 {
		return nil, nil
	}
	held, ok := volumes[plan.Volumes[0]]
	if !ok {
		return nil, nil
	}
	rt, ok := s.Registry.Runtime(held.AdapterRef)
	if !ok {
		return nil, nil
	}

	staged, err := os.CreateTemp(s.WorkDir, "pando-svc-*.dump")
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	err = rt.Run(ctx, api.WorkloadRef{BundleID: appID, Workload: plan.Workload},
		api.RunRequest{Command: plan.Dump, Stdout: staged})
	if errs.CodeOf(err) == errs.NotFound {
		return nil, nil
	}
	if err != nil {
		return nil, dumpFailed(sv.Type, err)
	}
	info, err := staged.Stat()
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not stage the service's data.", err)
	}
	if err := b.Add(ServicesPrefix+sv.ServiceID+".dump", info.Size(), staged); err != nil {
		return nil, err
	}
	return plan.Volumes, nil
}

// dumpFailed says which database could not be dumped, in the runtime's words
// for why.
func dumpFailed(t spec.SlotType, err error) error {
	why := "its dump tool failed."
	if e := errs.As(err); e != nil && e.Message != "" {
		why = e.Message
	}
	return errs.Newf(errs.AdapterFailed, "Pando could not dump this app's %s. %s", t.DisplayName(), why).
		WithRemedy("Check that the database is running and healthy, then back up again.")
}

// appRunning is whether any of the app's workloads is running, so that a
// volume copied now may be written to during the copy. Asked of the runtime
// holding its storage; a runtime that cannot say is taken to mean yes, which
// marks a copy that may be fine as live rather than one that may not be as
// consistent.
func (s *Service) appRunning(ctx context.Context, appID string, volumes []VolumeRef) bool {
	if len(volumes) == 0 {
		return false
	}
	rt, ok := s.Registry.Runtime(volumes[0].AdapterRef)
	if !ok {
		return true
	}
	observed, err := rt.Observe(ctx, api.BundleRef{BundleID: appID})
	if err != nil {
		return true
	}
	for _, w := range observed.Workloads {
		if w.Running {
			return true
		}
	}
	return false
}

// installKey is the per-app backup key: the local secrets key, as a passphrase.
//
// Reusing Encrypt rather than inventing a second cipher, so per-app backups get
// the same construction the DR bundle does — the chunked AEAD with the
// last-chunk marker that makes truncation detectable. The only difference is
// where the key comes from.
func (s *Service) installKey() (secret.Value, error) {
	if s.SecretsKeyPath == "" {
		return secret.Value{}, errs.New(errs.Internal,
			"This installation has no local secrets key, so Pando cannot encrypt an app backup.").
			WithRemedy("Back up the whole installation instead, which is encrypted under a passphrase you supply.")
	}

	raw, err := os.ReadFile(s.SecretsKeyPath)
	if err != nil {
		return secret.Value{}, errs.Wrap(errs.Internal,
			"Pando could not read the key it encrypts app backups with.", err)
	}
	if len(raw) == 0 {
		return secret.Value{}, errs.New(errs.Internal,
			"This installation's secrets key is empty, so Pando cannot encrypt an app backup.")
	}
	return secret.New(base64.StdEncoding.EncodeToString(raw)), nil
}

// VerifyApp checks a per-app backup, which unlocks with the install's own key.
func (s *Service) VerifyApp(ctx context.Context, adapterRef, objectName string) (Verified, error) {
	key, err := s.installKey()
	if err != nil {
		return Verified{}, err
	}
	return s.Verify(ctx, adapterRef, objectName, key)
}

// AppRestoreRequest asks for an app's data to be put back.
type AppRestoreRequest struct {
	AppID      string
	AdapterRef string
	ObjectName string

	// Volumes are the app's volumes as they exist now, to restore into.
	Volumes []VolumeRef

	// Services are the app's provisioned services as they exist now, to load
	// a dump into (R-423).
	Services []ServiceRef

	// Confirm is required. Restoring replaces the app's data with what was in
	// the backup, and anything written since is gone.
	Confirm bool
}

// AppRestoreResult reports what was put back.
type AppRestoreResult struct {
	Manifest        Manifest
	VolumesApplied  int
	ServicesApplied int
}

// RestoreApp puts one app's data back, in place (R-206).
//
// In place is the whole of R-206: a backup restores to an app recreated from
// the same spec, matched by Pando's own identity for it. Backups are not
// portable to arbitrary apps, because Pando cannot know what is inside a volume
// and promising portable restore means promising semantics it cannot verify.
//
// Same order as the DR path and for the same reason: decrypt, verify, confirm,
// and only then write. A bundle that fails any of those leaves the app's data
// exactly as it was.
func (s *Service) RestoreApp(ctx context.Context, req AppRestoreRequest) (AppRestoreResult, error) {
	if !req.Confirm {
		return AppRestoreResult{}, errs.New(errs.ValidInvalid,
			"Restoring replaces this app's data with what was in the backup.").
			WithRemedy("Send confirm: true once you are sure. Anything written since the backup is lost.")
	}

	key, err := s.installKey()
	if err != nil {
		return AppRestoreResult{}, err
	}
	dest, _, err := s.destination(req.AdapterRef)
	if err != nil {
		return AppRestoreResult{}, err
	}
	if err := s.ensureWorkDir(); err != nil {
		return AppRestoreResult{}, err
	}

	rc, err := dest.Reader(ctx, req.ObjectName)
	if err != nil {
		return AppRestoreResult{}, err
	}
	defer func() { _ = rc.Close() }()

	staged, err := os.CreateTemp(s.WorkDir, "pando-app-restore-*.tar")
	if err != nil {
		return AppRestoreResult{}, errs.Wrap(errs.Internal, "Pando could not stage the restore.", err)
	}
	defer func() {
		_ = staged.Close()
		_ = os.Remove(staged.Name())
	}()

	if err := Decrypt(staged, rc, key); err != nil {
		if errors.Is(err, ErrPassphrase) {
			// Not a passphrase problem here — an app backup is unlocked with
			// the install's own key, so this means the key has changed or the
			// bundle is damaged. Said as what it is rather than as a passphrase
			// error the operator cannot act on.
			return AppRestoreResult{}, errs.New(errs.BackupDecryptFailed,
				"This backup cannot be opened with this installation's secrets key.").
				WithRemedy("It was taken by a different installation, or the key has been replaced since.")
		}
		return AppRestoreResult{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return AppRestoreResult{}, errs.Wrap(errs.Internal, "Pando could not read the staged restore.", err)
	}

	v, err := Verify(staged)
	if err != nil {
		return AppRestoreResult{}, err
	}
	if _, err := staged.Seek(0, io.SeekStart); err != nil {
		return AppRestoreResult{}, errs.Wrap(errs.Internal, "Pando could not read the staged restore.", err)
	}

	byID := make(map[string]VolumeRef, len(req.Volumes))
	for _, vol := range req.Volumes {
		byID[vol.VolumeID] = vol
	}
	loads, err := s.planLoads(v.Manifest, req, byID)
	if err != nil {
		return AppRestoreResult{}, err
	}

	// Nothing has been touched until here.
	result := AppRestoreResult{Manifest: v.Manifest}

	tr := tar.NewReader(staged)
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
		if load, ok := loads[header.Name]; ok {
			if err := load.run(ctx, req.AppID, tr); err != nil {
				return result, err
			}
			result.ServicesApplied++
			continue
		}
		if !strings.HasPrefix(header.Name, VolumesPrefix) {
			continue
		}

		volumeID := strings.TrimSuffix(strings.TrimPrefix(header.Name, VolumesPrefix), ".tar")
		target, known := byID[volumeID]
		if !known {
			// In the backup, not on the app any more — the volume was removed
			// from the spec since. Reported rather than dropped silently: the
			// data is real and somebody may want it.
			return result, errs.Newf(errs.StateInvalid,
				"This backup holds data for storage the app no longer has (%s).", volumeID).
				WithRemedy("Add that storage back to the app and restore again, or restore a newer backup.")
		}

		rt, ok := s.Registry.Runtime(target.AdapterRef)
		if !ok {
			return result, errs.Newf(errs.AdapterFailed,
				"The runtime holding %s is not configured.", volumeID)
		}
		if err := rt.RestoreVolume(ctx, api.VolumeHandle{
			VolumeID: target.VolumeID, Handle: target.Handle,
		}, tr); err != nil {
			return result, err
		}
		result.VolumesApplied++
	}
	return result, nil
}

// load is one database dump to put back, and where (R-423).
type load struct {
	rt   api.RuntimeAdapter
	plan api.DumpPlan
	kind spec.SlotType
}

func (l load) run(ctx context.Context, appID string, src io.Reader) error {
	err := l.rt.Run(ctx, api.WorkloadRef{BundleID: appID, Workload: l.plan.Workload},
		api.RunRequest{Command: l.plan.Load, Stdin: src})
	if errs.CodeOf(err) == errs.NotFound {
		return errs.Newf(errs.StateInvalid,
			"This backup holds a dump of the app's %s, and loading one needs the database running.", l.kind.DisplayName()).
			WithRemedy("Start the app, then restore again.")
	}
	if err != nil {
		why := "its load tool failed."
		if e := errs.As(err); e != nil && e.Message != "" {
			why = e.Message
		}
		return errs.Newf(errs.AdapterFailed, "Pando could not load the backup into this app's %s. %s", l.kind.DisplayName(), why)
	}
	return nil
}

// planLoads resolves every dump in the backup to the service it loads into,
// before anything is written: a restore that found out halfway that it could
// not load the database would leave the volumes from the backup and the
// database from now.
func (s *Service) planLoads(m Manifest, req AppRestoreRequest, volumes map[string]VolumeRef) (map[string]load, error) {
	services := make(map[string]ServiceRef, len(req.Services))
	for _, sv := range req.Services {
		services[sv.ServiceID] = sv
	}
	out := map[string]load{}
	for _, e := range m.Entries {
		if !strings.HasPrefix(e.Name, ServicesPrefix) || !strings.HasSuffix(e.Name, ".dump") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(e.Name, ServicesPrefix), ".dump")
		sv, ok := services[id]
		if !ok {
			return nil, errs.Newf(errs.StateInvalid,
				"This backup holds a database dump for a service the app no longer has (%s).", id).
				WithRemedy("Restore a backup taken since the app's databases last changed.")
		}
		adapter, ok := s.Registry.Services(sv.AdapterRef)
		if !ok {
			return nil, errs.Newf(errs.AdapterFailed,
				"The provisioner holding %s is not configured, so its dump cannot be loaded.", id)
		}
		plan, ok := adapter.Dump(api.ServiceHandle{ServiceID: sv.ServiceID, Handle: sv.Handle}, sv.Type)
		var held VolumeRef
		if ok && len(plan.Volumes) > 0 {
			held, ok = volumes[plan.Volumes[0]]
		}
		if !ok {
			return nil, errs.Newf(errs.AdapterFailed,
				"This app's %s can no longer be loaded from a dump.", sv.Type.DisplayName())
		}
		rt, ok := s.Registry.Runtime(held.AdapterRef)
		if !ok {
			return nil, errs.Newf(errs.AdapterFailed, "The runtime holding %s is not configured.", id)
		}
		out[e.Name] = load{rt: rt, plan: plan, kind: sv.Type}
	}
	return out, nil
}
