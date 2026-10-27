package hostagent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// fakeClock is a clock a test moves by hand.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// hostCertAt issues the host name's certificate as of now.
func hostCertAt(t *testing.T, as Authorities, name string, now time.Time) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := as.IssueHostAt(now, name, []string{"app"}, &key.PublicKey)
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}
}

// handshake connects client to a listener serving server, both checking
// certificates as of clock.
func handshake(t *testing.T, server, client *tls.Config, clock *fakeClock) error {
	t.Helper()
	server, client = server.Clone(), client.Clone()
	server.Time, client.Time = clock.Now, clock.Now
	ln, err := tls.Listen("tcp", "127.0.0.1:0", server)
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	done := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = c.Close() }()
		done <- c.(*tls.Conn).Handshake()
	}()
	conn, err := tls.Dial("tcp", ln.Addr().String(), client)
	if err == nil {
		_ = conn.Close()
	}
	serr := <-done
	return errors.Join(err, serr)
}

// TestR421_CertificatesRenewBeforeTheyExpire asserts R-421: a host's own
// certificate and Pando's proxy's are issued again before they expire, and
// a process taking them at each handshake still connects 100 days on, where
// the same certificates issued once fail.
func TestR421_CertificatesRenewBeforeTheyExpire(t *testing.T) {
	as := testAuthorities(t)
	clock := &fakeClock{now: time.Now()}
	host := hostCertAt(t, as, "app-1", clock.Now())
	proxy, err := as.issueAt(clock.Now(), &x509.Certificate{Subject: pkix.Name{CommonName: ClientCommonName, OrganizationalUnit: []string{RoleControl}}}, x509.ExtKeyUsageClientAuth)
	require.NoError(t, err)

	// Issued once: fine today, and every handshake fails past day 90.
	static, err := RenewingServerTLS(nil, as.PoolPEM())
	require.NoError(t, err)
	static.Certificates = []tls.Certificate{host}
	staticClient := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{proxy}, RootCAs: as.Pool(), ServerName: ServerName("app-1")}
	require.NoError(t, handshake(t, static, staticClient, clock))
	later := &fakeClock{now: clock.Now().Add(100 * 24 * time.Hour)}
	require.ErrorContains(t, handshake(t, static, staticClient, later), "expired")

	// Renewed: the host's from its own renewal, the proxy's in memory.
	served := NewRenewingCert(host, func(_ context.Context, now time.Time) (tls.Certificate, error) {
		return hostCertAt(t, as, "app-1", now), nil
	})
	served.Now, served.OnHandshake = clock.Now, true
	presented := as.InMemory(proxy)
	presented.Now = clock.Now
	server, err := RenewingServerTLS(served, as.PoolPEM())
	require.NoError(t, err)
	client := &tls.Config{MinVersion: tls.VersionTLS13, GetClientCertificate: presented.GetClientCertificate, RootCAs: as.Pool(), ServerName: ServerName("app-1")}

	for day := 0; day <= 100; day += 10 {
		require.NoError(t, handshake(t, server, client, clock), "day %d", day)
		clock.Advance(10 * 24 * time.Hour)
	}
	require.NotEqual(t, host.Certificate[0], served.Current().Certificate[0], "the host's certificate was renewed")
	require.NotEqual(t, proxy.Certificate[0], presented.Current().Certificate[0], "the proxy's certificate was renewed")
	require.Equal(t, ClientCommonName, presented.Current().Leaf.Subject.CommonName, "renewed as what it was")
	require.False(t, presented.Due())
}

// TestRenewingCertKeepsTheOldOneWhenRenewalFails asserts that a renewal that
// fails leaves the certificate held in use, says why, and is not retried on
// every handshake.
func TestRenewingCertKeepsTheOldOneWhenRenewalFails(t *testing.T) {
	as := testAuthorities(t)
	clock := &fakeClock{now: time.Now()}
	cert := hostCertAt(t, as, "app-1", clock.Now())
	calls := 0
	var told error
	r := NewRenewingCert(cert, func(context.Context, time.Time) (tls.Certificate, error) {
		calls++
		return tls.Certificate{}, errors.New("the cluster is away")
	})
	r.Now, r.OnHandshake, r.OnError = clock.Now, true, func(err error) { told = err }

	got, err := r.GetCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, cert.Certificate[0], got.Certificate[0])
	require.Zero(t, calls, "not due yet")

	clock.Advance(70 * 24 * time.Hour)
	_, _ = r.GetClientCertificate(nil)
	_, _ = r.GetClientCertificate(nil)
	require.Equal(t, 1, calls, "retried after a while, not on every handshake")
	require.ErrorContains(t, told, "the cluster is away")
	require.Equal(t, cert.Certificate[0], r.Current().Certificate[0])

	clock.Advance(2 * time.Minute)
	renewed, err := r.Refresh(context.Background())
	require.Error(t, err)
	require.False(t, renewed)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r.Run(ctx, time.Hour)
	require.Equal(t, 3, calls)

	require.Nil(t, leafOf(tls.Certificate{}))
	require.Nil(t, leafOf(tls.Certificate{Certificate: [][]byte{[]byte("not der")}}))
	bad := as.InMemory(tls.Certificate{})
	_, err = bad.Renew(context.Background(), clock.Now())
	require.Error(t, err)
}

