package backup

import (
	"archive/tar"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"time"

	"github.com/trypando/pando/internal/errs"
)

// BundleVersion is the on-disk format version.
//
// Restore refuses a version it does not know rather than guessing, because a
// bundle written by a newer Pando may describe objects this one cannot place.
// Guessing here fails halfway through, which is the one outcome R-215 forbids.
const BundleVersion = 1

// Manifest is what restore verifies against before touching anything (R-215).
//
// Written as the FIRST entry in the tar so it can be read without buffering the
// whole bundle — a DR bundle holds a pg_dump and every app volume, and a verify
// that has to hold all of it in memory is a verify nobody runs.
type Manifest struct {
	Version   int       `json:"version"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`

	// PandoVersion and SchemaVersion are what restore checks compatibility
	// against. The schema version is the migration number: a bundle from a
	// newer schema cannot be restored into an older binary, because the
	// migrations that would make sense of it do not exist here yet.
	PandoVersion  string `json:"pando_version"`
	SchemaVersion uint   `json:"schema_version"`

	// Entries is every file in the bundle with its size and checksum. Restore
	// compares against this before applying, and the comparison is what makes
	// "incomplete" a detectable state rather than a surprise.
	Entries []Entry `json:"entries"`

	// Counts are object counts from the state store — apps, users, grants,
	// specs, volumes. Checksums prove the bytes arrived; counts prove the
	// bytes describe the install someone thinks they are restoring. An
	// operator staring at a restore prompt needs the second one.
	Counts map[string]int `json:"counts"`
}

// Entry is one file inside the bundle.
type Entry struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Named parts of a bundle. Fixed names because restore looks for them.
const (
	ManifestName  = "manifest.json"
	PostgresName  = "postgres.dump"
	SecretsKey    = "secrets.key"
	TokenKey      = "token.key"
	AdaptersName  = "adapters.json"
	PolicyName    = "policy.json"
	VolumesPrefix = "volumes/"

	// ServicesPrefix holds provisioned services whose data is not in an app
	// volume (R-212).
	//
	// Empty for every install running only the in-bundle provisioner, whose
	// databases live in app volumes and are already under volumes/. It exists
	// for the adapter that provisions somewhere Pando can only reach over the
	// wire, where Snapshot is the only way the data gets into the bundle.
	ServicesPrefix = "services/"

	// EdgesPrefix holds the storage of what Pando runs in front of itself —
	// Traefik's certificate store (R-174, design 03 §4.4). Restoring it means
	// a rebuilt host does not ask Let's Encrypt for every certificate again
	// at once and meet its rate limits. Named by the runtime's own handle.
	EdgesPrefix = "edges/"
)

// Writer assembles a bundle.
//
// Two passes are impossible — the payload is streamed to a destination that may
// be remote — so the manifest cannot list checksums it has not computed yet.
// The resolution: a Writer builds the payload into a buffer-like sink supplied
// by the caller, recording each entry as it goes, and Finish writes the
// manifest last. Verify therefore reads the manifest from the END of a bundle,
// and the cost is that a verify reads the whole bundle. That cost is the point:
// a verify that did not read every byte could not have checked every checksum.
type Writer struct {
	tw      *tar.Writer
	entries []Entry
	counts  map[string]int
	kind    string
	version string
	schema  uint
}

// NewWriter starts a bundle written into w.
func NewWriter(w io.Writer, kind, pandoVersion string, schemaVersion uint) *Writer {
	return &Writer{
		tw:      tar.NewWriter(w),
		counts:  map[string]int{},
		kind:    kind,
		version: pandoVersion,
		schema:  schemaVersion,
	}
}

// Add copies one file into the bundle, checksumming as it goes.
//
// Size must be known up front because tar requires it in the header. A caller
// that cannot say how large something is must buffer it first — and for a
// pg_dump that means to a temporary file, not to memory.
func (b *Writer) Add(name string, size int64, r io.Reader) error {
	if err := b.tw.WriteHeader(&tar.Header{
		Name: name, Mode: 0o600, Size: size, ModTime: time.Now().UTC(), Typeflag: tar.TypeReg,
	}); err != nil {
		return errs.Wrap(errs.Internal, "Pando could not assemble the backup.", err)
	}

	sum := sha256.New()
	written, err := io.Copy(io.MultiWriter(b.tw, sum), r)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not assemble the backup.", err)
	}
	if written != size {
		// tar would pad or truncate silently. A bundle whose header disagrees
		// with its contents is exactly the corruption verify exists to find,
		// and finding it here costs one comparison.
		return errs.New(errs.Internal,
			fmt.Sprintf("The backup entry %s changed size while it was being written.", name))
	}

	b.entries = append(b.entries, Entry{Name: name, Size: size, SHA256: hex.EncodeToString(sum.Sum(nil))})
	return nil
}

// Count records an object count for the manifest.
func (b *Writer) Count(object string, n int) { b.counts[object] = n }

