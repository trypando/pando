package buildkit

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gateway "github.com/moby/buildkit/frontend/gateway/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// fakeRailpack puts a `railpack` on PATH that runs script, a shell body that
// sees the plan's path as $PLAN and every argument as "$@".
func fakeRailpack(t *testing.T, script string) {
	t.Helper()
	bin := t.TempDir()
	body := "#!/bin/sh\n" +
		`PLAN=""; prev=""; for a in "$@"; do [ "$prev" = "--plan-out" ] && PLAN="$a"; prev="$a"; done` + "\n" +
		script + "\n"
	require.NoError(t, os.WriteFile(filepath.Join(bin, railpackBinary), []byte(body), 0o755)) //nolint:gosec // a test's own script
	t.Setenv("PATH", bin)
}

const railpackPlanFromPrepare = `{
  "steps": [
    {"name": "packages:mise", "inputs": [{"image": "ghcr.io/railwayapp/railpack-builder:mise-2026.9.12"}], "commands": [{"cmd": "mise install"}]},
    {"name": "build", "secrets": ["*"], "commands": [{"cmd": "python -m compileall ."}]}
  ],
  "deploy": {"base": {"image": "ghcr.io/railwayapp/railpack-runtime:mise-2026.9.12"}, "startCommand": "python main.py"},
  "secrets": ["RAILPACK_START_CMD"]
}`

// The plan Railpack writes is the plan Pando stores, after three changes, and
// the settings Pando passes reach Railpack as its own --env.
func TestR095_ARailpackPlanIsStoredPinnedAndWithoutSecrets(t *testing.T) {
	fakeRailpack(t, `echo "$@" > "$PLAN.args"; cat > "$PLAN" <<'EOF'
`+railpackPlanFromPrepare+`
EOF`)
	root := t.TempDir()

	name, err := runRailpack(root, []string{"RAILPACK_START_CMD=python main.py"})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(planDir, railpackPlanFile), name)

	args, err := os.ReadFile(filepath.Join(root, name+".args"))
	require.NoError(t, err)
	require.Contains(t, string(args), "prepare "+root)
	require.Contains(t, string(args), "--env RAILPACK_START_CMD=python main.py")

	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	var plan map[string]any
	require.NoError(t, json.Unmarshal(body, &plan))
	require.NotContains(t, plan, "secrets", "a required secret nobody provides fails the build")
	require.NotContains(t, string(body), `"*"`)
	require.Contains(t, string(body), railpackBuilderImage)
	require.Contains(t, string(body), railpackRuntimeImage)
	require.Equal(t, []any{planDir}, plan["exclude"])
	require.Equal(t, "python main.py", railpackStartCommand(body))
}

// TestR094_AProcfileProgramRailpackBuildsIntoBinIsRunFromThere asserts R-094:
// the Procfile is followed even though it was written for Heroku, which puts
// bin/ on PATH. DigitalOcean's Rust sample runs `sample-rust`, and exited 127
// with "sample-rust: command not found" (issue #90).
func TestR094_AProcfileProgramRailpackBuildsIntoBinIsRunFromThere(t *testing.T) {
	plan := func(start string) map[string]any {
		return map[string]any{
			"deploy": map[string]any{"startCommand": start},
			"steps": []any{map[string]any{"commands": []any{
				map[string]any{"cmd": "cargo build --release"},
				map[string]any{"cmd": "cp target/release/sample-rust bin"},
			}}},
		}
	}
	for start, want := range map[string]string{
		"sample-rust":              "./bin/sample-rust",
		"sample-rust --port $PORT": "./bin/sample-rust --port $PORT",
		"./bin/sample-rust":        "./bin/sample-rust", // already a path
		"other-program":            "other-program",     // not one the plan builds
	} {
		p := plan(start)
		startFromBin(p)
		require.Equal(t, want, p["deploy"].(map[string]any)["startCommand"], start)
	}
}

// Detection is told the start command a Railpack plan names, and is handed the
// plan whole (R-020, R-251).
func TestR020_PlanReportsARailpackPlansStartCommand(t *testing.T) {
	fakeRailpack(t, `cat > "$PLAN" <<'EOF'
`+railpackPlanFromPrepare+`
EOF`)
	root := writeFiles(t, map[string]string{"main.py": "print('hi')\n"})

	plan, err := New().Plan(t.Context(), view{root: root})
	require.NoError(t, err)
	require.Equal(t, ".pando-plan/railpack-plan.json", plan.Entry)
	require.Equal(t, "python main.py", plan.Start)
	require.Contains(t, plan.Files[plan.Entry], railpackRuntimeImage)
	require.Nil(t, plan.Declared, "convention-matching claims nothing")
}

