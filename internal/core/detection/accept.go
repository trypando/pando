package detection

import (
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// Accepted is the spec accepting proposal would pin, before the values a
// person sets in the same step: the answers given, then the install's
// defaults, then the decisions carried from the pinned spec.
//
// One function because two places need the same answer. Accepting pins it,
// and the detection diff shows what accepting changes (R-022). The diff used
// to compare the raw proposal, so it reported every variable, slot fill,
// hostname and limit that Carry keeps as removed (issue #350).
//
// defaults may be nil, where the install supplies none. pinned is nil on the
// first acceptance, and then nothing carries.
func Accepted(p detect.Proposal, answers map[string]string, appID, slug string, defaults *spec.Defaults, pinned *spec.AppSpec) spec.AppSpec {
	draft := p.WithAnswers(answers)
	draft.AppID = appID

	// Detection applies the defaults to the winning draft only. Answering the
	// tie-break adopts a different candidate's draft, which never went
	// through that — so accepting one produced a spec with no builder, no
	// isolation floor and no timeout, and the next deploy failed on a builder
	// named "".
	if defaults != nil {
		defaults.Apply(&draft, slug)
	}

	// What a person decided outlives a re-detection (R-022). A proposal
	// describes the repository, not the app: every slot arrives unfilled,
	// and it carries none of the environment somebody typed.
	return *spec.Carry(pinned, &draft)
}
