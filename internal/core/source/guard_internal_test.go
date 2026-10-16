package source

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strconv"
	"testing"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR092_AFetchNeverReachesTheMetadataServiceOrPando asserts which
// addresses a repository fetch may connect to (2026-10-16 assessment,
// finding 3): the private network and this machine are allowed, since an
// install's own Gitea is a normal place to deploy from, and link-local,
// unspecified and multicast addresses, Pando's own port and its database are
// not.
func TestR092_AFetchNeverReachesTheMetadataServiceOrPando(t *testing.T) {
	g := &Guard{
		OwnPorts: []uint16{8080},
		Database: []HostPort{{Host: "postgres", Port: 5432}, {Host: "10.0.0.9", Port: 5433}},
		Resolve: func(_ context.Context, host string) ([]netip.Addr, error) {
			if host == "postgres" {
				return []netip.Addr{netip.MustParseAddr("172.18.0.2")}, nil
			}
			return nil, errors.New("no such host")
		},
		LocalAddrs: func() ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("172.18.0.3")}, nil
		},
	}
	for _, tc := range []struct {
		addr    string
		refused bool
	}{
		{"169.254.169.254:80", true},
		{"[fe80::1]:443", true},
		{"[::ffff:169.254.169.254]:80", true},
		{"0.0.0.0:443", true},
		{"224.0.0.1:443", true},
		{"127.0.0.1:8080", true},
		{"[::1]:8080", true},
		{"172.18.0.3:8080", true},
		{"172.18.0.2:5432", true},
		{"10.0.0.9:5433", true},

		{"127.0.0.1:3000", false},
		{"10.0.0.5:443", false},
		{"192.168.1.20:22", false},
		{"172.18.0.2:443", false},
		{"172.18.0.3:3000", false},
		{"140.82.112.3:443", false},
	} {
		err := g.check(context.Background(), netip.MustParseAddrPort(tc.addr))
		if tc.refused {
			require.ErrorIs(t, err, errRefusedAddress, tc.addr)
		} else {
			require.NoError(t, err, tc.addr)
		}
	}

	var none *Guard
	require.NoError(t, none.check(context.Background(), netip.MustParseAddrPort("127.0.0.1:8080")),
		"before startup installs the guard, only the fixed classes are refused")
	require.ErrorIs(t, none.check(context.Background(), netip.MustParseAddrPort("169.254.169.254:80")), errRefusedAddress)
}

// withGuard installs g for the length of a test.
func withGuard(t *testing.T, g Guard) {
	t.Helper()
	prev := guard.Load()
	SetGuard(g)
	t.Cleanup(func() { guard.Store(prev) })
}

// TestR092_AFetchOfAMetadataAddressIsRefusedInWords asserts that a probe of
// an address no repository is at is refused, saying so, rather than being let
// through to fail at detection — the metadata service by its address, and
// Pando's own port on this machine.
func TestR092_AFetchOfAMetadataAddressIsRefusedInWords(t *testing.T) {
	hs := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(hs.Close)
	u, err := url.Parse(hs.URL)
	require.NoError(t, err)
	port, err := strconv.ParseUint(u.Port(), 10, 16)
	require.NoError(t, err)
	withGuard(t, Guard{OwnPorts: []uint16{uint16(port)}})

	for _, addr := range []string{
		"http://169.254.169.254/latest/meta-data.git",
		hs.URL + "/healthz",
		"ssh://git@169.254.169.254/team/app.git",
	} {
		err := Sources{}.Probe(context.Background(), spec.Source{Type: spec.SourceGit, URL: addr})
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "%s: %v", addr, err)
		require.Contains(t, err.Error(), "did not connect", addr)
		require.NotEmpty(t, errs.As(err).Remedy)
	}
}

// TestR092_ARedirectCannotBounceAFetchInward asserts that the check is made
// on every connection an HTTP fetch dials, not only on the address it was
// given: a repository host that answers with a redirect to Pando's own port
// gets nowhere.
func TestR092_ARedirectCannotBounceAFetchInward(t *testing.T) {
	inner := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(inner.Close)
	_, innerPort, err := net.SplitHostPort(inner.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.ParseUint(innerPort, 10, 16)
	require.NoError(t, err)
	withGuard(t, Guard{OwnPorts: []uint16{uint16(port)}})

	outer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, inner.URL+r.URL.RequestURI(), http.StatusFound)
	}))
	t.Cleanup(outer.Close)

	_, err = listRefs(context.Background(), &gitAccess{url: outer.URL + "/team/app.git"})
	require.Error(t, err)
	require.ErrorContains(t, accessError(err, outer.URL, ""), "did not connect")
}

