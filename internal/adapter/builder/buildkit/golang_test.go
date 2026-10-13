package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR095_AGoProgramIsBuiltOnTheGoItNames asserts R-095. nixpacks' Go could
// not download a toolchain newer than its own, and built without cgo
// (issue #55).
func TestR095_AGoProgramIsBuiltOnTheGoItNames(t *testing.T) {
	root := writeFiles(t, map[string]string{"go.mod": "module x\n\ngo 1.27\n", "main.go": "package main\n"})
	b, ok := readGoBuild(root)
	require.True(t, ok)
	require.Equal(t, goBuild{Version: "1.27", Package: "."}, b)

	cmd := writeFiles(t, map[string]string{"go.mod": "module x\ngo 1.24.2\n", "lib.go": "package x\n"})
	require.NoError(t, os.MkdirAll(filepath.Join(cmd, "cmd", "server"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cmd, "cmd", "server", "main.go"), []byte("package main\n"), 0o644))
	b, ok = readGoBuild(cmd)
	require.True(t, ok)
	require.Equal(t, goBuild{Version: "1.24", Package: "./cmd/server"}, b)

	_, ok = readGoBuild(writeFiles(t, map[string]string{"go.mod": "module x\ngo 1.24\n", "lib.go": "package x\n"}))
	require.False(t, ok, "a library has no main to build")

	declared := writeFiles(t, map[string]string{
		"go.mod": "module x\ngo 1.24\n", "main.go": "package main\n",
		"Makefile": "build:\n\tgo build -o bin/app .\n",
	})
	_, ok = readGoBuild(declared)
	require.False(t, ok, "a repository that says how it builds is built that way")

	name, err := writeGoPlan(root, b)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM golang:1.24 AS build")
	require.Contains(t, string(body), "go build -o /out/app ./cmd/server")
}

// A module without a go directive names no release to build on, and a main
// package found only in a test file is not a program. Both are left to
// Railpack.
func TestAGoModuleIsPlannedOnlyWithAReleaseAndARealMain(t *testing.T) {
	_, ok := readGoBuild(writeFiles(t, map[string]string{"go.mod": "module x\n", "main.go": "package main\n"}))
	require.False(t, ok, "no go directive")

	_, ok = readGoBuild(writeFiles(t, map[string]string{
		"go.mod": "module x\ngo 1.24\n", "lib.go": "package x\n", "main_test.go": "package main\n",
	}))
	require.False(t, ok, "a test file's package clause does not make a program")
}

// Heroku's PHP launcher exists only on Heroku; its argument is the document
// root (issue #55).
func TestR094_AHerokuPHPProcfileServesItsDocumentRoot(t *testing.T) {
	root, ok := herokuPHPRoot("heroku-php-apache2 web/")
	require.True(t, ok)
	require.Equal(t, "web", root)

	root, ok = herokuPHPRoot("heroku-php-nginx -C nginx.conf public")
	require.True(t, ok)
	require.Equal(t, "public", root)

	_, ok = herokuPHPRoot("php -S 0.0.0.0:8080")
	require.False(t, ok)

	dir := writeFiles(t, map[string]string{"composer.json": "{}", "Procfile": "web: heroku-php-apache2 web/\n"})
	require.Contains(t, railpackDefaults(dir), "RAILPACK_PHP_ROOT_DIR=/app/web")
	require.Equal(t, railpackPHPStart, declaredFor(api.BuildRequest{}, dir).Start)
}

// Django's static files are collected at build time, when there is somewhere
// to collect them to (issue #55).
func TestR095_ADjangoAppCollectsItsStaticFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{"manage.py": "#!/usr/bin/env python"})
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "site"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "site", "settings.py"), []byte("STATIC_ROOT = BASE_DIR / 'static'\n"), 0o644))
	require.True(t, djangoCollectsStatic(dir))

	bare := writeFiles(t, map[string]string{"manage.py": "#!/usr/bin/env python"})
	require.NoError(t, os.MkdirAll(filepath.Join(bare, "site"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(bare, "site", "settings.py"), []byte("DEBUG = True\n"), 0o644))
	require.False(t, djangoCollectsStatic(bare), "without a STATIC_ROOT the command fails")
}

