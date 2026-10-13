package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// The Railpack settings Pando supplies when the repository is silent, each of
// which a deploy QA app failed without (issue #55). Railpack serves public/
// only for Laravel, and a Slim app's front controller is there too.
func TestR095_APHPAppIsServedFromWhereItsFrontControllerIs(t *testing.T) {
	slim := writeFiles(t, map[string]string{"composer.json": "{}"})
	require.NoError(t, os.MkdirAll(filepath.Join(slim, "public"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(slim, "public", "index.php"), []byte("<?php"), 0o644))
	require.Contains(t, railpackDefaults(slim), "RAILPACK_PHP_ROOT_DIR=/app/public")

	root := writeFiles(t, map[string]string{"composer.json": "{}", "index.php": "<?php"})
	require.NotContains(t, railpackDefaults(root), "RAILPACK_PHP_ROOT_DIR=/app/public",
		"a front controller at the root is served from the root")
}

// A plan nixpacks made reads .ruby-version at build time, and a fresh checkout
// has none unless it is written from the Gemfile (issue #55). Kept for specs
// stored before Railpack, which reads the Gemfile itself.
func TestR095_ARubyProjectsVersionComesFromItsGemfile(t *testing.T) {
	dir := writeFiles(t, map[string]string{"Gemfile": "source 'https://rubygems.org'\nruby '3.2.4'\ngem 'sinatra'\n"})
	ensureRubyVersion(dir)
	body, err := os.ReadFile(filepath.Join(dir, ".ruby-version"))
	require.NoError(t, err)
	require.Equal(t, "3.2.4\n", string(body))

	silent := writeFiles(t, map[string]string{"Gemfile": "gem 'sinatra'\n"})
	ensureRubyVersion(silent)
	body, err = os.ReadFile(filepath.Join(silent, ".ruby-version"))
	require.NoError(t, err)
	require.Equal(t, defaultRubyVersion+"\n", string(body))

	kept := writeFiles(t, map[string]string{"Gemfile": "ruby '3.1.0'\n", ".ruby-version": "3.3.0\n"})
	ensureRubyVersion(kept)
	body, err = os.ReadFile(filepath.Join(kept, ".ruby-version"))
	require.NoError(t, err)
	require.Equal(t, "3.3.0\n", string(body), "one the repository has is left alone")
}

func TestR095_ADotnetProjectIsBuiltOnTheSDKItTargets(t *testing.T) {
	b, ok := readDotnetBuild(writeFiles(t, map[string]string{
		"DotnetMinimal.csproj": `<Project Sdk="Microsoft.NET.Sdk.Web"><PropertyGroup><TargetFramework>net8.0</TargetFramework></PropertyGroup></Project>`,
	}))
	require.True(t, ok)
	require.Equal(t, dotnetBuild{Project: "DotnetMinimal.csproj", Assembly: "DotnetMinimal", Version: "8.0"}, b)

	_, ok = readDotnetBuild(writeFiles(t, map[string]string{
		"A.csproj": "<TargetFramework>net8.0</TargetFramework>", "B.csproj": "<TargetFramework>net8.0</TargetFramework>",
	}))
	require.False(t, ok, "two projects are a solution whose entry point is not guessed")
}

// nixpacks started every Mix project with `mix phx.server` and every Rack app
// with a rackup bound to 127.0.0.1, and Pando overrode both (issue #55).
// Railpack starts a Mix project from its release and binds rackup to 0.0.0.0,
// so nothing is passed: `mix run` would not even exist in its runtime image,
// which carries the release and not the toolchain.
func TestR104_ElixirAndRackAppsAreStartedByRailpack(t *testing.T) {
	plug := writeFiles(t, map[string]string{"mix.exs": "defp deps, do: [{:bandit, \"~> 1.5\"}]"})
	require.Empty(t, declaredFor(api.BuildRequest{}, plug).Start)

	rack := writeFiles(t, map[string]string{"config.ru": "run App", "Gemfile": ""})
	require.Empty(t, declaredFor(api.BuildRequest{}, rack).Start)
}

// A Procfile beside a committed program is run as it is; the planner had
// nothing to recognize and refused it (issue #55).
func TestR094_ACommittedProgramIsRunAsItIs(t *testing.T) {
	dir := writeFiles(t, map[string]string{"server": "\x7fELF", "Procfile": "web: ./server --port $PORT\n"})
	require.Equal(t, "./server --port $PORT", committedProgram(dir, "./server --port $PORT"))
	require.Empty(t, committedProgram(dir, "./missing"), "only a program that is there")
	require.Empty(t, committedProgram(dir, "python app.py"), "only a program in the repository")
	require.Empty(t, committedProgram(dir, "./../etc/passwd"))

	name, err := writeCommittedProgramPlan(dir, "./server --port $PORT")
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(dir, name))
	require.NoError(t, err)
	require.Contains(t, string(body), `CMD ["./server --port $PORT"]`)
}

