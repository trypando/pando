package docker

import (
	"testing"
	"time"

	"github.com/moby/moby/api/types/registry"
	"github.com/stretchr/testify/require"
)

// Only a reply that starts the way HTTP's does counts as one. A port that
// repeats the request back, sends a binary greeting or says nothing is
// listening without serving (issue #90).
func TestAProbeCountsOnlyAnHTTPStatusLineAsAnAnswer(t *testing.T) {
	out := "8080 HTTP/1.1 200 OK\r\n" +
		"8078 GET / HTTP/1.0\r\n" +
		"3306 J\x00\x00\x00\n" +
		"4369 \n" +
		"8081 HTTP/1.0 301 Moved\n" +
		"9000 HTTP/2 404\n"

	require.Equal(t, map[int]int{8080: 200, 8081: 301, 9000: 404}, httpAnswers(out))
}

// The watch ends sooner once something answers HTTP, and later while nothing
// has: what it is waiting for then is a web server that has not opened yet.
func TestTheWatchWaitsLongerForAPortThatHasNotAnsweredYet(t *testing.T) {
	require.False(t, portsSettled(true, 2*time.Second))
	require.True(t, portsSettled(true, httpSettle))
	require.False(t, portsSettled(false, httpSettle))
	require.True(t, portsSettled(false, quietSettle))
}

// TestR097_APortIsReportedAskedOnlyOnceItHasBeenAskedEnough asserts the
// evidence detection refuses on (R-097, R-107): a port that answered was asked,
// and one that has not answered counts only after minProbes requests, since a
// server can bind a moment before it serves. A port that stopped listening is
// not reported at all.
func TestR097_APortIsReportedAskedOnlyOnceItHasBeenAskedEnough(t *testing.T) {
	ports := &portWatch{answered: map[int]int{3000: 200}, probes: map[int]int{3000: 1, 25565: minProbes, 25575: minProbes - 1, 9999: minProbes}}
	require.Equal(t, []int{3000, 25565}, probedPorts([]int{3000, 25565, 25575}, ports))
}

// TestR094_TheDaemonsHubMirrorsAreReported asserts what the runtime tells
// Pando's registry reads (R-094, issue #90): the daemon's registry-mirrors, or
// the mirrors its docker.io index lists, and nothing for a daemon with none.
func TestR094_TheDaemonsHubMirrorsAreReported(t *testing.T) {
	require.Nil(t, daemonMirrors(nil))
	require.Equal(t, []string{"https://mirror.gcr.io/"},
		daemonMirrors(&registry.ServiceConfig{Mirrors: []string{"https://mirror.gcr.io/"}}))
	require.Equal(t, []string{"http://10.0.0.5:5000/"},
		daemonMirrors(&registry.ServiceConfig{IndexConfigs: map[string]*registry.IndexInfo{
			"docker.io": {Name: "docker.io", Mirrors: []string{"http://10.0.0.5:5000/"}},
		}}))
	require.Empty(t, daemonMirrors(&registry.ServiceConfig{}))
}
