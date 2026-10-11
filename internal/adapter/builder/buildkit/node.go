package buildkit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Node servers are planned by Pando rather than by Railpack.
//
// nixpacks 1.41 installs Node from a pinned Nix package set whose newest 22 is
// 22.11 — which cannot require() an ES module, so Express and NestJS apps built
// and exited with ERR_REQUIRE_ESM — and which has no Node 24 at all, so asking
// for it failed every build with "undefined variable 'nodejs_24'" (issue #55).
// Installing Node through Nix is also the most memory-hungry step of a build,
// and several concurrent builds were killed for it. The official Node image
// carries current releases of every line and needs no Nix.

// nodeServer is what reading package.json said about a Node server.
type nodeServer struct {
	Node    string
	Install string
	Build   bool
	Start   string
}

// nodeEntryPoints are the files a Node server with no start script is started
// from, in the order Node developers reach for them.
var nodeEntryPoints = []string{"server.js", "index.js", "app.js", "main.js", "server.mjs", "index.mjs"}

// readNodeServer reports a Node app Pando knows how to build and start.
//
// A static site is not one (staticsite.go takes it first), and neither is a
// Bun project, which needs its own runtime and is left to Railpack. An app with
// no way to start is left to Railpack too, and detection asks for the start
// command.
func readNodeServer(contextDir, startCommand string) (nodeServer, bool) {
	body, err := os.ReadFile(filepath.Join(contextDir, "package.json"))
	if err != nil {
		return nodeServer{}, false
	}
	for _, bun := range []string{"bun.lockb", "bun.lock", "bunfig.toml"} {
		if _, err := os.Stat(filepath.Join(contextDir, bun)); err == nil {
			return nodeServer{}, false
		}
	}
	var pkg struct {
		Main       string            `json:"main"`
		Scripts    map[string]string `json:"scripts"`
		Engines    map[string]any    `json:"engines"`
		Workspaces json.RawMessage   `json:"workspaces"`
	}
	if json.Unmarshal(body, &pkg) != nil {
		return nodeServer{}, false
	}
	// A workspace root builds several packages, in an order Railpack and the
	// workspace tool know better than a generic plan does.
	if w := strings.TrimSpace(string(pkg.Workspaces)); w != "" && w != "null" && w != "[]" {
		return nodeServer{}, false
	}

	start := strings.TrimSpace(startCommand)
	switch {
	case start != "":
	case strings.TrimSpace(pkg.Scripts["start"]) != "":
		start = "npm start"
	case pkg.Main != "" && fileExists(contextDir, pkg.Main):
		start = "node " + pkg.Main
	default:
		for _, entry := range nodeEntryPoints {
			if fileExists(contextDir, entry) {
				start = "node " + entry
				break
			}
		}
	}
	if start == "" || strings.ContainsAny(start, "\n\r") {
		return nodeServer{}, false
	}

	return nodeServer{
		Node:    nodeMajor(contextDir, pkg.Engines),
		Install: installCommand(contextDir),
		Build:   strings.TrimSpace(pkg.Scripts["build"]) != "",
		Start:   start,
	}, true
}

func fileExists(dir, name string) bool {
	clean := filepath.Clean("/" + name)
	info, err := os.Stat(filepath.Join(dir, clean))
	return err == nil && !info.IsDir()
}

// writeNodeServerPlan builds and starts the app on the official Node image.
//
// Dependencies are installed with development dependencies included, because
// the build step usually needs them (TypeScript, a bundler), and NODE_ENV is set
// to production only for the running app.
func writeNodeServerPlan(contextDir string, n nodeServer) (string, error) {
	build := ""
	if n.Build {
		build = "RUN npm run build\n"
	}
	start, err := json.Marshal(n.Start)
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf(`# A Node server, built and run on Node %s.
FROM node:%s-slim
WORKDIR /app
COPY . .
RUN %s
%sENV NODE_ENV=production
ENTRYPOINT ["/bin/sh", "-c"]
CMD [%s]
`, n.Node, n.Node, n.Install, build, start)
	return writePlan(contextDir, content)
}