// Each way Railpack can fail says which it was (R-105).
func TestARailpackFailureSaysWhichItWas(t *testing.T) {
	t.Run("nothing it recognizes", func(t *testing.T) {
		fakeRailpack(t, `echo "Railpack could not determine how to build the app."; exit 1`)
		_, err := runRailpack(t.TempDir(), nil)
		require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "could not determine how to build the app")
		require.NotEmpty(t, errs.As(err).Remedy)
	})
	t.Run("a version lookup that could not reach its index", func(t *testing.T) {
		fakeRailpack(t, `echo "temporary failure fetching"; exit 75`)
		_, err := runRailpack(t.TempDir(), nil)
		require.Contains(t, errs.As(err).Message, "could not look up the language versions")
		require.Contains(t, errs.As(err).Remedy, "internet")
	})
	t.Run("success with no plan written", func(t *testing.T) {
		fakeRailpack(t, `exit 0`)
		_, err := runRailpack(t.TempDir(), nil)
		require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	})
	t.Run("a plan that is not JSON", func(t *testing.T) {
		fakeRailpack(t, `echo nonsense > "$PLAN"`)
		_, err := runRailpack(t.TempDir(), nil)
		require.Contains(t, errs.As(err).Message, "could not read")
	})
	t.Run("no railpack at all", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := runRailpack(t.TempDir(), nil)
		require.Equal(t, errs.PlanCapabilityUnsupported, errs.CodeOf(err))
	})
	t.Run("a checkout the plan cannot be written into", func(t *testing.T) {
		fakeRailpack(t, `exit 0`)
		_, err := runRailpack(writeFiles(t, map[string]string{planDir: ""}), nil)
		require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
	})
}

// Only Railpack's own images are rewritten; one the app's toolchain chose is
// left as the plan names it.
func TestOnlyRailpacksOwnImagesArePinned(t *testing.T) {
	plan := map[string]any{"steps": []any{
		map[string]any{"inputs": []any{map[string]any{"image": "dunglas/frankenphp:php8.4-trixie"}}},
		map[string]any{"commands": []any{map[string]any{"image": "ghcr.io/railwayapp/railpack-builder:mise-2026.9.12"}}},
	}}
	pinImages(plan)
	body, _ := json.Marshal(plan)
	require.Contains(t, string(body), "dunglas/frankenphp:php8.4-trixie\"")
	require.Contains(t, string(body), railpackBuilderImage)
}

func TestAConfiguredCommandIsUnwrappedFromItsShell(t *testing.T) {
	require.Equal(t, "make build", unwrapShell("sh -c 'make build'"))
	require.Equal(t, "npm run build", unwrapShell("  npm run build "))
	require.Empty(t, railpackBuildCommand([]byte("not json")))
	require.Empty(t, railpackStartCommand([]byte("not json")))
}

// fakeGateway is the client side of a frontend solve: it records what it was
// asked and answers with a result carrying an image config.
type fakeGateway struct {
	gateway.Client
	opts   map[string]string
	asked  gateway.SolveRequest
	config []byte
	err    error
}

func (f *fakeGateway) BuildOpts() gateway.BuildOpts { return gateway.BuildOpts{Opts: f.opts} }

func (f *fakeGateway) Solve(_ context.Context, req gateway.SolveRequest) (*gateway.Result, error) {
	f.asked = req
	if f.err != nil {
		return nil, f.err
	}
	res := gateway.NewResult()
	if f.config != nil {
		res.AddMeta(imageConfigKey, f.config)
	}
	return res, nil
}

// The plan goes to Railpack's pinned frontend with every option the build was
// given, and the image comes back with the bundle label added and nothing
// else of its config lost (issue #55).
func TestARailpackBuildIsHandedToTheFrontendAndLabeled(t *testing.T) {
	gw := &fakeGateway{
		opts:   map[string]string{"filename": railpackPlanFile, "build-arg:cache-key": "app_01X", "cache-imports": "[]"},
		config: []byte(`{"architecture":"arm64","config":{"Cmd":["python main.py"],"Labels":{"keep":"me"}}}`),
	}
	res, err := railpackBuild(map[string]string{api.ImageLabelBundle: "app_01X"})(context.Background(), gw)
	require.NoError(t, err)

	require.Equal(t, "gateway.v0", gw.asked.Frontend)
	require.Equal(t, railpackFrontendImage, gw.asked.FrontendOpt["source"])
	require.Equal(t, railpackPlanFile, gw.asked.FrontendOpt["filename"])
	require.Equal(t, "app_01X", gw.asked.FrontendOpt["build-arg:cache-key"])
	require.Equal(t, "[]", gw.asked.FrontendOpt["cache-imports"])

	var image struct {
		Architecture string
		Config       struct {
			Cmd    []string
			Labels map[string]string
		}
	}
	require.NoError(t, json.Unmarshal(res.Metadata[imageConfigKey], &image))
	require.Equal(t, "arm64", image.Architecture)
	require.Equal(t, []string{"python main.py"}, image.Config.Cmd)
	require.Equal(t, map[string]string{"keep": "me", api.ImageLabelBundle: "app_01X"}, image.Config.Labels)
}

