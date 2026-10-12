package buildkit

import (
	"bufio"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// R-094 tier 3 named this first: `.github/workflows` contains the maintainer's
// own build commands.
//
// It ranks at the top of the ladder for a reason that has nothing to do with
// parsing being easy. A Makefile target is what somebody wrote down; a workflow
// step is what runs on every push, and has been observed to work by every green
// check on the repository. When the two disagree, the workflow is the one that
// is true.
//
// The reading is deliberately timid, because the ways to be wrong here are
// worse than not answering. A workflow file is a program: matrices, conditional
// steps, reusable workflows, composite actions, and commands assembled from
// expressions. None of that is interpreted. What is taken is the `run:` steps
// of a single unambiguous build job, and anything that makes "single" or
// "unambiguous" false is declined — at which point the repository is planned by
// whatever the next source down says, exactly as it was before.

// jobPattern matches a job key: two spaces of indent under `jobs:`.
var jobPattern = regexp.MustCompile(`^  ([A-Za-z0-9][A-Za-z0-9_.\-]*)\s*:\s*$`)

// runStepPattern matches the start of a `run:` step, inline or block.
var runStepPattern = regexp.MustCompile(`^\s+-?\s*run:\s*(.*)$`)

// usesPattern matches a step that calls an action rather than a command.
var usesPattern = regexp.MustCompile(`^\s+-?\s*uses:\s*(\S+)`)

// buildJobNames are job names taken to mean "this builds the app", in
// preference order. A job called `test` or `lint` runs commands too, and
// running a test suite as an image's build step is a slow way to be wrong.
var buildJobNames = []string{"build", "release", "package", "publish", "docker"}

// skipCommands are lines that appear in a build job and are not part of
// building: reporting, uploading, and the checks that belong to CI rather than
// to the image.
var skipCommands = []string{
	"echo ", "cat ", "ls ", "codecov", "coveralls",
	"git config", "git diff", "git tag", "git push",
}

func readWorkflowBuild(contextDir string) declaredBuild {
	dir := filepath.Join(contextDir, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return declaredBuild{}
	}

	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if n := e.Name(); strings.HasSuffix(n, ".yml") || strings.HasSuffix(n, ".yaml") {
			names = append(names, n)
		}
	}
	sort.Strings(names)

	for _, name := range names {
		f, err := os.Open(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		steps := buildJobSteps(f)
		_ = f.Close()
		if len(steps) == 0 {
			continue
		}

		steps, ok := buildingOnly(steps)
		if !ok {
			continue
		}
		command := strings.Join(dropProvidedInstalls(steps, contextDir), " && ")
		if command == "" {
			continue
		}
		if !safeCommand(command) {
			continue
		}
		source := filepath.ToSlash(filepath.Join(".github", "workflows", name))
		return declaredBuild{
			Rank:   rankWorkflow,
			Source: source,
			Build:  command,
			Why: source + " builds this app on every push, and the plan runs the same commands" +
				" rather than guessing",
			Packages: toolchainPackages(contextDir),
		}
	}
	return declaredBuild{}
}

// buildJobSteps returns the run commands of the one build job in a workflow.
//
// Empty unless the file has a job whose name is in buildJobNames, that job runs
// no matrix, and its steps are commands rather than actions that would have to
// be reproduced. `uses:` steps are skipped rather than disqualifying, because
// actions/checkout and actions/setup-go are in every workflow and describe an
// environment Railpack is already providing.
func buildJobSteps(r io.Reader) []string {
	type job struct {
		steps    []string
		matrix   bool
		usesCall bool
		services bool
	}
	jobs := map[string]*job{}

	var current *job
	inJobs := false
	var pendingBlock bool
	var blockIndent int
	var block []string

	flushBlock := func() {
		if len(block) > 0 && current != nil {
			current.steps = append(current.steps, strings.Join(block, " && "))
		}
		block, pendingBlock = nil, false
	}

	scanner := bufio.NewScanner(io.LimitReader(r, 512<<10))
	for scanner.Scan() {
		raw := strings.TrimRight(scanner.Text(), " \t")
		trimmed := strings.TrimSpace(raw)

		if pendingBlock {
			indent := len(raw) - len(strings.TrimLeft(raw, " "))
			if trimmed != "" && indent >= blockIndent {
				if step := strings.TrimSpace(raw); step != "" && !skippable(step) {
					block = append(block, step)
				}
				continue
			}
			flushBlock()
		}

		if raw != "" && !strings.HasPrefix(raw, " ") {
			inJobs = strings.HasPrefix(raw, "jobs:")
			current = nil
			continue
		}
		if !inJobs || trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		if m := jobPattern.FindStringSubmatch(raw); m != nil {
			jobs[m[1]] = &job{}
			current = jobs[m[1]]
			continue
		}
		if current == nil {
			continue
		}

		// A job that starts a database beside itself is checking the app
		// against one: its steps migrate and test, and assume a server an
		// image build does not have.
		if strings.HasPrefix(trimmed, "services:") {
			current.services = true
			continue
		}
		if strings.HasPrefix(trimmed, "matrix:") || strings.HasPrefix(trimmed, "strategy:") {
			current.matrix = true
			continue
		}
		if m := usesPattern.FindStringSubmatch(raw); m != nil {
			// A reusable workflow — `uses:` at job level rather than step level
			// — is a whole other file's worth of steps.
			if strings.HasPrefix(strings.TrimSpace(raw), "uses:") && !strings.Contains(m[1], "@") {
				current.usesCall = true
			}
			continue
		}
		if m := runStepPattern.FindStringSubmatch(raw); m != nil {
			inline := strings.TrimSpace(m[1])
			switch {
			case inline == "|" || inline == ">" || inline == "|-" || inline == ">-":
				pendingBlock = true
				blockIndent = len(raw) - len(strings.TrimLeft(raw, " ")) + 2
				block = nil
			case inline != "" && !skippable(inline):
				current.steps = append(current.steps, inline)
			}
		}
	}
	flushBlock()

	for _, name := range buildJobNames {
		j, ok := jobs[name]
		if !ok || j == nil || len(j.steps) == 0 || j.matrix || j.usesCall || j.services {
			continue
		}
		return j.steps
	}
	return nil
}

// skippable reports whether a step is CI bookkeeping rather than building.
// installCommands are the dependency installs a package manager's own provider
// already runs.
var installCommands = []string{
	"npm ci", "npm install", "npm i ", "npm i",
	"yarn install", "yarn --", "yarn",
	"pnpm install", "pnpm i ", "pnpm i",
	"bun install",
}

// dropProvidedInstalls removes the install a workflow runs before its build,
// when Railpack is going to run one anyway.
//
// A workflow says `npm ci` then `npm run build` because CI starts with an
// empty directory. A Railpack plan has an install step that already did it,
// and its build step mounts node_modules/.cache as a build cache — so the
// second `npm ci`, which clears node_modules first, fails on a directory it
// cannot remove: "EBUSY: resource busy or locked, rmdir
// '/app/node_modules/.cache'". Mirroring a workflow is meant to reproduce
// what the repository already does, and doing the install twice is not part
// of that.
//
// Only when the package manager's provider is the one Railpack picked, which
// is what a package.json at the root means. A client below the root is built
// by a provider that knows nothing about it and has to install for itself.
func dropProvidedInstalls(steps []string, contextDir string) []string {
	if _, err := os.Stat(filepath.Join(contextDir, "package.json")); err != nil {
		return steps
	}

	var out []string
	for _, step := range steps {
		var kept []string
		for _, part := range strings.Split(step, "&&") {
			part = strings.TrimSpace(part)
			if part == "" || isInstall(part) {
				continue
			}
			kept = append(kept, part)
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, " && "))
		}
	}
	return out
}

