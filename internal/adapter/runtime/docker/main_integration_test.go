//go:build integration

package docker_test

import (
	"fmt"
	"os"
	"os/exec"
	"testing"

	"github.com/moby/moby/client"
)

// The images the parallel tests share, pulled before any of them runs.
//
// The adapter removes an image it pulled itself once no app claims it (R-224).
// Tests run in parallel (issue #31), so on a host without these, one test's
// Destroy could remove an image another test had just pulled and not yet
// claimed. An image the adapter found already present is never its to remove.
//
// Not busybox:1.36.1-musl: TestR224_AnImagePandoPulledGoesWithTheLastAppThatRanIt
// needs the adapter to be the one that pulls it, and nothing else uses it.
var sharedImages = []string{"alpine:3.20", "busybox:1.37"}

func TestMain(m *testing.M) {
	// One daemon for both halves of every test. The adapter talks to
	// DOCKER_HOST or, unset, the client library's default socket; the tests
	// set up and inspect through the docker CLI, which, unset, follows the
	// CLI's current context instead. On a machine with more than one daemon
	// — Docker Desktop beside colima, say — those were two daemons: the test
	// created a network or container on one and the adapter looked for it on
	// the other, and a dozen tests failed on properties that were fine.
	if os.Getenv("DOCKER_HOST") == "" {
		_ = os.Setenv("DOCKER_HOST", client.DefaultDockerHost)
	}

	for _, img := range sharedImages {
		if out, err := exec.Command("docker", "pull", "-q", img).CombinedOutput(); err != nil {
			// Not fatal: a test that needs the image fails with its own error,
			// and one that does not still runs.
			fmt.Fprintf(os.Stderr, "pulling %s: %v\n%s", img, err, out)
		}
	}
	os.Exit(m.Run())
}
