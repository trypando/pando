package hosts_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// joinedHost joins an app host to c and returns its files and what renews
// its certificates.
func joinedHost(t *testing.T, c *cluster, clock *testClock) (hosts.Joined, *hosts.Joiner, *hosts.HostRenewal, *hosts.HostCerts) {
	t.Helper()
	j := joiningHost(t)
	joined, err := j.Join(context.Background(), c.token("app"), "edge-2")
	require.NoError(t, err)
	dir := j.Files.Dir
	host, err := tls.LoadX509KeyPair(filepath.Join(dir, hosts.HostCertFile), filepath.Join(dir, hosts.HostKeyFile))
	require.NoError(t, err)
	probe, err := tls.LoadX509KeyPair(filepath.Join(dir, hosts.HostClientCertFile), filepath.Join(dir, hosts.HostClientKeyFile))
	require.NoError(t, err)
	r := &hosts.HostRenewal{
		Address: c.srv.URL, Dir: dir, Name: joined.Name,
		AuthorityPEM: []byte(read(t, filepath.Join(dir, hosts.AuthorityCertFile))), Now: clock.Now,
	}
	return joined, j, r, r.Certs(host, probe)
}

func verifyAt(t *testing.T, c *cluster, cert tls.Certificate, at time.Time) error {
	t.Helper()
	pool := x509.NewCertPool()
	pool.AddCert(c.svc.Authority.Cert)
	_, err := cert.Leaf.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny}})
	return err
}

// TestR421_CertificatesRenewBeforeExpiryAndA100DayClockLeavesTheClusterWorking
// asserts R-421: a joined host's certificates are issued for 90 days, and
// the host renews both from the cluster listener once its own has 30 left,
// so 100 days on it holds certificates that are valid, that the cluster
// takes as the host's, and that are on disk for its next start, where the
// ones it joined with have expired.
func TestR421_CertificatesRenewBeforeExpiryAndA100DayClockLeavesTheClusterWorking(t *testing.T) {
	clock := &testClock{now: time.Now()}
	c := newClusterAt(t, clock.Now)
	joined, j, _, certs := joinedHost(t, c, clock)
	ctx := context.Background()
	first := certs.Host.Current()

	renewed, err := certs.Host.Refresh(ctx)
	require.NoError(t, err)
	require.False(t, renewed, "not due on the day it was issued")

	renewals := 0
	for day := 5; day <= 100; day += 5 {
		clock.Advance(5 * 24 * time.Hour)
		did, err := certs.Host.Refresh(ctx)
		require.NoError(t, err, "day %d", day)
		if did {
			renewals++
		}
		cur := certs.Host.Current()
		require.NoError(t, verifyAt(t, c, cur, clock.Now()), "the host's certificate on day %d", day)
		require.NoError(t, verifyAt(t, c, certs.Probe.Current(), clock.Now()), "the probe certificate on day %d", day)
		from, ok := hostagent.ProbeSender(certs.Probe.Current().Leaf)
		require.True(t, ok)
		require.Equal(t, joined.HostID, from, "the probe certificate names the host")
		_, err = c.svc.Peers(ctx, cur.Certificate[0])
		require.NoError(t, err, "the cluster takes the host's certificate on day %d", day)
	}
	require.Equal(t, 1, renewals, "renewed once, at day 60, with 30 days left; the next is due at day 120")
	require.Error(t, verifyAt(t, c, first, clock.Now()), "the certificate it joined with has expired")

	onDisk, err := tls.LoadX509KeyPair(filepath.Join(j.Files.Dir, hosts.HostCertFile), filepath.Join(j.Files.Dir, hosts.HostKeyFile))
	require.NoError(t, err)
	require.Equal(t, certs.Host.Current().Certificate[0], onDisk.Certificate[0], "the renewed certificate is what the next start reads")
}

// lossy carries requests and, while drop is set, loses the next answer: the
// request is made and the client is told the connection failed.
type lossy struct {
	base http.RoundTripper
	drop bool
}

func (l *lossy) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := l.base.RoundTrip(req)
	if err == nil && l.drop {
		l.drop = false
		_ = resp.Body.Close()
		return nil, errors.New("connection reset by peer")
	}
	return resp, err
}

// losing makes r's renewals go through a lossy transport.
func losing(r *hosts.HostRenewal) *lossy {
	l := &lossy{}
	r.HTTP = func(cfg *tls.Config) *http.Client {
		l.base = &http.Transport{TLSClientConfig: cfg}
		return &http.Client{Transport: l}
	}
	return l
}

