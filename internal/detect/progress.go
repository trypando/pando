package detect

import "context"

// Detection's stages, in the order they happen, as reported while it runs.
// The console's onboarding page builds itself from them: it shows what Pando
// has found so far rather than a spinner and then everything at once.
//
// Two run beside the others rather than in turn: the scan of the source starts
// as soon as the auction has read the repository, alongside the build, and
// the scan of the built image runs alongside the trial. A running detection
// reports its stage — the step the rest wait on — and every step under way
// (Running), so a client shows both while both run.
const (
	StageFetching  = "fetching"  // cloning the repository
	StageDetecting = "detecting" // detectors bidding on what the app is
	StageBuilding  = "building"  // building the image the app would deploy (R-311)
	StageTrying    = "trying"    // a trial run of the winning draft (R-097)
	StageScanning  = "scanning"  // a security scan of the source and the image, when a scanner is configured
	StageScreening = "screening" // an AI adapter repairing the plan or answering its questions (R-336)
)

// ProgressFunc hears each stage as detection reaches it, with the proposal as
// far as it has got — nil before there is one.
type ProgressFunc func(stage string, partial *Proposal)

type progressKey struct{}

// WithProgress returns a context whose detection reports its stages to fn. A
// context and not a field on Job, because one Job serves every app's detection
// at once and each has its own listener.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// Report tells the context's listener, if any, that detection reached stage.
func Report(ctx context.Context, stage string, partial *Proposal) {
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && fn != nil {
		fn(stage, partial)
	}
}
