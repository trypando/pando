package hostagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// pingListener is a cluster listener's ping half on loopback: it negotiates
// PingProtocol and hands each connection to AnswerPing. A client certificate
// is checked when given, as the listener's HTTP half does, so AnswerPing's own
// check is what refuses a connection without one.
func pingListener(t *testing.T, live Liveness) (string, Authorities, chan string) {
	t.Helper()
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	as, err := ParseAuthorities(caPEM)
	require.NoError(t, err)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: as.Pool(), NextProtos: []string{PingProtocol},
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	negotiated := make(chan string, 8)
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				conn := raw.(*tls.Conn)
				defer func() { _ = conn.Close() }()
				if conn.HandshakeContext(context.Background()) != nil {
					return
				}
				negotiated <- conn.ConnectionState().NegotiatedProtocol
				AnswerPing(conn, live, nil)
			}()
		}
	}()
	return ln.Addr().String(), as, negotiated
}

// TestR421_AControlHostAnswersPingsOnItsClusterListener asserts R-421: a
// ping asks for PingProtocol, and a listener that negotiates it answers with
// its host's view, for a certificate from the cluster authority only, and
// nothing but a ping.
func TestR421_AControlHostAnswersPingsOnItsClusterListener(t *testing.T) {
	live := &fakeLiveness{}
	addr, as, negotiated := pingListener(t, live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	view, err := Ping(ctx, addr, hostTLS(t, as), peerID)
	require.NoError(t, err)
	require.Equal(t, `{"host":"host_1", "fenced":false}`, string(view))
	require.Equal(t, PingProtocol, <-negotiated)
	require.Equal(t, []string{peerID}, live.senders())

	anonymous := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: as.Pool(), ServerName: ServerName("host-1")}
	_, err = Ping(ctx, addr, anonymous, peerID)
	require.ErrorContains(t, err, "needs a certificate from the cluster authority")
	<-negotiated

	cfg := hostTLS(t, as).Clone()
	cfg.NextProtos = []string{PingProtocol}
	conn, err := tls.Dial("tcp", addr, cfg)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	_, err = io.WriteString(conn, "PANDO-AGENT/1 docker\n")
	require.NoError(t, err)
	reply, err := bufio.NewReader(conn).ReadString('\n')
	require.NoError(t, err)
	require.Contains(t, reply, "NO this port answers pings and nothing else")
	<-negotiated

	// A connection that closes before its line is left without an answer.
	quiet, err := tls.Dial("tcp", addr, cfg)
	require.NoError(t, err)
	require.NoError(t, quiet.Handshake())
	require.NoError(t, quiet.Close())
	<-negotiated
}

// TestPingToAnAgentNegotiatesNoProtocol asserts that asking for
// PingProtocol changes nothing for an agent, which negotiates none.
func TestPingToAnAgentNegotiatesNoProtocol(t *testing.T) {
	live := &fakeLiveness{}
	addr, as := pingAgent(t, live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := Ping(ctx, addr, hostTLS(t, as), peerID)
	require.NoError(t, err)
	require.Equal(t, []string{peerID}, live.senders())
}
