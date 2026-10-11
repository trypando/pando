package buildkit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every reader here is a shallow parser pointed at somebody else's repository,
// so what it does with a file it does not understand is most of its behaviour.
// The rule is the same everywhere: skip the line, and if that leaves nothing,
// declare nothing.

func TestAProcfileIsReadPastTheLinesThatAreNotProcesses(t *testing.T) {
	processes := parseProcfile(strings.NewReader(
		"# the web process\n" +
			"\n" +
			"this line has no colon\n" +
			": nothing before the colon\n" +
			"empty:\n" +
			"two words: ./app\n" +
			"WEB: ./app --serve\n"))

	require.Equal(t, map[string]string{"web": "./app --serve"}, processes,
		"a comment, a blank, a line with no colon, an empty key, an empty command "+
			"and a key with a space in it are all skipped; the case is folded")
}

func TestATaskfileIsReadPastTheLinesThatAreNotTasks(t *testing.T) {
	d := parseTaskfile(strings.NewReader(
		"# a comment\n" +
			"version: '3'\n" +
			"vars:\n" +
			"  BIN: app\n" + // indented, but not under tasks:
			"tasks:\n" +
			"  build:\n" +
			"    desc: compile it\n" + // a key that is not cmds:
			"    cmds:\n" +
			"      - go build -o app .\n" +
			"  start:\n" +
			"    cmds:\n" +
			"      - cmd: ./app\n" + // the long form
			"env:\n" +
			"  FOO: bar\n"))

	require.Equal(t, "build", d.BuildTarget)
	require.Equal(t, "./app", d.StartCommand, "the cmd: long form is read too")
}

func TestAJustfileIsReadPastTheLinesThatAreNotRecipes(t *testing.T) {
	d := parseJustfile(strings.NewReader(
		"# a comment\n" +
			"set shell := [\"bash\", \"-c\"]\n" +
			"export BIN := \"app\"\n" +
			"build:\n" +
			"    go build -o app .\n" +
			"\n" +
			"alias b := build\n" +
			"start:\n" +
			"    ./app\n"))

	require.Equal(t, "build", d.BuildTarget)
	require.Equal(t, "./app", d.StartCommand)
}

func TestAWorkflowIsReadPastTheLinesThatAreNotSteps(t *testing.T) {
	steps := buildJobSteps(strings.NewReader(
		"name: CI\n" +
			"# a comment\n" +
			"on:\n" +
			"  push:\n" +
			"    branches: [main]\n" + // indented, but before jobs:
			"env:\n" +
			"  CGO_ENABLED: 0\n" +
			"jobs:\n" +
			"  build:\n" +
			"    runs-on: ubuntu-latest\n" + // a job key that is not a step
			"    permissions:\n" +
			"      contents: read\n" +
			"    steps:\n" +
			"      - name: Build it\n" + // a step key that is not run:
			"      - run: go build -o app .\n"))

	require.Equal(t, []string{"go build -o app ."}, steps)
}

// Anything before `jobs:` is not a job, however much it looks like one.
func TestAWorkflowsTopLevelKeysAreNotJobs(t *testing.T) {
	require.Empty(t, buildJobSteps(strings.NewReader(
		"on:\n  build:\n    steps:\n      - run: not-a-job\n")),
		"a trigger called build is not a job called build")
}

func TestAMakefileIsReadPastTheLinesThatAreNotTargets(t *testing.T) {
	d := parseMakefile(strings.NewReader(
		"# build: a comment that looks like a target\n" +
			"GO ?= go\n" +
			"BIN := bin/app\n" +
			".PHONY: build start\n" +
			"\n" +
			"build:\n" +
			"\t# a comment inside a recipe\n" +
			"\t$(GO) build -o $(BIN) .\n" +
			"\n" +
			"start:\n" +
			"\t./app\n"))

	require.Equal(t, "build", d.BuildTarget)
	require.Equal(t, "./app", d.StartCommand)
}

// The package list is deduplicated and ordered, so two sources asking for
// nodejs produce one flag and the same command twice running produces the same
// plan.
func TestPackagesAreDeduplicatedAndOrdered(t *testing.T) {
	d := declaredBuild{
		Rank:     rankRunner,
		Build:    "make build",
		Packages: []string{"node", "just", "", "node"},
	}
	require.Equal(t,
		[]string{"RAILPACK_BUILD_CMD=make build", "RAILPACK_PACKAGES=just node"},
		d.railpackEnv(""))
}

// A variable is not a target, however much `build := ./out` looks like one.
//
// Go's regexp has no lookahead, so targetPattern's trailing `[^=]?` cannot say
// "a colon not followed by =" — being optional, it matches nothing and accepts
// the `=` it was meant to exclude. Both readers therefore saw a variable named
// after a target as declaring that target, and the plan ran `make build`
// against a target that does not exist.
func TestAVariableNamedAfterATargetIsNotOne(t *testing.T) {
	for name, tc := range map[string]struct {
		parse func(string) declaration
		body  string
	}{
		"make, :=": {parseMake, "build := ./out\n\nall:\n\tgo build .\n"},
		"make, =":  {parseMake, "build = ./out\n\nall:\n\tgo build .\n"},
		"make, ?=": {parseMake, "build ?= ./out\n\nall:\n\tgo build .\n"},
		"just, :=": {parseJust, "build := \"./out\"\n\ncompile:\n    go build .\n"},
	} {
		t.Run(name, func(t *testing.T) {
			d := tc.parse(tc.body)
			require.NotEqual(t, "build", d.BuildTarget,
				"an assignment is not a declaration that the target exists")
			require.NotEmpty(t, d.BuildTarget, "and the real target is still found")
		})
	}
}

func parseMake(body string) declaration { return parseMakefile(strings.NewReader(body)) }
func parseJust(body string) declaration { return parseJustfile(strings.NewReader(body)) }
