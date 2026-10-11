package security

import (
	"strings"

	"github.com/trypando/pando/internal/core/state"
)

// Scope is what a scan read (R-311): the source, the image, or both. Said on
// the wire rather than left for each client to work out, so a number is never
// read as covering more than it does.
type Scope string

const (
	// ScopeSource: the source alone, with no image.
	ScopeSource Scope = "source"
	// ScopeImage: an image alone — an image app, whose only source is the
	// image's own digest, or Scan now on a deployed app.
	ScopeImage Scope = "image"
	// ScopeImageAndSource: an image and the source it came from.
	ScopeImageAndSource Scope = "image_and_source"
)

// ScopeOf says what a scan read.
func ScopeOf(scan state.Scan) Scope {
	switch {
	case scan.Image == "":
		return ScopeSource
	case scan.Commit == "" || scan.Image == scan.Commit || strings.HasSuffix(scan.Image, "@"+scan.Commit):
		return ScopeImage
	default:
		return ScopeImageAndSource
	}
}

// AwaitsImage reports whether a scan read the source alone, before there was a
// revision: detection's scan of a repository Pando builds, or Scan now on an
// app not yet accepted. Its score is the source's and not the app's. The app
// runs an image nobody has built yet, and R-311 says the score comes from what
// the app deploys. The findings stand; the number waits for a scan of the
// image.
func AwaitsImage(scan state.Scan) bool {
	return scan.SpecID == "" && scan.Image == ""
}
