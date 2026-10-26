package api

import (
	"context"
	"io"
	"time"
)

// BackupAdapter stores and retrieves DR bundles and per-app backups (R-217).
//
// The eighth category, added in phase 9. It reverses an earlier decision that a
// destination was a byte sink rather than a category — design 03 §8.1 records
// the reversal and what the old argument got right. The short reason it is a
// category: an object store expires and versions objects on its own schedule
// and a filesystem path does not, so who owns retention is a real question with
// a real wrong answer, and R-254 says a question like that is answered by a
// capabilities struct rather than by a type assertion.
//
// Bundles are opaque here. The adapter never sees plaintext: encryption happens
// in core under a passphrase supplied at backup time (R-213), so a destination
// that is compromised yields ciphertext and a manifest-shaped blob.
type BackupAdapter interface {
	Adapter

	Capabilities() BackupCapabilities

	// Writer opens a bundle for writing. The caller closes it, and a bundle is
	// only considered present once Close returns without error — a destination
	// that can write atomically should do so, because a half-written bundle
	// that looks whole is the failure R-215 exists to catch.
	Writer(ctx context.Context, name string) (io.WriteCloser, error)

	// Reader opens a stored bundle.
	Reader(ctx context.Context, name string) (io.ReadCloser, error)

	// List returns what the destination is holding. Restore does not need it —
	// Pando records every bundle it wrote — but reconciling that record against
	// reality is how an operator learns a destination expired something out
	// from under them, which is the failure a DR plan cannot afford to discover
	// during a disaster (R-216).
	List(ctx context.Context) ([]StoredBundle, error)

	// Delete removes a bundle. Called only when Pando owns retention; see
	// BackupCapabilities.OwnsRetention.
	Delete(ctx context.Context, name string) error
}

// BackupCapabilities is what the destination can do, as data (R-254).
type BackupCapabilities struct {
	// OwnsRetention says the destination expires bundles on its own schedule —
	// an object-store lifecycle rule, say. When true Pando records
	// retain_until and does not enforce it: pruning what the store has already
	// locked fails every time, and the failure looks like a Pando bug.
	//
	// When false (the local filesystem) Pando prunes, which is R-211's daily
	// retention doing what it says.
	OwnsRetention bool

	// Immutable says a written bundle cannot be deleted or overwritten before
	// its retention expires — a compliance lock. Pando must not offer to delete
	// one, because the offer would fail.
	Immutable bool

	// MaxBundleBytes is 0 when the destination has no limit it knows about. A
	// DR bundle contains a pg_dump and every app volume, so this is a real
	// constraint on small destinations, and it is a plan-time question rather
	// than a discovery to make at byte 4,294,967,296.
	MaxBundleBytes int64

	// OffHost says bundles are kept somewhere other than the machine Pando
	// runs on, so losing that machine does not lose them. A cluster with a
	// second host requires it of the default destination (R-417).
	OffHost bool
}

// StoredBundle is one bundle as the destination sees it.
//
// Deliberately thin: the destination knows a name, a size and a time. Anything
// about what is *inside* comes from the manifest, which is Pando's (R-215).
type StoredBundle struct {
	Name      string
	SizeBytes int64
	StoredAt  time.Time
}