// A FastAPI or Flask module leaves serving the app to a server, and `python
// main.py` imported it and exited 0 (issue #55).
func TestR104_APythonWebAppIsStartedUnderItsServer(t *testing.T) {
	fastapi := writeFiles(t, map[string]string{
		"main.py":          "from fastapi import FastAPI\napp = FastAPI()\n",
		"requirements.txt": "fastapi\nuvicorn\n",
	})
	require.Equal(t, "uvicorn main:app --host 0.0.0.0 --port ${PORT:-8000}", pythonAppStart(fastapi))

	selfStarting := writeFiles(t, map[string]string{
		"app.py":           "from flask import Flask\napp = Flask(__name__)\nif __name__ == '__main__':\n    app.run(host='0.0.0.0')\n",
		"requirements.txt": "flask\ngunicorn\n",
	})
	require.Equal(t, "python app.py", pythonAppStart(selfStarting),
		"an app that starts itself is run as it is, not as Railpack's gunicorn main:app")

	// One that serves itself on the loopback address — Flask's app.run()
	// with no host — is served by its server instead, under its own module
	// (issue #90).
	loopback := writeFiles(t, map[string]string{
		"app.py":           "from flask import Flask\napp = Flask(__name__)\nif __name__ == '__main__':\n    app.run()\n",
		"requirements.txt": "flask\ngunicorn\n",
	})
	require.Equal(t, "gunicorn app:app --bind 0.0.0.0:${PORT:-8000}", pythonAppStart(loopback))

	noServer := writeFiles(t, map[string]string{
		"main.py": "from fastapi import FastAPI\napi = FastAPI()\n", "requirements.txt": "fastapi\n",
	})
	require.Empty(t, pythonAppStart(noServer), "a server the app does not install is not named")

	flask := writeFiles(t, map[string]string{
		"app.py":           "from flask import Flask\nserver = Flask(__name__)\n",
		"requirements.txt": "flask\ngunicorn\n",
	})
	require.Equal(t, "gunicorn app:server --bind 0.0.0.0:${PORT:-8000}", pythonAppStart(flask))
}

// A Django app's collectstatic becomes the build step when nothing else
// declares one, and does not displace a build the repository declares.
func TestR095_DjangosCollectstaticIsTheBuildWhenNoneIsDeclared(t *testing.T) {
	files := map[string]string{"manage.py": "#!/usr/bin/env python"}
	dir := writeFiles(t, files)
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "site"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "site", "settings.py"), []byte("STATIC_ROOT = 'static'\n"), 0o644))
	require.Equal(t, "python manage.py collectstatic --noinput", declaredFor(api.BuildRequest{}, dir).Build)

	require.NoError(t, os.WriteFile(filepath.Join(dir, "Makefile"), []byte("build:\n\tpython build.py\n"), 0o644))
	require.NotEqual(t, "python manage.py collectstatic --noinput", declaredFor(api.BuildRequest{}, dir).Build,
		"the repository's own build outranks the convention")
}

// TestR095_CollectstaticIsNotRunTwiceWhenTheStartRunsIt asserts R-095. Railway's
// Django template collects its static files at start, where the database
// variables its settings read at import exist; run again at build, the same
// command stopped the build on a KeyError (issue #90).
func TestR095_CollectstaticIsNotRunTwiceWhenTheStartRunsIt(t *testing.T) {
	dir := writeFiles(t, map[string]string{
		"manage.py": "#!/usr/bin/env python",
		"railway.json": `{"deploy": {"startCommand": "python manage.py migrate && python manage.py collectstatic ` +
			`--noinput && gunicorn mysite.wsgi"}}`,
	})
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "mysite"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "mysite", "settings.py"),
		[]byte("STATIC_ROOT = 'static'\nNAME = os.environ[\"PGDATABASE\"]\n"), 0o644))
	declared := declaredFor(api.BuildRequest{}, dir)
	require.Contains(t, declared.Start, "collectstatic")
	require.Empty(t, declared.Build)
}

// The lockfile's Ruby is used when the Gemfile names none, and the Gemfile's
// wins when both do: it is the declaration Bundler enforces.
func TestARubyVersionIsReadFromTheLockfileWhenTheGemfileIsSilent(t *testing.T) {
	lock := "GEM\n  specs:\n\nRUBY VERSION\n   ruby 3.1.6p260\n"
	dir := writeFiles(t, map[string]string{"Gemfile": "gem 'rack'\n", "Gemfile.lock": lock})
	ensureRubyVersion(dir)
	body, err := os.ReadFile(filepath.Join(dir, ".ruby-version"))
	require.NoError(t, err)
	require.Equal(t, "3.1.6\n", string(body))

	both := writeFiles(t, map[string]string{"Gemfile": "ruby '3.3.1'\n", "Gemfile.lock": lock})
	ensureRubyVersion(both)
	body, err = os.ReadFile(filepath.Join(both, ".ruby-version"))
	require.NoError(t, err)
	require.Equal(t, "3.3.1\n", string(body))

	notRuby := writeFiles(t, map[string]string{"go.mod": "module x\n"})
	ensureRubyVersion(notRuby)
	_, err = os.Stat(filepath.Join(notRuby, ".ruby-version"))
	require.True(t, os.IsNotExist(err), "only a Ruby project gets a Ruby version")
}

