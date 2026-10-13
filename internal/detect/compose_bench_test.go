package detect_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// Compose imports that failed in the issue #90 benchmark.

// TestR099a_AFileTheMountPlacesElsewhereIsCarried asserts R-099a. A file in
// the build context is in the image where the build put it, which is not
// always where the mount puts it: isso builds the whole repository, has
// contrib/isso-dev.cfg under /isso, and mounts it at /config/isso-dev.cfg,
// where its server looks. Dropped as "already in the image", /config was empty
// and the server stopped.
func TestR099a_AFileTheMountPlacesElsewhereIsCarried(t *testing.T) {
	draft, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n  isso:\n    build: .\n" +
			"    volumes:\n      - ./contrib/isso-dev.cfg:/config/isso-dev.cfg\n" +
			"      - ./package.json:/src/package.json\n",
		"Dockerfile":           "FROM python:3.12\n",
		"contrib/isso-dev.cfg": "[general]\ndbpath = /db/comments.db\n",
		"package.json":         `{"name":"isso"}`,
	}, "compose.yaml")
	require.NoError(t, err)

	files := draft.Workloads[0].Files
	require.Len(t, files, 1, "the file where `COPY . <dir>` would have put it is left to the image")
	require.Equal(t, "/config/isso-dev.cfg", files[0].Path)
	require.Equal(t, "[general]\ndbpath = /db/comments.db\n", files[0].Content)
	require.Empty(t, draft.Workloads[0].Mounts, "carried, not also a volume")
}

// TestR096_AVariableAProvisionedServiceFillsIsOneSlot asserts R-096.
// Linkwarden's .env.sample leaves DATABASE_URL blank, and its compose file
// points DATABASE_URL at the Postgres beside it. That is one value, which the
// provisioned database fills; two slots of the same name failed accepting the
// plan.
func TestR096_AVariableAProvisionedServiceFillsIsOneSlot(t *testing.T) {
	draft, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n" +
			"  postgres:\n    image: postgres:16-alpine\n" +
			"  linkwarden:\n    image: ghcr.io/linkwarden/linkwarden:latest\n    env_file: .env\n" +
			"    environment:\n      - DATABASE_URL=postgresql://postgres:pw@postgres:5432/postgres\n",
		".env.sample": "NEXTAUTH_SECRET=\nDATABASE_URL=\n",
	}, "compose.yaml")
	require.NoError(t, err)

	var named []spec.Slot
	for _, s := range draft.Slots {
		if s.Key == "DATABASE_URL" {
			named = append(named, s)
		}
	}
	require.Len(t, named, 1)
	require.Equal(t, spec.SlotPostgres, named[0].Type)
	require.NotNil(t, named[0].Resolution)
	require.Equal(t, spec.ResolutionProvisioned, named[0].Resolution.Mode)

	_, other := slotNamed(draft.Slots, "NEXTAUTH_SECRET")
	require.True(t, other, "the rest of the template is still asked for")
}

// TestR096_AServiceTheAppReachesByNameStartsFirst asserts R-096. A Redis or
// database the app reaches by host name is imported as a service, and starts
// before the app: compose starts every service at once, so a compose file
// seldom says the app depends on it. Started last, the nginx-nodejs-redis
// backends crashed on a refused connection and nginx stopped on "host not
// found in upstream".
func TestR096_AServiceTheAppReachesByNameStartsFirst(t *testing.T) {
	draft, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n" +
			"  redis:\n    image: redislabs/redismod\n" +
			"  web1:\n    build: ./web\n" +
			"  nginx:\n    build: ./nginx\n    depends_on:\n      - web1\n",
		"web/Dockerfile":   "FROM node:20\n",
		"nginx/Dockerfile": "FROM nginx\n",
	}, "compose.yaml")
	require.NoError(t, err)

	require.Len(t, draft.Workloads, 3)
	require.Equal(t, "redis", draft.Workloads[0].Name)
	require.Empty(t, draft.Slots, "reached by host name, so imported as written rather than provisioned")
}

