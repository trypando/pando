package detect_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func workloadsByName(d detect.Draft) map[string]spec.Workload {
	out := map[string]spec.Workload{}
	for _, w := range d.Workloads {
		out[w.Name] = w
	}
	return out
}

// TestR096_AFileWhereEveryServiceHasAProfileKeepsThemAll asserts R-096.
// `docker compose up` leaves out a service behind a profile, but a file where
// every service has one has no default set at all, and importing nothing would
// leave an app with nothing to run.
func TestR096_AFileWhereEveryServiceHasAProfileKeepsThemAll(t *testing.T) {
	const file = `
services:
  web:
    image: nginx:alpine
    profiles: [app]
  worker:
    image: busybox
    profiles: [app]
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	byName := workloadsByName(draft)
	require.Len(t, byName, 2)
	require.Contains(t, byName, "web")
	require.Contains(t, byName, "worker")
	require.False(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "profile"),
		"nothing was left out, so nothing is reported as left out")
}

// TestR096_ComposeSecretsInEveryShapeComposeAccepts asserts R-096. The long
// form names a target; a secret with no source, one not backed by a file, and
// one whose file is not in the repository have nothing to carry and are
// skipped rather than placed empty.
func TestR096_ComposeSecretsInEveryShapeComposeAccepts(t *testing.T) {
	const file = `
services:
  db:
    image: postgres:16
    secrets:
      - source: db-password
        target: /etc/db/password
      - source: relative
        target: api-key
      - target: /run/secrets/nothing
      - from-env
      - missing-file
      - undeclared
secrets:
  db-password:
    file: ./secrets/db.txt
  relative:
    file: ./secrets/api.txt
  from-env:
    environment: DB_PASSWORD
  missing-file:
    file: ./secrets/not-committed.txt
`
	draft, err := detect.ImportCompose(memSource{
		"compose.yml":     file,
		"secrets/db.txt":  "hunter2\n",
		"secrets/api.txt": "k-123\n",
	}, "compose.yml")
	require.NoError(t, err)
	require.Len(t, draft.Workloads, 1)
	require.ElementsMatch(t, []spec.File{
		{Path: "/etc/db/password", Content: "hunter2\n"},
		{Path: "/run/secrets/api-key", Content: "k-123\n"},
	}, draft.Workloads[0].Files, "a relative target lands under /run/secrets, as compose puts it")
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "/etc/db/password"))
}

// TestR096_ADirectoryTooBigToCarryIsStorageWhenNothingBuildsIt asserts R-096
// and R-200. Twenty files is source, not configuration; with no build context
// that already holds it, the only place left for it is a volume, which keeps
// whatever the service writes there.
func TestR096_ADirectoryTooBigToCarryIsStorageWhenNothingBuildsIt(t *testing.T) {
	src := memSource{"compose.yml": `
services:
  web:
    image: nginx:alpine
    volumes:
      - ./site:/usr/share/nginx/html
`}
	for n := 0; n < 20; n++ {
		src[fmt.Sprintf("site/page-%02d.html", n)] = "<p>page</p>"
	}

	draft, err := detect.ImportCompose(src, "compose.yml")
	require.NoError(t, err)
	web := draft.Workloads[0]
	require.Empty(t, web.Files, "twenty files are more than a mounted directory carries")
	require.Len(t, web.Mounts, 1)
	require.Equal(t, "/usr/share/nginx/html", web.Mounts[0].Path)
	require.NotEmpty(t, draft.Volumes)
}

// A mount of the repository root, or of something above it, names nothing
// Pando can read from the repository. Both are storage, as a bind mount of a
// directory compose would create is.
func TestAMountOfTheRepositoryRootOrAboveItIsStorage(t *testing.T) {
	const file = `
services:
  web:
    image: node:22-alpine
    volumes:
      - ./:/app
      - ../shared:/shared
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file, "index.js": "1\n"}, "compose.yml")
	require.NoError(t, err)
	web := draft.Workloads[0]
	require.Empty(t, web.Files, "the repository root is not copied into the spec")

	paths := map[string]bool{}
	for _, m := range web.Mounts {
		paths[m.Path] = true
	}
	require.True(t, paths["/app"])
	require.True(t, paths["/shared"])
}

// A mounted directory is walked four levels deep and no further, and what a
// checkout cannot list or stat is skipped rather than failing the import. A
// listing that returns a path from another directory is not taken as this
// one's.
func TestAMountedDirectoryIsReadAsFarAsTheCheckoutAllows(t *testing.T) {
	const file = `
services:
  web:
    image: nginx:alpine
    volumes:
      - ./conf:/etc/app
      - ./locked:/etc/locked
`
	src := faultySource{
		memSource: memSource{
			"compose.yml":              file,
			"conf/top.conf":            "top\n",
			"conf/unstatable.conf":     "hidden\n",
			"conf/a/b/c/d/e/deep.conf": "too deep\n",
			"locked/x.conf":            "x\n",
			"elsewhere/stray.conf":     "stray\n",
		},
		statFails: map[string]bool{"conf/unstatable.conf": true},
		globFails: map[string]bool{"locked/*": true},
		globExtra: map[string][]string{"conf/*": {"elsewhere/stray.conf"}},
	}

	draft, err := detect.ImportCompose(src, "compose.yml")
	require.NoError(t, err)
	web := draft.Workloads[0]
	require.Equal(t, []spec.File{{Path: "/etc/app/top.conf", Content: "top\n"}}, web.Files,
		"only the file the walk could see, at a depth it reads to, is carried")

	require.Len(t, web.Mounts, 1, "a directory that could not be listed is storage")
	require.Equal(t, "/etc/locked", web.Mounts[0].Path)
}

