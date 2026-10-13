package detect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// yopassCompose is deploy/docker-compose/insecure/docker-compose.yml from
// github.com/jhaals/yopass, verbatim.
const yopassCompose = `version: "3.8"

services:
  memcached:
    image: memcached
    restart: always
    expose:
      - "11211"

  yopass:
    image: jhaals/yopass
    restart: always
    ports:
      - "127.0.0.1:80:80"
    environment:
      - MEMCACHED=memcached:11211
      - PORT=80
    healthcheck:
      test: ["CMD", "/yopass-server", "--health-check"]
      interval: 30s
      timeout: 3s
      retries: 3
      start_period: 5s
`

// yopassRepo is the shape of yopass's repository: a Dockerfile at the root
// that builds the server, and example compose files below it.
func yopassRepo(compose string) memSource {
	return memSource{
		"Dockerfile": "FROM golang:1.27-bookworm AS app\nWORKDIR /yopass\nCOPY . .\n" +
			"RUN go build ./cmd/yopass-server\n\nFROM gcr.io/distroless/base\n" +
			"COPY --from=app /yopass/yopass-server /\nENTRYPOINT [\"/yopass-server\"]\n",
		"go.mod":                    "module github.com/jhaals/yopass\n\ngo 1.27\n",
		"cmd/yopass-server/main.go": "package main\n\nfunc main() {}\n",
		"deploy/docker-compose/insecure/docker-compose.yml": compose,
	}
}

var yopassSource = spec.Source{Type: spec.SourceGit, URL: "https://github.com/jhaals/yopass"}

func yopassDraft(t *testing.T, src memSource, from spec.Source) detect.Draft {
	t.Helper()
	result, err := detect.NewAuction(detect.DockerfileDetector{}, detect.ComposeDetector{}, detect.BuildpackDetector{}).
		Run(context.Background(), src)
	require.NoError(t, err)
	require.Equal(t, spec.BuildDockerfile, result.Winner.Strategy, "the Dockerfile at the root is the reading")
	return detect.WithExampleCompose(src, from, result.Winner.Draft)
}

// TestR130_AnExampleComposeFileTypesTheCacheTheAppNeeds asserts R-130 and
// R-021. Yopass deployed from its Dockerfile without the memcached its own
// example deployment runs beside it, served pages, and failed the first secret
// anybody stored on "dial tcp 127.0.0.1:11211: connect: connection refused".
// The example compose file runs the project's own image and points MEMCACHED at
// a memcached: that is the author declaring the dependency, and it becomes a
// Memcached slot Pando provisions, wired to the variable the app reads.
func TestR130_AnExampleComposeFileTypesTheCacheTheAppNeeds(t *testing.T) {
	d := yopassDraft(t, yopassRepo(yopassCompose), yopassSource)

	slot, ok := slotNamed(d.Slots, "MEMCACHED")
	require.True(t, ok, "the cache is a slot")
	require.Equal(t, spec.SlotMemcached, slot.Type)
	require.True(t, slot.Required, "yopass cannot store a secret without it")
	require.NotNil(t, slot.Resolution)
	require.Equal(t, spec.ResolutionProvisioned, slot.Resolution.Mode, "provisioned, as the example runs it")
	require.Contains(t, slot.Evidence[0], "deploy/docker-compose/insecure/docker-compose.yml")

	require.Len(t, d.Workloads, 1, "the example's memcached is not a workload, and nor is its copy of the app")
	var wired bool
	for _, e := range d.Workloads[0].Env {
		if e.Key == "MEMCACHED" {
			require.NotNil(t, e.SlotRef)
			require.Equal(t, "MEMCACHED", *e.SlotRef)
			wired = true
		}
		require.NotEqual(t, "PORT", e.Key, "only the connection is taken, not how the example runs the app")
	}
	require.True(t, wired, "MEMCACHED is filled from the slot")

	// The image is named for the repository however the reference is spelled.
	for _, image := range []string{"docker.io/jhaals/yopass:latest", "ghcr.io/jhaals/yopass@sha256:00", "JHAALS/yopass:1"} {
		d := yopassDraft(t, yopassRepo(replaceImage(yopassCompose, image)), yopassSource)
		_, ok := slotNamed(d.Slots, "MEMCACHED")
		require.True(t, ok, image)
	}
}

