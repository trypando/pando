package buildkit

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestR095_ANodeServerIsBuiltOnTheOfficialNodeImage asserts R-095.
//
// nixpacks' Nix Node is 22.11, which cannot require() an ES module, and it has
// no Node 24 at all; every Node build failed one way or the other (issue #55).
func TestR095_ANodeServerIsBuiltOnTheOfficialNodeImage(t *testing.T) {
	cases := map[string]struct {
		files  map[string]string
		answer string
		want   nodeServer
	}{
		"start script": {
			files: map[string]string{"package.json": `{"scripts":{"start":"node server.js"}}`, "package-lock.json": "{}"},
			want:  nodeServer{Node: "22", Install: "npm ci", Start: "npm start"},
		},
		"typescript with a build": {
			files: map[string]string{"package.json": `{"scripts":{"build":"tsc","start":"node dist/main.js"},"engines":{"node":"20"}}`, "pnpm-lock.yaml": ""},
			want:  nodeServer{Node: "20", Install: "npm install -g pnpm@9 && pnpm install --frozen-lockfile", Build: true, Start: "npm start"},
		},
		"no start script, an index.js": {
			files: map[string]string{"package.json": `{"name":"x"}`, "index.js": ""},
			want:  nodeServer{Node: "22", Install: "npm install", Start: "node index.js"},
		},
		"no start script, a main that exists": {
			files: map[string]string{"package.json": `{"main":"run.js"}`, "run.js": "", "index.js": ""},
			want:  nodeServer{Node: "22", Install: "npm install", Start: "node run.js"},
		},
		"an answered start command": {
			files:  map[string]string{"package.json": `{"name":"x"}`},
			answer: "node src/index.js",
			want:   nodeServer{Node: "22", Install: "npm install", Start: "node src/index.js"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got, ok := readNodeServer(writeFiles(t, tc.files), tc.answer)
			require.True(t, ok)
			require.Equal(t, tc.want, got)
		})
	}

	for name, files := range map[string]map[string]string{
		"nothing to start": {"package.json": `{"name":"x"}`},
		"bun":              {"package.json": `{"scripts":{"start":"bun run x"}}`, "bun.lockb": ""},
		"workspace root":   {"package.json": `{"workspaces":["apps/*"],"scripts":{"start":"x"}}`},
		"no package.json":  {"index.js": ""},
		"malformed":        {"package.json": `{"scripts":`, "index.js": ""},
	} {
		t.Run(name, func(t *testing.T) {
			_, ok := readNodeServer(writeFiles(t, files), "")
			require.False(t, ok)
		})
	}
}

func TestANodeServerPlanBuildsThenStartsThroughAShell(t *testing.T) {
	root := t.TempDir()
	name, err := writeNodeServerPlan(root, nodeServer{Node: "22", Install: "npm ci", Build: true, Start: "npm start"})
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(root, name))
	require.NoError(t, err)
	require.Contains(t, string(body), "FROM node:22-slim")
	require.Contains(t, string(body), "RUN npm run build")
	require.Contains(t, string(body), `CMD ["npm start"]`)
}

// TestR095_ANodeServerWhoseScriptsCallPnpmGetsPnpm asserts R-095.
//
// The Node image has npm and not pnpm. A start script that ran `pnpx srvx`
// in a project with no pnpm lockfile exited 127 with "pnpx: not found"
// (issue #90).
func TestR095_ANodeServerWhoseScriptsCallPnpmGetsPnpm(t *testing.T) {
	got, ok := readNodeServer(writeFiles(t, map[string]string{
		"package.json": `{"scripts":{"build":"vite build","start":"pnpx srvx --prod dist/server/server.js"}}`,
	}), "")
	require.True(t, ok)
	require.Equal(t, "npm install -g pnpm@9 && npm install", got.Install)

	got, ok = readNodeServer(writeFiles(t, map[string]string{
		"package.json": `{"scripts":{"start":"node server.js"}}`, "pnpm-lock.yaml": "",
	}), "")
	require.True(t, ok)
	require.Equal(t, "npm install -g pnpm@9 && pnpm install --frozen-lockfile", got.Install, "installed already")

	got, ok = readNodeServer(writeFiles(t, map[string]string{
		"package.json": `{"scripts":{"start":"node mypnpmx.js"}}`, "package-lock.json": "",
	}), "")
	require.True(t, ok)
	require.Equal(t, "npm ci", got.Install, "a name that only contains pnpm is not pnpm")
}
