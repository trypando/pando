//go:build integration

package docker_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

func trialID(t *testing.T) string {
	t.Helper()
	return strings.ToLower(strings.NewReplacer("/", "-", "_", "-").Replace(t.Name())) +
		"-" + time.Now().Format("150405")
}

// R-097: the port is discovered by watching what the process binds.
//
// The real check is not that a number comes back. It is that it comes back from
// an image with no shell and no tools — which a Go binary on scratch is, and
// which any implementation that exec'd into the app's own container would fail
// on. Busybox with httpd stands in for "a server", and the assertion that
// matters is that nothing was asked of the image itself.
func TestR097_ATrialRunObservesTheBoundPort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		Command: []string{"httpd", "-f", "-p", "8123"},
		Timeout: 30 * time.Second,
	})
	require.NoError(t, err)

	require.True(t, result.Started)
	require.Nil(t, result.ExitCode, "it was still running, which is the passing outcome")
	require.Contains(t, result.ObservedPorts, 8123,
		"the port was read from the container's network namespace, not from the image")
}

// A trial that crashes produces the log, which is the whole answer (R-107).
func TestR107_ACrashingTrialReturnsItsLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		Command: []string{"sh", "-c", "echo 'FATAL: DATABASE_URL is not set' >&2; exit 1"},
		Timeout: 30 * time.Second,
	})
	require.NoError(t, err, "a crash is a result, not a failure of the trial run")

	require.True(t, result.Started)
	require.NotNil(t, result.ExitCode)
	require.Equal(t, 1, *result.ExitCode)
	require.Contains(t, result.Log, "DATABASE_URL is not set")

	// Docker frames non-TTY output with 8-byte headers. Leaving them in puts
	// control bytes through the middle of the log the user is shown.
	require.False(t, strings.ContainsRune(result.Log, '\x00'),
		"the stream framing must be stripped before this reaches a console")
}

// R-202: what the app wrote outside its declared storage, by name.
func TestR202_ATrialRunObservesWritesOutsideDeclaredStorage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		Command: []string{"sh", "-c",
			"mkdir -p /app/uploads /app/keep && " +
				"echo x > /app/uploads/file && echo y > /app/keep/file && " +
				"echo z > /tmp/scratch && sleep 5"},
		DeclaredPaths: []string{"/app/keep"},
		Timeout:       20 * time.Second,
	})
	require.NoError(t, err)

	require.Contains(t, result.ObservedWrites, "/app/uploads")
	require.NotContains(t, result.ObservedWrites, "/app/keep",
		"a declared path is storage the app said it wanted; writing there is the point")
	require.NotContains(t, result.ObservedWrites, "/tmp",
		"a warning that fires on /tmp for every app is one people learn to dismiss unread")
}

// Nothing survives a trial. A leaked container is a running copy of a
// stranger's app that nothing is tracking.
func TestATrialLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := trialID(t)

	_, err := a.Trial(ctx, api.TrialRequest{
		TrialID: id,
		Image:   "busybox:1.37",
		Command: []string{"sleep", "3"},
		Timeout: 10 * time.Second,
	})
	require.NoError(t, err)

	containers, err := execCommand("docker", "ps", "-a", "--filter", "label=io.pando.trial", "--format", "{{.Names}}")
	require.NoError(t, err)
	require.NotContains(t, containers, id)

	networks, err := execCommand("docker", "network", "ls", "--filter", "label=io.pando.trial", "--format", "{{.Name}}")
	require.NoError(t, err)
	require.NotContains(t, networks, id)
}

// Cleanup also has to survive the trial being interrupted, which is the case
// that actually leaks: Pando restarting mid-detection.
func TestATimedOutTrialStillCleansUp(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)
	id := trialID(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: id,
		Image:   "busybox:1.37",
		Command: []string{"sleep", "3600"},
		Timeout: 5 * time.Second,
	})
	require.NoError(t, err)
	require.True(t, result.Started, "still running at the timeout is the app working")
	require.Nil(t, result.ExitCode)

	out, err := execCommand("docker", "ps", "-a", "--filter", "name=pando-trial-"+id, "--format", "{{.Names}}")
	require.NoError(t, err)
	require.Empty(t, strings.TrimSpace(out),
		"the context that bounds observation must not also decide whether the container is removed")
}