// TestClientTLSSharesOneRenewal asserts that configurations made for one
// client certificate renew it once between them, and that one made with no
// certificate presents none.
func TestClientTLSSharesOneRenewal(t *testing.T) {
	as := testAuthorities(t)
	client, err := as.IssueClient()
	require.NoError(t, err)
	a, b := ClientTLS(as, client, "h"), ClientTLS(as, client, "h")
	ca, err := a.GetClientCertificate(nil)
	require.NoError(t, err)
	cb, err := b.GetClientCertificate(nil)
	require.NoError(t, err)
	require.Equal(t, ca.Certificate[0], cb.Certificate[0])
	require.Len(t, ClientTLS(as, tls.Certificate{}, "h").Certificates, 1)
}

// TestR421_AHostCannotClaimAnothersIdentityInAPing asserts R-421: a ping is
// taken to be from the host its certificate was issued to. A host naming
// another, a host naming the collector, the collector naming a host, and a
// certificate that names no host are refused, so one host cannot answer for
// the others and keep a cut-off host from fencing.
func TestR421_AHostCannotClaimAnothersIdentityInAPing(t *testing.T) {
	live := &fakeLiveness{}
	addr, as := pingAgent(t, live)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const other = "host_01HQ8YYYYYYYYYYYYYYYYYYYYY"

	_, err := Ping(ctx, addr, hostTLS(t, as), other)
	require.ErrorContains(t, err, "the ping names")
	_, err = Ping(ctx, addr, hostTLS(t, as), "")
	require.ErrorContains(t, err, "the ping names")
	_, err = Ping(ctx, addr, collectorTLS(t, as), peerID)
	require.ErrorContains(t, err, "the ping names")

	// The one certificate every host used to share, which named nobody.
	shared, err := as.issue(pkix.Name{CommonName: HostCommonName}, nil, x509.ExtKeyUsageClientAuth)
	require.NoError(t, err)
	_, err = Ping(ctx, addr, ClientTLS(as, shared, "host-1"), other)
	require.ErrorContains(t, err, "issued to no host of the cluster")
	require.Empty(t, live.senders(), "no refused ping was recorded")

	_, err = Ping(ctx, addr, hostTLSFor(t, as, other), other)
	require.NoError(t, err)
	require.Equal(t, []string{other}, live.senders())

	// Two host IDs, or an ID and the collector's mark: neither.
	two, err := as.issueAt(time.Now(), &x509.Certificate{
		Subject: pkix.Name{CommonName: HostCommonName, OrganizationalUnit: []string{CollectorUnit}},
		URIs:    mustCert(t, as, peerID).Leaf.URIs,
	}, x509.ExtKeyUsageClientAuth)
	require.NoError(t, err)
	_, ok := ProbeSender(two.Leaf)
	require.False(t, ok)
	_, ok = ProbeSender(nil)
	require.False(t, ok)
}

func mustCert(t *testing.T, as Authorities, hostID string) tls.Certificate {
	t.Helper()
	c, err := as.IssueHostClient(hostID)
	require.NoError(t, err)
	return c
}

// TestR415_ARemovedHostsPingsAreRefused asserts R-415's removal: an agent
// refuses a ping from a host no longer in the cluster, whatever certificate
// the authority issued it.
func TestR415_ARemovedHostsPingsAreRefused(t *testing.T) {
	as := testAuthorities(t)
	certPEM, keyPEM, err := as.IssueServer("host-1")
	require.NoError(t, err)
	serverTLS, err := ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	live := &fakeLiveness{}
	agent := &Agent{TLS: serverTLS, Pool: testPool, Liveness: live, Member: func(id string) bool { return id != peerID }}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })

	_, err = Ping(context.Background(), ln.Addr().String(), hostTLS(t, as), peerID)
	require.ErrorContains(t, err, "is not in this cluster")
	require.Empty(t, live.senders())
}
