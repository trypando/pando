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
