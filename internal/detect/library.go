package detect

import (
	"encoding/json"
	"io"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// nodeLibraryReason says why a package.json describes something Pando cannot
// host — a library published to npm, or a mobile app — or nil.
//
// Both were deployed as web apps (issue #90). Express and axios are the
// libraries every Node app is built from; Express has no start script, and
// axios's starts a sandbox that exercises the client. An Expo app's start
// script is Metro, the bundler that serves a phone its development build.
//
// Conservative on purpose, because a wrong refusal is worse than a wrong
// guess the trial can still catch: a library is one that says what it
// publishes (files, exports or types) and starts nothing of its own, and a
// framework an app is built with is never taken for one.
func nodeLibraryReason(src api.SourceView) error {
	var pkg packageManifest
	if !readJSON(src, "package.json", &pkg) {
		return nil
	}

	if pkg.depends("react-native", "expo") && !hasWebTarget(src, pkg) {
		return errs.New(errs.PlanCapabilityUnsupported,
			"This repository is a mobile app built with React Native or Expo: it depends on react-native "+
				"or expo and does not build for the web — its Expo configuration lists no web platform, or "+
				"nothing in it builds or serves a web version. It runs on a phone, and its start script "+
				"serves a development build to one rather than a web page to a browser. Pando deploys web "+
				"apps.").
			WithRemedy("If the app has a web version, add react-native-web and a build that exports it, " +
				"or point Pando at the directory that holds the web app.")
	}

	if !pkg.publishes() || pkg.depends(appFrameworks...) {
		return nil
	}
	start := strings.TrimSpace(pkg.Scripts["start"])
	switch {
	case start == "":
		return errs.New(errs.PlanCapabilityUnsupported,
			"This repository is a Node.js library published to npm: its package.json says which files "+
				"it publishes and has no start script, so there is no program in it to run. Pando deploys "+
				"apps, and a library is used by other programs rather than run on its own.").
			WithRemedy("If an app in this repository uses the library, set the app's subdirectory to that " +
				"app's directory, or add a Dockerfile that builds it.")
	case startsAnExample.MatchString(start):
		return errs.Newf(errs.PlanCapabilityUnsupported,
			"This repository is a Node.js library published to npm: its package.json says which files "+
				"it publishes, and its start script, %q, only runs an example or a sandbox that exercises "+
				"the library. Pando deploys apps, and a library is used by other programs rather than run "+
				"on its own.", start).
			WithRemedy("If an app in this repository uses the library, set the app's subdirectory to that " +
				"app's directory, or add a Dockerfile that builds it.")
	}
	return nil
}

// hasWebTarget reports whether a React Native or Expo app also builds for the
// browser.
//
// An Expo app's configuration says which platforms it targets, and when it
// says, that is the answer: Jellyfin's lists only iOS, although its
// package.json carries react-native-web like every Expo template's does (issue
// #90). Without that, a script that serves or exports the web build, or a
// web framework beside it, is the evidence.
func hasWebTarget(src api.SourceView, pkg packageManifest) bool {
	var app struct {
		Expo struct {
			Platforms []string `json:"platforms"`
		} `json:"expo"`
	}
	if readJSON(src, "app.json", &app) && len(app.Expo.Platforms) > 0 {
		for _, p := range app.Expo.Platforms {
			if p == "web" {
				return true
			}
		}
		return false
	}
	if pkg.depends("next", "react-native-web") {
		return true
	}
	for name, script := range pkg.Scripts {
		if name == "web" || strings.Contains(script, "--web") || strings.Contains(script, "expo export") {
			return true
		}
	}
	return false
}

// startsAnExample matches a start script that runs something kept beside a
// library to exercise it rather than an app: axios's `node ./sandbox/server.js`.
var startsAnExample = regexp.MustCompile(`(^|[\s"'=./])(examples?|sandbox|demos?|tests?|playground|benchmarks?)/`)

// appFrameworks are dependencies only an app has. A package that depends on
// one and also says what it publishes is an app that happens to be published.
var appFrameworks = []string{
	"next", "nuxt", "@sveltejs/kit", "astro", "gatsby", "react-scripts", "@angular/core",
	"@remix-run/node", "@remix-run/serve", "@nestjs/core", "@redwoodjs/core", "vitepress",
	"@docusaurus/core",
}

type packageManifest struct {
	Private         bool              `json:"private"`
	Files           []string          `json:"files"`
	Exports         json.RawMessage   `json:"exports"`
	Types           string            `json:"types"`
	Typings         string            `json:"typings"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

// publishes reports a package.json that says what it gives other programs: the
// files npm publishes, the entry points it exports, or its type declarations.
// An app has no reason to say any of it.
func (p packageManifest) publishes() bool {
	if p.Private {
		return false
	}
	exports := strings.TrimSpace(string(p.Exports))
	return len(p.Files) > 0 || (exports != "" && exports != "null") || p.Types != "" || p.Typings != ""
}

func (p packageManifest) depends(names ...string) bool {
	for _, name := range names {
		if _, ok := p.Dependencies[name]; ok {
			return true
		}
		if _, ok := p.DevDependencies[name]; ok {
			return true
		}
	}
	return false
}

// isPythonWebAppPackage reports a src-layout package that is an app all the
// same: one that ships templates or static files, or whose pyproject says it
// is never to be uploaded to PyPI.
//
// Flask's own Celery example keeps its app in src/task_app with its templates
// beside it, and is started with `flask -A task_app run`; it was refused as a
// library (issue #90). A library ships code to import, not pages to serve.
func isPythonWebAppPackage(src api.SourceView) bool {
	for _, pattern := range []string{"src/*/templates/*", "src/*/static/*"} {
		if found, err := src.Glob(pattern); err == nil && len(found) > 0 {
			return true
		}
	}
	f, err := src.Open("pyproject.toml")
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, 256<<10))
	return err == nil && strings.Contains(string(raw), "Private :: Do Not Upload")
}
