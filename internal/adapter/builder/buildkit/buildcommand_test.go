package buildkit

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func workflow(steps string) string {
	return "name: CI\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n    steps:\n" +
		"      - uses: actions/checkout@v4\n" + steps
}

// TestR011_AWorkflowsTestStepsAreNotTheBuild asserts R-011: Pando does not
// test. A build job's test and lint steps are dropped from the build it
// mirrors, and what builds is kept.
func TestR011_AWorkflowsTestStepsAreNotTheBuild(t *testing.T) {
	root := withFiles(t, map[string]string{
		"go.mod": "module example.com/app\n\ngo 1.23\n",
		".github/workflows/ci.yml": workflow(
			"      - run: go vet ./...\n" +
				"      - run: go test -v ./...\n" +
				"      - run: go build -o bin/app .\n"),
	})
	d := readWorkflowBuild(root)
	require.Equal(t, rankWorkflow, d.Rank)
	require.Equal(t, "go build -o bin/app .", d.Build)

	for _, step := range []string{
		"npm test", "npm run test", "yarn lint", "pytest -q", "python -m pytest",
		"python manage.py test -v3", "./manage.py test", "cargo test --all", "cargo clippy",
		"bundle exec rspec", "npx vitest run", "golangci-lint run",
	} {
		require.True(t, testCommand.MatchString(step), step)
	}
	for _, step := range []string{"npm run build", "go build ./...", "cargo build --release", "python manage.py collectstatic"} {
		require.False(t, testCommand.MatchString(step), step)
	}
}

// TestR094_ACIRecipeIsNotADeclaredBuild asserts R-094's ladder: a workflow is
// the maintainer's build only when what it runs could be an image's build. One
// that ships with a deploy CLI, runs containers, curls a server or needs sudo
// or a database service is CI's own recipe, and the next source decides.
func TestR094_ACIRecipeIsNotADeclaredBuild(t *testing.T) {
	cases := map[string]string{
		"deploy CLI, docker and curl": workflow(
			"      - run: go test -v ./...\n" +
				"      - run: doctl apps dev build --no-cache\n" +
				"      - run: docker run -d -e PORT=8080 -p 8080:8080 web:dev\n" +
				"      - run: |\n          sleep 5\n          curl http://localhost:8080/ping\n"),
		"sudo": workflow(
			"      - run: |\n          pip install -r requirements.txt\n" +
				"      - run: |\n          sudo apt-get install -y postgresql-client\n          python manage.py migrate --noinput\n"),
		"nothing but tests": workflow("      - run: npm test\n"),
		"a database service": "name: CI\non: [push]\njobs:\n  build:\n    runs-on: ubuntu-latest\n" +
			"    services:\n      postgres:\n        image: postgres\n    steps:\n" +
			"      - run: pip install -r requirements.txt\n      - run: python manage.py migrate\n",
	}
	for name, wf := range cases {
		t.Run(name, func(t *testing.T) {
			root := withFiles(t, map[string]string{
				"go.mod":                   "module example.com/app\n\ngo 1.23\n",
				".github/workflows/ci.yml": wf,
			})
			require.False(t, readWorkflowBuild(root).found())
		})
	}
}

// TestR094_AMakefileThatBuildsOnlyDocumentsIsNotTheBuild asserts R-094: a
// build target whose whole job is typesetting documents is passed over, and a
// target that builds the app as well is not.
func TestR094_AMakefileThatBuildsOnlyDocumentsIsNotTheBuild(t *testing.T) {
	docs := "top = $(shell git rev-parse --show-toplevel)\n\n" +
		"all: book.epub book.pdf\n\n" +
		".PHONY: clean\nclean:\n\trm -f book.*\n\n" +
		"book.tmp:\n\tsed 's~^---~~' README.md > book.tmp\n\n" +
		"book.epub: book.tmp\n\tpandoc-from-markdown-to-epub book.tmp -o book.epub\n\n" +
		"book.pdf: book.tmp\n\tpandoc-from-markdown-to-pdf book.tmp -o book.pdf\n"
	d := parseMakefile(strings.NewReader(docs))
	require.False(t, d.declares(), "the EPUB is not the app")

	both := "all: app manual.pdf\n\napp:\n\tcargo build --release\n\nmanual.pdf:\n\tpandoc README.md -o manual.pdf\n"
	d = parseMakefile(strings.NewReader(both))
	require.Equal(t, "all", d.BuildTarget, "a target that builds the app too is still the build")

	build := "build:\n\tgo build -o out .\n"
	require.Equal(t, "build", parseMakefile(strings.NewReader(build)).BuildTarget)
}
