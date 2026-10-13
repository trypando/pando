package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR094_APlatformManifestsStartCommandIsHonored asserts R-094 tier 2: a
// hosting platform's manifest is the author saying how the app runs, as a
// Procfile is. railway.json and render.yaml's commands were ignored, and the
// apps were started as `python main.py` (issue #90).
func TestR094_APlatformManifestsStartCommandIsHonored(t *testing.T) {
	railway := writeTree(t, map[string]string{
		"main.py":          "from fastapi import FastAPI\napp = FastAPI()\n",
		"requirements.txt": "fastapi\nhypercorn\n",
		"railway.json":     `{"build":{"builder":"NIXPACKS"},"deploy":{"startCommand":"hypercorn main:app --bind \"[::]:$PORT\""}}`,
	})
	d := readDeclaredBuild(railway)
	require.Equal(t, rankProcfile, d.Rank)
	require.Equal(t, "railway.json", d.Source)
	require.Equal(t, `hypercorn main:app --bind "[::]:$PORT"`, d.Start)
	require.Equal(t, d.Start, declaredFor(api.BuildRequest{}, railway).Start, "it outranks what the framework implies")

	toml := writeTree(t, map[string]string{"railway.toml": "[deploy]\nstartCommand = \"gunicorn main:app\"\n"})
	require.Equal(t, "gunicorn main:app", readDeclaredBuild(toml).Start)

	render := writeTree(t, map[string]string{"render.yaml": `services:
  - type: web
    name: fastapi-example
    runtime: python
    buildCommand: pip install -r requirements.txt
    startCommand: uvicorn main:app --host 0.0.0.0 --port $PORT
`})
	d = readDeclaredBuild(render)
	require.Equal(t, "uvicorn main:app --host 0.0.0.0 --port $PORT", d.Start)
	require.Empty(t, d.Build, "installing the requirements is Railpack's own step, not a build")

	two := writeTree(t, map[string]string{"render.yaml": "services:\n  - type: web\n    startCommand: a\n  - type: web\n    startCommand: b\n"})
	require.False(t, readDeclaredBuild(two).found(), "two web services are two apps")

	substituted := writeTree(t, map[string]string{"railway.json": `{"deploy":{"startCommand":"./run $(cat args)"}}`})
	require.False(t, readDeclaredBuild(substituted).found())
}

// A source that says how the app is built and not how it starts leaves the
// start to one that does: a Makefile builds, the Procfile runs.
func TestR094_TheStartComesFromWhicheverSourceSaysIt(t *testing.T) {
	root := writeTree(t, map[string]string{
		"Makefile": "build:\n\tnpm run build\n",
		"Procfile": "web: node server.js\n",
	})
	d := readDeclaredBuild(root)
	require.Equal(t, rankRunner, d.Rank)
	require.Equal(t, "make build", d.Build)
	require.Equal(t, "node server.js", d.Start)
}

// TestR094_AMakefilesGoBuildOutputIsWhatStarts asserts R-094 tier 3: a
// Makefile that compiles `go build -o realtime-advanced` has said what the
// program is, and Railpack's default ./out does not exist (issue #90).
func TestR094_AMakefilesGoBuildOutputIsWhatStarts(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":   "module x\ngo 1.24\n",
		"Makefile": "all: deps build\n\ndeps:\n\tgo get -d ./...\n\nbuild: deps\n\tgo build -o realtime-advanced main.go rooms.go\n",
	})
	d := readDeclaredBuild(root)
	require.Equal(t, "make build", d.Build)
	require.Equal(t, "./realtime-advanced", d.Start)

	run := writeTree(t, map[string]string{"Makefile": "build:\n\tgo build -o bin/app .\nrun: build\n\t./bin/app --serve\n"})
	require.Equal(t, "./bin/app --serve", readDeclaredBuild(run).Start, "a run target says it better")
}