// TestR421_ARenewedHostKeepsBothCertificatesUntilItUsesTheNewOne asserts
// R-421's handover: after a renewal both certificates are the host's, so
// one whose answer was lost keeps working with the one it holds and, renewing
// again, is given the certificate already issued; once the host presents the
// new one the old one is no longer its own.
func TestR421_ARenewedHostKeepsBothCertificatesUntilItUsesTheNewOne(t *testing.T) {
	clock := &testClock{now: time.Now()}
	c := newClusterAt(t, clock.Now)
	_, _, r, certs := joinedHost(t, c, clock)
	ctx := context.Background()
	old := certs.Host.Current()
	oldPin := hostagent.Fingerprint256(old.Certificate[0])

	clock.Advance(61 * 24 * time.Hour)
	losing(r).drop = true
	_, _, err := r.Renew(ctx, old)
	require.Error(t, err, "the answer was lost")
	issued := c.store.hosts[0].CertSHA256
	require.Equal(t, issued+","+oldPin, c.store.hosts[0].Pins(), "the runtime pins both")
	_, err = c.svc.Peers(ctx, old.Certificate[0])
	require.NoError(t, err, "the one it holds is still its own")

	// The host renews again with the old one, and is given what was issued.
	again, _, err := r.Renew(ctx, old)
	require.NoError(t, err)
	againPin := hostagent.Fingerprint256(again.Certificate[0])
	require.Equal(t, issued, againPin, "the certificate already issued, not another")
	require.Equal(t, againPin+","+oldPin, c.store.hosts[0].Pins())
	require.NoFileExists(t, filepath.Join(r.Dir, hosts.HostNextKeyFile), "the next key is used up")

	_, err = c.svc.Peers(ctx, again.Certificate[0])
	require.NoError(t, err)
	require.Equal(t, againPin, c.store.hosts[0].Pins(), "presenting the new one ends the old one")
	_, err = c.svc.Peers(ctx, old.Certificate[0])
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	_, _, err = r.Renew(ctx, old)
	require.Error(t, err, "and it renews nothing")
}

// TestR421_OverlappingRenewalsNeverLockAHostOut asserts R-421 for two
// renewals presenting one certificate: only the first is issued a
// certificate. A second asking for the same key — the host's own renewal
// tried again, or made twice at once — is answered with that certificate,
// and one asking for another key is refused. Whichever answer the host
// installs, or none, the runtime pins the certificate it ends up with.
func TestR421_OverlappingRenewalsNeverLockAHostOut(t *testing.T) {
	clock := &testClock{now: time.Now()}
	c := newClusterAt(t, clock.Now)
	_, _, _, certs := joinedHost(t, c, clock)
	ctx := context.Background()
	old := certs.Host.Current()
	oldPin := hostagent.Fingerprint256(old.Certificate[0])
	clock.Advance(61 * 24 * time.Hour)

	csr := csrFor(t, "edge-2")
	first, err := c.svc.Renew(ctx, old.Certificate[0], csr)
	require.NoError(t, err)
	same, err := c.svc.Renew(ctx, old.Certificate[0], csr)
	require.NoError(t, err)
	require.Equal(t, first.Certificate, same.Certificate, "the same key is answered with the same certificate")

	_, err = c.svc.Renew(ctx, old.Certificate[0], csrFor(t, "edge-2"))
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err), "another key is refused")
	require.Contains(t, errs.As(err).Remedy, "Restart Pando on the host")

	block, _ := pem.Decode(first.Certificate)
	require.NotNil(t, block)
	pins := strings.Split(c.store.hosts[0].Pins(), ",")
	require.ElementsMatch(t, []string{hostagent.Fingerprint256(block.Bytes), oldPin}, pins,
		"the pins hold the one the host was answered with and the one it holds")
}

// TestR415_ARemovedHostsCertificatesAreRefused asserts R-415's removal: a
// removed host's certificates are refused by the peer list and by renewal.
func TestR415_ARemovedHostsCertificatesAreRefused(t *testing.T) {
	clock := &testClock{now: time.Now()}
	c := newClusterAt(t, clock.Now)
	joined, _, r, certs := joinedHost(t, c, clock)
	ctx := context.Background()
	cur := certs.Host.Current()

	removed, err := c.svc.Remove(ctx, joined.HostID)
	require.NoError(t, err)
	require.Empty(t, removed.Rotate, "an app host was handed no key")
	require.True(t, c.store.revoked[hostagent.Fingerprint256(cur.Certificate[0])])

	_, err = c.svc.Peers(ctx, cur.Certificate[0])
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	_, _, err = r.Renew(ctx, cur)
	require.Error(t, err)
	_, err = c.svc.Renew(ctx, cur.Certificate[0], nil)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	_, err = c.svc.Renew(ctx, nil, nil)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
}

