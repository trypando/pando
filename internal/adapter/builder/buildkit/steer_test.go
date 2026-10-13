package buildkit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// envOf is every setting the steers pass, merged.
func envOf(steers []steer) []string {
	var env []string
	for _, s := range steers {
		env = append(env, s.Env...)
	}
	return mergeEnv(env)
}

// editedPlan applies the steers' edits to plan and returns the result.
func editedPlan(t *testing.T, steers []steer, plan string) map[string]any {
	t.Helper()
	var p map[string]any
	require.NoError(t, json.Unmarshal([]byte(plan), &p))
	for _, s := range steers {
		if s.Edit != nil {
			s.Edit(p)
		}
	}
	return p
}

const railpackPythonPlan = `{
  "steps": [
    {"name": "packages:mise", "commands": [{"path": "/mise/shims"}, {"src": ".python-version", "dest": ".python-version"}, {"cmd": "mise install"}],
     "assets": {"mise.toml": "[tools]\npython = \"3.13.15\"\n"}},
    {"name": "install", "commands": [{"cmd": "pdm install --check --prod --no-editable", "customName": "pdm install --check --prod --no-editable"}],
     "variables": {"BUNDLE_GEMFILE": "/app/Gemfile"}}
  ],
  "deploy": {"startCommand": "python main.py"}
}`

// TestR095_ARailpackBuildThatFailsIsSteeredByItsLog asserts R-095: Pando wraps
// Railpack, and where Railpack's choice fails in a way the build log names,
// the failure becomes the documented setting or plan edit that answers it
// (issue #90). A failure nothing recognizes steers nothing.
func TestR095_ARailpackBuildThatFailsIsSteeredByItsLog(t *testing.T) {
	plain := t.TempDir()
	cases := []struct {
		name string
		log  string
		dir  string
		env  []string
		want func(t *testing.T, plan map[string]any)
	}{
		{
			name: "a crate needs a newer rustc",
			log:  "error: rustc 1.85.1 is not supported by the following packages:\n  time@0.3.47 requires rustc 1.93.0\n  time-core@0.1.8 requires rustc 1.88.0\n!! cargo build --release: process \"cargo build --release\" did not complete successfully: exit code: 101\n",
			env:  []string{"RAILPACK_RUST_VERSION=1.93.0"},
		},
		{
			name: "a crate needs Cargo's edition2024",
			log:  "Caused by:\n  feature `edition2024` is required\n!! cargo build --release: process \"cargo build --release\" did not complete successfully: exit code: 101\n",
			env:  []string{"RAILPACK_RUST_VERSION=" + defaultRustVersion},
		},
		{
			name: "no build of the Python a version file names",
			log:  "mise ERROR Failed to install core:python@3.15: no precompiled python found for core:python@3.15 on x86_64-unknown-linux-gnu\n",
			env:  []string{"RAILPACK_PYTHON_VERSION=3.14"},
			want: func(t *testing.T, plan map[string]any) {
				require.Contains(t, plan["exclude"], ".python-version")
				require.NotContains(t, mustJSON(t, plan["steps"]), `"src":".python-version"`, "no step copies it")
			},
		},
		{
			name: "a version file holding a range",
			log:  "mise WARN  semver range \">=\" is not supported for python (source: /app/.python-version)\nmise ERROR Failed to install core:python@>=\n",
			want: func(t *testing.T, plan map[string]any) {
				require.Contains(t, plan["exclude"], ".python-version")
			},
		},
		{
			name: "a C extension that does not build on Python 3.13",
			log:  "    | uvloop/loop.c:178739:27: error: too few arguments to function '_PyLong_AsByteArray'\n    | /mise/installs/python/3.13.15/include/python3.13/cpython/longobject.h:111:17: note: declared here\n!! poetry install --no-interaction --no-ansi --only main --no-root: process \"poetry install\" did not complete successfully: exit code: 1\n",
			dir:  writeTree(t, map[string]string{"pyproject.toml": "[tool.poetry.dependencies]\npython = \"^3.10\"\n"}),
			env:  []string{"RAILPACK_PYTHON_VERSION=3.12"},
		},
		{
			name: "Composer names a missing extension",
			log:  "  - codeigniter4/framework[v4.7.4, ..., v4.7.5] require ext-intl * -> it is missing from your system. Install or enable PHP's intl extension.\n    - drupal/core 11.x-dev requires ext-gd * -> it is missing from your system.\n",
			env:  []string{"RAILPACK_PHP_EXTENSIONS=gd intl"},
		},
		{
			name: "a pdm lockfile older than the project",
			log:  "WARNING: Lockfile doesn't satisfy the project requirements, packages may be outdated\n!! pdm install --check --prod --no-editable: process\n",
			want: func(t *testing.T, plan map[string]any) {
				body := mustJSON(t, plan["steps"])
				require.Contains(t, body, `"pdm install --prod --no-editable"`)
				require.NotContains(t, body, "--check")
			},
		},
		{
			name: "a native gem older than GCC 14",
			log:  "selector.c:22:5: error: passing argument 1 of 'rb_ensure' from incompatible pointer type [-Wincompatible-pointer-types]\nAn error occurred while installing nio4r (2.5.8), and Bundler cannot continue.\n",
			want: func(t *testing.T, plan map[string]any) {
				require.Contains(t, mustJSON(t, plan["steps"]), `"BUNDLE_BUILD__NIO4R":"--with-cflags=-Wno-error=incompatible-pointer-types`)
			},
		},
		{
			name: "a Bundler binstub looking for ruby beside it",
			log:  "stderr: '/usr/local/bundle/bin/bundle: 6: exec: /usr/local/bundle/bin/ruby: not found\\n'\n",
			want: func(t *testing.T, plan map[string]any) {
				require.Contains(t, mustJSON(t, plan["steps"]), `mise which ruby`)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.dir
			if dir == "" {
				dir = plain
			}
			steers := reactiveSteers(tc.log, dir, []byte(railpackPythonPlan))
			require.NotEmpty(t, steers)
			for _, s := range steers {
				require.NotEmpty(t, s.Key)
				require.NotEmpty(t, s.Why, "every steer says why, for the build log and the record")
			}
			if tc.env != nil {
				require.Equal(t, tc.env, envOf(steers))
			}
			if tc.want != nil {
				tc.want(t, editedPlan(t, steers, railpackPythonPlan))
			}
		})
	}

	require.Empty(t, reactiveSteers("npm ERR! missing script: build\n!! npm run build: process\n", plain, nil),
		"a failure nothing recognizes is reported as it was")
}

