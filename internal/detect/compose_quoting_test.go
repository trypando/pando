package detect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// A compose command keeps its quoting. It was split on whitespace, so
// `sh -c "a && b"` reached the shell as `sh -c "a` and the service died with
// "unexpected EOF while looking for matching" (issue #55).
func TestR096_AComposeCommandKeepsItsQuoting(t *testing.T) {
	const file = `
services:
  web:
    image: node:22-alpine
    command: sh -c "npm run migrate && npm start"
    entrypoint: ["/bin/sh", "-c"]
    ports: ["3000:3000"]
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	require.Len(t, draft.Workloads, 1)
	require.Equal(t, []string{"sh", "-c", "npm run migrate && npm start"}, draft.Workloads[0].Command)
	require.Equal(t, []string{"/bin/sh", "-c"}, draft.Workloads[0].Entrypoint)
}

// The port traffic goes to is the web one. Gitea's compose file lists SSH and
// HTTP, and the app was routed to port 22 (issue #55).
func TestR097_AComposeServiceIsRoutedToItsWebPortNotSSH(t *testing.T) {
	const file = `
services:
  gitea:
    image: gitea/gitea:1
    ports:
      - "2221:22"
      - "3000:3000"
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	ports := draft.Workloads[0].Ports
	require.Len(t, ports, 2)
	require.Equal(t, 3000, ports[0].Number)
	require.Equal(t, "http", ports[0].Protocol)
	require.Equal(t, 22, ports[1].Number)
	require.Equal(t, "tcp", ports[1].Protocol)
}

// TestR096_AServiceReachedByHostnameIsImportedAsWritten asserts R-096.
//
// `REDIS_HOST: redis` reaches the compose Redis by name, with no password. It
// was replaced by a provisioned Redis under another name that wanted one, and
// REDIS_HOST was handed the whole DSN — which Python then tried to resolve as a
// hostname: "label too long" (issue #55). With nothing to rewire, the compose
// file is imported as it was written.
func TestR096_AServiceReachedByHostnameIsImportedAsWritten(t *testing.T) {
	const file = `
services:
  web:
    build: ./web
    ports: ["8000:8000"]
    environment:
      REDIS_HOST: redis
    depends_on: [redis]
  redis:
    image: redis:7-alpine
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	require.Empty(t, draft.Slots)

	names := map[string]bool{}
	for _, w := range draft.Workloads {
		names[w.Name] = true
		if w.Name == "web" {
			require.Equal(t, []string{"redis"}, w.DependsOn)
			require.Equal(t, "redis", *w.Env[0].Value, "the hostname is left alone")
		}
	}
	require.True(t, names["redis"], "the compose Redis runs, under the name the app uses")
}

// A connection URL is what makes the swap safe: it is rewired, credentials and
// all. A JDBC URL is not one Pando's provisioned URL can stand in for.
func TestOnlyAMatchingConnectionURLTurnsAServiceIntoASlot(t *testing.T) {
	const file = `
services:
  api:
    image: example/api
    environment:
      SPRING_DATASOURCE_URL: jdbc:postgresql://db:5432/example
  db:
    image: postgres:16
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	require.Empty(t, draft.Slots)
	require.Len(t, draft.Workloads, 2)
}

// A compose file of nothing but databases is what the app needs, not the app.
// Spring Petclinic's was taken as the app and deployed two databases (issue
// #55); the build beside it wins now.
func TestR096_AComposeFileOfOnlyDatabasesDoesNotOutbidTheApp(t *testing.T) {
	src := memSource{
		"docker-compose.yml": "services:\n  mysql:\n    image: mysql:9\n  postgres:\n    image: postgres:18\n",
		"pom.xml":            "<project/>",
	}
	result, err := detect.NewAuction(detect.ComposeDetector{}, detect.BuildpackDetector{}).Run(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, spec.BuildBuildpack, result.Winner.Strategy)
}

// TestR096_AComposeSecretIsPlacedWhereComposePutsIt asserts R-096. It was
// dropped, and a Postgres told POSTGRES_PASSWORD_FILE never initialized
// (issue #55).
func TestR096_AComposeSecretIsPlacedWhereComposePutsIt(t *testing.T) {
	const file = `
services:
  db:
    image: postgres
    secrets:
      - db-password
    environment:
      POSTGRES_PASSWORD_FILE: /run/secrets/db-password
secrets:
  db-password:
    file: db/password.txt
`
	draft, err := detect.ImportCompose(memSource{"compose.yaml": file, "db/password.txt": "hunter2\n"}, "compose.yaml")
	require.NoError(t, err)
	require.Len(t, draft.Workloads, 1)
	require.Contains(t, draft.Workloads[0].Files, spec.File{Path: "/run/secrets/db-password", Content: "hunter2\n"})
}