// TestR414_TheInstallHostAndThisHostCannotBeRemoved asserts R-414: the
// install's own first host, and the host serving the request, are refused;
// removing a control host names the keys it was handed.
func TestR414_TheInstallHostAndThisHostCannotBeRemoved(t *testing.T) {
	store := &fakeStore{hosts: []state.Host{
		{ID: "host_first", Name: "control", Roles: []string{"app", "control"}},
		{ID: "host_self", Name: "c-2", Roles: []string{"control"}, JoinedWith: "jt_1"},
		{ID: "host_other", Name: "c-3", Roles: []string{"app", "control"}, JoinedWith: "jt_1"},
	}}
	svc := newService(t, store)
	svc.IsSelf = func(h state.Host) bool { return h.ID == "host_self" }
	ctx := context.Background()

	_, err := svc.Remove(ctx, "host_first")
	require.ErrorIs(t, err, state.ErrInstallHost)
	_, err = svc.Remove(ctx, "host_self")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "another control host")

	removed, err := svc.Remove(ctx, "host_other")
	require.NoError(t, err)
	require.Equal(t, []string{"secrets key", "API token key", "cluster certificate authority", "database password"}, removed.Rotate)
	require.Contains(t, removed.Notice, "Host c-3 held the control role")
}

// TestR415_ABadAddressIsRefusedAtJoin asserts R-415: what a joining host
// says about itself is checked before it is recorded, so an address the
// runtime cannot dial never reaches every host's runtime configuration, and
// the refusal spends no use of the token.
func TestR415_ABadAddressIsRefusedAtJoin(t *testing.T) {
	c := newCluster(t)
	token := c.token("app")
	for _, addr := range []string{"", "edge-2", "edge-2:http", "edge-2:0", "edge-2:70000", "bad host:7443", "-x-:7443", strings.Repeat("a", 300) + ":7443"} {
		j := joiningHost(t)
		j.AgentAddress = addr
		_, err := j.Join(context.Background(), token, "edge-2")
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "address %q", addr)
		require.Less(t, len(errs.As(err).Message), 300, "the message does not echo all of it")
	}
	for _, p := range []string{strings.Repeat("x", 65), "linux amd64"} {
		j := joiningHost(t)
		j.Platform = p
		_, err := j.Join(context.Background(), token, "edge-2")
		require.Contains(t, errs.As(err).Message, "platform")
	}
	j := joiningHost(t)
	j.Version = strings.Repeat("9", 65)
	_, err := j.Join(context.Background(), token, "edge-2")
	require.Contains(t, errs.As(err).Message, "version")

	for _, addr := range []string{"10.0.0.6:7443", "[fd00::6]:7443", "edge-2.example.com:7443"} {
		require.NoError(t, hosts.CheckAgentAddress(addr), addr)
	}
	_, err = joiningHost(t).Join(context.Background(), token, "edge-2")
	require.NoError(t, err, "no refusal spent a use")
}

// TestR347_ARefusedJoinRevealsNoVersionAndAuditsNoUnboundedName asserts
// R-347 and R-415: what the audit log keeps of a refused join is bounded,
// whatever the caller sent, and a caller without a good token is not told
// the cluster's version.
func TestR347_ARefusedJoinRevealsNoVersionAndAuditsNoUnboundedName(t *testing.T) {
	long := strings.Repeat("n", 64<<10)
	d := hosts.JoinRequest{Name: long, Version: long}.AuditDetail()
	require.Equal(t, hosts.InvalidField, d["name"])
	require.Equal(t, hosts.InvalidField, d["version"])
	d = hosts.JoinRequest{Name: "edge-2", Version: "1.2.3"}.AuditDetail()
	require.Equal(t, map[string]any{"name": "edge-2", "version": "1.2.3"}, d)

	err := hosts.CheckHostName(long)
	require.Less(t, len(errs.As(err).Message), 300)

	c := newCluster(t)
	_, err = c.svc.Join(context.Background(), hosts.JoinRequest{
		Secret: secret.New("jt_01nope.wrong"), Name: "edge-2", Version: "0.0.1", Address: "edge-2:7443",
	})
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err))
	require.NotContains(t, errs.As(err).Message, "1.2.3")
}

func TestHostRenewalRefusesWhatItCannotUse(t *testing.T) {
	r := &hosts.HostRenewal{AuthorityPEM: []byte("not pem")}
	_, _, err := r.Renew(context.Background(), tls.Certificate{})
	require.Error(t, err)

	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "ca.pem"), []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"), 0o600))
	r.AuthorityPEM = []byte(read(t, filepath.Join(dir, "ca.pem")))
	_, _, err = r.Renew(context.Background(), tls.Certificate{})
	require.Error(t, err)
}