// Finish writes the manifest and closes the archive.
func (b *Writer) Finish() (Manifest, error) {
	sort.Slice(b.entries, func(i, j int) bool { return b.entries[i].Name < b.entries[j].Name })

	m := Manifest{
		Version:       BundleVersion,
		Kind:          b.kind,
		CreatedAt:     time.Now().UTC(),
		PandoVersion:  b.version,
		SchemaVersion: b.schema,
		Entries:       b.entries,
		Counts:        b.counts,
	}

	body, err := json.Marshal(m)
	if err != nil {
		return Manifest{}, errs.Wrap(errs.Internal, "Pando could not write the backup manifest.", err)
	}
	if err := b.tw.WriteHeader(&tar.Header{
		Name: ManifestName, Mode: 0o600, Size: int64(len(body)),
		ModTime: m.CreatedAt, Typeflag: tar.TypeReg,
	}); err != nil {
		return Manifest{}, errs.Wrap(errs.Internal, "Pando could not write the backup manifest.", err)
	}
	if _, err := b.tw.Write(body); err != nil {
		return Manifest{}, errs.Wrap(errs.Internal, "Pando could not write the backup manifest.", err)
	}
	if err := b.tw.Close(); err != nil {
		return Manifest{}, errs.Wrap(errs.Internal, "Pando could not finish the backup.", err)
	}
	return m, nil
}

// Verified is the result of checking a bundle.
type Verified struct {
	Manifest Manifest

	// Found is every entry actually present, by name.
	Found map[string]Entry
}

// Verify reads a decrypted bundle and checks it against its own manifest
// (R-215, R-216).
//
// Reads the whole stream and checksums every entry. Returns an error naming
// what is wrong — a missing entry, a size mismatch, a checksum mismatch, an
// unknown format version — rather than a boolean, because an operator deciding
// whether to restore needs to know which.
//
// Touches nothing. That is the property Sequence D asserts: a bad bundle is
// rejected with the target install completely untouched, not mostly untouched.
func Verify(r io.Reader) (Verified, error) {
	tr := tar.NewReader(r)
	found := map[string]Entry{}
	var manifest *Manifest

	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Verified{}, errs.New(errs.BackupIncomplete,
				"This backup is damaged and cannot be read.").
				WithRemedy("Restore from an earlier backup, and check the destination it was written to.")
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}

		if header.Name == ManifestName {
			var m Manifest
			if err := json.NewDecoder(tr).Decode(&m); err != nil {
				return Verified{}, errs.New(errs.BackupIncomplete,
					"This backup's manifest is damaged, so Pando cannot tell what should be inside it.")
			}
			manifest = &m
			continue
		}

		sum := sha256.New()
		// G110: nothing is buffered — the entry streams through a hash, so a
		// bundle claiming a petabyte costs time and not memory. Reaching here
		// also means the bundle's passphrase already decrypted it, so the input
		// is not attacker-supplied in the sense the rule assumes.
		n, err := io.Copy(sum, tr) //nolint:gosec
		if err != nil {
			return Verified{}, errs.New(errs.BackupIncomplete, "This backup is damaged and cannot be read.")
		}
		found[header.Name] = Entry{Name: header.Name, Size: n, SHA256: hex.EncodeToString(sum.Sum(nil))}
	}

	if manifest == nil {
		return Verified{}, errs.New(errs.BackupIncomplete,
			"This backup has no manifest, so Pando cannot check whether it is complete.").
			WithRemedy("A bundle without a manifest was not written by Pando, or was not written fully.")
	}
	if manifest.Version != BundleVersion {
		return Verified{}, errs.Newf(errs.BackupIncomplete,
			"This backup is in format version %d and this version of Pando reads version %d.",
			manifest.Version, BundleVersion).
			WithRemedy("Restore it with the version of Pando that wrote it.")
	}

	for _, want := range manifest.Entries {
		got, present := found[want.Name]
		if !present {
			return Verified{}, errs.Newf(errs.BackupIncomplete,
				"This backup is missing %s, which its manifest says should be there.", want.Name).
				WithRemedy("Restore from an earlier backup.")
		}
		if got.Size != want.Size {
			return Verified{}, errs.Newf(errs.BackupIncomplete,
				"%s in this backup is %d bytes and its manifest says %d.", want.Name, got.Size, want.Size)
		}
		if got.SHA256 != want.SHA256 {
			return Verified{}, errs.Newf(errs.BackupIncomplete,
				"%s in this backup does not match its checksum, so its contents have changed since it was written.",
				want.Name).
				WithRemedy("Do not restore this bundle. Use an earlier backup.")
		}
	}

	// An entry present but unlisted is as much a problem as one missing: it is
	// a file somebody added, and restore would place it.
	for name := range found {
		if !listed(manifest.Entries, name) {
			return Verified{}, errs.Newf(errs.BackupIncomplete,
				"This backup contains %s, which its manifest does not list.", name).
				WithRemedy("Do not restore this bundle.")
		}
	}

	return Verified{Manifest: *manifest, Found: found}, nil
}

func listed(entries []Entry, name string) bool {
	for _, e := range entries {
		if e.Name == name {
			return true
		}
	}
	return false
}
