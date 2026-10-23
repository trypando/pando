package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// linkedCheckout is a checkout holding a link named name that leads, as a
// relative path, to a directory outside it, and returns both directories.
// Nothing confined the links, which is the point: everything this builder
// writes must refuse such a link by itself.
func linkedCheckout(t *testing.T, name string) (checkout, outside string) {
	t.Helper()
	checkout, outside = t.TempDir(), t.TempDir()
	rel, err := filepath.Rel(checkout, outside)
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(filepath.Join(checkout, name)), 0o755))
	require.NoError(t, os.Symlink(rel, filepath.Join(checkout, name)))
	return checkout, outside
}

// requireUntouched asserts nothing was written into dir.
func requireUntouched(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "nothing is written outside the checkout")
}

// TestR020_NothingIsWrittenThroughALinkOutOfTheCheckout asserts R-020 for
// every build input this builder writes into a checkout: a link the
// repository carries cannot carry the write onto the host.
func TestR020_NothingIsWrittenThroughALinkOutOfTheCheckout(t *testing.T) {
	t.Run("generated files", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, "x")
		err := writeInto(checkout, map[string]string{"x/tmp/railpack/mise": "#!/bin/sh\n"})
		require.Error(t, err)
		requireUntouched(t, outside)
	})
	t.Run("a written plan", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, planDir)
		_, err := writePlan(checkout, "FROM scratch\n")
		require.Error(t, err)
		_, err = writeDotnetPlan(checkout, dotnetBuild{Version: "8.0", Project: "a.csproj", Assembly: "a"})
		require.Error(t, err)
		_, err = writeStaticSitePlan(checkout, staticSite{Framework: "vite", Node: "22", Install: "npm ci", Build: "npm run build", OutDir: "dist"})
		require.Error(t, err)
		_, err = writeJekyllPlan(checkout, jekyllSite{Ruby: "3.3", Config: "_config.yml"})
		require.Error(t, err)
		requireUntouched(t, outside)
	})
	t.Run("the plan's ignore file", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, planDir)
		require.Error(t, writePlanIgnore(checkout, filepath.Join(planDir, "Dockerfile")))
		requireUntouched(t, outside)
	})
	t.Run("a Railpack plan", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, planDir)
		_, err := runRailpack(checkout, nil)
		require.Error(t, err)
		require.Error(t, recordSteers(checkout, []steer{{Key: "k", Why: "w"}}))
		require.Error(t, editPlan(checkout, filepath.Join(planDir, railpackPlanFile),
			[]steer{{Key: "k", Edit: func(map[string]any) {}}}))
		requireUntouched(t, outside)
	})
	t.Run("a Ruby version", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, "dir")
		require.NoError(t, os.WriteFile(filepath.Join(checkout, "Gemfile"), []byte("source 'x'\n"), 0o644))
		require.NoError(t, os.Symlink(filepath.Join("dir", "ruby"), filepath.Join(checkout, ".ruby-version")))
		ensureRubyVersion(checkout)
		requireUntouched(t, outside)
	})
	t.Run("a pinned PHP manifest", func(t *testing.T) {
		checkout, outside := linkedCheckout(t, "dir")
		manifest := []byte(`{"require":{"php":">=8.1"}}`)
		require.NoError(t, os.WriteFile(filepath.Join(outside, "composer.json"), manifest, 0o644))
		require.NoError(t, os.Symlink(filepath.Join("dir", "composer.json"), filepath.Join(checkout, phpComposerJSON)))
		pinRailpackPHP(checkout)()
		body, err := os.ReadFile(filepath.Join(outside, "composer.json"))
		require.NoError(t, err)
		require.Equal(t, manifest, body, "a manifest outside the checkout is neither read nor rewritten")
	})
}

// TestR020_ALinkInsideTheCheckoutStillTakesAWrite asserts the other half of
// R-020's confinement: a link that stays inside the checkout is followed as
// it always was, so a repository that has one still builds.
func TestR020_ALinkInsideTheCheckoutStillTakesAWrite(t *testing.T) {
	checkout := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "real"), 0o755))
	require.NoError(t, os.Symlink("real", filepath.Join(checkout, "alias")))
	require.NoError(t, writeInto(checkout, map[string]string{"alias/generated.txt": "x"}))
	require.FileExists(t, filepath.Join(checkout, "real", "generated.txt"))
}