// TestR132_ANameTwoEnvFileTemplatesLeaveEmptyIsOneSlot asserts R-132. Two
// services pointing at the same uncommitted env file share a template, and the
// value it leaves blank is one value to supply, not two.
func TestR132_ANameTwoEnvFileTemplatesLeaveEmptyIsOneSlot(t *testing.T) {
	result, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n" +
			"  api:\n    image: example/api\n    env_file: .env\n" +
			"  worker:\n    image: example/worker\n    env_file: .env\n",
		".env.example": "API_TOKEN=\n",
	}, "compose.yaml")
	require.NoError(t, err)

	var slots int
	for _, s := range result.Slots {
		if s.Key == "API_TOKEN" {
			slots++
			require.False(t, s.Required, "blank, and nothing yet says the app needs it")
		}
	}
	require.Equal(t, 1, slots)
	for _, w := range result.Workloads {
		var ref *string
		for _, e := range w.Env {
			if e.Key == "API_TOKEN" {
				ref = e.SlotRef
			}
		}
		require.NotNil(t, ref, "%s reads the value from the one slot", w.Name)
	}
}

// A health check written as a plain argument list, with no CMD or CMD-SHELL in
// front, is run as it stands: the first element is the program.
func TestAHealthCheckWithoutAKeywordIsRunAsWritten(t *testing.T) {
	const file = `
services:
  web:
    image: nginx:alpine
    healthcheck:
      test: ["curl", "-f", "http://localhost/"]
      interval: 1m30s
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	h := draft.Workloads[0].Health
	require.NotNil(t, h)
	require.Equal(t, []string{"curl", "-f", "http://localhost/"}, h.Command)
	require.Equal(t, 90, h.IntervalSeconds)
}

// A command a shell could not split — an unclosed quote — is kept whole rather
// than cut at a guessed boundary. The service will fail on it as it would under
// compose, and the review shows the command exactly as written.
func TestACommandAShellCannotSplitIsKeptWhole(t *testing.T) {
	const file = `
services:
  web:
    image: node:22-alpine
    command: sh -c "npm start
`
	draft, err := detect.ImportCompose(memSource{"compose.yml": file}, "compose.yml")
	require.NoError(t, err)
	require.Equal(t, []string{`sh -c "npm start`}, draft.Workloads[0].Command)
}

// TestR103_ABlankTemplateNameIsNotAQuestion asserts R-103 and R-130 for an
// env_file template. Linkding's .env.sample leaves thirteen names blank and its
// trial ran with none of them set; asking for all thirteen blocked the deploy,
// and LD_DB_OPTIONS given a value broke the app's json.loads (issue #90). A
// blank name is an optional slot the trial can promote. A placeholder is the
// author saying the value must be replaced, and a name typed as a service is a
// dependency Pando provisions: both stay required.
func TestR103_ABlankTemplateNameIsNotAQuestion(t *testing.T) {
	result, err := detect.ImportCompose(memSource{
		"docker-compose.yml": "services:\n  linkding:\n    image: sissbruecker/linkding:latest\n" +
			"    ports:\n      - 9090:9090\n    env_file: .env\n",
		".env.sample": "LD_CONTAINER_NAME=linkding\nLD_SUPERUSER_NAME=\nLD_SUPERUSER_PASSWORD=\n" +
			"LD_DB_OPTIONS=\nSESSION_SECRET=change-me\nDATABASE_URL=\n",
	}, "docker-compose.yml")
	require.NoError(t, err)

	slots := map[string]spec.Slot{}
	for _, s := range result.Slots {
		slots[s.Key] = s
	}
	for _, key := range []string{"LD_SUPERUSER_NAME", "LD_SUPERUSER_PASSWORD", "LD_DB_OPTIONS"} {
		require.Contains(t, slots, key, "a blank name is still a hole in review")
		require.False(t, slots[key].Required, key)
	}
	require.True(t, slots["SESSION_SECRET"].Required, "a placeholder says the value must be replaced")
	require.True(t, slots["DATABASE_URL"].Required, "a database is a dependency")
	require.NotEqual(t, spec.SlotUnknown, slots["DATABASE_URL"].Type)
	require.NotContains(t, slots, "LD_CONTAINER_NAME", "a sample value is a default")
}