// The project's own Python requirement bounds how far a build is stepped
// down, and a version it names exactly is not stepped down at all.
func TestR095_PythonIsSteppedDownOnlyWithinWhatTheProjectAllows(t *testing.T) {
	log := "error: command '/usr/bin/gcc' failed with exit code 1\n/mise/installs/python/3.13.2/include\n!! pip install -r requirements.txt: process\n"

	steers := reactiveSteers(log, writeTree(t, map[string]string{"pyproject.toml": "[project]\nrequires-python = \">=3.13\"\n"}), nil)
	require.Empty(t, steers, "3.12 is outside >=3.13")

	steers = reactiveSteers(log, writeTree(t, map[string]string{".python-version": "3.13\n"}), nil)
	require.Empty(t, steers, "a version the project pins is the author's")

	steers = reactiveSteers("ModuleNotFoundError: No module named 'pkg_resources'\n/mise/installs/python/3.13.2/\n!! pip install -r requirements.txt: process\n", t.TempDir(), nil)
	require.Equal(t, []string{"RAILPACK_PYTHON_VERSION=3.11"}, envOf(steers),
		"pkg_resources was last in a fresh virtualenv on 3.11")
}

// TestR095_SteeringUpFrontFromWhatTheRepositorySays asserts R-095's steering
// where the repository already says enough, before any build fails.
func TestR095_SteeringUpFrontFromWhatTheRepositorySays(t *testing.T) {
	keys := func(steers []steer) []string {
		var out []string
		for _, s := range steers {
			out = append(out, s.Key)
		}
		return out
	}

	// Rust: a current stable unless the project pins one. "stable" pins
	// nothing; Railpack mapped edition 2021 to 1.84.0.
	require.Contains(t, envOf(proactiveSteers(writeTree(t, map[string]string{"Cargo.toml": "[package]\nedition = \"2021\"\n"}))),
		"RAILPACK_RUST_VERSION="+defaultRustVersion)
	require.Contains(t, envOf(proactiveSteers(writeTree(t, map[string]string{
		"Cargo.toml": "[package]\n", "rust-toolchain.toml": "[toolchain]\nchannel = \"stable\"\n",
	}))), "RAILPACK_RUST_VERSION="+defaultRustVersion)
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{
		"Cargo.toml": "[package]\n", "rust-toolchain.toml": "[toolchain]\nchannel = \"1.80.0\"\n",
	})), "a pinned toolchain is the author's")
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{"Cargo.toml": "[package]\nrust-version = \"1.75\"\n"})))

	// A .python-version mise cannot read.
	require.Equal(t, []string{"python-version-file"}, keys(proactiveSteers(writeTree(t, map[string]string{".python-version": ">= 3.10\n"}))))
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{".python-version": "3.12\n"})))

	// gunicorn before 22 imports pkg_resources.
	require.Equal(t, []string{"RAILPACK_PYTHON_VERSION=3.11"},
		envOf(proactiveSteers(writeTree(t, map[string]string{"requirements.txt": "Flask==1.1.2\ngunicorn==20.0.4\n"}))))
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{"requirements.txt": "gunicorn==23.0.0\n"})))
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{"requirements.txt": "gunicorn==20.0.4\n", "runtime.txt": "python-3.9\n"})))

	// composer.lock's packages' extensions, which Railpack does not read.
	lock := `{"packages":[{"name":"drupal/core","require":{"php":">=8.3","ext-gd":"*","ext-PDO":"*"}},{"name":"x/y","require":{"ext-intl":"*"}}]}`
	require.Equal(t, []string{"RAILPACK_PHP_EXTENSIONS=gd intl pdo"},
		envOf(proactiveSteers(writeTree(t, map[string]string{"composer.json": "{}", "composer.lock": lock}))))

	// Rack 2 on a Ruby that no longer ships base64.
	rack2 := "GEM\n  specs:\n    rack (2.2.8)\n    sinatra (3.0.0)\n"
	require.Equal(t, []string{"RAILPACK_RUBY_VERSION=3.3"}, envOf(proactiveSteers(writeTree(t, map[string]string{"Gemfile": "gem 'sinatra'\n", "Gemfile.lock": rack2}))))
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{"Gemfile": "ruby '3.4.1'\n", "Gemfile.lock": rack2})))
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{"Gemfile": "", "Gemfile.lock": rack2 + "    base64 (0.2.0)\n"})))

	// A pyproject.toml with no lockfile Railpack installs from: the project
	// is installed itself, and its virtualenv's bin/ is on the PATH.
	flit := proactiveSteers(writeTree(t, map[string]string{
		"pyproject.toml": "[build-system]\nrequires = [\"flit_core<4\"]\nbuild-backend = \"flit_core.buildapi\"\n[project]\nname = \"js_example\"\n",
	}))
	require.Equal(t, []string{"RAILPACK_INSTALL_CMD=python -m venv /app/.venv && /app/.venv/bin/pip install ."}, envOf(flit))
	plan := editedPlan(t, flit, `{"deploy":{"startCommand":"flask run"}}`)
	require.Equal(t, []any{"/app/.venv/bin"}, plan["deploy"].(map[string]any)["paths"])
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{
		"pyproject.toml": "[build-system]\nrequires = [\"poetry-core\"]\n", "poetry.lock": "",
	})), "Railpack installs from a lockfile it reads")

	// Reflex installs Bun with unzip when it starts.
	require.Contains(t, envOf(proactiveSteers(writeTree(t, map[string]string{"requirements.txt": "reflex==0.6.0\n"}))),
		"RAILPACK_DEPLOY_APT_PACKAGES=unzip curl")
}