// TestR021_AnExampleComposeFileIsTakenOnlyForThisApp asserts R-021 and R-102:
// what an example compose file runs is this app's dependency only when the
// service that reaches it is this app, by its build context or by the image
// named for the repository.
func TestR021_AnExampleComposeFileIsTakenOnlyForThisApp(t *testing.T) {
	cases := map[string]struct {
		src  memSource
		from spec.Source
		want bool
	}{
		"another project's image": {
			src: yopassRepo(replaceImage(yopassCompose, "someone/else")), from: yopassSource,
		},
		"an upload, which names no repository": {
			src: yopassRepo(yopassCompose), from: spec.Source{Type: spec.SourceUpload},
		},
		"a service built from the app's own directory": {
			src: yopassRepo(replaceImage(yopassCompose, "jhaals/yopass\n    build: ../../..")), from: spec.Source{Type: spec.SourceUpload},
			want: true,
		},
		"a fixture under test/": {
			src: memSource{
				"Dockerfile":                  "FROM alpine\nCMD [\"/app\"]\n",
				"test/e2e/docker-compose.yml": yopassCompose,
			},
			from: yopassSource,
		},
	}
	two := yopassRepo(yopassCompose)
	two["deploy/docker-compose/insecure/docker-compose.yml"] = yopassCompose +
		"  yopass-two:\n    image: jhaals/yopass\n    environment:\n      - MEMCACHED=memcached:11211\n"
	cases["two services that are both this app"] = struct {
		src  memSource
		from spec.Source
		want bool
	}{src: two, from: yopassSource}

	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			d := yopassDraft(t, c.src, c.from)
			_, ok := slotNamed(d.Slots, "MEMCACHED")
			require.Equal(t, c.want, ok)
		})
	}
}

// TestR131_AMemcachedTheComposeFileRunsIsProvisioned asserts R-131 and R-096
// for a compose file at the root: a memcached the app reaches at
// "memcached:11211" is a slot Pando provisions and fills, as a database reached
// by URL is. One reached by a bare host name beside a separate port is run as
// the file wrote it, because Pando's one address cannot fill two variables.
func TestR131_AMemcachedTheComposeFileRunsIsProvisioned(t *testing.T) {
	result, err := detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), memSource{
		"docker-compose.yml": yopassCompose,
	})
	require.NoError(t, err)
	d := result.Winner.Draft
	slot, ok := slotNamed(d.Slots, "MEMCACHED")
	require.True(t, ok)
	require.Equal(t, spec.SlotMemcached, slot.Type)
	require.Equal(t, spec.ResolutionProvisioned, slot.Resolution.Mode)
	require.Contains(t, slot.Aliases, "memcached")
	for _, w := range d.Workloads {
		require.NotEqual(t, "memcached", w.Name, "provisioned, not run beside the app")
	}

	result, err = detect.NewAuction(detect.ComposeDetector{}).Run(context.Background(), memSource{
		"docker-compose.yml": "services:\n  cache:\n    image: memcached:1.6\n  app:\n    image: example/app\n" +
			"    environment:\n      MEMCACHED_HOST: cache\n      MEMCACHED_PORT: \"11211\"\n",
	})
	require.NoError(t, err)
	require.Empty(t, result.Winner.Draft.Slots)
	require.Len(t, result.Winner.Draft.Workloads, 2, "the cache is imported as written")
}

func replaceImage(compose, image string) string {
	return strings.ReplaceAll(compose, "image: jhaals/yopass", "image: "+image)
}

// TestR130_DetectionGivesYopassItsCache asserts R-130 through the detection
// job, which is what a one-shot deploy runs: the proposal's spec carries the
// Memcached slot, so a deploy provisions it and fills MEMCACHED.
func TestR130_DetectionGivesYopassItsCache(t *testing.T) {
	job := &detect.Job{Auction: detect.NewAuction(detect.DockerfileDetector{}, detect.ComposeDetector{}, detect.BuildpackDetector{})}
	p, err := job.Run(context.Background(), "app_1", yopassSource, yopassRepo(yopassCompose))
	require.NoError(t, err)
	slot, ok := p.DraftSpec.Slot("MEMCACHED")
	require.True(t, ok)
	require.Equal(t, spec.SlotMemcached, slot.Type)
	require.Equal(t, spec.ResolutionProvisioned, slot.Resolution.Mode)
}
