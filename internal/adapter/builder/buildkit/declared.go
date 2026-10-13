package buildkit

import (
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
)

// What a repository says about how it is built, and how much that is worth.
//
// R-094's ladder is a ladder of evidence, and everything here sits on it. The
// bottom rung is convention-matching: Railpack reads a repository and infers,
// which is a guess that is usually right. Every source above it is the app's
// author saying something, and the difference is not confidence in the abstract
// — it is whether being wrong is a bug in Pando or a mistake in the repository.
//
// Ranked, highest first, with the requirement each one answers to:
//
//	workflow   .github/workflows — the commands CI actually runs      R-094 tier 3
//	runner     Makefile, Taskfile.yml, justfile — named commands      R-094 tier 3
//	procfile   Procfile — the run command                             R-094 tier 2
//	embedded   two declarations naming one directory                  see below
//
// `embedded` is the odd one and the reason this file exists. Nothing in a repo
// like macscout-without-a-Makefile says "build the client first" — but
// `web/vite.config.ts` says its output goes to `../cmd/server/dist`, and
// `cmd/server/main.go` says `//go:embed all:dist`. Two files, written by the
// same person for different reasons, naming one directory from opposite ends.
// Reading that pair is not inference; it is joining two declarations. It ranks
// below the sources that name commands because it says an *ordering* and not a
// command — it knows the client must be built first, and has to be told by
// Railpack what "the build" it comes before actually is.

// rank orders declaration sources. Higher wins.
type rank int

const (
	rankNone rank = iota
	rankEmbedded
	rankProcfile
	rankRunner
	rankWorkflow
)

func (r rank) String() string {
	switch r {
	case rankWorkflow:
		return "workflow"
	case rankRunner:
		return "runner"
	case rankProcfile:
		return "procfile"
	case rankEmbedded:
		return "embedded"
	default:
		return "none"
	}
}

// declaredBuild is one reading of what a repository says about its build.
type declaredBuild struct {
	Rank rank

	// Source is the file this was read from, for evidence.
	Source string

	// Build and Start replace what Railpack would have chosen, when they are
	// set. Empty means "Railpack decides this one".
	Build string
	Start string

	// ImpliedStart is a start no source states, inferred from what the build
	// writes (a Makefile's `go build -o x` starts ./x). It is used only when no
	// source declares a start: a Procfile beside the Makefile says how the
	// program runs, and outranks the guess (R-094).
	ImpliedStart string

	// Before runs ahead of whatever Railpack chose as the build command, rather
	// than instead of it. This is what needs the second planning pass: the
	// command it precedes is not known until Railpack has been asked.
	Before string

	// Packages are toolchains the build needs that the chosen provider will not
	// bring — make, or Node for a Go module that builds a client.
	Packages []string

	// Why explains the reading in one line, shown as evidence.
	Why string
}

func (d declaredBuild) found() bool { return d.Rank != rankNone }

// needsSecondPass reports whether planning has to run twice: once to learn what
// Railpack would build, and again to put something in front of it.
func (d declaredBuild) needsSecondPass() bool { return d.Before != "" && d.Build == "" }

// readDeclaredBuild asks every source and returns the best-ranked answer.
//
// Every source is consulted rather than stopping at the first, because "which
// of these does this repository have" is cheap and the ranking is the point. A
// repository with both a CI workflow and a Makefile has said the same thing
// twice, and the workflow is what actually runs on every push.
func readDeclaredBuild(contextDir string) declaredBuild {
	sources := []func(string) declaredBuild{
		readWorkflowBuild,
		readRunnerBuild,
		readDenoTasks,
		readProcfileBuild,
		readPlatformBuild,
		readEmbeddedBuild,
	}

	var best declaredBuild
	var readings []declaredBuild
	for _, read := range sources {
		d := read(contextDir)
		readings = append(readings, d)
		if d.Rank > best.Rank {
			best = d
		}
	}
	// A source that says how the app is built and not how it starts — a CI
	// workflow, a Makefile with no run target — leaves the start to the next
	// source that says it: a Makefile builds, and the Procfile beside it runs
	// what it built. A `go run` start gives way the same way, to a start or a
	// program a build writes.
	if best.found() && (best.Start == "" || runsFromSource(best.Start)) {
		best.Start = startFrom(best, readings)
	}
	return best
}

