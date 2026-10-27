package hostagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"strings"
	"time"
)

// The liveness check between hosts (R-421). After the TLS handshake a host
// sends one line,
//
//	PANDO-AGENT/1 ping <host-id>\n
//
// naming itself, or "-" for the leader's collector, which is not a host's
// monitor. The agent answers "PONG <view>\n", where the view is the JSON its
// Liveness gives (liveness.View), and closes. The agent decides nothing about
// it: what the view says, and what anyone does about it, is core's.
//
// A ping asks for PingProtocol by ALPN. An agent negotiates no protocol and
// ignores it; a control host's cluster listener, which serves HTTP otherwise,
// answers pings for its own host on a connection that negotiated it
// (AnswerPing), so a control host is watched like any other (R-421).
const (
	pingVerb     = "ping"
	pongPrefix   = "PONG "
	noSender     = "-"
	maxPongBytes = 1 << 20
)

// PingProtocol is the ALPN protocol a ping asks for.
const PingProtocol = "pando-ping/1"

// HostCommonName is the subject of the certificate a host presents when it
// checks another (IssueHost). An agent takes a ping from it and nothing else:
// a host's certificate opens no connection to an app, so a host that was
// broken into cannot use it to reach another host's apps around Pando's
// proxy (R-023).
const HostCommonName = "pando-host"

// hostID is what a host's ID looks like (id.Host); anything else naming the
// sender is refused before it is recorded.
var hostID = regexp.MustCompile(`^host_[0-9A-HJKMNP-TV-Z]{26}$`)

// Liveness answers pings: core's liveness.Monitor. from is the host that sent
// the ping, or "" for the leader's collector.
type Liveness interface {
	Answer(from string) []byte
}

// IssueHostClient issues the certificate a host presents to other hosts' agents
// when it checks them. Client authentication only.
func (as Authorities) IssueHostClient() (tls.Certificate, error) {
	return as.issue(pkix.Name{CommonName: HostCommonName}, nil, x509.ExtKeyUsageClientAuth)
}

// answerPing handles a ping line, and refuses anything else sent with a
// host's certificate. It reports whether it handled the connection; when it
// did not, the line is a forward, for Pando's proxy only.
func (a *Agent) answerPing(conn *tls.Conn, line string) bool {
	if pong(conn, line, a.Liveness) {
		return true
	}
	if peerName(conn) == HostCommonName {
		refuse(conn, "a host's certificate opens no connection to an app")
		return true
	}
	return false
}

// pong answers line when it is a ping, and reports whether it was. A nil
// liveness refuses it.
func pong(conn net.Conn, line string, liveness Liveness) bool {
	fields := strings.Fields(line)
	if len(fields) != 3 || fields[0] != protocolVersion || fields[1] != pingVerb {
		return false
	}
	from, err := pingSender(fields[2])
	switch {
	case err != nil:
		refuse(conn, err.Error())
	case liveness == nil:
		refuse(conn, "this agent takes no part in the cluster's liveness checks")
	default:
		_, _ = io.WriteString(conn, pongPrefix+strings.ReplaceAll(string(liveness.Answer(from)), "\n", " ")+"\n")
	}
	return true
}

// AnswerPing answers the one ping sent on conn, a connection whose handshake
// negotiated PingProtocol and verified the client's certificate against the
// cluster authority: a control host's cluster listener hands it such
// connections (R-421). Anything but a ping is refused.
func AnswerPing(conn *tls.Conn, liveness Liveness) {
	_ = conn.SetDeadline(time.Now().Add(headerTimeout))
	if len(conn.ConnectionState().VerifiedChains) == 0 {
		refuse(conn, "a ping needs a certificate from the cluster authority")
		return
	}
	line, err := readLine(bufio.NewReaderSize(conn, maxHeaderLine))
	if err != nil {
		return
	}
	if !pong(conn, line, liveness) {
		refuse(conn, "this port answers pings and nothing else on "+PingProtocol)
	}
}

func pingSender(s string) (string, error) {
	switch {
	case s == noSender:
		return "", nil
	case hostID.MatchString(s):
		return s, nil
	}
	return "", fmt.Errorf("%q is not a host's ID", s)
}

func peerName(conn *tls.Conn) string {
	chains := conn.ConnectionState().VerifiedChains
	if len(chains) == 0 || len(chains[0]) == 0 {
		return ""
	}
	return chains[0][0].Subject.CommonName
}

// Ping checks the agent at addr, as the client tlsConfig authenticates, and
// returns the view it answered with. from is the checking host's ID, or ""
// for the leader's collector.
func Ping(ctx context.Context, addr string, tlsConfig *tls.Config, from string) ([]byte, error) {
	if from == "" {
		from = noSender
	}
	cfg := tlsConfig.Clone()
	cfg.NextProtos = []string{PingProtocol}
	d := tls.Dialer{NetDialer: &net.Dialer{Timeout: headerTimeout}, Config: cfg}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("could not reach the host agent at %s: %w", addr, err)
	}
	conn := raw.(*tls.Conn)
	defer func() { _ = conn.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(headerTimeout)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := fmt.Fprintf(conn, "%s %s %s\n", protocolVersion, pingVerb, from); err != nil {
		return nil, fmt.Errorf("the host agent at %s closed the connection: %w", addr, err)
	}
	reply, err := bufio.NewReader(io.LimitReader(conn, maxPongBytes)).ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("the host agent at %s did not answer the ping: %w", addr, err)
	}
	reply = strings.TrimRight(reply, "\r\n")
	switch {
	case strings.HasPrefix(reply, pongPrefix):
		return []byte(strings.TrimPrefix(reply, pongPrefix)), nil
	case strings.HasPrefix(reply, "NO "):
		return nil, fmt.Errorf("the host agent at %s refused the ping: %s", addr, strings.TrimPrefix(reply, "NO "))
	}
	return nil, errors.New("the host agent at " + addr + " answered the ping with something else")
}
