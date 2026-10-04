package api

import (
	"context"

	"github.com/trypando/pando/internal/secret"
)

// SelfWorkload is how Pando's own workload runs (R-355).
type SelfWorkload struct {
	// ID identifies the workload to the runtime.
	ID string
	// Image is the image reference it was started from, as its deployment
	// configuration names it: trypando/pando:latest, trypando/pando:0.3.1,
	// trypando/pando@sha256:…. Whether that is a moving tag decides whether
	// an in-place upgrade is possible.
	Image string
}

// HelperSpec is the short-lived workload that replaces Pando (R-359). It runs
// Pando's current image, so the code doing the replacing is the version
// already trusted, with Pando's networks, its data and the runtime's socket.
type HelperSpec struct {
	// Args are the pando subcommand and its flags.
	Args []string
	// Env carries what must not appear in a process listing: the database
	// URL. Rendered as [redacted] everywhere but the runtime call (R-194).
	Env map[string]secret.Value
}

// SelfUpgrader is implemented by a runtime adapter that reports
// SupportsSelfUpgrade. It does nothing to Pando's state: the database copy
// and its restore are core's, done by the helper (R-027).
type SelfUpgrader interface {
	// Self reports Pando's own workload, or an error saying why Pando is not
	// running on this runtime.
	Self(ctx context.Context) (SelfWorkload, error)

	// PullImage fetches an image by digest, so a missing or unreachable image
	// fails while Pando is still running rather than after it has stopped.
	PullImage(ctx context.Context, ref string) error

	// StartHelper starts the helper and returns its ID. Pando is stopped by
	// the helper shortly after this returns.
	StartHelper(ctx context.Context, spec HelperSpec) (string, error)

	// RemoveHelpers removes helpers that have finished, once the Pando that
	// started next has read what they recorded.
	RemoveHelpers(ctx context.Context) error
}
