// Package tokenkey holds the key API token digests are made with (R-063).
//
// An API token is stored as HMAC-SHA-256 of its secret under this key, so a
// database dump alone cannot be used to test a guess at any token: the key is
// in a file beside the process, not in Postgres. It is core's own key, not an
// adapter's — unlike the secrets key, no adapter can be swapped in for it.
//
// Every replica on an install must hold the same key, which is why the file is
// created the same way the local secrets key is (several replicas on one
// shared volume end with one key) and why state.Tokens.VerifyKey checks it at
// start. A DR bundle carries it beside the secrets key (R-212); a restore
// without it makes every API token unusable.
package tokenkey

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/trypando/pando/internal/errs"
)

// Size is the key length in bytes.
const Size = 32

// DefaultPath is where the key lives unless server.token_key_path says
// otherwise: beside the secrets key, on the volume every replica shares.
const DefaultPath = "/var/lib/pando/token.key"

// LoadOrCreate reads the key at path, generating it on first use.
//
// Written 0600 beside the path and linked into place, which fails if a key is
// already there. Several replicas sharing one volume start at once, and two
// that each wrote their own key would each issue tokens the other could not
// verify; the one that loses the link reads the winner's instead (issue #72).
func LoadOrCreate(path string) ([]byte, error) {
	key, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(key) != Size {
			return nil, errs.Newf(errs.Internal,
				"The API token key at %s is %d bytes long, and Pando expects %d. No API token can be checked without the right key.", path, len(key), Size).
				WithRemedy("Restore the key file from a backup of this installation, or from another Pando replica on it. If no copy exists, delete the file and start Pando: every API token then has to be issued again.")
		}
		return key, nil

	case errors.Is(err, os.ErrNotExist):
		key = make([]byte, Size)
		if _, err := io.ReadFull(rand.Reader, key); err != nil {
			return nil, errs.Wrap(errs.Internal, "Pando could not generate an API token key.", err)
		}
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not create %s for the API token key.", dir), err)
		}
		tmp, err := os.CreateTemp(dir, ".token-key-*")
		if err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the API token key to %s.", path), err)
		}
		defer func() { _ = os.Remove(tmp.Name()) }()
		_, werr := tmp.Write(key)
		cerr := tmp.Close()
		if werr != nil || cerr != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the API token key to %s.", path), errors.Join(werr, cerr))
		}
		if err := os.Chmod(tmp.Name(), 0o600); err != nil {
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the API token key to %s.", path), err)
		}
		if err := os.Link(tmp.Name(), path); err != nil {
			if errors.Is(err, os.ErrExist) {
				return LoadOrCreate(path)
			}
			return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write the API token key to %s.", path), err)
		}
		return key, nil

	default:
		return nil, errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not read the API token key at %s.", path), err)
	}
}
