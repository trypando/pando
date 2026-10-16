package source

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
	"sync/atomic"
	"syscall"
)

// What a repository fetch may connect to (R-092; 2026-10-16 assessment,
// finding 3).
//
// A repository can be on the private network, or on the same machine: an
// install's own Gitea is a normal place to deploy from, so private and
// loopback addresses are allowed. Refused are the addresses no repository is
// ever at and that a fetch must never reach on someone's behalf: link-local
// ones, which is where a cloud's metadata service answers (169.254.169.254),
// unspecified and multicast ones, and Pando's own — its listening port on any
// address of this machine, and its database.
//
// Checked on the address actually dialed, after DNS, so a name that resolves
// to something harmless when it is checked and to the metadata service when
// it is used gets nowhere.

// Guard is the endpoints a fetch must not reach beyond the fixed classes.
type Guard struct {
	// OwnPorts are the ports Pando listens on. Refused on loopback and on
	// every address of this machine's interfaces.
	OwnPorts []uint16

	// Database is where Pando's state store is, as host and port. A host
	// name is resolved when a dial is checked, since it may be a Compose
	// service name whose address changes.
	Database []HostPort

	// Resolve looks a database host up. net.DefaultResolver when nil.
	Resolve func(ctx context.Context, host string) ([]netip.Addr, error)

	// LocalAddrs lists this machine's interface addresses.
	// net.InterfaceAddrs when nil.
	LocalAddrs func() ([]netip.Addr, error)
}

// HostPort is a host and the port on it.
type HostPort struct {
	Host string
	Port uint16
}

// errRefusedAddress is what a refused dial fails with.
var errRefusedAddress = errors.New("this address is never a repository's")

// errLocalRepository is what a file:// address or a local path fails with.
var errLocalRepository = errors.New("a path on Pando's own disk is not a repository address")

var guard atomic.Pointer[Guard]

// SetGuard installs the endpoints Pando's own fetches must not reach. Called
// once at startup; until then only the fixed classes are refused.
func SetGuard(g Guard) { guard.Store(&g) }

// refusedClass is an address no repository is ever at: link-local (the
// metadata service among them), unspecified and multicast.
func refusedClass(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() || a.IsInterfaceLocalMulticast() ||
		a.IsUnspecified() || a.IsMulticast()
}

// check refuses an address a fetch must not connect to.
func (g *Guard) check(ctx context.Context, ap netip.AddrPort) error {
	a := ap.Addr().Unmap()
	if refusedClass(a) {
		return errRefusedAddress
	}
	if g == nil {
		return nil
	}
	if g.isOwnPort(a, ap.Port()) || g.isDatabase(ctx, a, ap.Port()) {
		return errRefusedAddress
	}
	return nil
}

// isOwnPort is Pando's listening port on loopback or on an address of this
// machine.
func (g *Guard) isOwnPort(a netip.Addr, port uint16) bool {
	if !slices.Contains(g.OwnPorts, port) {
		return false
	}
	if a.IsLoopback() {
		return true
	}
	local := g.LocalAddrs
	if local == nil {
		local = interfaceAddrs
	}
	addrs, err := local()
	return err == nil && containsAddr(addrs, a)
}

// isDatabase is one of the database's hosts at its port. A host that does not
// resolve is no address at all.
func (g *Guard) isDatabase(ctx context.Context, a netip.Addr, port uint16) bool {
	for _, db := range g.Database {
		if db.Port == port && containsAddr(g.resolve(ctx, db.Host), a) {
			return true
		}
	}
	return false
}

// resolve is host's addresses: itself when it is one, or what DNS says.
func (g *Guard) resolve(ctx context.Context, host string) []netip.Addr {
	if ip, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{ip}
	}
	lookup := g.Resolve
	if lookup == nil {
		lookup = func(ctx context.Context, host string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		}
	}
	addrs, _ := lookup(ctx, host)
	return addrs
}

func containsAddr(addrs []netip.Addr, a netip.Addr) bool {
	return slices.ContainsFunc(addrs, func(x netip.Addr) bool { return x.Unmap() == a })
}

func interfaceAddrs() ([]netip.Addr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	out := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if p, err := netip.ParsePrefix(a.String()); err == nil {
			out = append(out, p.Addr())
		}
	}
	return out, nil
}

// guardedControl is the dialer hook that enforces the guard on every
// connection an HTTP fetch makes, redirects included.
func guardedControl(ctx context.Context, _, address string, _ syscall.RawConn) error {
	ap, err := netip.ParseAddrPort(address)
	if err != nil {
		return err
	}
	return guard.Load().check(ctx, ap)
}

// checkHost resolves host and refuses it when any address it has is refused,
// before a session opens, so the refusal is said in words. It is not what
// enforces the guard: every dial, HTTP and SSH, is checked again on the
// socket by guardedDialer, which is what a name re-pointed in between meets.
func checkHost(ctx context.Context, host string, port int) error {
	// A name that does not resolve leaves nothing to check: not reachable is
	// go-git's to report, in its own words.
	var addrs []netip.Addr
	if ip, err := netip.ParseAddr(host); err == nil {
		addrs = []netip.Addr{ip}
	} else if resolved, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host); err == nil {
		addrs = resolved
	}
	g := guard.Load()
	for _, a := range addrs {
		if err := g.check(ctx, netip.AddrPortFrom(a, uint16(port))); err != nil { //nolint:gosec // G115: a port from a parsed URL
			return err
		}
	}
	return nil
}

// portOr is the endpoint's port, or def when it names none.
func portOr(port, def int) int {
	if port == 0 {
		return def
	}
	return port
}
