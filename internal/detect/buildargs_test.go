package detect_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// TestR104_OnlyABuildArgumentTheBuildStopsOverIsAsked asserts R-104: a build
// argument is a question only when the Dockerfile refuses to build without
// it. One it guards with a fallback is not asked, and one it only needs set
// (read under `set -u`) is passed as empty rather than asked.
func TestR104_OnlyABuildArgumentTheBuildStopsOverIsAsked(t *testing.T) {
	cases := map[string]struct {
		dockerfile string
		asked      []string
		empty      []string
	}{
		"test -n assertion": {
			dockerfile: "FROM golang:1.23\nARG APP_VERSION\n" +
				"RUN test -n \"$APP_VERSION\" || (echo \"APP_VERSION is required\" && exit 1)\n",
			asked: []string{"APP_VERSION"},
		},
		"parameter expansion assertion": {
			dockerfile: "FROM alpine\nARG TOKEN\nRUN echo ${TOKEN:?set TOKEN}\n",
			asked:      []string{"TOKEN"},
		},
		"-z test that exits": {
			dockerfile: "FROM alpine\nARG TOKEN\nRUN if [ -z \"$TOKEN\" ]; then echo missing; exit 1; fi\n",
			asked:      []string{"TOKEN"},
		},
		"-z guard with a fallback": {
			dockerfile: "FROM rust\nARG ARCH\nRUN __ARCH=\"$(dpkg --print-architecture)\"; \\\n" +
				"    [ -z  $ARCH ] || __ARCH=$ARCH; \\\n    echo $__ARCH\n",
		},
		"-z branch that assigns a default": {
			dockerfile: "FROM alpine\nARG MODE\nRUN if [ -z \"$MODE\" ]; then MODE=prod; fi; echo $MODE\n",
		},
		"read under set -u": {
			dockerfile: "FROM rust\nARG     EXTRA_ARGS\nRUN     set -eux; \\\n" +
				"        cargo build --release ${EXTRA_ARGS}\n",
			empty: []string{"EXTRA_ARGS"},
		},
		"read with its own default under set -u": {
			dockerfile: "FROM rust\nARG EXTRA_ARGS\nRUN set -eux; cargo build ${EXTRA_ARGS:-}\n",
		},
		"unused": {
			dockerfile: "FROM alpine\nARG VERSION\nRUN echo hello\n",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := detect.DockerfileDetector{}.Bid(context.Background(), memSource{"Dockerfile": tc.dockerfile})
			require.NoError(t, err)

			var asked []string
			for _, q := range c.Questions {
				if arg, ok := strings.CutPrefix(q.Key, detect.BuildArgKeyPrefix); ok {
					asked = append(asked, arg)
					require.NoError(t, q.Validate(), "R-105")
				}
			}
			require.Equal(t, tc.asked, asked)

			var empty []string
			for _, kv := range c.Draft.Build.Args {
				require.Equal(t, spec.KV{Key: kv.Key}, kv, "passed as empty")
				empty = append(empty, kv.Key)
			}
			require.Equal(t, tc.empty, empty)
		})
	}
}
