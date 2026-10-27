// Package atomicfile writes a file a watcher reads: whole or not at all, and
// not at all when it already holds what would be written. Traefik reloads on
// every write to the directory it watches and reads a half-written file the
// moment it appears, so both the Traefik routing adapter and the Docker
// runtime's edge certificates (R-417) write that way. A leaf package, as
// internal/registrylimit is, so adapters may import it.
package atomicfile

import (
	"bytes"
	"os"
	"path/filepath"
)

// WriteIfChanged writes body to path through a temporary file renamed into
// place, unless path already holds body. The directory is created if missing.
func WriteIfChanged(path string, body []byte, mode os.FileMode) error {
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, body) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, mode); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
