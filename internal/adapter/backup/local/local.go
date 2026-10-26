// Package local stores backup bundles on a filesystem path.
//
// The v1 backup destination (R-217). Honest about what it is: a bundle beside
// the install it backs up does not survive the disk failing, and R-217 says so
// plainly. It exists because a DR path that only works once a remote
// destination is configured is a DR path nobody tests, and because "recover
// from a recent mistake" (R-210) is served perfectly well by local disk.
//
// Retention is Pando's here: a filesystem has no lifecycle rules, so
// OwnsRetention is false and core prunes (R-211).
package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// Kind is the adapter kind.
const Kind = "local"

// Adapter writes bundles into a directory.
type Adapter struct {
	dir string
}

// New returns an unconfigured adapter.
func New() *Adapter { return &Adapter{} }

type config struct {
	// Path is the directory bundles are written to. Created if missing.
	Path string `json:"path"`
}

func (a *Adapter) Kind() string           { return Kind }
func (a *Adapter) Category() api.Category { return api.CategoryBackup }

// Configure sets the directory, and logs a warning when bundles written there
// would not outlive Pando's container (see Warnings). Logged rather than
// refused: the directory works until the container goes, so this is a warning,
// and warnings are never blockers.
func (a *Adapter) Configure(ctx context.Context, raw json.RawMessage) error {
	cfg := config{Path: DefaultPath}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return errs.Wrap(errs.ValidInvalid, "The local backup destination could not be configured.", err)
		}
	}
	if cfg.Path == "" {
		return errs.New(errs.ValidInvalid, "The local backup destination needs a path.").
			WithRemedy("Set path to a directory Pando can write to, for example " + DefaultPath + ".")
	}
	a.dir = cfg.Path
	for _, w := range pathWarnings(cfg.Path) {
		log.From(ctx).Warn("backups in the local backup directory will be lost",
			zap.String("path", cfg.Path), zap.String("reason", w))
	}
	return nil
}

// HealthCheck writes and removes a probe file.
//
// Not a stat: a directory that exists and is not writable is the failure this
// needs to catch, and it must be caught before the disaster rather than during
// it. A read-only mount reports healthy to every check that only looks.
func (a *Adapter) HealthCheck(_ context.Context) error {
	if a.dir == "" {
		return errs.New(errs.Internal, "The local backup destination is not configured.")
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return errs.Wrap(errs.Internal, fmt.Sprintf("Pando cannot create the backup directory %s.", a.dir), err)
	}

	probe := filepath.Join(a.dir, ".pando-write-probe")
	if err := os.WriteFile(probe, []byte("ok"), 0o600); err != nil {
		return errs.Wrap(errs.Internal,
			fmt.Sprintf("Pando cannot write to the backup directory %s.", a.dir), err)
	}
	return os.Remove(probe)
}

// Capabilities: a filesystem expires nothing on its own.
func (a *Adapter) Capabilities() api.BackupCapabilities {
	return api.BackupCapabilities{
		OwnsRetention: false,
		Immutable:     false,

		// The adapter does not know the size of the filesystem it sits on and
		// will not guess. A destination that reports a limit it invented would
		// refuse a backup that would have succeeded.
		MaxBundleBytes: 0,

		// A directory on this host, as far as the adapter can tell (R-417).
		OffHost: false,
	}
}

// Writer creates a bundle, written to a temporary name and renamed on Close.
//
// Atomic because a half-written bundle that looks whole is exactly the failure
// R-215 exists to catch, and catching it at restore time is catching it too
// late. A crash mid-write leaves a .partial file, which List ignores.
func (a *Adapter) Writer(_ context.Context, name string) (io.WriteCloser, error) {
	final, err := a.path(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(a.dir, 0o700); err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not create the backup directory.", err)
	}

	tmp := final + ".partial"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not write the backup.", err)
	}
	return &atomicFile{f: f, tmp: tmp, final: final}, nil
}

type atomicFile struct {
	f          *os.File
	tmp, final string
	closed     bool
}

func (w *atomicFile) Write(p []byte) (int, error) { return w.f.Write(p) }

func (w *atomicFile) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true

	// Sync before rename. Without it the rename can land while the contents
	// have not, and the bundle is whole in the directory listing and empty on
	// disk after a power loss — which is the one failure mode a backup must not
	// have.
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		_ = os.Remove(w.tmp)
		return errs.Wrap(errs.Internal, "Pando could not finish writing the backup.", err)
	}
	if err := w.f.Close(); err != nil {
		_ = os.Remove(w.tmp)
		return errs.Wrap(errs.Internal, "Pando could not finish writing the backup.", err)
	}
	if err := os.Rename(w.tmp, w.final); err != nil {
		_ = os.Remove(w.tmp)
		return errs.Wrap(errs.Internal, "Pando could not finish writing the backup.", err)
	}
	return nil
}

func (a *Adapter) Reader(_ context.Context, name string) (io.ReadCloser, error) {
	p, err := a.path(name)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errs.New(errs.NotFound, "That backup is not in this destination.").
			WithRemedy("Check which destination it was written to.")
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not read the backup.", err)
	}
	return f, nil
}

func (a *Adapter) List(_ context.Context) ([]api.StoredBundle, error) {
	entries, err := os.ReadDir(a.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Pando could not list the backups.", err)
	}

	out := make([]api.StoredBundle, 0, len(entries))
	for _, e := range entries {
		// A .partial is a crashed write, not a bundle. Listing one would let a
		// restore be attempted against a file that was never finished.
		if e.IsDir() || strings.HasSuffix(e.Name(), ".partial") || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, api.StoredBundle{
			Name: e.Name(), SizeBytes: info.Size(), StoredAt: info.ModTime().UTC(),
		})
	}
	return out, nil
}

func (a *Adapter) Delete(_ context.Context, name string) error {
	p, err := a.path(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return errs.Wrap(errs.Internal, "Pando could not remove the backup.", err)
	}
	return nil
}

// path joins the name to the directory, refusing anything that would escape it.
//
// Bundle names are Pando's own IDs, so this cannot trigger today. It is here
// because "the caller only ever passes safe values" is how path traversal
// arrives later, in a change that looks unrelated.
func (a *Adapter) path(name string) (string, error) {
	if a.dir == "" {
		return "", errs.New(errs.Internal, "The local backup destination is not configured.")
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", errs.New(errs.ValidInvalid, "That is not a valid backup name.")
	}
	return filepath.Join(a.dir, name), nil
}

var _ api.BackupAdapter = (*Adapter)(nil)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category:    api.CategoryBackup,
		Kind:        Kind,
		Name:        "Local disk",
		Description: "Writes backups to a directory in Pando's container, by default on its data volume.",
		IDPrefix:    "bkp_",
		Fields: []api.Field{
			{Key: "path", Label: "Directory", Type: "string", Help: "A directory in Pando's container, created if it does not exist.", Default: DefaultPath},
		},
		Steps: steps(),
		Warn:  Warnings,
	}
}