func TestARailpackBuildWithoutAnImageConfigFails(t *testing.T) {
	_, err := railpackBuild(nil)(context.Background(), &fakeGateway{})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))

	_, err = railpackBuild(nil)(context.Background(), &fakeGateway{config: []byte("{")})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))

	_, err = railpackBuild(nil)(context.Background(), &fakeGateway{config: []byte(`{"config":"no"}`)})
	require.Equal(t, errs.BuildFailed, errs.CodeOf(err))

	res, err := railpackBuild(map[string]string{"a": "b"})(context.Background(), &fakeGateway{config: []byte(`{}`)})
	require.NoError(t, err)
	require.Contains(t, string(res.Metadata[imageConfigKey]), `"Labels":{"a":"b"}`)

	boom := errs.New(errs.BuildFailed, "the frontend refused")
	_, err = railpackBuild(nil)(context.Background(), &fakeGateway{err: boom})
	require.ErrorIs(t, err, boom)
}

// A stored Railpack plan is built through the frontend, and the build gets as
// far as asking the build service, which is all an unreachable one can show.
func TestAStoredRailpackPlanIsBuiltThroughTheFrontend(t *testing.T) {
	t.Setenv("PANDO_BUILD_CACHE_DIR", t.TempDir())
	entry := filepath.ToSlash(filepath.Join(planDir, railpackPlanFile))
	var sink strings.Builder
	_, err := unreachable(t).Build(context.Background(), api.BuildRequest{
		Strategy:       spec.BuildBuildpack,
		Source:         view{root: t.TempDir()},
		Dockerfile:     entry,
		GeneratedFiles: map[string]string{entry: railpackPlanFromPrepare},
		CacheNamespace: "app_01RP/web",
		ImageSink:      &sink,
		Timeout:        5 * time.Second,
	})
	require.Contains(t, []errs.Code{errs.BuildFailed, errs.BuildTimeout}, errs.CodeOf(err))
	require.True(t, isRailpackPlan(entry))
}

// Railpack copies optional files from directories an app may not have, and
// BuildKit fails the build on the missing directory. Those copies go; a copy
// from an image, of a path at the root, or from a directory that is there
// stays (issue #172).
func TestACopyFromADirectoryTheAppDoesNotHaveIsDropped(t *testing.T) {
	root := withFiles(t, map[string]string{"lib/app.ex": "", "priv/static/x": ""})
	step := map[string]any{"commands": []any{
		map[string]any{"src": "config/config.exs*", "dest": "config/"},
		map[string]any{"src": "lib*", "dest": "."},
		map[string]any{"src": "priv/static*", "dest": "priv/"},
		map[string]any{"src": "/usr/bin/composer", "dest": "/usr/bin/", "image": "composer:2"},
		map[string]any{"src": "config/required.exs", "dest": "config/"},
		map[string]any{"cmd": "mix release"},
	}}
	dropCopiesFromMissingDirs(step, root)
	body, _ := json.Marshal(step)
	require.NotContains(t, string(body), "config.exs*")
	for _, kept := range []string{"lib*", "priv/static*", "composer", "config/required.exs", "mix release"} {
		require.Contains(t, string(body), kept)
	}
}

// A start command that runs a script the repository does not have is not
// reported to detection, which asks instead (issue #172).
func TestAStartCommandRunningAMissingScriptIsNotReported(t *testing.T) {
	root := withFiles(t, map[string]string{"src/index.js": "", "app.py": ""})
	require.True(t, runsMissingScript(root, "node index.js"))
	require.False(t, runsMissingScript(root, "node src/index.js"))
	require.False(t, runsMissingScript(root, "node --enable-source-maps src/index.js"))
	require.False(t, runsMissingScript(root, "python app.py"))
	require.False(t, runsMissingScript(root, "deno run --allow-all main.ts"), "the first argument is a subcommand")
	require.False(t, runsMissingScript(root, "gunicorn app:app"))
	require.False(t, runsMissingScript(root, "node"))

	fakeRailpack(t, `echo '{"steps": [], "deploy": {"startCommand": "node index.js"}}' > "$PLAN"`)
	plan, err := New().Plan(t.Context(), view{root: withFiles(t, map[string]string{"package.json": `{"main":"index.js"}`, "src/index.js": ""})})
	require.NoError(t, err)
	require.Empty(t, plan.Start)
}

// A Railpack plan is replayed without the .ruby-version a nixpacks plan needed:
// Railpack's mise reads it too, and it overrode the Ruby the plan names, so
// native gems were built for one Ruby and run on another (issue #172).
func TestAReplayedRailpackPlanGetsNoRubyVersionWritten(t *testing.T) {
	entry := filepath.ToSlash(filepath.Join(planDir, railpackPlanFile))
	root := writeFiles(t, map[string]string{"Gemfile": "gem 'sinatra'\n"})
	_, err := synthesize(api.BuildRequest{
		Strategy: spec.BuildBuildpack, Dockerfile: entry,
		GeneratedFiles: map[string]string{entry: railpackPlanFromPrepare},
	}, root)
	require.NoError(t, err)
	require.NoFileExists(t, filepath.Join(root, ".ruby-version"))
}