// TestR096_ARoutingLabelNamesTheUpstreamPort asserts R-096. A service routed
// by a socket proxy names its port in a label and nowhere else; the proxy is
// left out, and the label is the port Pando's own proxy sends traffic to.
func TestR096_ARoutingLabelNamesTheUpstreamPort(t *testing.T) {
	for name, labels := range map[string]string{
		"list": "    labels:\n      - \"traefik.enable=true\"\n" +
			"      - \"traefik.http.services.go.loadbalancer.server.port=80\"\n",
		"map": "    labels:\n      traefik.enable: \"true\"\n" +
			"      traefik.http.services.go.loadbalancer.server.port: 80\n",
	} {
		t.Run(name, func(t *testing.T) {
			draft, err := detect.ImportCompose(memSource{
				"compose.yaml": "services:\n" +
					"  frontend:\n    image: traefik:2.6\n" +
					"    volumes:\n      - /var/run/docker.sock:/var/run/docker.sock\n" +
					"  backend:\n    build: backend\n" + labels,
				"backend/Dockerfile": "FROM golang:1.22\n",
			}, "compose.yaml")
			require.NoError(t, err)

			require.Len(t, draft.Workloads, 1)
			require.Equal(t, []spec.Port{{Number: 80, Protocol: "http", Source: spec.PortCompose}},
				draft.Workloads[0].Ports)
		})
	}

	// A port the service declares itself is the port; the label is not read.
	draft, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n  backend:\n    image: example/api\n    expose:\n      - 8080\n" +
			"    labels:\n      - \"traefik.http.services.api.loadbalancer.server.port=9000\"\n",
	}, "compose.yaml")
	require.NoError(t, err)
	require.Equal(t, 8080, draft.Workloads[0].Ports[0].Number)
	require.Len(t, draft.Workloads[0].Ports, 1)
}

// TestR130_AVariablePassedThroughIsAHoleTheTrialCanFill asserts R-130 and
// R-132. Remark42's compose file passes SECRET and REMARK_URL through from the
// shell, and the app stops on "the required flags `--secret' and `--url' were
// not specified". Each is an optional slot, and the trial run's crash promotes
// the ones whose flags it names; the others stay optional.
func TestR130_AVariablePassedThroughIsAHoleTheTrialCanFill(t *testing.T) {
	draft, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n  remark:\n    image: ghcr.io/umputun/remark42:latest\n" +
			"    environment:\n      - REMARK_URL\n      - SECRET\n      - AUTH_GITHUB_CID\n      - DEBUG=true\n",
	}, "compose.yaml")
	require.NoError(t, err)

	env := envOf(draft.Workloads[0])
	for _, key := range []string{"REMARK_URL", "SECRET", "AUTH_GITHUB_CID"} {
		require.NotNil(t, env[key].SlotRef, key)
		s, ok := slotNamed(draft.Slots, key)
		require.True(t, ok, key)
		require.False(t, s.Required, "%s is optional until something says otherwise", key)
	}
	require.Equal(t, "true", *env["DEBUG"].Value)

	draft, _ = detect.ApplyTrial(draft, nil, detect.Trial{
		Ran: true, Started: true, Crashed: true,
		Log: "remark42 v1.18.0\nthe required flags `--secret' and `--url' were not specified\n",
	})
	for key, required := range map[string]bool{"REMARK_URL": true, "SECRET": true, "AUTH_GITHUB_CID": false} {
		s, _ := slotNamed(draft.Slots, key)
		require.Equal(t, required, s.Required, key)
	}
	secret, _ := slotNamed(draft.Slots, "SECRET")
	require.Equal(t, spec.FormatRandom, secret.Format, "Generate makes one")

	// A flag that only begins with the name is another flag.
	draft2, err := detect.ImportCompose(memSource{
		"compose.yaml": "services:\n  app:\n    image: example/app\n    environment:\n      - APP_URL\n",
	}, "compose.yaml")
	require.NoError(t, err)
	draft2, _ = detect.ApplyTrial(draft2, nil, detect.Trial{
		Ran: true, Started: true, Crashed: true, Log: "unknown flag --url-prefix\n",
	})
	s, _ := slotNamed(draft2.Slots, "APP_URL")
	require.False(t, s.Required)
}