// TestR095_ASrcLayoutPackageIsInstalledAfterItsRequirements asserts R-095.
// Flask's Celery example pins its dependencies in requirements.txt and keeps
// task_app under src/, and stopped with "Could not import 'task_app'"
// (issue #90).
func TestR095_ASrcLayoutPackageIsInstalledAfterItsRequirements(t *testing.T) {
	project := map[string]string{
		"requirements.txt":         "flask==2.3.2\n",
		"pyproject.toml":           "[project]\nname = \"x\"\n\n[build-system]\nrequires = [\"flit_core<4\"]\n",
		"src/task_app/__init__.py": "",
	}
	steers := proactiveSteers(writeTree(t, project))
	require.Len(t, steers, 1)
	require.Equal(t, "python-install-src-package", steers[0].Key)

	plan := map[string]any{"steps": []any{
		map[string]any{"name": "install", "commands": []any{map[string]any{"cmd": "pip install -r requirements.txt"}}},
		map[string]any{"name": "build", "commands": []any{map[string]any{"src": ".", "dest": "."}}},
	}}
	steers[0].Edit(plan)
	build := planSteps(plan)[1]["commands"].([]any)
	require.Equal(t, map[string]any{"cmd": "/app/.venv/bin/pip install --no-deps ."}, build[len(build)-1])
	require.Len(t, planSteps(plan)[0]["commands"], 1)

	// A package at the top is importable from the working directory as it is.
	delete(project, "src/task_app/__init__.py")
	project["task_app/__init__.py"] = ""
	require.Empty(t, proactiveSteers(writeTree(t, project)))
}

