package detect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// proposalFor is a winner's draft with one primary workload, which is the shape
// every answer below applies to.
func proposalFor(workloads ...spec.Workload) detect.Proposal {
	return detect.Proposal{
		Winner:    detect.Candidate{Strategy: spec.BuildDockerfile},
		DraftSpec: spec.AppSpec{Workloads: workloads},
	}
}

func primary(name string) spec.Workload {
	return spec.Workload{Name: name, Primary: true, Exposed: true}
}

// An answer is worth more than anything Pando worked out for itself, so it
// overwrites — and a port supplied by a person is recorded with Source "user",
// which is what the review UI shows beside it. Where the value came from is
// part of the value.
func TestAPortSuppliedByAPersonIsRecordedAsTheirs(t *testing.T) {
	p := proposalFor(primary("web"))
	p.DraftSpec.Workloads[0].Ports = []spec.Port{{Number: 8080, Source: spec.PortFramework}}

	got := p.WithAnswers(map[string]string{detect.KeyPrimaryPort: "3000"})

	require.Len(t, got.Workloads[0].Ports, 1)
	require.Equal(t, 3000, got.Workloads[0].Ports[0].Number)
	require.Equal(t, "http", got.Workloads[0].Ports[0].Protocol)
	require.Equal(t, spec.PortUser, got.Workloads[0].Ports[0].Source)
}

func TestAPortThatIsNotAPortIsIgnoredRatherThanApplied(t *testing.T) {
	p := proposalFor(primary("web"))

	for _, answer := range []string{"not a number", "0", "-1", "65536", "99999"} {
		got := p.WithAnswers(map[string]string{detect.KeyPrimaryPort: answer})
		require.Empty(t, got.Workloads[0].Ports, answer)
	}

	require.NotEmpty(t, p.WithAnswers(map[string]string{detect.KeyPrimaryPort: "65535"}).Workloads[0].Ports)
	require.NotEmpty(t, p.WithAnswers(map[string]string{detect.KeyPrimaryPort: "1"}).Workloads[0].Ports)
}

// Exactly one workload is primary — the app has one canonical endpoint — so the
// flag is cleared everywhere before being set, rather than trusting the draft
// to have had it in one place.
func TestChoosingAPrimaryServiceMovesTheFlagRatherThanAddingOne(t *testing.T) {
	p := proposalFor(primary("web"), spec.Workload{Name: "api"}, spec.Workload{Name: "worker"})

	got := p.WithAnswers(map[string]string{detect.KeyPrimaryService: "api"})

	byName := map[string]spec.Workload{}
	for _, w := range got.Workloads {
		byName[w.Name] = w
	}
	require.True(t, byName["api"].Primary)
	require.True(t, byName["api"].Exposed)
	require.False(t, byName["web"].Primary)
	require.False(t, byName["web"].Exposed)
	require.False(t, byName["worker"].Primary)
}

// An answer naming something that is not there is not a choice. Ignored rather
// than invented: stamping it on the draft is the bug this guards against.
func TestAnAnswerNamingAWorkloadThatIsNotThereChangesNothing(t *testing.T) {
	p := proposalFor(primary("web"), spec.Workload{Name: "api"})

	got := p.WithAnswers(map[string]string{detect.KeyPrimaryService: "nonexistent"})
	require.True(t, got.Workloads[0].Primary, "web is still the primary")
	require.False(t, got.Workloads[1].Primary)
}

// Kept as a shell command rather than split into argv: splitting on whitespace
// breaks the first quoted argument anyone writes, and the person answering this
// wrote a command line, not an argv.
func TestAStartCommandIsRunThroughAShell(t *testing.T) {
	p := proposalFor(primary("web"), spec.Workload{Name: "worker"})

	got := p.WithAnswers(map[string]string{detect.KeyStartCommand: `node server.js --flag "a b"`})

	require.Equal(t, []string{"sh", "-c", `node server.js --flag "a b"`}, got.Workloads[0].Command)
	require.Empty(t, got.Workloads[1].Command, "only the primary is given a start command")
}

func TestAnsweringTheDockerfilePathAlsoSettlesTheStrategy(t *testing.T) {
	got := proposalFor(primary("web")).
		WithAnswers(map[string]string{detect.KeyDockerfilePath: "docker/Dockerfile.prod"})

	require.Equal(t, "docker/Dockerfile.prod", got.Build.Dockerfile)
	require.Equal(t, spec.BuildDockerfile, got.Build.Strategy)
}

