// Package atomicfile writes a file a watcher reads: whole or not at all, and
// not at all when it already holds what would be written. Traefik reloads on
// every write to the directory it watches and reads a half-written file the
// moment it appears, so both the Traefik routing adapter and the Docker
// runtime's edge certificates (R-417) write that way. A leaf package, as
// internal/registrylimit is, so adapters may import it.
package atomicfile

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
)

// WriteIfChanged writes body to path through a temporary file renamed into
// place, unless path already holds body. The directory is created if missing.
//
// The temporary file's name is its own (CreateTemp), as keyfile.place's is:
// route sync, the reconciler and a deploy may write one route at once (R-417),
// and with one fixed name a writer renamed another's half-written file into
// place. It is hidden and has no .yml or .toml extension, so a watcher does
// not load it.
func WriteIfChanged(path string, body []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	// Removed once renamed too, which finds nothing.
	defer func() { _ = os.Remove(tmp.Name()) }()

	_, werr := tmp.Write(body)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
