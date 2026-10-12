package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func asksFor(result detect.Result, key string) (detect.Question, bool) {
	for _, q := range result.Questions {
		if q.Key == key {
			return q, true
		}
	}
	return detect.Question{}, false
}

// TestR104_APnpmWorkspaceFileHoldingOnlySettingsIsNotAWorkspace asserts R-104.
// pnpm reads pnpm-workspace.yaml for settings as well as packages, and a
// single-package app that keeps onlyBuiltDependencies or `packages: ['.']`
// in it was asked which project to deploy, with no project to name (issue
// #90: the Nuxt, Adonis, Next.js and Laravel starter kits).
func TestR104_APnpmWorkspaceFileHoldingOnlySettingsIsNotAWorkspace(t *testing.T) {
	for name, workspace := range map[string]string{
		"settings only":   "onlyBuiltDependencies:\n  - esbuild\nallowBuilds:\n  esbuild: true\n",
		"the root itself": "packages:\n  - '.'\nminimumReleaseAge: 1440\n",
		"empty":           "",
	} {
		t.Run(name, func(t *testing.T) {
			result, err := auction().Run(context.Background(), memSource{
				"pnpm-workspace.yaml": workspace,
				"pnpm-lock.yaml":      "lockfileVersion: '9.0'\n",
				"package.json":        `{"name":"site","scripts":{"build":"nuxt build","start":"node .output/server/index.mjs"}}`,
			})
			require.NoError(t, err)
			require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
			_, asked := asksFor(result, detect.KeyDeployableProject)
			require.False(t, asked, "one package is one app")
		})
	}

	// A file that does list packages still declares a workspace.
	result, err := auction().Run(context.Background(), memSource{
		"pnpm-workspace.yaml":    "packages:\n  - 'apps/*'\n",
		"package.json":           `{"name":"mono","private":true}`,
		"apps/web/package.json":  `{"name":"web"}`,
		"apps/docs/package.json": `{"name":"docs"}`,
	})
	require.NoError(t, err)
	require.Equal(t, detect.StrategyUnknown, result.Winner.Strategy)
	q, asked := asksFor(result, detect.KeyDeployableProject)
	require.True(t, asked)
	require.Equal(t, []string{"apps/docs", "apps/web"}, q.Options)
}

