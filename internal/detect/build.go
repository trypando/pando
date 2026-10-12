package detect

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// Built is the image detection built for a proposal (R-311, design 09 §4.1).
//
// Detection builds what the app would deploy, so the plan shows the score of
// that image and the trial run has something to start (R-097). A deploy whose
// spec has the same build inputs, from the same source, runs this image and
// reuses its scan instead of building again: the score in the plan is the
// score of what runs.
type Built struct {
	// Inputs is spec.BuildInputs of the spec it was built from.
	Inputs string `json:"inputs"`

	// Source is the checkout it was built from: a commit, or an upload's
	// digest (source.Checkout.Identity).
	Source string `json:"source"`

	// Image is the app's image, by ID or by digest. Empty for a compose app,
	// whose services each have their own.
	Image string `json:"image,omitempty"`

	// Images are a compose app's built services' images, by workload name.
	Images map[string]string `json:"images,omitempty"`
}

// Scanned is the image a scan of this build reads, and the one the trial
// starts: the app's image, or a compose app's primary service's. The same
// one a deploy scans (R-311), so a deploy of this build finds its scan.
func (b *Built) Scanned(s *spec.AppSpec) string {
	if b == nil {
		return ""
	}
	if b.Image != "" {
		return b.Image
	}
	if s == nil {
		return ""
	}
	if primary, ok := s.PrimaryWorkload(); ok {
		return b.Images[primary.Name]
	}
	return ""
}

// Matches reports whether this build is what s would build from source.
func (b *Built) Matches(s *spec.AppSpec, source string) bool {
	return b != nil && source != "" && b.Source == source && b.Inputs != "" && b.Inputs == spec.BuildInputs(s)
}

// BuildOutcome is what a BuildFunc did: the build, when there was one, and
// the credential a trial pulls its image with.
type BuildOutcome struct {
	Built    *Built
	PullAuth *api.RegistryAuth
}

// BuildFunc builds what a proposal would deploy, once the auction has read
// the repository. It decides whether there is anything to build yet — a
// required build argument nobody has given means there is not — and returns
// an error only for a build that failed, which fails the detection with its
// log (R-107).
type BuildFunc func(ctx context.Context, partial *Proposal) (BuildOutcome, error)

type buildKey struct{}

// WithBuild returns a context whose detection builds through fn after the
// auction and before the trial run. A context and not a field on Job for the
// reason WithProgress is: one Job serves every app, and the build needs the
// app's own answers, defaults and checkout.
func WithBuild(ctx context.Context, fn BuildFunc) context.Context {
	return context.WithValue(ctx, buildKey{}, fn)
}

func buildFrom(ctx context.Context) BuildFunc {
	fn, _ := ctx.Value(buildKey{}).(BuildFunc)
	return fn
}

// notBuildInputs are the questions whose answers change how an image runs and
// not what it is. Every other open question can change the build — a build
// argument, a start command a buildpack plans around, which Dockerfile, which
// reading of the repository — so the build waits for it.
var notBuildInputs = map[string]bool{KeyPrimaryPort: true, KeyPrimaryService: true}

// BuildWaitsOn is the open questions a build of this proposal waits on: those
// whose answers change what would be built. Empty when it can be built now.
func (p Proposal) BuildWaitsOn(answers map[string]string) []string {
	var keys []string
	for _, q := range Open(p.Questions, answers) {
		if !notBuildInputs[q.Key] {
			keys = append(keys, q.Key)
		}
	}
	return keys
}

// NeedsBuild reports whether a spec runs an image Pando builds: not a
// published image, and for a compose file, at least one service with a
// build of its own.
func NeedsBuild(s *spec.AppSpec) bool {
	if s.Build.Strategy == spec.BuildPrebuilt || s.Source.Type == spec.SourceImage || s.Build.Strategy == "" {
		return false
	}
	if s.Build.Strategy == spec.BuildCompose {
		for _, w := range s.Workloads {
			if w.Build != nil {
				return true
			}
		}
		return false
	}
	return true
}

// withPrimaryImage is draft with its primary workload running image, as a
// copy, for the trial run of an image detection built. The proposal's own
// draft keeps naming no image: the spec says how the app is built, and the
// image a build produced belongs to the build.
func withPrimaryImage(draft Draft, image string) Draft {
	out := draft
	out.Workloads = append([]spec.Workload(nil), draft.Workloads...)
	for i, w := range out.Workloads {
		if w.Primary || len(out.Workloads) == 1 {
			out.Workloads[i].Image = image
			return out
		}
	}
	return out
}
