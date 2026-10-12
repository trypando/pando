package docker

import (
	"testing"
	"time"

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
