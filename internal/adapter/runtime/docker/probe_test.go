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
	out := "8080 HTTP/1.1 200 OK  \r\n" +
		"8078 GET / HTTP/1.0\r\n" +
		"3306 J\x00\x00\x00\n" +
		"4369 \n" +
		"8081 HTTP/1.0 301 Moved\n" +
		"9000 HTTP/2 404\n"

	require.Equal(t, map[int]int{8080: 200, 8081: 301, 9000: 404}, httpAnswers(out))
}

// TestR097_ATLSPortIsNotAnHTTPAnswer asserts R-097: a port that answers plain
// HTTP only to say it speaks TLS is not where a browser is sent. Kanboard's
// nginx answered on 443 with a 400 and was routed to (issue #90).
func TestR097_ATLSPortIsNotAnHTTPAnswer(t *testing.T) {
	out := "443 HTTP/1.1 400 Bad Request  Server: nginx    <html> <head><title>400 The plain HTTP request was sent to HTTPS port</title></head>\n" +
		"8443 HTTP/1.0 400 Bad Request    Client sent an HTTP request to an HTTPS server. \n" +
		"9443 HTTP/1.1 400 Bad Request    <p>You're speaking plain HTTP to an SSL-enabled server port.<br />\n" +
		"80 HTTP/1.1 302 Found  Location: /login\n" +
		"8000 HTTP/1.1 400 Bad Request  invalid host\n"

	require.Equal(t, map[int]int{80: 302, 8000: 400}, httpAnswers(out))
}

// The probe reads past the headers, on one line per port, so the body that
// names a TLS port is there to read.
func TestR097_TheProbeReadsTheReplyOnOneLine(t *testing.T) {
	script := probeScript([]int{80, 443})
	require.Contains(t, script, "for p in 80 443;")
	require.Contains(t, script, `head -c 1024 | tr '\r\n' '  '`)
}

// TestR097_TheWatchWaitsForTheWebPort asserts R-097: the trial watches until
// the port people open has answered, not until the first port that answers.
func TestR097_TheWatchWaitsForTheWebPort(t *testing.T) {
	// A page on a web port ends the watch soon after.
	require.False(t, portsSettled([]int{8080}, map[int]int{8080: 200}, 2*time.Second))
	require.True(t, portsSettled([]int{8080}, map[int]int{8080: 200}, httpSettle))
	require.True(t, portsSettled([]int{8080, 9090}, map[int]int{8080: 302}, httpSettle))

	// A port outside the ephemeral range that has not answered is waited for,
	// for a while: a database's port never will.
	require.False(t, portsSettled([]int{8080}, nil, pendingSettle-time.Second))
	require.True(t, portsSettled([]int{6379}, nil, pendingSettle))

	// Grist: the plugin server's ephemeral port answered while 8484 had not.
	require.False(t, portsSettled([]int{8484, 37759}, map[int]int{37759: 200}, pendingSettle-time.Second))
	require.True(t, portsSettled([]int{8484, 37759}, map[int]int{37759: 200, 8484: 200}, httpSettle))

	// An API answering 404 on its own port, nothing else waiting: settled.
	require.True(t, portsSettled([]int{8080}, map[int]int{8080: 404}, httpSettle))

	// Open Liberty: only an ephemeral port, silent. 9080 is still to come.
	require.False(t, portsSettled([]int{59611}, nil, 10*time.Minute))

	// A server given no port listens on a random one, and answers there.
	require.True(t, portsSettled([]int{46559}, map[int]int{46559: 200}, httpSettle))
}