// TestR020_ASteeredPlanRecordsHowItWasSteered asserts R-020: the plan the
// spec carries says what changed it. The steers' settings reach the planner,
// their edits are made to what it wrote, and the record is beside the plan.
func TestR020_ASteeredPlanRecordsHowItWasSteered(t *testing.T) {
	root := t.TempDir()
	var got []string
	base := func(contextDir string, env []string) (string, error) {
		got = env
		name := filepath.Join(planDir, railpackPlanFile)
		require.NoError(t, os.MkdirAll(filepath.Join(contextDir, planDir), 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(contextDir, name), []byte(railpackPythonPlan), 0o644))
		return name, nil
	}
	steers := append(reactiveSteers("Lockfile doesn't satisfy the project requirements\n", root, nil),
		steer{Key: "packages:a", Why: "a", Env: []string{"RAILPACK_DEPLOY_APT_PACKAGES=unzip"}},
		steer{Key: "packages:b", Why: "b", Env: []string{"RAILPACK_DEPLOY_APT_PACKAGES=curl unzip"}})

	name, err := steeredPlanner(base, steers)(root, []string{"RAILPACK_START_CMD=python main.py", "RAILPACK_DEPLOY_APT_PACKAGES=git"})
	require.NoError(t, err)
	require.Equal(t, []string{"RAILPACK_START_CMD=python main.py", "RAILPACK_DEPLOY_APT_PACKAGES=git unzip curl"}, got,
		"list settings add up; nothing is passed twice")

	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.NotContains(t, string(body), "--check")

	files, err := collectPlan(root)
	require.NoError(t, err)
	record := files[filepath.Join(planDir, steeringFile)]
	require.Contains(t, record, "pdm-install: ")
	require.Contains(t, record, "packages:b: b")
}

// A steer already applied is not applied again: the retry is bounded by what
// is new, and by maxSteers.
func TestASteerIsAppliedOnce(t *testing.T) {
	a := steer{Key: "rust-version:1.90.0"}
	b := steer{Key: "php-extensions:intl"}
	require.Equal(t, []steer{b}, newSteers([]steer{a, b, b}, []steer{a}))
	require.Empty(t, newSteers([]steer{a}, []steer{a}))
	require.LessOrEqual(t, maxSteers, 3)
}

// The log kept for steering is the end of the build's, passed on whole.
func TestTheSteeringLogKeepsTheEnd(t *testing.T) {
	var out strings.Builder
	tail := newLogTail(&out)
	_, _ = tail.Write([]byte(strings.Repeat("x", logTailMax)))
	_, _ = tail.Write([]byte("the failing step"))
	require.True(t, strings.HasSuffix(tail.String(), "the failing step"))
	require.Len(t, tail.String(), logTailMax)
	require.Equal(t, logTailMax+len("the failing step"), out.Len())
	tail.Reset()
	require.Empty(t, tail.String())
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}