// isInstall reports whether a command is only a dependency install.
func isInstall(command string) bool {
	lower := strings.ToLower(strings.TrimSpace(command))
	for _, install := range installCommands {
		if lower == strings.TrimSpace(install) || strings.HasPrefix(lower, install+" ") {
			return true
		}
	}
	return false
}

// testCommand matches a step that checks the app rather than building it: a
// test suite, a linter, a vet. R-011 — Pando does not test — and a test run as
// an image's build step fails the build on whatever the suite needs and the
// image does not have: a database, a browser, the CI runner's tools.
var testCommand = regexp.MustCompile(`^(` +
	`(go|cargo|mix|deno|dotnet|mvn|gradle|\./gradlew|\./mvnw|rake|bundle exec rake)\s+(test|vet|clippy|fmt|lint)\b` +
	`|(npm|yarn|pnpm|bun)(\s+run)?\s+(test|lint|check|typecheck|e2e)\b` +
	`|(python3?\s+(-m\s+)?)?(\./)?(\S*/)?manage\.py\s+(test|check)\b` +
	`|(python3?\s+-m\s+|npx\s+|bundle\s+exec\s+|uv\s+run\s+|poetry\s+run\s+|vendor/bin/)?` +
	`(pytest|tox|nox|jest|vitest|rspec|phpunit|golangci-lint|eslint|ruff|flake8|mypy|playwright|cypress)\b` +
	`)`)

// notBuilding are commands that cannot be part of an image's build: they need
// the CI runner's privileges or its daemon, ship the app somewhere, or talk to
// a server the job started. A job that runs one is CI's recipe — "build with
// doctl, run the container, curl it" — and not a description of the build.
var notBuilding = map[string]bool{
	"sudo": true, "docker": true, "docker-compose": true, "podman": true, "buildah": true,
	"doctl": true, "flyctl": true, "fly": true, "heroku": true, "kubectl": true, "helm": true,
	"vercel": true, "netlify": true, "wrangler": true, "gcloud": true, "aws": true, "az": true,
	"curl": true, "wget": true, "sleep": true, "ssh": true, "scp": true, "rsync": true,
}

// buildingOnly drops the steps that test, and declines the job (false) when
// what is left still does something an image build cannot, or nothing at all.
func buildingOnly(steps []string) ([]string, bool) {
	var out []string
	for _, step := range steps {
		var kept []string
		for _, part := range strings.Split(step, "&&") {
			part = strings.TrimSpace(part)
			if part == "" || testCommand.MatchString(strings.ToLower(part)) {
				continue
			}
			if notBuilding[strings.ToLower(strings.Fields(part)[0])] {
				return nil, false
			}
			kept = append(kept, part)
		}
		if len(kept) > 0 {
			out = append(out, strings.Join(kept, " && "))
		}
	}
	return out, len(out) > 0
}

func skippable(step string) bool {
	lower := strings.ToLower(strings.TrimLeft(step, "@-+ "))
	for _, prefix := range skipCommands {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}
