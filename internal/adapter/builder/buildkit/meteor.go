package buildkit

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A Meteor app.
//
// Meteor builds with its own tool, not with npm: `npm start` in a Meteor
// project runs `meteor run`, the development server, and a plain Node image has
// no meteor command, so the app exited 127 with "meteor: not found"
// (issue #90). What Meteor deploys is the bundle `meteor build` writes — a
// Node program started with `node main.js` — and that is what this plans.
//
// The bundle needs MongoDB, which `meteor run` starts for development and a
// production server is given as MONGO_URL. Detection asks for it as a required
// value (R-132); Pando does not supply MongoDB.

// meteorRelease matches .meteor/release: "METEOR@3.0.4".
var meteorRelease = regexp.MustCompile(`^METEOR@(\d+(?:\.\d+){1,3}(?:-[0-9A-Za-z.]+)?)\s*$`)

// meteorSettings matches the settings file a start script passes.
var meteorSettings = regexp.MustCompile(`--settings(?:=|\s+)([^\s"';|&]+)`)

// meteorApp is what reading the repository said about a Meteor app.
type meteorApp struct {
	Release  string
	Install  string
	Settings string // the settings file the start script names, or ""
}

// readMeteorApp reports a Meteor app: a .meteor/release naming a release.
func readMeteorApp(contextDir string) (meteorApp, bool) {
	m := meteorRelease.FindStringSubmatch(strings.TrimSpace(readFile(contextDir, ".meteor/release")))
	if m == nil {
		return meteorApp{}, false
	}
	app := meteorApp{Release: m[1], Install: "meteor npm install"}
	if fileExists(contextDir, "package-lock.json") {
		app.Install = "meteor npm ci"
	}
	if pkg, ok := readPackageJSON(contextDir); ok {
		if s := meteorSettings.FindStringSubmatch(pkg.Scripts["start"]); s != nil &&
			safeRelative(s[1]) && fileExists(contextDir, s[1]) {
			app.Settings = s[1]
		}
	}
	return app, true
}

// meteorRuntimeImage is the image the bundle runs on. Its node is replaced
// with the one the Meteor release builds with, so the bundle's native modules
// and the runtime agree; the image supplies the libraries that node links.
const meteorRuntimeImage = "node:22-slim"

// writeMeteorPlan builds the server bundle with the app's Meteor release and
// runs it on that release's Node.
//
// ROOT_URL is required by Meteor and is only used to write absolute links; it
// is set to where the app listens, and an app that needs its public address
// sets it in the app's settings. The settings file the start script passes is
// given to the server as METEOR_SETTINGS, as `meteor run --settings` gives it.
func writeMeteorPlan(contextDir string, app meteorApp) (string, error) {
	settingsCopy, start := "", "exec node main.js"
	if app.Settings != "" {
		settingsCopy = fmt.Sprintf("COPY --from=build /app/%s /app/meteor-settings.json\n", app.Settings)
		start = `export METEOR_SETTINGS="$(cat /app/meteor-settings.json)"; ` + start
	}
	cmd, err := json.Marshal(start)
	if err != nil {
		return "", err
	}
	content := fmt.Sprintf(`# A Meteor %[1]s app, built into a Node bundle with Meteor.
FROM node:22-slim AS build
RUN apt-get update && apt-get install -y --no-install-recommends curl ca-certificates python3 g++ make git && \
    rm -rf /var/lib/apt/lists/*
ENV METEOR_ALLOW_SUPERUSER=1 METEOR_DISABLE_OPTIMISTIC_CACHING=1
RUN curl -fsSL "https://install.meteor.com/?release=%[1]s" | sh
WORKDIR /app
COPY . .
RUN %[2]s
RUN meteor build --server-only --directory /out
RUN cd /out/bundle/programs/server && meteor npm install --omit=dev
RUN cp "$(meteor node -e 'process.stdout.write(process.execPath)')" /out/node

FROM %[3]s
WORKDIR /app
COPY --from=build /out/node /usr/local/bin/node
COPY --from=build /out/bundle /app
%[4]sENV NODE_ENV=production PORT=3000 ROOT_URL=http://localhost:3000
EXPOSE 3000
ENTRYPOINT ["/bin/sh", "-c"]
CMD [%[5]s]
`, app.Release, app.Install, meteorRuntimeImage, settingsCopy, cmd)
	return writePlan(contextDir, content)
}