// TestR020_ABuildReadsNothingOutsideTheCheckout asserts R-020 for what a
// build is given to read: its context and its Dockerfile are checked where
// they resolve, not as text.
func TestR020_ABuildReadsNothingOutsideTheCheckout(t *testing.T) {
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "Dockerfile"), []byte("FROM scratch\n"), 0o644))
	checkout := t.TempDir()
	rel, err := filepath.Rel(checkout, outside)
	require.NoError(t, err)
	require.NoError(t, os.Symlink(rel, filepath.Join(checkout, "ctx")))
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "app", "docker"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(checkout, "app", "docker", "Dockerfile.debian"), []byte("FROM scratch\n"), 0o644))
	require.NoError(t, os.Symlink(filepath.Join("..", rel, "Dockerfile"), filepath.Join(checkout, "app", "Escape.Dockerfile")))
	require.NoError(t, os.Symlink(filepath.Join("docker", "Dockerfile.debian"), filepath.Join(checkout, "app", "Dockerfile")))

	within := func(sub string) api.SourceView {
		return checkoutView{view{root: filepath.Join(checkout, sub)}, checkout}
	}

	for name, req := range map[string]api.BuildRequest{
		"a subdirectory that is a link out": {Source: within("ctx"), Strategy: spec.BuildBuildpack},
		"a context that is a link out":      {Source: within(""), Context: "ctx", Strategy: spec.BuildBuildpack},
		"a Dockerfile that is a link out":   {Source: within("app"), Strategy: spec.BuildDockerfile, Dockerfile: "Escape.Dockerfile"},
	} {
		_, err := sourceDir(req)
		require.Error(t, err, name)
		require.Equal(t, errs.BuildFailed, errs.CodeOf(err), name)
	}

	dir, err := sourceDir(api.BuildRequest{Source: within("app"), Strategy: spec.BuildDockerfile})
	require.NoError(t, err, "a Dockerfile linked to another file inside the checkout still builds")
	require.Equal(t, filepath.Join(checkout, "app"), dir)

	_, err = sourceDir(api.BuildRequest{Source: within("app"), Strategy: spec.BuildDockerfile, Dockerfile: "Missing"})
	require.NoError(t, err, "a missing Dockerfile is left for the build to report")

	_, err = sourceDir(api.BuildRequest{Source: view{root: filepath.Join(checkout, "ctx")}, Strategy: spec.BuildBuildpack})
	require.NoError(t, err, "a view that does not name its checkout is checked against itself")
}

// TestR020_ABuildContextBesideTheSubdirectoryIsTheOneNamed asserts R-020's
// rule for ".." in a build context: relative to the subdirectory and inside
// the repository, it names the sibling it says; past the repository, as text
// or in a Dockerfile, it is refused.
func TestR020_ABuildContextBesideTheSubdirectoryIsTheOneNamed(t *testing.T) {
	checkout := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "services", "web"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(checkout, "shared"), 0o755))
	src := checkoutView{view{root: filepath.Join(checkout, "services")}, checkout}

	dir, err := sourceDir(api.BuildRequest{Source: src, Context: "../shared", Strategy: spec.BuildBuildpack})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(checkout, "shared"), dir)

	for name, req := range map[string]api.BuildRequest{
		"a context past the repository":    {Source: src, Context: "../../..", Strategy: spec.BuildBuildpack},
		"a Dockerfile past the repository": {Source: src, Context: "web", Strategy: spec.BuildDockerfile, Dockerfile: "../../../etc/passwd"},
	} {
		_, err := sourceDir(req)
		require.Error(t, err, name)
		require.Equal(t, errs.BuildFailed, errs.CodeOf(err), name)
	}
}

// checkoutView is a view rooted below the checkout it belongs to.
type checkoutView struct {
	view
	checkout string
}

func (v checkoutView) CheckoutRoot() string { return v.checkout }

func TestConfinedReportsACheckoutItCannotRead(t *testing.T) {
	require.Error(t, confined(filepath.Join(t.TempDir(), "gone"), t.TempDir(), "build context"))
}

func TestContextHelpersReportADirectoryThatIsNotThere(t *testing.T) {
	gone := filepath.Join(t.TempDir(), "gone")
	require.Error(t, writeContextFile(gone, "a", nil))
	require.Error(t, mkdirContext(gone, "a"))
	require.Error(t, removeContextFile(gone, "a"))
	_, err := readContextFile(gone, "a")
	require.Error(t, err)
	require.False(t, statContext(gone, "a"))
}