// `$$` is compose's escape for `$`. Passed on doubled, the shell read it as its
// own process ID, and a health check reading a password file ran as
// `--password="1234(cat …)"` (issue #55).
func TestR096_ADoubledDollarIsALiteralDollar(t *testing.T) {
	const file = `
services:
  db:
    image: mariadb:10
    command: sh -c 'echo $$HOME'
    healthcheck:
      test: ['CMD-SHELL', 'mysqladmin ping --password="$$(cat /run/secrets/db-password)"']
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	w := draft.Workloads[0]
	require.Equal(t, []string{"sh", "-c", "echo $HOME"}, w.Command)
	require.Equal(t, []string{"sh", "-c", `mysqladmin ping --password="$(cat /run/secrets/db-password)"`}, w.Health.Command)
}

// TestR096_ACommandKeepsAVariableNothingSets asserts R-096: a command naming a
// variable the repository does not set reads it when the workload starts, from
// the slot the person fills. Substituted away, docuseal's caddy ran
// `caddy reverse-proxy --from --to app:3000`, exited, and took the app with it
// (issue #90).
func TestR096_ACommandKeepsAVariableNothingSets(t *testing.T) {
	const file = `
services:
  app:
    image: docuseal/docuseal:latest
    environment:
      - FORCE_SSL=${HOST}
  caddy:
    image: caddy:latest
    command: caddy reverse-proxy --from $HOST --to app:3000
    environment:
      - HOST=${HOST}
  worker:
    image: example/worker
    command: ["run", "--name", "a b", "--region=${REGION}", "--cost=$$5"]
  echo:
    image: busybox
    command: sh -c 'echo $$HOME'
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	byName := map[string]spec.Workload{}
	for _, w := range draft.Workloads {
		byName[w.Name] = w
	}

	caddy := byName["caddy"]
	require.Equal(t, []string{"sh", "-c", `caddy reverse-proxy --from "${HOST}" --to app:3000`}, caddy.Command)
	require.Len(t, caddy.Env, 1)
	require.Equal(t, "HOST", *caddy.Env[0].SlotRef)

	// A variable the environment does not name is added to it, as a slot.
	worker := byName["worker"]
	require.Equal(t, []string{"sh", "-c", `run --name "a b" "--region=${REGION}" "--cost=\$5"`}, worker.Command)
	require.Equal(t, "REGION", worker.Env[0].Key)
	require.Equal(t, "REGION", *worker.Env[0].SlotRef)
	var slots []string
	for _, s := range draft.Slots {
		if s.Required {
			slots = append(slots, s.Key)
		}
	}
	require.Contains(t, slots, "HOST")
	require.Contains(t, slots, "REGION")

	// A command with nothing unset is imported as before.
	require.Equal(t, []string{"sh", "-c", "echo $HOME"}, byName["echo"].Command)
}