// R-112 again, from the other side: the trial run is the runtime's, and the
// build path still never gets a socket.
func TestR254_TrialCapabilitiesAreDeclaredNotAssumed(t *testing.T) {
	t.Parallel()
	caps, err := adapter(t).Capabilities(context.Background())
	require.NoError(t, err)
	require.True(t, caps.SupportsTrialRun)
	require.True(t, caps.SupportsPortObservation)
	require.True(t, caps.SupportsWriteObservation)
}

// Docker runs an embedded DNS resolver on 127.0.0.11 inside every container on
// a user-defined network, on a port that changes each run. Reported as the
// app's port, it makes detection propose a random number.
func TestAnIdleContainerObservesNoPort(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		Command: []string{"sleep", "3600"},
		Timeout: 8 * time.Second,
	})
	require.NoError(t, err)

	require.True(t, result.Started)
	require.Empty(t, result.ObservedPorts,
		"a container that listens on nothing must not be reported as listening on Docker's resolver")
	require.Empty(t, result.LoopbackPorts)
}

// An app bound to 127.0.0.1 is listening, and unreachable. Both halves matter.
func TestALoopbackOnlyBindIsReportedSeparately(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		// -h so httpd serves something; bound to loopback only.
		Command: []string{"httpd", "-f", "-p", "127.0.0.1:8124", "-h", "/etc"},
		Timeout: 12 * time.Second,
	})
	require.NoError(t, err)

	require.Contains(t, result.LoopbackPorts, 8124)
	require.NotContains(t, result.ObservedPorts, 8124,
		"routing to a loopback bind would time out with the app reporting itself healthy")
}

// TestR097_ATrialReportsWhichPortAnswersHTTP asserts R-097 for an app that
// listens on more than one port. Listening is not serving: a port that speaks
// another protocol — epmd, MQTT, Tomcat's shutdown port, or here an echo
// server that repeats the request back — is observed, and only the one that
// answers an HTTP request is reported as answering it (issue #90).
func TestR097_ATrialReportsWhichPortAnswersHTTP(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	if !caps.SupportsHTTPProbe {
		t.Skip("this daemon's trial cannot see into the container")
	}

	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		// An echo server on 8125, and a web server on 8126.
		Command: []string{"sh", "-c", "nc -ll -p 8125 -e cat & httpd -f -p 8126 -h /etc"},
		Timeout: 40 * time.Second,
	})
	require.NoError(t, err)

	require.Nil(t, result.ExitCode, "the watch ended because the ports settled, not on the clock or a crash")
	require.ElementsMatch(t, []int{8125, 8126}, result.ObservedPorts)
	require.Len(t, result.HTTPAnswers, 1)
	require.Equal(t, 8126, result.HTTPAnswers[0].Port)
	require.NotZero(t, result.HTTPAnswers[0].Status)
}

// TestR097_ATrialHearsAServerThatDropsAHalfClosedRequest asserts R-097 for the
// web servers that treat a client closing its sending side as having left —
// hyper, uvicorn, Tornado, httpuv. Probed with a request whose input ended at
// once, they answered nothing, and their apps were refused as serving no web
// page (issue #90). This server reads the request, then answers only if the
// client's side is still open: end of input arrives at once, a client still
// connected leaves the read to time out.
func TestR097_ATrialHearsAServerThatDropsAHalfClosedRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	a := adapter(t)

	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	if !caps.SupportsHTTPProbe {
		t.Skip("this daemon's trial cannot see into the container")
	}

	server := `while IFS= read -r l; do [ "$l" = "$(printf '\r')" ] && break; done; ` +
		`s=$(date +%s); read -t 2 x; [ $(($(date +%s)-s)) -lt 1 ] && exit 0; ` +
		`printf 'HTTP/1.0 200 OK\r\n\r\nok'`
	result, err := a.Trial(ctx, api.TrialRequest{
		TrialID: trialID(t),
		Image:   "busybox:1.37",
		Command: []string{"nc", "-ll", "-p", "8127", "-e", "sh", "-c", server},
		Timeout: 60 * time.Second,
	})
	require.NoError(t, err)

	require.Equal(t, []int{8127}, result.ObservedPorts)
	require.Len(t, result.HTTPAnswers, 1, "the probe must wait for the reply with its side still open")
	require.Equal(t, 8127, result.HTTPAnswers[0].Port)
	require.Equal(t, 200, result.HTTPAnswers[0].Status)
}
