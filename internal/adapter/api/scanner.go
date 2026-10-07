package api

import (
	"context"
	"time"
)

// The scanner category (R-317, design 09 §2).
//
// The ninth category. Pando does not implement a scanner: it asks one what is
// wrong with an image and a checkout, turns the answer into a score, and lets
// host policy act on it. Which findings exist is the scanner's expertise; what
// they cost and what happens next is Pando's (R-251).
//
// Nothing here is a Docker, Trivy or CVE concept. `Severity` is Pando's own
// five-value scale because the arithmetic in core cannot be handed a string
// some scanner invented — an adapter maps its own vocabulary into this one, and
// a finding whose severity nobody has decided is `unknown` rather than a guess.

// Severity is how much a finding costs.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"

	// SeverityUnknown is a finding the scanner reported without deciding how
	// bad it is. It scores as low and is shown as unknown: inventing a severity
	// is how a score stops meaning anything.
	SeverityUnknown Severity = "unknown"
)

// ScanRequest is what to look at.
//
// An image and a directory, and deliberately nothing else. The scanner is given
// no secrets, no volumes and no network of the app's — a component whose job is
// to read untrusted code is the last one that should hold anything.
type ScanRequest struct {
	AppID  string
	SpecID string

	// Commit is the source commit SourceDir holds, recorded with the scan so
	// a deploy of the same commit uses it rather than scanning again.
	Commit string

	// Image is what the build produced, in the local runtime. Empty for an app
	// that has never been built.
	Image string

	// PullAuth fetches Image from the install's registry when the build was
	// pushed there rather than imported (issue #72, PR 5). Nil for an image
	// already on the scanner's host.
	PullAuth *RegistryAuth

	// SourceDir is a checkout of the app's source, for scanners that read the
	// tree rather than the image. Empty when there is none.
	SourceDir string
}

// Finding is one thing wrong.
type Finding struct {
	// ID is the scanner's identifier for it: CVE-2024-1234, or the rule that
	// fired. Shown verbatim, and what somebody searches for.
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Title    string   `json:"title"`

	// Target is where it is: the package, the file, the image layer.
	Target string `json:"target"`

	// Fix is the version that fixes it, when the scanner knows one. Empty is
	// common and is not an error — plenty of findings have no fix yet.
	Fix string `json:"fix,omitempty"`
}

// ScanResult is what a scanner found.
type ScanResult struct {
	Findings []Finding

	// Scanner names what produced this, with its version. It is stored with the
	// scan and shown beside the score, because "scanned" is not a fact on its
	// own — by what, and how long ago, is the rest of it.
	Scanner string

	Ran time.Time
}

// ScannerCapabilities is what a scanner can look at.
//
// Data, never a type assertion (R-254): the planner decides whether a scan is
// possible from this, and an adapter that cannot read an image says so here
// rather than by failing when asked.
type ScannerCapabilities struct {
	// ScansImages is whether it can read a built image.
	ScansImages bool
	// ScansSource is whether it can read a source tree.
	ScansSource bool
}

// ScannerAdapter scans what an app deploys.
type ScannerAdapter interface {
	Adapter
	Scan(ctx context.Context, req ScanRequest) (ScanResult, error)
	ScannerCapabilities() ScannerCapabilities
}
