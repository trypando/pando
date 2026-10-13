package detect

import (
	"fmt"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Question keys detection produces. Answers are matched back by these, so they
// are a small fixed vocabulary rather than free-form strings.
const (
	KeyPrimaryPort       = "primary_port"
	KeyPrimaryService    = "primary_service"
	KeyStartCommand      = "start_command"
	KeyDockerfilePath    = "dockerfile_path"
	KeyDeployableProject = "deployable_project"
	KeyStaticSource      = "static_source"
	KeyBuildStrategy     = "build_strategy"
	KeyBuildMethod       = "build_method"

	// BuildArgKeyPrefix, followed by an ARG's name, asks for a build argument
	// the Dockerfile requires. The answer is passed to the build.
	BuildArgKeyPrefix = "build_arg."
)

// answerOrder is the order answers are applied in, and it is load-bearing.
//
// Ranging over the answers map applied them in Go's randomized map order, and
// two of them are not independent: withPrimaryService decides which workload is
// primary, and withPort and withStartCommand both write to "the primary
// workload". Applied the other way round, the port landed on whichever workload
// was primary beforehand. So the same answers produced two different specs at
// random — and the spec someone reviewed was not necessarily the one that got
// pinned.
//
// The same reasoning as resolving the tie-break first: an answer that changes
// what the rest of the answers apply to has to be applied before them.
var answerOrder = []string{
	// What is being deployed at all.
	KeyDeployableProject,
	KeyDockerfilePath,
	KeyStaticSource,

	// Which workload the rest of the answers mean.
	KeyPrimaryService,

	// And then the ones that write to it.
	KeyPrimaryPort,
	KeyStartCommand,

	// Applied by chosen() before any of this; listed so the set is complete.
	KeyBuildStrategy,
	KeyBuildMethod,
}

// WithAnswers folds a person's answers, and any AI adapter's suggestions they
// did not override, into the draft spec.
//
// An answer is worth more than anything Pando worked out for itself, so it
// overwrites rather than merges — and a port supplied by a person is recorded
// with Source "user", which is what the review UI shows beside it (design 01
// §2.3). Where the value came from is part of the value, so a port that came
// from a suggestion is recorded as screened instead: the review shows which it
// was, and spec.Carry does not keep it across a re-detection as though
// somebody had chosen it.
//
// Suggestions go through the same machinery as a person's answers,
// deliberately: an answer that changes which workload the other answers mean
// does so whoever supplied it, and two ways to apply an answer is how the two
// would drift.
func (p Proposal) WithAnswers(answers map[string]string) spec.AppSpec {
	source := spec.PortUser
	if strings.TrimSpace(answers[KeyPrimaryPort]) == "" {
		source = spec.PortScreened // only matters if a suggestion supplies it
	}
	return p.withAnswers(p.Answers(answers), source)
}

// Answers is every answer in effect: an AI adapter's suggestions, overridden
// by whatever a person answered (R-338).
func (p Proposal) Answers(answers map[string]string) map[string]string {
	out := make(map[string]string, len(answers))
	for _, q := range p.Questions {
		if q.Suggested != nil && strings.TrimSpace(q.Suggested.Value) != "" {
			out[q.Key] = q.Suggested.Value
		}
	}
	for key, value := range answers {
		if strings.TrimSpace(value) != "" {
			out[key] = value
		}
	}
	return out
}

func (p Proposal) withAnswers(answers map[string]string, portSource spec.PortSource) spec.AppSpec {
	// The tie-break is resolved first, because it chooses which reading of the
	// repository the rest of the answers apply to.
	out := p.chosen(answers)

	// When another reading was adopted, only answers to its own questions
	// apply to it. A port given for the Dockerfile reading is not the compose
	// file's port: stamped on the adopted compose app anyway, it sent traffic
	// to 80 while the compose file had the app on 8000 (issue #55).
	if adopted, ok := p.adoptedCandidate(answers); ok {
		asked := map[string]bool{KeyBuildStrategy: true, KeyBuildMethod: true}
		for _, q := range adopted.Questions {
			asked[q.Key] = true
		}
		kept := make(map[string]string, len(answers))
		for key, value := range answers {
			if asked[key] {
				kept[key] = value
			}
		}
		answers = kept
	}

	// Copy the workload slice: a proposal is read from storage and may be
	// applied more than once, and mutating it in place would make the second
	// application see the first one's results.
	out.Workloads = append([]spec.Workload(nil), out.Workloads...)

	for _, key := range answerOrder {
		value := strings.TrimSpace(answers[key])
		if value == "" {
			continue
		}
		switch key {
		case KeyPrimaryPort:
			out = withPort(out, value, portSource)
		case KeyPrimaryService:
			out = withPrimaryService(out, value)
		case KeyStartCommand:
			out = withStartCommand(out, value)
		case KeyDockerfilePath:
			out.Build.Dockerfile = value
			out.Build.Strategy = spec.BuildDockerfile
		case KeyDeployableProject:
			// The answer names a directory inside the repository, which is the
			// same thing a subdir is. Detection runs again against it
			// (Subproject), and the proposal that run produces already reads
			// that directory and asks nothing about it — so the answer applies
			// only to the proposal that asked, the one that read the parent.
			if p.asks(KeyDeployableProject) {
				if dir, ok := cleanSubproject(value); ok {
					out.Source.Subdir = path.Join(out.Source.Subdir, dir)
				}
			}
		case KeyStaticSource:
			out.Build.Strategy = spec.BuildStatic
			if value == "run-build" {
				out.Build.Strategy = spec.BuildBuildpack
			}
		case KeyBuildStrategy, KeyBuildMethod:
			// Applied by chosen() above, which swapped in that candidate's
			// whole draft. Setting the strategy here is what used to produce a
			// spec describing one detector's reading under another's name.
		}
	}

	// Build arguments, in name order so the same answers make the same spec.
	var buildArgs []string
	for key := range answers {
		if strings.HasPrefix(key, BuildArgKeyPrefix) && strings.TrimSpace(answers[key]) != "" {
			buildArgs = append(buildArgs, key)
		}
	}
	sort.Strings(buildArgs)
	for _, key := range buildArgs {
		out.Build.Args = withArg(out.Build.Args, strings.TrimPrefix(key, BuildArgKeyPrefix), strings.TrimSpace(answers[key]))
	}

	// Last, after every answer: an answer naming the primary wins, and the
	// election only fills a gap nobody filled.
	return withElectedPrimary(out)
}

// chosen returns the reading the answers select, defaulting to the winner's.
//
// The tie-break offers the two close candidates' own strategies (see tieBreak),
// so an answer naming one of them means "that detector read this repository
// correctly" — which is a different draft, not a different label on this one.
//
// One honest limitation: the winner's draft has been through the trial run and
// a runner-up's has not, so adopting one gives up whatever the trial
// discovered — a port observed rather than declared, mostly. Re-running the
// trial here would be better and is a larger change; producing a coherent spec
// instead of a corrupt one is the part that could not wait.
func (p Proposal) chosen(answers map[string]string) spec.AppSpec {
	want := spec.BuildStrategy(strings.TrimSpace(answers[KeyBuildStrategy]))
	if want == "" {
		want = spec.BuildStrategy(strings.TrimSpace(answers[KeyBuildMethod]))
	}
	if want == "" || want == p.Winner.Strategy {
		return p.DraftSpec
	}

	for _, c := range p.RunnersUp {
		if c.Strategy != want {
			continue
		}
		// A candidate that recognized the repository and could not import it —
		// a compose file using a construct that cannot cross the boundary
		// (R-099) — has no workloads. Adopting it would replace a working
		// proposal with an empty one.
		if len(c.Draft.Workloads) == 0 {
			return p.DraftSpec
		}

		// The completed spec, which detection filled with the install's
		// defaults, its routing and its port. Assembling the raw draft here
		// instead produces a spec with none of that, and the failure lands at
		// deploy time as a complaint about a port number.
		if c.Spec != nil {
			return *c.Spec
		}

		// No completed spec — a proposal from a build that filled in the
		// winner's only. Its routing still belongs to the app rather than to
		// the reading: the host port was allocated to this app, so it is the
		// same port whichever candidate is adopted.
		adopted := Assemble(p.DraftSpec.AppID, p.DraftSpec.Source, c.Draft)
		adopted.Routing = p.DraftSpec.Routing
		return adopted
	}

	// An answer naming something that did not bid is not a choice between
	// readings. Ignored rather than invented: stamping it on the winner is
	// exactly the bug this function exists to remove.
	return p.DraftSpec
}

// asks reports whether the proposal put the question with this key.
func (p Proposal) asks(key string) bool {
	for _, q := range p.Questions {
		if q.Key == key {
			return true
		}
	}
	return false
}

// Subproject is the directory, within the one this proposal read, that
// detection reads instead: the project a person named in answer to
// deployable_project, or the only project a repository with no app at its
// root holds. why says which, for the evidence. ok is false when the proposal
// describes what to deploy as it stands.
//
// Answering deployable_project used to set the subdirectory and nothing else:
// the proposal still described the repository root, which had no workloads,
// and accepting it pinned a spec that ran nothing (issue #90). The project's
// own reading is what there is to accept, so detection runs again in it.
func (p Proposal) Subproject(answers map[string]string) (dir, why string, ok bool) {
	if !p.asks(KeyDeployableProject) {
		return "", "", false
	}
	if dir, ok := cleanSubproject(answers[KeyDeployableProject]); ok {
		return dir, fmt.Sprintf("deploying %s/, the project chosen in this repository", dir), true
	}
	for _, q := range p.Questions {
		if q.Key == KeyDeployableProject && len(q.Options) == 1 && p.Winner.Strategy == StrategyUnknown {
			if dir, ok := cleanSubproject(q.Options[0]); ok {
				return dir, fmt.Sprintf("deploying %s/, the only project in this repository", dir), true
			}
		}
	}
	return "", "", false
}

// RedetectsOn reports whether these answers change which directory this
// proposal should have read, so detection runs again rather than the image
// being rebuilt (Subproject).
func (p Proposal) RedetectsOn(answers map[string]string) bool {
	_, ok := cleanSubproject(answers[KeyDeployableProject])
	return ok && p.asks(KeyDeployableProject)
}

// cleanSubproject is a deployable_project answer as a directory inside the
// repository: relative, below the root, and not climbing out of it.
func cleanSubproject(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	clean := path.Clean(strings.TrimPrefix(value, "./"))
	if clean == "." || path.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

// withArg sets a build argument, replacing one of the same name. A new slice,
// because a proposal is applied more than once.
func withArg(args []spec.KV, key, value string) []spec.KV {
	out := make([]spec.KV, 0, len(args)+1)
	for _, kv := range args {
		if kv.Key != key {
			out = append(out, kv)
		}
	}
	return append(out, spec.KV{Key: key, Value: value})
}

// CheckAnswers refuses an answer that cannot become a spec, when it is given.
//
// A build_method or build_strategy answer selects a reading of the repository,
// and only a reading some detector made has anything to run. Any other answer
// was recorded, applied to nothing, and refused at accept with "This app has no
// workloads" — after the person had moved on (issue #55). The AI adapter's
// prose answers ("serve the repository root with php -S …") were accepted the
// same way.
func (p Proposal) CheckAnswers(answers map[string]string) error {
	if value, given := answers[KeyDeployableProject]; given {
		if err := p.checkDeployableProject(value); err != nil {
			return err
		}
	}
	for _, key := range []string{KeyBuildStrategy, KeyBuildMethod} {
		value := strings.TrimSpace(answers[key])
		if value == "" {
			continue
		}
		if err := p.checkBuildAnswer(value); err != nil {
			return err
		}
	}
	return p.checkLoopbackAnswer(answers[KeyStartCommand])
}

// checkLoopbackAnswer refuses, as the answer to the question asked when an app
// listened only on 127.0.0.1, the command the trial already started it with.
//
// The question asks for a command that makes the app listen on 0.0.0.0, and
// that one did not. Accepted, it was deployed and refused there with the same
// explanation the question gave (issue #90); the person answering is better
// told now, while what to change is in front of them.
func (p Proposal) checkLoopbackAnswer(answer string) error {
	answer = strings.Join(strings.Fields(answer), " ")
	tried := strings.Join(strings.Fields(p.LoopbackCommand), " ")
	if answer == "" || answer != tried {
		return nil
	}
	return errs.Newf(errs.ValidInvalid,
		"Pando already started this app with %q, and it listened only on 127.0.0.1 inside its container, "+
			"which nothing outside the container can reach, including Pando. Valid answer: a command that "+
			"starts the app listening on 0.0.0.0, such as \"npm start -- --host 0.0.0.0\" or "+
			"\"uvicorn main:app --host 0.0.0.0\".", answer).
		WithRemedy("If the address is written into the app's code, change the code to listen on 0.0.0.0 " +
			"instead of 127.0.0.1 or localhost — in Node, `app.listen(process.env.PORT, '0.0.0.0')` — " +
			"then detect the app again.")
}

// checkDeployableProject refuses a deployable_project answer that is not a
// directory inside the repository, or not one of the choices offered.
func (p Proposal) checkDeployableProject(value string) error {
	if _, ok := cleanSubproject(value); !ok {
		return errs.Newf(errs.ValidInvalid,
			"%q is not a directory inside this repository, so Pando cannot read a project from it. "+
				"Valid answer: the path of the project's directory from the top of the repository, such as apps/web.",
			value)
	}
	// A choice names the directories that exist; another would be read
	// as an empty one.
	for _, q := range p.Questions {
		if q.Key == KeyDeployableProject && len(q.Options) > 0 && !containsString(q.Options, strings.TrimSpace(value)) {
			return errs.Newf(errs.ValidInvalid,
				"%q is not one of the projects Pando found in this repository. Valid answer: one of %s.",
				value, strings.Join(q.Options, ", "))
		}
	}
	return nil
}

// checkBuildAnswer refuses a build_strategy or build_method answer that names
// no reading a detector made.
func (p Proposal) checkBuildAnswer(value string) error {
	readings := p.buildReadings()
	if containsString(readings, value) {
		return nil
	}
	if len(readings) == 0 {
		return errs.New(errs.ValidInvalid,
			"Pando found nothing in this repository it knows how to build, so no answer to how it is "+
				"built can be turned into something Pando runs.").
			WithRemedy("Add a Dockerfile to the repository, or a compose file, or a Procfile naming the " +
				"command that starts the app, then run detection again.")
	}
	return errs.Newf(errs.ValidInvalid,
		"%q is not one of the ways Pando found to build this app. Valid answer: one of %s.",
		value, strings.Join(readings, ", "))
}

// buildReadings is the strategies of the candidates that have something to
// run, the winner first.
func (p Proposal) buildReadings() []string {
	var readings []string
	for _, c := range append([]Candidate{p.Winner}, p.RunnersUp...) {
		if c.Strategy != StrategyUnknown && c.Strategy != "" && len(c.Draft.Workloads) > 0 {
			readings = append(readings, string(c.Strategy))
		}
	}
	return readings
}

// adoptedCandidate is the runner-up the tie-break answer selects, when chosen()
// adopts one rather than keeping the winner.
func (p Proposal) adoptedCandidate(answers map[string]string) (Candidate, bool) {
	want := spec.BuildStrategy(strings.TrimSpace(answers[KeyBuildStrategy]))
	if want == "" {
		want = spec.BuildStrategy(strings.TrimSpace(answers[KeyBuildMethod]))
	}
	if want == "" || want == p.Winner.Strategy {
		return Candidate{}, false
	}
	for _, c := range p.RunnersUp {
		if c.Strategy == want && len(c.Draft.Workloads) > 0 {
			return c, true
		}
	}
	return Candidate{}, false
}

func withPort(s spec.AppSpec, value string, source spec.PortSource) spec.AppSpec {
	number, err := strconv.Atoi(value)
	if err != nil || number <= 0 || number > 65535 {
		return s
	}
	for i, w := range s.Workloads {
		if !w.Primary {
			continue
		}
		s.Workloads[i].Ports = []spec.Port{{
			Number: number, Protocol: "http", Source: source,
		}}
		break
	}
	return s
}

// withPrimaryService moves Primary to the named workload.
//
// Exactly one workload is primary — the app has one canonical endpoint — so
// this clears the flag everywhere before setting it, rather than assuming the
// caller's draft had it in one place.
func withPrimaryService(s spec.AppSpec, name string) spec.AppSpec {
	var found bool
	for i := range s.Workloads {
		if s.Workloads[i].Name == name {
			found = true
		}
	}
	if !found {
		return s
	}
	for i := range s.Workloads {
		isPrimary := s.Workloads[i].Name == name
		s.Workloads[i].Primary = isPrimary
		s.Workloads[i].Exposed = isPrimary
	}
	return s
}

func withStartCommand(s spec.AppSpec, command string) spec.AppSpec {
	for i, w := range s.Workloads {
		if !w.Primary {
			continue
		}
		// Kept as a shell command rather than split into argv. Splitting on
		// whitespace breaks the first quoted argument anyone writes, and the
		// person answering this question wrote a command line, not an argv.
		s.Workloads[i].Command = []string{"sh", "-c", command}

		// A buildpack image already starts through a shell: its entrypoint is
		// a login shell that takes the command line as one argument, which is
		// also what sets up the PATH its toolchain was installed on. `sh -c`
		// handed to that became `bash -l -c sh`, a shell with no command that
		// read an empty stdin and exited 0 (issue #55). So the line goes to it
		// whole, the way the plan's own start command does.
		if s.Build.Strategy == spec.BuildBuildpack {
			s.Workloads[i].Command = []string{command}
		}
		break
	}
	return s
}

// withElectedPrimary makes sure exactly one workload is primary.
//
// A compose file with several services does not say which one serves the app
// (R-026), so detection asks — and the question rides on the compose candidate,
// which is not necessarily the one a person adopts when they answer the
// tie-break. Accepting without that answer produced a spec with no primary at
// all: refused at deploy with "mark exactly one workload as primary", on a
// screen with nothing to mark it with. A dead end, and this is the end of it.
//
// [P] overriding design 01's "asks rather than picks": it still asks, and now
// it also picks, and says which it picked. R-104's rule is that a thing with a
// sane default is configuration rather than a blocker; R-102's is that the
// person sees the reasoning. A warning carries both.
func withElectedPrimary(s spec.AppSpec) spec.AppSpec {
	if len(s.Workloads) == 0 {
		return s
	}
	for _, w := range s.Workloads {
		if w.Primary {
			return s
		}
	}

	// Depended on by nothing, and serving HTTP: the shape of the thing a person
	// opens. `app depends_on db` makes the app the candidate and the database
	// not one, which is the usual compose file.
	dependedOn := map[string]bool{}
	for _, w := range s.Workloads {
		for _, dep := range w.DependsOn {
			dependedOn[dep] = true
		}
	}

	pick := -1
	for i, w := range s.Workloads {
		if dependedOn[w.Name] {
			continue
		}
		if !servesHTTP(w) {
			continue
		}
		pick = i
		break
	}
	if pick < 0 {
		// Nothing obvious. The first workload is still a better answer than
		// none: it produces a deployable app somebody can correct, where no
		// primary produces a refusal they cannot.
		pick = 0
	}

	s.Workloads = append([]spec.Workload(nil), s.Workloads...)
	s.Workloads[pick].Primary = true
	s.Workloads[pick].Exposed = true

	s.Warnings = append(s.Warnings, spec.Warning{
		Code: spec.WarnPrimaryWorkloadAssumed,
		Message: fmt.Sprintf(
			"This app has %d parts and did not say which one people open in a browser. "+
				"Pando is sending traffic to %q. If that is wrong, answer the question on this app's "+
				"configuration and accept it again.",
			len(s.Workloads), s.Workloads[pick].Name),
	})
	return s
}

// servesHTTP reports whether a workload declares a port that looks like a web
// endpoint.
func servesHTTP(w spec.Workload) bool {
	for _, p := range w.Ports {
		if p.Protocol == "" || p.Protocol == "http" || p.Protocol == "https" {
			return true
		}
	}
	return false
}