// TestR104_AWorkspaceWhoseRootStartsTheAppIsDeployedFromItsRoot asserts R-104:
// a workspace whose root package.json starts the app has said what to deploy,
// and the workspace tool builds the members it needs from the root (issue #90).
func TestR104_AWorkspaceWhoseRootStartsTheAppIsDeployedFromItsRoot(t *testing.T) {
	members := memSource{
		"apps/web/package.json":    `{"name":"web","scripts":{"start":"node server.js"}}`,
		"apps/web/server.js":       "",
		"packages/ui/package.json": `{"name":"@repo/ui"}`,
	}
	for name, root := range map[string]memSource{
		"pnpm": {
			"pnpm-workspace.yaml": "packages:\n  - 'apps/*'\n  - 'packages/*'\n",
			"package.json":        `{"name":"mono","private":true,"scripts":{"start":"pnpm --filter web start"}}`,
		},
		"npm workspaces": {
			"package.json": `{"name":"mono","workspaces":["apps/*","packages/*"],"scripts":{"start":"npm run start --workspace=web"}}`,
		},
		"yarn workspaces object": {
			"package.json": `{"name":"mono","workspaces":{"packages":["apps/*"]},"scripts":{"start":"yarn workspace web start"}}`,
		},
		"turborepo": {
			"turbo.json":   `{"tasks":{"build":{"dependsOn":["^build"]}}}`,
			"package.json": `{"name":"mono","workspaces":["apps/*","packages/*"],"scripts":{"build":"turbo run build","start":"npm run start --workspace=web"}}`,
		},
		"nx": {
			"nx.json":      "{}",
			"package.json": `{"name":"mono","scripts":{"start":"nx run server:serve"}}`,
		},
		"lerna": {
			"lerna.json":   `{"packages":["apps/*"]}`,
			"package.json": `{"name":"mono","scripts":{"start":"lerna run start --scope web"}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			src := memSource{}
			for k, v := range members {
				src[k] = v
			}
			for k, v := range root {
				src[k] = v
			}
			result, err := auction().Run(context.Background(), src)
			require.NoError(t, err)
			require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
			_, asked := asksFor(result, detect.KeyDeployableProject)
			require.False(t, asked)

			// Without the start script, the root says nothing about which
			// member is the app, and that is still asked.
			src["package.json"] = `{"name":"mono","private":true,"workspaces":["apps/*","packages/*"]}`
			result, err = auction().Run(context.Background(), src)
			require.NoError(t, err)
			require.Equal(t, detect.StrategyUnknown, result.Winner.Strategy)
			_, asked = asksFor(result, detect.KeyDeployableProject)
			require.True(t, asked)
			require.NoError(t, detect.ValidateAll(result.Questions))
		})
	}
}

// TestR104_AGoOrCargoWorkspaceWithOneProgramIsBuiltFromItsRoot asserts R-104:
// a workspace with one program in it has one thing to deploy, and only its
// root has the modules or crates the program uses (issue #90).
func TestR104_AGoOrCargoWorkspaceWithOneProgramIsBuiltFromItsRoot(t *testing.T) {
	goWork := memSource{
		"go.work":                     "go 1.23\n\nuse (\n\t./api\n\t./shared // the library\n)\n",
		"api/go.mod":                  "module example.com/api\n\ngo 1.23\n",
		"api/main.go":                 "package main\n",
		"shared/go.mod":               "module example.com/shared\n\ngo 1.23\n",
		"shared/greeting/greeting.go": "package greeting\n",
	}
	result, err := auction().Run(context.Background(), goWork)
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy, "built from the go.work root")

	goWork["worker/go.mod"] = "module example.com/worker\n\ngo 1.23\n"
	goWork["worker/main.go"] = "package main\n"
	goWork["go.work"] = "go 1.23\n\nuse ./api\nuse ./shared\nuse ./worker\n"
	result, err = auction().Run(context.Background(), goWork)
	require.NoError(t, err)
	q, asked := asksFor(result, detect.KeyDeployableProject)
	require.True(t, asked, "two programs, and nothing saying which is the app")
	require.Equal(t, []string{"api", "shared", "worker"}, q.Options)

	cargo := memSource{
		"Cargo.toml":                "[workspace]\nresolver = \"2\"\nmembers = [\"crates/*\"]\n",
		"crates/core/Cargo.toml":    "[package]\nname = \"core\"\n",
		"crates/core/src/lib.rs":    "",
		"crates/server/Cargo.toml":  "[package]\nname = \"server\"\n\n[dependencies]\ncore = { path = \"../core\" }\n",
		"crates/server/src/main.rs": "fn main() {}\n",
	}
	result, err = auction().Run(context.Background(), cargo)
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy, "built from the workspace root")
	_, asked = asksFor(result, detect.KeyDeployableProject)
	require.False(t, asked)
}

// TestR104_TheProjectsOneLevelDownAreWhatIsDeployed asserts R-104 and R-105: a
// repository with no app at its root and no workspace manifest, whose
// projects sit one level down, was asked how it is built at all (issue #90).
// The only project is read without asking; several are named in the question.
func TestR104_TheProjectsOneLevelDownAreWhatIsDeployed(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"README.md":       "# service",
		"docs/api.md":     "",
		"service/go.mod":  "module example.com/service\n\ngo 1.23\n",
		"service/main.go": "package main\n",
	})
	require.NoError(t, err)
	require.NoError(t, detect.ValidateAll(result.Questions))
	p := detect.Proposal{Winner: result.Winner, Questions: result.Questions}
	dir, why, ok := p.Subproject(nil)
	require.True(t, ok, "the only project is read without a question")
	require.Equal(t, "service", dir)
	require.Contains(t, why, "only project")

	result, err = auction().Run(context.Background(), memSource{
		"README.md":                            "# shop",
		"services/catalog/main.py":             "",
		"services/catalog/requirements.txt":    "fastapi\n",
		"services/catalog/Procfile":            "web: uvicorn main:app\n",
		"services/storefront/app.py":           "",
		"services/storefront/requirements.txt": "flask\n",
		"services/storefront/Procfile":         "web: gunicorn app:app\n",
	})
	require.NoError(t, err)
	require.NoError(t, detect.ValidateAll(result.Questions))
	q, asked := asksFor(result, detect.KeyDeployableProject)
	require.True(t, asked)
	require.Equal(t, []string{"services/catalog", "services/storefront"}, q.Options)
	p = detect.Proposal{Winner: result.Winner, Questions: result.Questions}
	_, _, ok = p.Subproject(nil)
	require.False(t, ok, "two projects, and nothing saying which")

	// A root that is an app is read as one, whatever sits beside it.
	result, err = auction().Run(context.Background(), memSource{
		"main.go":          "package main\n",
		"go.mod":           "module x\n\ngo 1.23\n",
		"tools/go.mod":     "module tools\n",
		"web/package.json": `{"name":"web"}`,
	})
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
}

// TestR104_AnsweringWhichProjectRunsDetectionInIt asserts R-104: answering
// deployable_project used to set the subdirectory on a proposal of the
// repository root, which had no workloads, and accepting it pinned a spec
// that ran nothing (issue #90). The answer now sends detection into the
// project.
func TestR104_AnsweringWhichProjectRunsDetectionInIt(t *testing.T) {
	p := detect.Proposal{
		Winner: detect.Candidate{Strategy: detect.StrategyUnknown},
		Questions: []detect.Question{{
			Key: detect.KeyDeployableProject, Options: []string{"apps/api", "apps/web"},
		}},
	}
	answers := map[string]string{detect.KeyDeployableProject: "apps/web"}

	require.True(t, p.RedetectsOn(answers))
	dir, _, ok := p.Subproject(answers)
	require.True(t, ok)
	require.Equal(t, "apps/web", dir)
	require.NoError(t, p.CheckAnswers(answers))

	for _, bad := range []string{"../elsewhere", "/etc", "apps/admin"} {
		require.Error(t, p.CheckAnswers(map[string]string{detect.KeyDeployableProject: bad}), bad)
	}

	// The project's own proposal does not ask, and a kept answer does not
	// send detection anywhere again.
	project := detect.Proposal{Winner: detect.Candidate{Strategy: spec.BuildBuildpack}}
	require.False(t, project.RedetectsOn(answers))
	_, _, ok = project.Subproject(answers)
	require.False(t, ok)
}
