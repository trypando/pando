package detect_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/detect"
)

// TestR021_WhatIsNotAWebAppIsRefusedByWhatTheRepositorySays asserts R-021's
// reading in issue #55: a library has nothing to run, and the honest answer is
// to say so. Issue #90 found four more that were deployed: Express (no start
// script), axios (a start script that runs its sandbox), logrus (whose doc
// comment shows an example `package main`), and an Expo app whose start script
// serves a phone, not a browser. Round 2 found an Electron desktop app whose
// start script opens a window.
func TestR021_WhatIsNotAWebAppIsRefusedByWhatTheRepositorySays(t *testing.T) {
	for name, tc := range map[string]struct {
		src  memSource
		says string
	}{
		"a node library with no start script": {
			src: memSource{
				"package.json": `{"name":"express","files":["index.js","lib/"],"scripts":{"test":"mocha"}}`,
				"index.js":     "module.exports = require('./lib/express');\n",
			},
			says: "library",
		},
		"a node library whose start script runs its sandbox": {
			src: memSource{
				"package.json": `{"name":"axios","main":"./dist/node/axios.cjs","types":"index.d.ts",` +
					`"exports":{".":"./index.js"},"scripts":{"start":"node ./sandbox/server.js"}}`,
				"sandbox/server.js": "require('http').createServer().listen(3000);\n",
			},
			says: "sandbox",
		},
		"a go library whose doc comment shows a main package": {
			src: memSource{
				"go.mod": "module github.com/sirupsen/logrus\n",
				"doc.go": "/*\nPackage logrus is a structured logger.\n\n  package main\n\n  func main() {}\n*/\n" +
					"package logrus\n",
				"logrus.go": "package logrus\n",
			},
			says: "library",
		},
		"an expo app that targets only phones": {
			src: memSource{
				"package.json": `{"name":"jellyfin-ios","private":true,"scripts":{"start":"expo start --dev-client"},` +
					`"dependencies":{"expo":"~52.0.0","react-native":"0.76.0","react-native-web":"0.19.0","react-dom":"18.3.1"}}`,
				"app.json": `{"expo":{"name":"Jellyfin","platforms":["ios"]}}`,
			},
			says: "mobile app",
		},
		"an electron app whose start script opens its window": {
			src: memSource{
				"package.json": `{"name":"electron-react-boilerplate","main":"./release/app/dist/main/main.js",` +
					`"scripts":{"build":"electron-vite build","start":"electron-vite dev --watch"},` +
					`"devDependencies":{"electron":"35.0.0","electron-vite":"4.0.0","react":"19.0.0"}}`,
			},
			says: "desktop app",
		},
		"an electron app started with electron .": {
			src: memSource{
				"package.json": `{"name":"notes","main":"main.js","scripts":{"start":"electron ."},` +
					`"devDependencies":{"electron":"35.0.0"}}`,
			},
			says: "Electron",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := detect.NewAuction(detect.BuildpackDetector{}).Run(context.Background(), tc.src)
			require.NoError(t, err)
			require.Equal(t, detect.StatusBlocked, result.Status)
			require.Contains(t, result.Blocked.Error(), tc.says)
		})
	}
}

// And the other direction, which matters more: a wrong refusal is a dead end,
// where a wrong guess is still caught by the trial.
func TestR021_AppsThatLookLikeLibrariesAreNotRefused(t *testing.T) {
	for name, src := range map[string]memSource{
		"an express app with npm init's main and no start script": {
			"package.json": `{"name":"api","main":"index.js","dependencies":{"express":"^4"}}`,
			"index.js":     "require('express')().listen(3000);\n",
		},
		"a published package whose start script runs its server": {
			"package.json": `{"name":"verdaccio","files":["build/"],"bin":{"verdaccio":"bin/verdaccio"},` +
				`"scripts":{"start":"node build/server.js"}}`,
		},
		"a next app that declares its types": {
			"package.json": `{"name":"site","types":"index.d.ts","dependencies":{"next":"15.0.0"}}`,
		},
		"an expo app with a web target": {
			"package.json": `{"name":"app","scripts":{"start":"expo start","web":"expo start --web"},` +
				`"dependencies":{"expo":"~52.0.0","react-native":"0.76.0","react-native-web":"0.19.0","react-dom":"18.3.1"}}`,
			"app.json": `{"expo":{"name":"App","platforms":["ios","android","web"]}}`,
		},
		"a web server that also packages an electron build": {
			"package.json": `{"name":"web","scripts":{"start":"node server.js","desktop":"electron ."},` +
				`"dependencies":{"express":"^4"},"devDependencies":{"electron":"35.0.0"}}`,
			"server.js": "require('express')().listen(3000);\n",
		},
		"a go app beside a doc comment": {
			"go.mod":  "module example.com/server\n",
			"doc.go":  "// Package main serves the site.\npackage main\n",
			"main.go": "/* the entry point */ package main\n",
		},
		"a flask app in a src layout that ships its templates": {
			"pyproject.toml":                           "[project]\nname = \"flask-example-celery\"\ndependencies = [\"flask\"]\n",
			"requirements.txt":                         "flask\n",
			"src/task_app/__init__.py":                 "from flask import Flask\n",
			"src/task_app/templates/index.html":        "<h1>Celery Example</h1>\n",
			"make_celery.py":                           "from task_app import create_app\n",
			"src/task_app/views.py":                    "",
			"src/task_app/tasks.py":                    "",
			"src/task_app/templates/partials/row.html": "",
		},
	} {
		t.Run(name, func(t *testing.T) {
			result, err := detect.NewAuction(detect.BuildpackDetector{}).Run(context.Background(), src)
			require.NoError(t, err)
			require.NotEqual(t, detect.StatusBlocked, result.Status, "%v", result.Blocked)
		})
	}
}
