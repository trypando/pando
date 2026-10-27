// Package keyfile writes a newly generated key to disk so it survives a crash.
//
// The secrets key and the API token key are each created once, on first
// start, and everything encrypted or digested afterwards depends on them. A key
// that was linked into place but never flushed can come back from a power loss
// as an empty file, while the secrets encrypted under it are already committed
// to Postgres and can no longer be read.
package keyfile

import (
	"errors"
	"os"
	"path/filepath"
)

// Create writes key to path, 0600, and flushes it and its directory entry to
// disk before returning.
//
// The key is written beside path and linked into place, which fails with an
// error matching os.ErrExist if a file is already there. Several replicas
// sharing one volume start at once, and two that each wrote their own key would
// each use one the other cannot; the one that loses the link reads the
// winner's instead (issue #72).
func Create(path string, key []byte) error {
	return place(path, key, os.Link)
}

// Replace writes key to path, 0600, in place of whatever is there, flushed as
// Create's is. A host joining a cluster takes the cluster's keys in place of
// the ones it made at its own first start (R-415); renamed into place, so a
// crash leaves the old key or the new one, never half of either.
func Replace(path string, key []byte) error {
	return place(path, key, os.Rename)
}

func place(path string, key []byte, put func(from, to string) error) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()

	_, werr := tmp.Write(key)
	serr := tmp.Sync()
	cerr := tmp.Close()
	if err := errors.Join(werr, serr, cerr); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return err
	}
	if err := put(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(dir)
}

// syncDir flushes a directory, so that a link made in it survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- the directory of a configured key path
	if err != nil {
		return err
	}
	serr := d.Sync()
	return errors.Join(serr, d.Close())
}