// The answer names a directory inside the repository, which is what a subdir
// is. Detection runs again against it: nothing about a proposal describing the
// repository root applies to a project one level down.
func TestNamingTheDeployableProjectSetsTheSubdirectory(t *testing.T) {
	asked := proposalFor()
	asked.Winner.Strategy = detect.StrategyUnknown
	asked.Questions = []detect.Question{{Key: detect.KeyDeployableProject}}
	asked.DraftSpec.Source.Subdir = "repo"

	got := asked.WithAnswers(map[string]string{detect.KeyDeployableProject: "services/api"})
	require.Equal(t, "repo/services/api", got.Source.Subdir, "inside the directory the proposal read")

	// The proposal detection made in that project does not ask, and already
	// reads it: the kept answer is not applied to it a second time.
	project := proposalFor(primary("web"))
	project.DraftSpec.Source.Subdir = "repo/services/api"
	got = project.WithAnswers(map[string]string{detect.KeyDeployableProject: "services/api"})
	require.Equal(t, "repo/services/api", got.Source.Subdir)
}

func TestAStaticSourceAnswerChoosesBetweenServingAndBuilding(t *testing.T) {
	served := proposalFor(primary("web")).
		WithAnswers(map[string]string{detect.KeyStaticSource: "dist"})
	require.Equal(t, spec.BuildStatic, served.Build.Strategy)

	built := proposalFor(primary("web")).
		WithAnswers(map[string]string{detect.KeyStaticSource: "run-build"})
	require.Equal(t, spec.BuildBuildpack, built.Build.Strategy)
}

// An empty answer is not an answer: somebody cleared the field rather than
// deciding, and overwriting a detected value with nothing would lose it.
func TestABlankAnswerIsSkipped(t *testing.T) {
	p := proposalFor(primary("web"))
	p.DraftSpec.Workloads[0].Ports = []spec.Port{{Number: 8080, Source: spec.PortFramework}}

	got := p.WithAnswers(map[string]string{
		detect.KeyPrimaryPort:  "   ",
		detect.KeyStartCommand: "",
	})

	require.Equal(t, 8080, got.Workloads[0].Ports[0].Number)
	require.Equal(t, spec.PortFramework, got.Workloads[0].Ports[0].Source)
	require.Empty(t, got.Workloads[0].Command)
}

func TestAnswersAreTrimmed(t *testing.T) {
	got := proposalFor(primary("web")).
		WithAnswers(map[string]string{detect.KeyPrimaryPort: "  3000  "})
	require.Equal(t, 3000, got.Workloads[0].Ports[0].Number)
}

// A proposal is read from storage and may be applied more than once. Mutating
// it in place would make the second application see the first one's results.
func TestApplyingAnswersTwiceGivesTheSameSpec(t *testing.T) {
	p := proposalFor(primary("web"), spec.Workload{Name: "api"})
	answers := map[string]string{
		detect.KeyPrimaryPort:    "3000",
		detect.KeyPrimaryService: "api",
	}

	// Repeated, because the bug this guards against was Go's randomized map
	// order: a single pair of runs agreed most of the time.
	first := p.WithAnswers(answers)
	for range 50 {
		require.Equal(t, first, p.WithAnswers(answers))
	}

	require.True(t, p.DraftSpec.Workloads[0].Primary, "the proposal itself is untouched")
	require.Empty(t, p.DraftSpec.Workloads[0].Ports)
}

// An answer that changes what the other answers apply to has to be applied
// before them.
//
// withPrimaryService decides which workload is primary; withPort and
// withStartCommand both write to "the primary workload". Ranging over the
// answers map applied them in random order, so the port landed on whichever
// workload happened to be primary first — and the spec someone reviewed was not
// necessarily the one that got pinned.
func TestAnswersThatDependOnEachOtherAreAppliedInOrder(t *testing.T) {
	p := proposalFor(primary("web"), spec.Workload{Name: "api"})

	got := p.WithAnswers(map[string]string{
		detect.KeyPrimaryService: "api",
		detect.KeyPrimaryPort:    "3000",
		detect.KeyStartCommand:   "node api.js",
	})

	byName := map[string]spec.Workload{}
	for _, w := range got.Workloads {
		byName[w.Name] = w
	}

	require.True(t, byName["api"].Primary)
	require.Len(t, byName["api"].Ports, 1, "the port goes on the workload the answers chose")
	require.Equal(t, 3000, byName["api"].Ports[0].Number)
	require.Equal(t, []string{"sh", "-c", "node api.js"}, byName["api"].Command)

	require.Empty(t, byName["web"].Ports, "and not on the one that used to be primary")
	require.Empty(t, byName["web"].Command)
}