// startFrom is the start for best, which says none or only `go run`: the
// best start another reading says, else a program a build writes.
func startFrom(best declaredBuild, readings []declaredBuild) string {
	start := best
	for _, d := range readings {
		if d.Start != "" && startBeats(d, start) {
			start = d
		}
	}
	switch {
	case start.Start == "":
		return best.ImpliedStart
	case runsFromSource(start.Start):
		for _, d := range append([]declaredBuild{best}, readings...) {
			if d.ImpliedStart != "" {
				return d.ImpliedStart
			}
		}
	}
	return start.Start
}

// startBeats reports whether d's start is a better one to run than best's. A
// start that compiles the program as it runs, `go run main.go`, is a developer's
// command: the image the app runs in has no Go, and miniflux's Makefile `run`
// target exited 127 where its Procfile names the program the build makes
// (issue #90). Any other start beats it; between equals, the higher rank wins.
func startBeats(d, best declaredBuild) bool {
	if best.Start == "" {
		return true
	}
	if dev, bestDev := runsFromSource(d.Start), runsFromSource(best.Start); dev != bestDev {
		return bestDev
	}
	return d.Rank > best.Rank
}

// runsFromSource reports a start command that runs `go run`, after any
// VAR=value settings in front of it.
func runsFromSource(start string) bool {
	fields := strings.Fields(start)
	for len(fields) > 0 && strings.Contains(fields[0], "=") {
		fields = fields[1:]
	}
	return len(fields) > 1 && fields[0] == "go" && fields[1] == "run"
}

// railpackEnv turns a reading into Railpack settings, as KEY=value.
//
// `chosen` is the build command Railpack picked on a first pass, and is only
// needed when the reading wraps rather than replaces. It is empty on the first
// pass, which is exactly when Before is not yet expressible.
func (d declaredBuild) railpackEnv(chosen string) []string {
	var env []string

	build := d.Build
	if build == "" && d.Before != "" && chosen != "" {
		// The client build, then whatever Railpack was going to do. `&&` rather
		// than `;` so a failed client build fails the image instead of shipping
		// one with a stale bundle in it — which is the failure this whole file
		// exists to prevent.
		build = d.Before + " && " + chosen
	}

	if build != "" {
		env = append(env, "RAILPACK_BUILD_CMD="+build)
	}
	if d.Start != "" {
		env = append(env, "RAILPACK_START_CMD="+d.Start)
	}
	if pkgs := dedupe(d.Packages); len(pkgs) > 0 {
		env = append(env, "RAILPACK_PACKAGES="+strings.Join(pkgs, " "))
	}
	return env
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// safeCommand reports whether a command read out of a repository can be handed
// to Railpack as-is.
//
// Shared by every source, because they all have the same problem: the string
// ends up in a Dockerfile, and one that has to be escaped to survive the round
// trip is one where a mistake is silent. Shell operators are allowed in a build
// command — `cd web && npm ci` is the normal shape — and quotes, backticks,
// variables and backslashes are not.
func safeCommand(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 512 {
		return false
	}
	return !strings.ContainsAny(s, "\"'`$\\\n\r")
}

// asPlanDeclaration is what detection is told about this reading.
//
// Nil for convention-matching, which is the bottom of R-094's ladder and
// carries no claim worth showing.
func (d declaredBuild) asPlanDeclaration() *api.PlanDeclaration {
	if !d.found() {
		return nil
	}
	return &api.PlanDeclaration{
		Source:     d.Source,
		Why:        d.Why,
		Confidence: d.Rank.confidence(),
	}
}

// confidence maps a rung of the ladder to the bid a detector should make.
//
// These sit between the buildpack detector's own 0.45, which is
// convention-matching, and the 0.92 a Dockerfile at the repository root earns.
// A Dockerfile still outranks every one of them: it is the whole answer, where
// these are the app's author describing one part of it.
func (r rank) confidence() float64 {
	switch r {
	case rankWorkflow:
		// What actually runs on every push, observed to work by every green
		// check on the repository.
		return 0.80
	case rankRunner:
		// Written down, and usually true.
		return 0.72
	case rankProcfile:
		// Says how to run it, and nothing about building it.
		return 0.62
	case rankEmbedded:
		// Says an ordering, and no commands at all.
		return 0.58
	default:
		return 0
	}
}