// A service's build stage and arguments reach its build. Neither did, and a
// service meant to build `target: development` with NODE_ENV=development was
// built as production, without the dev server its command ran (issue #55).
func TestR096_AComposeBuildsTargetAndArgumentsAreImported(t *testing.T) {
	const file = `
services:
  backend:
    build:
      context: backend
      target: development
      args:
        - NODE_ENV=development
    command: npm run start-watch
  worker:
    build:
      context: .
      args:
        GO_VERSION: "1.24"
        FROM_SHELL:
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)

	byName := map[string]spec.Workload{}
	for _, w := range draft.Workloads {
		byName[w.Name] = w
	}
	require.Equal(t, "development", byName["backend"].Build.Target)
	require.Equal(t, []spec.KV{{Key: "NODE_ENV", Value: "development"}}, byName["backend"].Build.Args)
	require.Equal(t, []spec.KV{{Key: "GO_VERSION", Value: "1.24"}}, byName["worker"].Build.Args,
		"an argument compose would take from the shell is left out")
}

// TestR096_AMountedRepositoryDirectoryKeepsItsFiles asserts R-096 and R-020. A
// repository directory mounted into a service became an empty volume, so the
// voting app's health check scripts were "not found" and its database never
// went healthy (issue #55).
func TestR096_AMountedRepositoryDirectoryKeepsItsFiles(t *testing.T) {
	const file = `
services:
  db:
    image: postgres:15-alpine
    volumes:
      - ./healthchecks:/healthchecks
      - ./data:/var/lib/postgresql/data
    healthcheck:
      test: /healthchecks/postgres.sh
  frontend:
    build:
      context: frontend
    volumes:
      - ./frontend/src:/code/src
  vote:
    build:
      context: vote
      target: dev
    volumes:
      - ./vote:/usr/local/app
`
	draft, err := detect.ImportCompose(memSource{
		"compose.yml":              file,
		"healthchecks/postgres.sh": "#!/bin/sh\npg_isready\n",
		"healthchecks/lib/util.sh": "true\n",
		"data/.gitkeep":            "",
		"frontend/Dockerfile":      "FROM node:22\n",
		"frontend/src/index.js":    "console.log(1)\n",
		"frontend/src/bundle.js":   strings.Repeat("x", spec.FileSizeLimit+1),
		"vote/Dockerfile":          "FROM python:3.11-slim AS dev\nCMD [\"python\", \"app.py\"]\n",
		"vote/app.py":              "print('vote')\n",
	}, "compose.yml")
	require.NoError(t, err)

	byName := map[string]spec.Workload{}
	for _, w := range draft.Workloads {
		byName[w.Name] = w
	}
	require.ElementsMatch(t, []spec.File{
		{Path: "/healthchecks/postgres.sh", Content: "#!/bin/sh\npg_isready\n", Mode: 0o755},
		{Path: "/healthchecks/lib/util.sh", Content: "true\n"},
	}, byName["db"].Files, "the scripts travel in the spec, the first one executable")
	require.Len(t, byName["db"].Mounts, 1, "a directory holding only a placeholder is still data")
	require.Equal(t, "/var/lib/postgresql/data", byName["db"].Mounts[0].Path)

	require.Empty(t, byName["frontend"].Mounts, "source too big to carry is left to the image it was built into")
	require.Empty(t, byName["frontend"].Files)

	// A development stage that copies nothing gets its source from the mount.
	require.Contains(t, byName["vote"].Files, spec.File{Path: "/usr/local/app/app.py", Content: "print('vote')\n"})
	require.Empty(t, byName["vote"].Mounts)
	for _, v := range draft.Volumes {
		require.NotContains(t, v.Name, "healthchecks")
		require.NotContains(t, v.Name, "src")
	}
}

// TestR200_AnAnonymousVolumeBelongsToItsService asserts R-200. Two services
// keeping node_modules in anonymous volumes shared one, and the frontend ran
// with the backend's packages: "react-scripts: not found" (issue #55).
func TestR200_AnAnonymousVolumeBelongsToItsService(t *testing.T) {
	const file = `
services:
  frontend:
    image: example/frontend
    volumes:
      - /usr/src/app/node_modules
  backend:
    image: example/backend
    volumes:
      - type: volume
        target: /usr/src/app/node_modules
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)

	got := map[string]string{}
	for _, w := range draft.Workloads {
		require.Len(t, w.Mounts, 1, w.Name)
		got[w.Name] = w.Mounts[0].VolumeID
	}
	require.Equal(t, "frontend-node_modules", got["frontend"])
	require.Equal(t, "backend-node_modules", got["backend"])
	require.Len(t, draft.Volumes, 2)
}

// TestR096_AServiceBehindAProfileIsNotImported asserts R-096: Pando runs what
// `docker compose up` runs.
func TestR096_AServiceBehindAProfileIsNotImported(t *testing.T) {
	const file = `
services:
  vote:
    image: example/vote
    ports: ["8080:80"]
  seed:
    image: example/seed
    profiles: ["seed"]
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	require.Len(t, draft.Workloads, 1)
	require.Equal(t, "vote", draft.Workloads[0].Name)
}

// A CMD-SHELL health check runs in a shell. It was passed on as one argv
// element, which the runtime tried to execute as a file named after the whole
// line, and the service was unhealthy for as long as it ran.
func TestR221_AComposeShellHealthCheckRunsInAShell(t *testing.T) {
	const file = `
services:
  shell:
    image: nginx:alpine
    healthcheck:
      test: ["CMD-SHELL", "curl -f http://localhost/ || exit 1"]
  exec:
    image: nginx:alpine
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost/"]
  bare:
    image: nginx:alpine
    healthcheck:
      test: curl -f http://localhost/
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)

	got := map[string][]string{}
	for _, w := range draft.Workloads {
		require.NotNil(t, w.Health, w.Name)
		got[w.Name] = w.Health.Command
	}
	require.Equal(t, []string{"sh", "-c", "curl -f http://localhost/ || exit 1"}, got["shell"])
	require.Equal(t, []string{"curl", "-f", "http://localhost/"}, got["exec"])
	require.Equal(t, []string{"sh", "-c", "curl -f http://localhost/"}, got["bare"])
}
