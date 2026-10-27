package docker

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/atomicfile"
	"github.com/trypando/pando/internal/errs"
)

// certificateName is what a certificate may be called, as a file name.
var certificateName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// writeEdgeCertificates gives an edge the certificates Pando issued for it as
// files in dir (EdgePlan.CertificateDir): <Name>.crt and <Name>.key, written
// only when changed so the edge does not reload for nothing, and any other
// .crt or .key file there removed. Each control host's runtime writes the same
// certificates from the database, so every edge serves what the leader issued
// once (R-417, O-68).
//
// The key is 0600: the edge's process is root in its container with the
// runtime's default capabilities, and reads it as such. Revealed here and
// nowhere earlier, into the edge's own storage (R-194).
func writeEdgeCertificates(dir string, certs []api.EdgeCertificate) error {
	if dir == "" {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not create the directory the edge reads its certificates from.", err)
	}
	keep := map[string]bool{}
	for _, c := range certs {
		if !certificateName.MatchString(c.Name) {
			return errs.Newf(errs.Internal, "%q is not a name a certificate file can have.", c.Name)
		}
		keep[c.Name+".crt"], keep[c.Name+".key"] = true, true
		if err := writeFileIfChanged(dir, c.Name+".crt", c.CertPEM, 0o644); err != nil {
			return err
		}
		if err := writeFileIfChanged(dir, c.Name+".key", []byte(c.KeyPEM.Reveal()), 0o600); err != nil {
			return err
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not read the directory the edge reads its certificates from.", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || keep[name] || (!strings.HasSuffix(name, ".crt") && !strings.HasSuffix(name, ".key")) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not remove the edge's old certificate file %s.", name), err)
		}
	}
	return nil
}

// writeFileIfChanged writes one file atomically, unless it already holds body.
func writeFileIfChanged(dir, name string, body []byte, mode os.FileMode) error {
	if err := atomicfile.WriteIfChanged(filepath.Join(dir, name), body, mode); err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not write the edge's certificate file %s.", name), err)
	}
	return nil
}