// TestR094_DenoTasksBuildAndStartTheApp asserts R-094 tier 3: deno.json's
// tasks are the project's runner. Fresh 2's main.ts only exports the app, so
// `deno run main.ts` exited 0 at once (issue #90).
func TestR094_DenoTasksBuildAndStartTheApp(t *testing.T) {
	root := writeTree(t, map[string]string{"deno.json": `{
  // Fresh
  "tasks": {"dev": "vite", "build": "vite build", "start": "deno serve -A _fresh/server.js"}
}`})
	d := readDeclaredBuild(root)
	require.Equal(t, rankRunner, d.Rank)
	require.Equal(t, "deno install && deno task build", d.Build)
	require.Equal(t, "deno task start", d.Start)

	require.False(t, readDeclaredBuild(writeTree(t, map[string]string{"deno.json": `{"tasks":{"dev":"deno run -A dev.ts"}}`})).found())
}

// TestR104_APythonAppIsServedByTheServerItInstalls asserts R-104's sane
// default for how a Python app starts, across the frameworks that leave
// serving to a server (issue #90).
func TestR104_APythonAppIsServedByTheServerItInstalls(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"BlackSheep, served by uvicorn", map[string]string{
			"server.py":        "from blacksheep import Application\napp = Application(show_error_details=True)\n",
			"requirements.txt": "blacksheep>=2.0.6\nuvicorn==0.34.2\n",
		}, "uvicorn server:app --host 0.0.0.0 --port ${PORT:-8000}"},
		{"an app imported from a package", map[string]string{
			"server.py":        "from app.main import app\n",
			"requirements.txt": "uvicorn[standard]\nblacksheep\n",
		}, "uvicorn server:app --host 0.0.0.0 --port ${PORT:-8000}"},
		{"FastAPI's [all] extra brings uvicorn", map[string]string{
			"main.py":          "from fastapi import FastAPI\napp = FastAPI()\n",
			"requirements.txt": "fastapi[all]\n",
		}, "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}"},
		{"Hypercorn when that is the server", map[string]string{
			"main.py":          "from fastapi import FastAPI\napp = FastAPI()\n",
			"requirements.txt": "fastapi==0.100.0\nhypercorn==0.14.4\n",
		}, "hypercorn main:app --bind 0.0.0.0:${PORT:-8000}"},
		{"a module that runs uvicorn itself", map[string]string{
			"main.py":          "import uvicorn\nif __name__ == \"__main__\":\n    uvicorn.run(\"app.app:app\", host=\"0.0.0.0\")\n",
			"requirements.txt": "fastapi-users\nuvicorn\n",
		}, "python main.py"},
		{"Flask on its own server, on every interface", map[string]string{
			"app.py":           "from flask import Flask\napp = Flask(__name__)\nif __name__ == '__main__':\n    app.run()\n",
			"requirements.txt": "Flask==3.1.0\n",
		}, "flask --app app:app run --host 0.0.0.0 --port ${PORT:-5000}"},
		{"Streamlit", map[string]string{
			"app.py":           "import streamlit as st\nst.title('hi')\n",
			"requirements.txt": "streamlit>=1.55\n",
		}, "streamlit run app.py --server.address 0.0.0.0 --server.port ${PORT:-8501}"},
		{"Reflex", map[string]string{
			"rxconfig.py":      "import reflex as rx\n",
			"requirements.txt": "reflex==0.6.0\n",
		}, "reflex run --env prod"},
		{"aiohttp runs itself", map[string]string{
			"app.py":           "from aiohttp import web\napp = web.Application()\nif __name__ == '__main__':\n    web.run_app(app)\n",
			"requirements.txt": "aiohttp\n",
		}, "python app.py"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pythonAppStart(writeTree(t, tc.files)))
		})
	}

	require.Equal(t, 8501, pythonAppPort(writeTree(t, cases[6].files)))
	require.Equal(t, 3000, pythonAppPort(writeTree(t, cases[7].files)))
	require.Zero(t, pythonAppPort(writeTree(t, cases[0].files)))
}

