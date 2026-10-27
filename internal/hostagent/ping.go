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
	"syscall"
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

// PingError is a ping that got no view back. Contact says whether the
// machine at the address answered anyway (R-421): a connection refused or
// reset, a TLS alert, a certificate, a refusal or any other reply all come
// from a machine that is up and on the network, whatever its Pando process is
// doing. Only silence — a timeout, no route — is not contact. A host whose
// Pando process is down, restarting or waiting for its database is not cut
// off, and must never be taken for one that is: its apps may be running.
type PingError struct {
	Addr    string
	Contact bool
	Err     error
}

func (e *PingError) Error() string { return e.Err.Error() }
func (e *PingError) Unwrap() error { return e.Err }

// Contacted reports whether err, from Ping, still came from the machine:
// true for nil, and for a PingError with Contact.
func Contacted(err error) bool {
	if err == nil {
		return true
	}
	var pe *PingError
	return errors.As(err, &pe) && pe.Contact
}

// Ping checks the agent at addr, as the client tlsConfig authenticates, and
// returns the view it answered with. from is the checking host's ID, or ""
// for the leader's collector. An error is a *PingError, which says whether
// the machine answered at all (Contacted).
func Ping(ctx context.Context, addr string, tlsConfig *tls.Config, from string) ([]byte, error) {
	if from == "" {
		from = noSender
	}
	d := net.Dialer{Timeout: headerTimeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, &PingError{Addr: addr, Contact: refusedByPeer(err),
			Err: fmt.Errorf("could not reach the host agent at %s: %w", addr, err)}
	}
	// The machine took the connection: from here on whatever happens is an
	// answer from it.
	defer func() { _ = raw.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(headerTimeout)
	}
	_ = raw.SetDeadline(deadline)
	view, err := pingOn(ctx, raw, addr, tlsConfig, from)
	if err != nil {
		return nil, &PingError{Addr: addr, Contact: true, Err: err}
	}
	return view, nil
}

// pingOn sends the ping over an open connection and reads the answer.
func pingOn(ctx context.Context, raw net.Conn, addr string, tlsConfig *tls.Config, from string) ([]byte, error) {
	cfg := tlsConfig.Clone()
	cfg.NextProtos = []string{PingProtocol}
	conn := tls.Client(raw, cfg)
	if err := conn.HandshakeContext(ctx); err != nil {
		return nil, fmt.Errorf("the host agent at %s did not complete the handshake: %w", addr, err)
	}
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

// refusedByPeer reports whether a dial failed because the machine itself
// turned the connection away: refused or reset. A timeout, an unreachable
// network or host, or a canceled context is silence.
func refusedByPeer(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ECONNRESET)
}
