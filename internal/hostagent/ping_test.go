package hostagent

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
)

type fakeLiveness struct {
	mu   sync.Mutex
	from []string
}

func (f *fakeLiveness) Answer(from string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.from = append(f.from, from)
	return []byte("{\"host\":\"host_1\",\n\"fenced\":false}")
}

func (f *fakeLiveness) senders() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.from...)
}

// pingAgent is an agent on loopback with the given liveness, and the
// authorities it trusts.
func pingAgent(t *testing.T, live Liveness) (string, Authorities) {
	t.Helper()
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	as, err := ParseAuthorities(caPEM)
	require.NoError(t, err)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	serverTLS, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	agent := &Agent{
		TLS: serverTLS, Pool: testPool, Liveness: live,
		Resolve: func(context.Context, string) ([]netip.Addr, error) { return nil, errors.New("no such host") },
		Dial: func(context.Context, string) (net.Conn, error) {
			t.Error("a ping must never dial a workload")
			return nil, errors.New("no")
		},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), as
}

func hostTLS(t *testing.T, as Authorities) *tls.Config {
	t.Helper()
	cert, err := as.IssueHost()
	require.NoError(t, err)
	require.Equal(t, HostCommonName, cert.Leaf.Subject.CommonName)
	return ClientTLS(as, cert, "host-1")
}

const peerID = "host_01HQ8ZZZZZZZZZZZZZZZZZZZZZ"

// TestR421_HostsPingEachOtherOnTheAgentPort asserts R-421's "hosts check each
// other on the agent port": a host's certificate is answered with the agent's
// view, and its sender recorded.
func TestR421_HostsPingEachOtherOnTheAgentPort(t *testing.T) {
	live := &fakeLiveness{}
	addr, as := pingAgent(t, live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	view, err := Ping(ctx, addr, hostTLS(t, as), peerID)
	require.NoError(t, err)
	require.Equal(t, `{"host":"host_1", "fenced":false}`, string(view), "one line, whatever the view")

	// The leader's collector names nobody.
	_, err = Ping(context.Background(), addr, hostTLS(t, as), "")
	require.NoError(t, err)
	require.Equal(t, []string{peerID, ""}, live.senders())

	// Pando's proxy certificate may ping as well.
	proxy, err := as.IssueClient()
	require.NoError(t, err)
	_, err = Ping(ctx, addr, ClientTLS(as, proxy, "host-1"), "")
	require.NoError(t, err)
}

// TestR023_AHostCertificateOpensNoConnectionToAnApp asserts R-023: the
// certificate a host checks others with is good for a ping and nothing else,
// so it is no way around Pando's proxy.
func TestR023_AHostCertificateOpensNoConnectionToAnApp(t *testing.T) {
	addr, as := pingAgent(t, &fakeLiveness{})
	_, err := Dial(context.Background(), addr, hostTLS(t, as), "pando-app_1-web", 3000)
	require.ErrorContains(t, err, "a host's certificate opens no connection to an app")
}

func TestPingRefusals(t *testing.T) {
	addr, as := pingAgent(t, nil)
	ctx := context.Background()
	_, err := Ping(ctx, addr, hostTLS(t, as), peerID)
	require.ErrorContains(t, err, "takes no part in the cluster's liveness checks")

	addr, as = pingAgent(t, &fakeLiveness{})
	_, err = Ping(ctx, addr, hostTLS(t, as), "app_01HQ8ZZZZZZZZZZZZZZZZZZZZZ")
	require.ErrorContains(t, err, "is not a host's ID")

	// No certificate at all: the handshake fails.
	_, err = Ping(ctx, addr, &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS13}, peerID) //nolint:gosec // G402: the test checks the agent refuses a client without a certificate.
	require.Error(t, err)

	// Nobody listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := ln.Addr().String()
	require.NoError(t, ln.Close())
	_, err = Ping(ctx, closed, hostTLS(t, as), peerID)
	require.ErrorContains(t, err, "could not reach the host agent")
}

// fakeAgentAnswering is a TLS server that answers any line with reply.
func fakeAgentAnswering(t *testing.T, reply string) (string, *tls.Config) {
	t.Helper()
	caPEM, err := NewAuthority()
	require.NoError(t, err)
	as, err := ParseAuthorities(caPEM)
	require.NoError(t, err)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 128)
			_, _ = c.Read(buf)
			_, _ = io.WriteString(c, reply)
			_ = c.Close()
		}
	}()
	return ln.Addr().String(), hostTLS(t, as)
}

func TestPingOddAnswers(t *testing.T) {
	addr, cfg := fakeAgentAnswering(t, "HELLO\n")
	_, err := Ping(context.Background(), addr, cfg, peerID)
	require.ErrorContains(t, err, "answered the ping with something else")

	addr, cfg = fakeAgentAnswering(t, "PONG no newline")
	_, err = Ping(context.Background(), addr, cfg, peerID)
	require.ErrorContains(t, err, "did not answer the ping")
}

type fakeDocker struct {
	items   []container.Summary
	listErr error
	stopErr map[string]bool
	stopped []string
}

func (f *fakeDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	if o.All {
		return client.ContainerListResult{}, errors.New("a fence lists running containers only")
	}
	return client.ContainerListResult{Items: f.items}, f.listErr
}

func (f *fakeDocker) ContainerStop(_ context.Context, id string, o client.ContainerStopOptions) (client.ContainerStopResult, error) {
	if o.Timeout == nil || *o.Timeout != fenceStopSeconds {
		return client.ContainerStopResult{}, errors.New("a fence stops with its own timeout")
	}
	if f.stopErr[id] {
		return client.ContainerStopResult{}, errors.New("would not stop")
	}
	f.stopped = append(f.stopped, id)
	return client.ContainerStopResult{}, nil
}

// TestR421_AFenceStopsOnlyAppContainers asserts R-421's fence: every running
// app container is stopped, one that will not stop does not keep the others
// running, and the edge and Pando's own containers are left alone.
func TestR421_AFenceStopsOnlyAppContainers(t *testing.T) {
	d := &fakeDocker{
		items: []container.Summary{
			{ID: "web", Labels: map[string]string{labelManaged: "true", labelApp: "app_1"}},
			{ID: "stuck", Labels: map[string]string{labelManaged: "true", labelApp: "app_2"}},
			{ID: "worker", Labels: map[string]string{labelManaged: "true", labelApp: "app_2"}},
			{ID: "edge", Labels: map[string]string{labelManaged: "true", labelApp: ""}},
		},
		stopErr: map[string]bool{"stuck": true},
	}
	n, err := DockerFencer{Docker: d}.Fence(context.Background())
	require.ErrorContains(t, err, "could not stop app container stuck")
	require.Equal(t, 2, n)
	require.Equal(t, []string{"web", "worker"}, d.stopped)

	d = &fakeDocker{listErr: errors.New("socket gone")}
	_, err = DockerFencer{Docker: d}.Fence(context.Background())
	require.ErrorContains(t, err, "could not list")
	require.False(t, strings.Contains(err.Error(), "\n"))
}