// TestR104_ARack3AppWithoutRackupStartsUnderPuma asserts R-104's default for
// a bare Rack app: Rack 3 moved rackup into its own gem, and Railpack's
// `bundle exec rackup` was "command not found" (issue #90).
func TestR104_ARack3AppWithoutRackupStartsUnderPuma(t *testing.T) {
	lock := "GEM\n  specs:\n    puma (6.4.2)\n    rack (3.1.7)\n"
	root := writeTree(t, map[string]string{"config.ru": "run ->(env) { [200, {}, ['hi']] }\n", "Gemfile": "gem 'puma'\n", "Gemfile.lock": lock})
	require.Equal(t, "bundle exec puma -b tcp://0.0.0.0:${PORT:-9292}", rackStart(root))
	require.Equal(t, rackStart(root), declaredFor(api.BuildRequest{}, root).Start)

	require.Empty(t, rackStart(writeTree(t, map[string]string{"config.ru": "", "Gemfile.lock": lock + "    rackup (2.1.0)\n"})),
		"with rackup, Railpack's command works")
	require.Empty(t, rackStart(writeTree(t, map[string]string{"config.ru": "", "Gemfile.lock": "    puma (6.4.2)\n    rack (2.2.8)\n"})),
		"Rack 2 still has rackup")
}

// TestR094_PHPIsServedFromItsWebRoot asserts R-094's reading of a PHP app's
// layout: Nette's www/, and an entry point in a directory of its own, as
// Adminer's is (issue #90).
func TestR094_PHPIsServedFromItsWebRoot(t *testing.T) {
	nette := writeTree(t, map[string]string{"composer.json": "{}", "www/index.php": "<?php", "app/Bootstrap.php": "<?php"})
	require.Contains(t, railpackDefaults(nette), "RAILPACK_PHP_ROOT_DIR=/app/www")

	adminer := writeTree(t, map[string]string{
		"composer.json": `{"name":"vrana/adminer"}`, "adminer/index.php": "<?php", "editor/index.php": "<?php",
		"tests/index.php": "<?php",
	})
	require.Contains(t, railpackDefaults(adminer), "RAILPACK_PHP_ROOT_DIR=/app/adminer", "of several, the one named after the package")

	root := writeTree(t, map[string]string{"composer.json": "{}", "index.php": "<?php", "www/index.php": "<?php"})
	require.Empty(t, railpackDefaults(root), "an index.php at the root is served from the root")

	router := writeTree(t, map[string]string{"composer.json": `{"require":{"php":">=8.1"},"scripts":{"start":["Composer\\Config::disableProcessTimeout","php -S 0.0.0.0:8000 router.php"]}}`})
	require.Equal(t, "php -S 0.0.0.0:8000 router.php", declaredFor(api.BuildRequest{}, router).Start)
}

// TestR095_RailpackIsGivenAPHPVersionItCanResolve asserts R-095: Railpack
// stopped planning on any require.php but a plain version or a caret ("No
// version available for php >=7.4", issue #90). It is handed the newest
// release the constraint allows while it plans, and the repository's own
// file is put back.
func TestR095_RailpackIsGivenAPHPVersionItCanResolve(t *testing.T) {
	for constraint, want := range map[string]string{
		">=7.4":                                "8.4",
		">=5.4.8":                              "8.4",
		"~8.2.0 || ~8.3.0 || ~8.4.0 || ~8.5.0": "8.4",
		">=8.1 <8.4":                           "8.3",
		"^7.4|^8.2":                            "8.4",
	} {
		got, ok := railpackPHPConstraint(constraint)
		require.True(t, ok, constraint)
		require.Equal(t, want, got, constraint)
	}
	for _, readable := range []string{"^8.2", "8.3", "8.3.4", ""} {
		_, ok := railpackPHPConstraint(readable)
		require.False(t, ok, readable)
	}
	_, ok := railpackPHPConstraint("^7.4")
	require.False(t, ok, "nothing Railpack builds on satisfies it, and planning says so")

	original := `{"name":"getkirby/starterkit","require":{"php":"~8.2.0 || ~8.3.0","getkirby/cms":"^5.2"}}`
	root := writeTree(t, map[string]string{"composer.json": original})
	restore := pinRailpackPHP(root)
	during, err := os.ReadFile(filepath.Join(root, "composer.json"))
	require.NoError(t, err)
	require.Contains(t, string(during), `"php": "8.3"`)
	require.Contains(t, string(during), `"getkirby/cms": "^5.2"`)
	restore()
	after, err := os.ReadFile(filepath.Join(root, "composer.json"))
	require.NoError(t, err)
	require.Equal(t, original, string(after), "the build installs against what the author wrote")
}