// A Heroku launcher's options are skipped, whether or not they take a value,
// and a launcher with no document root serves the repository root.
func TestAHerokuPHPLaunchersOptionsAreNotTheDocumentRoot(t *testing.T) {
	root, ok := herokuPHPRoot("heroku-php-apache2 --verbose -F fpm.conf docs/")
	require.True(t, ok)
	require.Equal(t, "docs", root)

	root, ok = herokuPHPRoot("heroku-php-nginx")
	require.True(t, ok)
	require.Empty(t, root, "no argument means the repository root")
}

// A version read out of the repository is written into a plan, so anything
// that is not a plain version is refused rather than passed on.
func TestAVersionFileHoldsOnlyAVersion(t *testing.T) {
	require.True(t, safeVersion("20.11.1"))
	require.True(t, safeVersion("20.x"))
	require.False(t, safeVersion("20 --inspect"))
	require.False(t, safeVersion("lts/iron"))
	require.False(t, safeVersion("1.2.3.4.5.6.7.8.9"))
}

// Every planner writes its Dockerfile to .pando-plan/Dockerfile, and a checkout
// where that cannot be written fails as a build failure that says so, rather
// than leaving a plan that is not there.
func TestAPlanThatCannotBeWrittenIsABuildFailure(t *testing.T) {
	writers := map[string]func(string) (string, error){
		"committed program": func(dir string) (string, error) { return writeCommittedProgramPlan(dir, "./server") },
		"dotnet": func(dir string) (string, error) {
			return writeDotnetPlan(dir, dotnetBuild{Project: "A.csproj", Assembly: "A", Version: "8.0"})
		},
		"jvm": func(dir string) (string, error) { return writeJVMPlan(dir, jvmBuild{Tool: "maven", JDK: 21}) },
		"go":  func(dir string) (string, error) { return writeGoPlan(dir, goBuild{Version: "1.24", Package: "."}) },
		"node": func(dir string) (string, error) {
			return writeNodeServerPlan(dir, nodeServer{Node: "22", Install: "npm ci", Start: "npm start"})
		},
		"static site": func(dir string) (string, error) {
			return writeStaticSitePlan(dir, staticSite{Framework: "vite", OutDir: "dist", Install: "npm ci", Node: "22"})
		},
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			// .pando-plan is a file, so the directory cannot be made.
			blocked := writeFiles(t, map[string]string{planDir: ""})
			_, err := write(blocked)
			require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
			require.Equal(t, "Could not prepare the build.", errs.As(err).Message)

			// .pando-plan/Dockerfile is a directory, so the file cannot be written.
			occupied := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(occupied, planDir, "Dockerfile"), 0o755))
			_, err = write(occupied)
			require.Equal(t, errs.BuildFailed, errs.CodeOf(err))
		})
	}
}

// A single project file is read for its target framework and assembly. One
// that cannot be read, targets no framework, or names an assembly that could
// not be written plainly into a Dockerfile is left to Railpack.
func TestADotnetProjectIsReadOnlyWhenItSaysWhatItBuilds(t *testing.T) {
	named, ok := readDotnetBuild(writeFiles(t, map[string]string{
		"Api.fsproj": "<TargetFramework>net8.0</TargetFramework><AssemblyName>Shop.Web</AssemblyName>",
	}))
	require.True(t, ok)
	require.Equal(t, dotnetBuild{Project: "Api.fsproj", Assembly: "Shop.Web", Version: "8.0"}, named)

	_, ok = readDotnetBuild(writeFiles(t, map[string]string{"Api.csproj": "<TargetFrameworks>net8.0;net9.0</TargetFrameworks>"}))
	require.False(t, ok, "no single target framework")

	_, ok = readDotnetBuild(writeFiles(t, map[string]string{"My App.csproj": "<TargetFramework>net8.0</TargetFramework>"}))
	require.False(t, ok, "a project name with a space is not written into a command")

	unreadable := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(unreadable, "Api.csproj"), 0o755))
	_, ok = readDotnetBuild(unreadable)
	require.False(t, ok, "a directory named like a project is not one")

	out := t.TempDir()
	name, err := writeDotnetPlan(out, named)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(out, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "RUN dotnet publish Api.fsproj -c Release -o /out")
	require.Contains(t, string(body), `CMD ["exec dotnet Shop.Web.dll"]`)
}
