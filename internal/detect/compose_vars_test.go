package detect_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func envOf(w spec.Workload) map[string]spec.EnvEntry {
	out := map[string]spec.EnvEntry{}
	for _, e := range w.Env {
		out[e.Key] = e
	}
	return out
}

func slotNamed(slots []spec.Slot, key string) (spec.Slot, bool) {
	for _, s := range slots {
		if s.Key == key {
			return s, true
		}
	}
	return spec.Slot{}, false
}

// TestR096_AComposeFileIsInterpolatedAsComposeWouldInterpolateIt asserts R-096.
// Every value is substituted before anything reads it — an image tag, a build's
// Dockerfile path, a bind-mount source, an environment value — against the
// committed .env beside the file, with PWD the directory the file is in. Each of
// these reached a container as written (issue #90).
func TestR096_AComposeFileIsInterpolatedAsComposeWouldInterpolateIt(t *testing.T) {
	src := memSource{
		".env": "# settings\n" +
			"PGADMIN_MAIL=admin@example.com\n" +
			"export PGADMIN_PW='p#ss w$rd'\n" +
			"PYTHON_VERSION=314 # which dockerfile to build\n" +
			"GREETING=\"hello ${PGADMIN_MAIL}\"\n",
		"compose.yaml": `
x-base: &base
  image: ghcr.io/acme/app:${APP_TAG:-release}
  environment:
    SHARED: ${PYTHON_VERSION}
services:
  web:
    <<: *base
    build:
      context: .
      dockerfile: ./dockerfiles/python${PYTHON_VERSION}/Dockerfile
    environment:
      PGADMIN_DEFAULT_EMAIL: ${PGADMIN_MAIL}
      PGADMIN_DEFAULT_PASSWORD: ${PGADMIN_PW}
      GREETING: ${GREETING}
      NESTED: ${UNSET_ONE:-${UNSET_TWO:-fallback}}
      ALT: ${PGADMIN_MAIL:+configured}
      ALT_UNSET: ${NOPE:+configured}
      LITERAL: $${NOT_A_VARIABLE}
  auth:
    image: example/auth
    volumes:
      - ${PWD}/var/demo.htpasswd:/var/local/demo.htpasswd
`,
		"var/demo.htpasswd": "demo:$apr1$x$y\n",
	}
	draft, err := detect.ImportCompose(src, "compose.yaml")
	require.NoError(t, err)
	byName := workloadsByName(draft)
	web := byName["web"]

	require.Equal(t, "ghcr.io/acme/app:release", web.Image, "a default in an image reference is resolved")
	require.NotNil(t, web.Build)
	require.Equal(t, "./dockerfiles/python314/Dockerfile", web.Build.Dockerfile,
		"an inline comment in .env is not part of the value")

	env := envOf(web)
	value := func(key string) string {
		require.NotNil(t, env[key].Value, key)
		return *env[key].Value
	}
	require.Equal(t, "admin@example.com", value("PGADMIN_DEFAULT_EMAIL"), "the committed .env is read")
	require.Equal(t, "p#ss w$rd", value("PGADMIN_DEFAULT_PASSWORD"), "a single-quoted value is literal")
	require.Equal(t, "hello admin@example.com", value("GREETING"))
	require.Equal(t, "fallback", value("NESTED"))
	require.Equal(t, "configured", value("ALT"))
	require.Equal(t, "", value("ALT_UNSET"))
	require.Equal(t, "${NOT_A_VARIABLE}", value("LITERAL"), "$$ is a literal $")
	require.NotContains(t, env, "SHARED", "the service's own environment replaces the anchor's")

	auth := byName["auth"]
	require.Len(t, auth.Files, 1, "${PWD}/x is ./x, a file in the repository, carried in the spec")
	require.Equal(t, "/var/local/demo.htpasswd", auth.Files[0].Path)
	for _, v := range draft.Volumes {
		require.NotContains(t, v.Name, "$")
	}
}

// TestR132_AComposeVariableNothingSetsIsARequiredSlot asserts R-132 for a
// compose file. A value that is one variable with no default, and that nothing
// in the repository sets, comes from the shell of whoever runs compose. It
// reached the app as the literal "${HOST}" — which Rails read as a truthy
// FORCE_SSL — and is now a required slot, asked for before the deploy.
func TestR132_AComposeVariableNothingSetsIsARequiredSlot(t *testing.T) {
	src := memSource{
		"compose.yaml": `
services:
  web:
    image: example/web
    environment:
      - ROOT_URL=${ROOT_URL}
      - FORCE_SSL=${HOST}
      - SECRET_KEY=${SECRET_KEY:?set a secret key}
      - EMBEDDED=https://${DOMAIN}/app
      - OPTIONAL=${OPTIONAL:-}
      - PASSED_THROUGH
`,
	}
	draft, err := detect.ImportCompose(src, "compose.yaml")
	require.NoError(t, err)
	env := envOf(draft.Workloads[0])

	for key, slot := range map[string]string{"ROOT_URL": "ROOT_URL", "FORCE_SSL": "HOST", "SECRET_KEY": "SECRET_KEY"} {
		require.Nil(t, env[key].Value, key)
		require.NotNil(t, env[key].SlotRef, key)
		require.Equal(t, slot, *env[key].SlotRef)
		s, ok := slotNamed(draft.Slots, slot)
		require.True(t, ok, slot)
		require.True(t, s.Required, "%s blocks the deploy until it is filled", slot)
		require.NotEmpty(t, s.Evidence)
	}

	// Part of a value has nothing a slot could fill on its own. It is empty,
	// as under compose, and the import says which variable it was.
	require.Equal(t, "https:///app", *env["EMBEDDED"].Value)
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "DOMAIN"))

	require.Equal(t, "", *env["OPTIONAL"].Value, "an empty default is a value")
	_, asked := slotNamed(draft.Slots, "OPTIONAL")
	require.False(t, asked)

	require.NotNil(t, env["PASSED_THROUGH"].Value)
	require.Equal(t, spec.EnvFromDetection, env["PASSED_THROUGH"].Source,
		"a pass-through nothing sets is a hole on the app's variables")
	require.True(t, hasWarning(draft.Warnings, spec.WarnComposeConstructRewritten, "PASSED_THROUGH"))

	for _, e := range draft.Workloads[0].Env {
		if e.Value != nil {
			require.False(t, strings.Contains(*e.Value, "${"), "%s reached the app unresolved", e.Key)
		}
	}
}

// TestR096_AnEnvFileIsReadAsComposeReadsIt asserts R-096 for `env_file:`.
func TestR096_AnEnvFileIsReadAsComposeReadsIt(t *testing.T) {
	src := memSource{
		"app.env":      "PORT=8080 # the web port\nNAME=\"two words\"\nlower_case=ok\n",
		"compose.yaml": "services:\n  web:\n    image: example/web\n    env_file: app.env\n",
	}
	draft, err := detect.ImportCompose(src, "compose.yaml")
	require.NoError(t, err)
	env := envOf(draft.Workloads[0])
	require.Equal(t, "8080", *env["PORT"].Value)
	require.Equal(t, "two words", *env["NAME"].Value)
	require.Equal(t, "ok", *env["lower_case"].Value)
}