// TestR094_AGoProgramStartsTheWayItsProcfileSays asserts R-094 (tier 2).
//
// The Procfile ran `chi-flag -addr 0.0.0.0:9090`; the plan ran the program
// with no flags, it listened on its default 127.0.0.1:9090, and the deploy
// refused it as reachable only from inside the container (issue #90). The
// Procfile names the program by the name `go build` gives it.
func TestR094_AGoProgramStartsTheWayItsProcfileSays(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"go.mod":   "module github.com/example/chi-flag\n\ngo 1.23\n",
		"main.go":  "package main\n",
		"Procfile": "web: chi-flag -addr 0.0.0.0:9090\n",
	})
	b, ok := readGoBuild(root)
	require.True(t, ok)
	require.Equal(t, goBuild{Version: "1.23", Package: ".", Program: "chi-flag", Start: "chi-flag -addr 0.0.0.0:9090"}, b)

	name, err := writeGoPlan(root, b)
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "COPY --from=build /out/app /usr/local/bin/chi-flag\n")
	require.Contains(t, string(body), `CMD ["exec chi-flag -addr 0.0.0.0:9090"]`)

	for procfile, want := range map[string]string{
		"web: bin/chi-flag --port 80":     "chi-flag --port 80", // where the Go buildpack puts it
		"web: ./chi-flag":                 "chi-flag",
		"web: something-else -addr :9090": "", // not a program this plan builds
		"web: go run . -addr :9090":       "",
		"web: ./start.sh && echo started": "./start.sh && echo started", // committed
	} {
		dir := writeFiles(t, map[string]string{
			"go.mod": "module github.com/example/chi-flag/v2\ngo 1.23\n", "main.go": "package main\n",
			"Procfile": procfile + "\n", "start.sh": "#!/bin/sh\n",
		})
		b, ok := readGoBuild(dir)
		require.True(t, ok)
		require.Equal(t, want, b.Start, procfile)
	}

	// A program in cmd/ is named for its directory.
	cmd := withFiles(t, map[string]string{
		"go.mod": "module example.com/tools\ngo 1.24\n", "cmd/api/main.go": "package main\n", "Procfile": "web: api --listen :8080\n",
	})
	b, ok = readGoBuild(cmd)
	require.True(t, ok)
	require.Equal(t, "api --listen :8080", b.Start)

	_, err = writeGoPlan(t.TempDir(), goBuild{Version: "1.24", Package: ".", Program: "x", Start: "x $(rm -rf /)"})
	require.Error(t, err, "a command that would need quoting is not written into a plan")

	// A Heroku-style Procfile reads the port Pando sets, and the plan runs it
	// under sh -c, which expands it.
	port := writeFiles(t, map[string]string{
		"go.mod": "module github.com/example/chi-flag\ngo 1.23\n", "main.go": "package main\n",
		"Procfile": "web: chi-flag -addr 0.0.0.0:$PORT\n",
	})
	b, ok = readGoBuild(port)
	require.True(t, ok)
	require.Equal(t, "chi-flag -addr 0.0.0.0:$PORT", b.Start)
	name, err = writeGoPlan(port, b)
	require.NoError(t, err)
	body, err = os.ReadFile(filepath.Join(port, name))
	require.NoError(t, err)
	require.Contains(t, string(body), `CMD ["exec chi-flag -addr 0.0.0.0:$PORT"]`)
}

// TestR094_ARailpackGoBuildMakesTheProgramItsProcfileRuns asserts R-094.
//
// miniflux's Procfile runs `miniflux.app`, the name Heroku's Go buildpack
// gives the module miniflux.app/v2. Railpack ran its Makefile's build, which
// writes `miniflux`, then the Procfile's command, and the app exited 127 with
// "miniflux.app: command not found" (issue #90).
func TestR094_ARailpackGoBuildMakesTheProgramItsProcfileRuns(t *testing.T) {
	root := writeFiles(t, map[string]string{
		"go.mod":   "module miniflux.app/v2\n\ngo 1.24\n",
		"main.go":  "package main\n",
		"Procfile": "web: miniflux.app\n",
		"Makefile": "build:\n\tgo build -o miniflux main.go\n",
	})
	d := declaredFor(api.BuildRequest{}, root)
	require.Equal(t, "make build && go build -o bin/miniflux.app .", d.Build)
	require.Equal(t, "/app/bin/miniflux.app", d.Start)

	// With no declared build, Railpack's own is replaced by one that writes
	// the program where the Procfile's command finds it.
	several := withFiles(t, map[string]string{
		"go.mod": "module example.com/shop\ngo 1.24\n", "cmd/shop/main.go": "package main\n", "cmd/other/main.go": "package main\n",
		"Procfile": "web: shop -port 8080\n",
	})
	d = declaredFor(api.BuildRequest{}, several)
	require.Equal(t, "go build -o bin/shop ./cmd/shop", d.Build)
	require.Equal(t, "/app/bin/shop -port 8080", d.Start)

	// An answered start command, a committed program, and a program the
	// module does not have are left as they are.
	require.Equal(t, "./run", declaredFor(api.BuildRequest{StartCommand: "./run"}, root).Start)
	committed := writeFiles(t, map[string]string{"go.mod": "module x\ngo 1.24\n", "main.go": "package main\n", "Procfile": "web: ./x\n", "x": ""})
	require.Empty(t, declaredFor(api.BuildRequest{}, committed).Build)
	other := writeFiles(t, map[string]string{"go.mod": "module x\ngo 1.24\n", "main.go": "package main\n", "Procfile": "web: gunicorn app:app\n"})
	require.Equal(t, "gunicorn app:app", declaredFor(api.BuildRequest{}, other).Start)
	require.Empty(t, declaredFor(api.BuildRequest{}, other).Build)
}
