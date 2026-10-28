package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// Issue #349: a Node server that keeps its page in public/ is misdetected as a
// static site. The static bid stays at 0.7 because the package.json only has a
// start script (no build), so none of the switch cases in StaticDetector.Bid
// adjust the confidence and the committed page in public/ wins over a real
// server.
func TestIssue349_NodeServerWithPublicDirIsNotStatic(t *testing.T) {
	// A hand-rolled http server (the issue's repro): start script, no build,
	// a page in public/.
	result, err := auction().Run(context.Background(), memSource{
		"package.json":      `{"scripts":{"start":"node server.js"},"main":"server.js"}`,
		"server.js":         "const http=require('http');http.createServer((q,r)=>{r.end('hi')}).listen(3000)",
		"public/index.html": "<!doctype html><h1>stars</h1>",
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy,
		"a package.json that starts a server means public/ holds its assets, not the whole site")
}

// The h5bp shape must keep working: a committed site with a package.json that
// has no start/main (only a build or nothing) stays static.
func TestIssue349_CommittedSiteWithoutStartStaysStatic(t *testing.T) {
	// h5bp-style: committed site in public/, package.json with no start/main.
	result, err := auction().Run(context.Background(), memSource{
		"public/index.html": "<!doctype html><h1>committed</h1>",
		"package.json":      `{"name":"site","dependencies":{"normalize.css":"^8"}}`,
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildStatic, result.Winner.Strategy,
		"a committed page with no start script is still a static site")
	require.NotEqual(t, detect.StrategyUnknown, result.Winner.Strategy)
}

// A start script beside docs/ is a library entry point, not a server omitting
// its site: an npm package that ships its docs site keeps a main and often a
// start script, and the docs/ index is the whole site.
func TestIssue349_StartScriptBesideDocsStaysStatic(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"docs/index.html": "<!doctype html><h1>library docs</h1>",
		"index.js":        "module.exports = {};",
		"package.json":    `{"name":"lib","main":"index.js","scripts":{"start":"node server.js"}}`,
		"server.js":       "require('./index.js')",
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildStatic, result.Winner.Strategy,
		"a start script beside docs/ does not make the docs a server asset")
}

// A committed build in dist/ next to a dev-server start (webpack serve,
// Docusaurus) is still the site: the demotion must not fire outside public/.
func TestIssue349_CommittedBuildBesideDevServerStartStaysStatic(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"dist/index.html": "<!doctype html><h1>built</h1>",
		"package.json":    `{"scripts":{"start":"webpack serve","build":"webpack"}}`,
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildStatic, result.Winner.Strategy,
		"a committed dist/ next to a dev-server start is still the site")
}

// A main of "./server.js" reads the same as "server.js" (issue #349 follow-up).
func TestIssue349_DotSlashMainIsAStartServer(t *testing.T) {
	result, err := auction().Run(context.Background(), memSource{
		"package.json":      `{"main":"./server.js"}`,
		"server.js":         "const http=require('http');http.createServer((q,r)=>{r.end('hi')}).listen(3000)",
		"public/index.html": "<!doctype html><h1>stars</h1>",
	})
	require.NoError(t, err)

	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy,
		"a ./-prefixed main pointing at a file is a start server")
}