// The tie-break offers the close candidates' own strategies, so an answer
// naming one means "that detector read this repository correctly" — a different
// draft, not a different label on this one.
func TestAnsweringTheTieBreakAdoptsThatCandidatesWholeDraft(t *testing.T) {
	runnerUp := spec.AppSpec{
		Workloads: []spec.Workload{{Name: "static", Primary: true}},
		Build:     spec.Build{Strategy: spec.BuildStatic},
	}
	p := proposalFor(primary("web"))
	p.RunnersUp = []detect.Candidate{{
		Strategy: spec.BuildStatic,
		Draft:    detect.Draft{Workloads: []spec.Workload{{Name: "static", Primary: true}}},
		Spec:     &runnerUp,
	}}

	got := p.WithAnswers(map[string]string{detect.KeyBuildStrategy: string(spec.BuildStatic)})

	require.Equal(t, spec.BuildStatic, got.Build.Strategy)
	require.Equal(t, "static", got.Workloads[0].Name,
		"the whole draft is adopted, not just the strategy label")
}

func TestAnsweringTheTieBreakWithTheWinnerChangesNothing(t *testing.T) {
	p := proposalFor(primary("web"))

	got := p.WithAnswers(map[string]string{detect.KeyBuildStrategy: string(spec.BuildDockerfile)})
	require.Equal(t, "web", got.Workloads[0].Name)

	// And so does naming a candidate that did not bid.
	got = p.WithAnswers(map[string]string{detect.KeyBuildMethod: "carrier-pigeon"})
	require.Equal(t, "web", got.Workloads[0].Name)
}

func TestTheOlderBuildMethodKeyIsAcceptedToo(t *testing.T) {
	runnerUp := spec.AppSpec{
		Workloads: []spec.Workload{{Name: "static", Primary: true}},
		Build:     spec.Build{Strategy: spec.BuildStatic},
	}
	p := proposalFor(primary("web"))
	p.RunnersUp = []detect.Candidate{{
		Strategy: spec.BuildStatic,
		Draft:    detect.Draft{Workloads: []spec.Workload{{Name: "static", Primary: true}}},
		Spec:     &runnerUp,
	}}

	got := p.WithAnswers(map[string]string{detect.KeyBuildMethod: string(spec.BuildStatic)})
	require.Equal(t, "static", got.Workloads[0].Name)
}

// A candidate that recognized the repository and could not import it — a
// compose file using a construct that cannot cross the boundary (R-099) — has
// no workloads. Adopting it would replace a working proposal with an empty one.
func TestR099_ACandidateThatImportedNothingIsNotAdopted(t *testing.T) {
	p := proposalFor(primary("web"))
	p.RunnersUp = []detect.Candidate{{Strategy: spec.BuildCompose}}

	got := p.WithAnswers(map[string]string{detect.KeyBuildStrategy: string(spec.BuildCompose)})
	require.Equal(t, "web", got.Workloads[0].Name, "the working proposal stands")
}

// The completed spec is what detection filled with the install's defaults, its
// routing and its port. Assembling the raw draft instead produces a spec with
// none of that, and the failure lands at deploy time as a complaint about a
// port number.
func TestACandidateWithNoCompletedSpecIsAssembledFromItsDraft(t *testing.T) {
	p := proposalFor(primary("web"))
	p.RunnersUp = []detect.Candidate{{
		Strategy: spec.BuildStatic,
		Draft:    detect.Draft{Workloads: []spec.Workload{{Name: "static", Primary: true}}},
	}}

	got := p.WithAnswers(map[string]string{detect.KeyBuildStrategy: string(spec.BuildStatic)})
	require.Equal(t, "static", got.Workloads[0].Name)
}

func TestNoAnswersLeavesTheWinnersDraft(t *testing.T) {
	p := proposalFor(primary("web"))
	require.Equal(t, p.DraftSpec.Workloads, p.WithAnswers(nil).Workloads)
	require.Equal(t, p.DraftSpec.Workloads, p.WithAnswers(map[string]string{}).Workloads)
}
