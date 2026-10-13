package buildkit

import (
	"testing"

	"github.com/stretchr/testify/require"
)

const railpackPHPPlan = `{
  "steps": [
    {"name": "install:composer", "commands": [{"cmd": "composer install --optimize-autoloader --no-scripts --no-interaction"}]},
    {"name": "build", "inputs": [{"step": "install:composer"}, {"local": true, "include": ["."]}]}
  ],
  "deploy": {"startCommand": "/start-container.sh"}
}`

// TestR095_RailpackIsSteeredWhereTheRepositoryAlreadySays asserts R-095's
// up-front steering for what the benchmark's second round found Railpack
// missing (issue #90).
func TestR095_RailpackIsSteeredWhereTheRepositoryAlreadySays(t *testing.T) {
	// A .python-version that holds a range names no Python, and an old
	// gunicorn still needs one that ships pkg_resources, when the range
	// allows it.
	flask := proactiveSteers(writeTree(t, map[string]string{
		".python-version": ">= 3.10\n", "requirements.txt": "Flask >= 2.2\ngunicorn==20.1.0\n",
	}))
	require.Contains(t, envOf(flask), "RAILPACK_PYTHON_VERSION=3.11")
	require.NotContains(t, envOf(proactiveSteers(writeTree(t, map[string]string{
		".python-version": ">=3.12\n", "requirements.txt": "gunicorn==20.1.0\n",
	}))), "RAILPACK_PYTHON_VERSION=3.11", "a range that does not allow 3.11 is the author's")
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{
		".python-version": "3.13\n", "requirements.txt": "gunicorn==20.1.0\n",
	})), "a plain version is the author's")

	// Composer's post-install scripts, which Railpack's install skips, run
	// once the app's files are in place.
	cake := proactiveSteers(writeTree(t, map[string]string{
		"composer.json": `{"scripts":{"post-install-cmd":"App\\Console\\Installer::postInstall"}}`,
	}))
	require.Len(t, cake, 1)
	plan := editedPlan(t, cake, railpackPHPPlan)
	build := planSteps(plan)[1]
	require.Equal(t, "build", build["name"])
	require.Equal(t, []any{map[string]any{"cmd": `sh -c 'env -u APP_ENV -u APP_DEBUG composer run-script post-install-cmd --no-interaction || echo "Pando: composer post-install-cmd scripts failed; the build goes on without them" >&2'`}}, build["commands"])
	symfony := proactiveSteers(writeTree(t, map[string]string{
		"composer.json": `{"scripts":{"auto-scripts":{"sass:build":"symfony-cmd"},"post-install-cmd":["@auto-scripts"]}}`,
	}))
	require.Equal(t, "composer-post-install", symfony[0].Key)
	require.Empty(t, proactiveSteers(writeTree(t, map[string]string{
		"composer.json": `{"scripts":{"post-update-cmd":["@auto-scripts"],"post-install-cmd":[]}}`,
	})))

	// Drupal's front controller is written by Composer into the web root
	// composer.json names, and Railpack serves it from there.
	drupal := writeTree(t, map[string]string{
		"composer.json": `{"extra":{"drupal-scaffold":{"locations":{"web-root":"web/"}}}}`, "composer.lock": "{}",
	})
	require.Equal(t, []string{"RAILPACK_PHP_ROOT_DIR=/app/web"}, railpackDefaults(drupal))
	require.Equal(t, "public", composerWebRoot(writeTree(t, map[string]string{"composer.json": `{"extra":{"public-dir":"./public"}}`})))
	require.Empty(t, composerWebRoot(writeTree(t, map[string]string{"composer.json": `{"extra":{"public-dir":"../elsewhere"}}`})))
}

// TestR095_APDMProjectThatDoesNotBuildIsInstalledWithoutItself asserts R-095's
// steering by the log: `pdm install` builds the project itself, and in
// Railpack's install step, which has only pyproject.toml and pdm.lock,
// hatchling could not ("Readme file does not exist: README.md", issue #90).
// The dependencies are installed alone, after the stale lock is let through.
func TestR095_APDMProjectThatDoesNotBuildIsInstalledWithoutItself(t *testing.T) {
	stale := reactiveSteers("WARNING: Lockfile doesn't satisfy the project requirements\n!! pdm install --check --prod --no-editable: process\n", t.TempDir(), nil)
	project := reactiveSteers("  ✖ Install litestar-hello-world 0.1.0 failed\n[BuildError]: Build backend raised error: Showing the last 10 lines\nOSError: Readme file does not exist: README.md\n"+
		"!! pdm install --prod --no-editable: process \"pdm install --prod --no-editable\" did not complete successfully: exit code: 1\n", t.TempDir(), nil)
	require.Len(t, project, 1)
	require.Equal(t, "pdm-no-self", project[0].Key)

	plan := editedPlan(t, append(stale, project...), railpackPythonPlan)
	cmd := planSteps(plan)[1]["commands"].([]any)[0].(map[string]any)["cmd"]
	require.Equal(t, "pdm install --no-self --prod --no-editable", cmd)

	require.Empty(t, reactiveSteers("[BuildError]: Build backend raised error\n!! pip install -r requirements.txt: process\n", t.TempDir(), nil),
		"only a pdm install is steered this way")
}