// TestR092_ALocalPathIsNotARepositoryAddress asserts that file:// and a bare
// path on Pando's own disk are refused by the fetch itself, not only at
// validation, and that git:// is not fetched at all.
func TestR092_ALocalPathIsNotARepositoryAddress(t *testing.T) {
	prev := localRepositories.Load()
	localRepositories.Store(false)
	t.Cleanup(func() { localRepositories.Store(prev) })

	for _, addr := range []string{"/etc", "file:///etc"} {
		_, err := Sources{}.Fetch(context.Background(), spec.Source{Type: spec.SourceGit, URL: addr})
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), addr)
		require.Contains(t, err.Error(), "is a path on the machine Pando runs on", addr)
	}

	_, err := Sources{}.Fetch(context.Background(), spec.Source{Type: spec.SourceGit, URL: "git://example.com/team/app.git"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unsupported scheme")
}

// TestR092_TheGuardsDefaultsAskTheSystem asserts the guard as startup
// installs it, with no stubs: this machine's own interface addresses on
// Pando's port, and a database named by a host name the system resolves.
func TestR092_TheGuardsDefaultsAskTheSystem(t *testing.T) {
	g := &Guard{OwnPorts: []uint16{8080}, Database: []HostPort{
		{Host: "localhost", Port: 5432},
		{Host: "10.0.0.9", Port: 5432},
		{Host: "no-such-host.invalid", Port: 5432},
	}}
	ctx := context.Background()

	local, err := interfaceAddrs()
	require.NoError(t, err)
	for _, a := range local {
		if !refusedClass(a) {
			require.ErrorIs(t, g.check(ctx, netip.AddrPortFrom(a, 8080)), errRefusedAddress, a.String())
		}
	}
	require.NoError(t, g.check(ctx, netip.MustParseAddrPort("203.0.113.7:8080")), "Pando's port elsewhere is someone else's")

	require.ErrorIs(t, g.check(ctx, netip.MustParseAddrPort("127.0.0.1:5432")), errRefusedAddress, "localhost resolves to the database")
	require.NoError(t, g.check(ctx, netip.MustParseAddrPort("203.0.113.7:5432")))

	withGuard(t, *g)
	require.ErrorIs(t, checkHost(ctx, "localhost", 8080), errRefusedAddress)
	require.Error(t, guardedControl(ctx, "tcp", "not-an-address", nil))
	require.ErrorIs(t, guardedControl(ctx, "tcp", "169.254.169.254:80", nil), errRefusedAddress)
}

// stubTransport opens a session for anything it is asked.
type stubTransport struct{ transport.Transport }

func (stubTransport) NewUploadPackSession(*transport.Endpoint, transport.AuthMethod) (transport.UploadPackSession, error) {
	return nil, nil
}

func (stubTransport) NewReceivePackSession(*transport.Endpoint, transport.AuthMethod) (transport.ReceivePackSession, error) {
	return nil, nil
}

// TestR092_EverySessionIsGuarded asserts that both kinds of session go
// through the guard and the local-path refusal, not only the fetch Pando
// makes today: a push is the same connection the other way.
func TestR092_EverySessionIsGuarded(t *testing.T) {
	metadata, err := transport.NewEndpoint("http://169.254.169.254/team/app.git")
	require.NoError(t, err)
	forge, err := transport.NewEndpoint("https://203.0.113.7/team/app.git")
	require.NoError(t, err)

	g := guarded{stubTransport{}, 443}
	_, err = g.NewReceivePackSession(metadata, nil)
	require.ErrorIs(t, err, errRefusedAddress)
	_, err = g.NewReceivePackSession(forge, nil)
	require.NoError(t, err)

	prev := localRepositories.Load()
	t.Cleanup(func() { localRepositories.Store(prev) })
	l := localOnly{stubTransport{}}

	localRepositories.Store(false)
	_, err = l.NewReceivePackSession(forge, nil)
	require.ErrorIs(t, err, errLocalRepository)
	_, err = l.NewUploadPackSession(forge, nil)
	require.ErrorIs(t, err, errLocalRepository)

	localRepositories.Store(true)
	_, err = l.NewReceivePackSession(forge, nil)
	require.NoError(t, err)
}

func TestCheckHostLetsAnUnresolvableNameThroughToGoGit(t *testing.T) {
	require.NoError(t, checkHost(context.Background(), "no-such-host.invalid", 443))
	require.ErrorIs(t, checkHost(context.Background(), "169.254.169.254", 80), errRefusedAddress)
	require.Equal(t, 22, portOr(0, 22))
	require.Equal(t, 2222, portOr(2222, 22))
}
