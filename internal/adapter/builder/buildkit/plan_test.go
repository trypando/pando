package buildkit

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// recordingPlanner stands in for Railpack: it writes the plan it is told to
// write and remembers what it was asked.
type recordingPlanner struct {
	body  string
	calls [][]string
	err   error
}

func (p *recordingPlanner) plan(contextDir string, env []string) (string, error) {
	p.calls = append(p.calls, env)
	if p.err != nil {
		return "", p.err
	}
	name := filepath.Join(planDir, railpackPlanFile)
	full := filepath.Join(contextDir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(full, []byte(p.body), 0o600); err != nil {
		return "", err
	}
	return name, nil
}

const planWithGoBuild = `{
  "steps": [
    {"name": "packages:mise", "commands": [{"cmd": "mise install"}]},
    {"name": "install", "commands": [{"cmd": "go mod download"}]},
    {"name": "build", "commands": [{"src": ".", "dest": "."}, {"cmd": "go build -o out ./cmd/server"}]}
  ],
  "deploy": {"startCommand": "./out"}
}`

// An ordering needs both passes: the first to learn what the build is, the
// second to put the client build in front of it.
func TestAnOrderingIsPlannedTwice(t *testing.T) {
	p := &recordingPlanner{body: planWithGoBuild}
	declared := declaredBuild{
		Rank:     rankEmbedded,
		Before:   "(cd web && npm ci && npm run build)",
		Packages: []string{"node"},
	}

	_, err := planTwice(t.TempDir(), declared, p.plan)
	require.NoError(t, err)
	require.Len(t, p.calls, 2)

	require.Equal(t, []string{"RAILPACK_PACKAGES=node"}, p.calls[0],
		"the first pass has nothing to come before yet")
	require.Contains(t, p.calls[1],
		"RAILPACK_BUILD_CMD=(cd web && npm ci && npm run build) && go build -o out ./cmd/server",
		"the client build, then whatever Railpack chose")
}

// A declaration that names the build replaces Railpack's choice, and one call
// settles it.
func TestANamedBuildIsPlannedOnce(t *testing.T) {
	p := &recordingPlanner{body: planWithGoBuild}
	declared := declaredBuild{Rank: rankRunner, Build: "make build"}

	_, err := planTwice(t.TempDir(), declared, p.plan)
	require.NoError(t, err)
	require.Len(t, p.calls, 1)
	require.Contains(t, p.calls[0], "RAILPACK_BUILD_CMD=make build")
}

// Railpack's settings for what it would get wrong reach both passes.
func TestPandosRailpackSettingsReachEveryPass(t *testing.T) {
	root := withFiles(t, map[string]string{"composer.json": "{}", "public/index.php": "<?php"})
	p := &recordingPlanner{body: planWithGoBuild}

	_, err := planTwice(root, declaredBuild{Rank: rankEmbedded, Before: "(cd web && npm ci)"}, p.plan)
	require.NoError(t, err)
	require.Len(t, p.calls, 2)
	for _, call := range p.calls {
		require.Contains(t, call, "RAILPACK_PHP_ROOT_DIR=/app/public")
	}
}

// So does no declaration at all, which is the ordinary case.
func TestConventionMatchingIsPlannedOnce(t *testing.T) {
	p := &recordingPlanner{body: planWithGoBuild}

	_, err := planTwice(t.TempDir(), declaredBuild{}, p.plan)
	require.NoError(t, err)
	require.Len(t, p.calls, 1)
	require.Empty(t, p.calls[0])
}

// A plan with no build step is one there is nothing to come before, so the
// first answer stands rather than being replaced by a truncated second.
func TestAPlanWithNoBuildStepIsNotWrapped(t *testing.T) {
	p := &recordingPlanner{body: `{"steps": [{"name": "install"}], "deploy": {"startCommand": "./out"}}`}
	declared := declaredBuild{Rank: rankEmbedded, Before: "(cd web && npm ci)"}

	name, err := planTwice(t.TempDir(), declared, p.plan)
	require.NoError(t, err)
	require.Len(t, p.calls, 1)
	require.Equal(t, filepath.Join(planDir, railpackPlanFile), name)
}

// A planner that fails fails the build, on either pass.
func TestAFailedPlanIsNotSwallowed(t *testing.T) {
	boom := errors.New("railpack said no")

	p := &recordingPlanner{err: boom}
	_, err := planTwice(t.TempDir(), declaredBuild{}, p.plan)
	require.ErrorIs(t, err, boom)

	// And on the second pass, where it would be easy to return the first
	// pass's answer and call it success.
	second := &failingSecondPlanner{body: planWithGoBuild, err: boom}
	_, err = planTwice(t.TempDir(),
		declaredBuild{Rank: rankEmbedded, Before: "(cd web && npm ci)"}, second.plan)
	require.ErrorIs(t, err, boom)
}

type failingSecondPlanner struct {
	body  string
	err   error
	calls int
}

func (p *failingSecondPlanner) plan(contextDir string, env []string) (string, error) {
	p.calls++
	if p.calls > 1 {
		return "", p.err
	}
	return (&recordingPlanner{body: p.body}).plan(contextDir, env)
}

// A plan the planner claims to have written and did not is an error, not a
// silent second pass against nothing.
func TestAMissingPlanFileIsAnError(t *testing.T) {
	_, err := planTwice(t.TempDir(),
		declaredBuild{Rank: rankEmbedded, Before: "(cd web && npm ci)"},
		func(string, []string) (string, error) { return "nowhere/" + railpackPlanFile, nil })
	require.Error(t, err)
	require.Contains(t, err.Error(), "Could not read the build plan")
}

// --- what detection is told -------------------------------------------------

func TestTheDeclarationDetectionSeesCarriesTheReading(t *testing.T) {
	d := declaredBuild{
		Rank:   rankRunner,
		Source: "Makefile",
		Why:    "Makefile declares how this app is built",
	}
	got := d.asPlanDeclaration()
	require.NotNil(t, got)
	require.Equal(t, &api.PlanDeclaration{
		Source:     "Makefile",
		Why:        "Makefile declares how this app is built",
		Confidence: 0.72,
	}, got)
}

// Convention-matching carries no claim worth showing, and nil is how detection
// is told to bid its own number.
func TestConventionMatchingDeclaresNothing(t *testing.T) {
	require.Nil(t, declaredBuild{}.asPlanDeclaration())
	require.Zero(t, rankNone.confidence())
}

// The rung names are what a failing assertion prints, which is the whole
// reason they exist.
func TestRungsAreNamed(t *testing.T) {
	require.Equal(t, "workflow", rankWorkflow.String())
	require.Equal(t, "runner", rankRunner.String())
	require.Equal(t, "procfile", rankProcfile.String())
	require.Equal(t, "embedded", rankEmbedded.String())
	require.Equal(t, "none", rankNone.String())
}

func TestACommandIsRefusedWhenItCannotBeWrittenPlainly(t *testing.T) {
	require.True(t, safeCommand("go build -o out ./cmd/server"))
	require.True(t, safeCommand("(cd web && npm ci) && go build ."))

	require.False(t, safeCommand(""), "nothing is not a command")
	require.False(t, safeCommand("   "))
	require.False(t, safeCommand("go build -o "+string(make([]byte, 512))),
		"and a command this long is not one somebody wrote on purpose")
}
