// Package clientaddr decides which address a request came from, for the audit
// log (R-379, R-380), and carries it on the request's context to the audit
// writer.
//
// A leaf: the API, the proxy and the audit writer all import it, and it
// imports nothing of Pando's.
package clientaddr

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Trusted is the set of proxies whose X-Forwarded-For is believed. The zero
// value trusts nobody, which records the connecting peer — what Pando did
// before issue #129.
type Trusted struct {
	prefixes []netip.Prefix
}

// The narrowest prefixes refused as a trusted proxy: anything wider makes the
// header the client's to write for most of the internet (design 12 §3.2).
const (
	minIPv4Bits = 8
	minIPv6Bits = 16
)

// Parse reads a comma- or space-separated list of addresses and CIDRs. A bare
// address is a single host. An empty list trusts nobody.
func Parse(list string) (Trusted, error) {
	var t Trusted
	sep := func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }
	for _, item := range strings.FieldsFunc(list, sep) {
		var p netip.Prefix
		var err error
		if strings.Contains(item, "/") {
			p, err = netip.ParsePrefix(item)
		} else {
			var a netip.Addr
			if a, err = netip.ParseAddr(item); err == nil {
				p = netip.PrefixFrom(a, a.BitLen())
			}
		}
		if err != nil {
			return Trusted{}, fmt.Errorf(
				"PANDO_SERVER_TRUSTED_PROXIES has %q, which is not an address or a CIDR range. "+
					"Valid answer: the addresses of the proxies in front of Pando, such as 10.0.0.5 or 10.0.0.0/24, separated by commas", item)
		}
		p = p.Masked()
		if limit := minBits(p.Addr()); p.Bits() < limit {
			return Trusted{}, fmt.Errorf(
				"PANDO_SERVER_TRUSTED_PROXIES has %s, which covers most of the internet, so Pando would record whatever "+
					"address a client wrote in X-Forwarded-For. List only the proxies in front of Pando: a range of /%d or narrower",
				p, limit)
		}
		t.prefixes = append(t.prefixes, p)
	}
	return t, nil
}

func minBits(a netip.Addr) int {
	if a.Is4() || a.Is4In6() {
		return minIPv4Bits
	}
	return minIPv6Bits
}

// Empty reports whether no proxy is trusted.
func (t Trusted) Empty() bool { return len(t.prefixes) == 0 }

func (t Trusted) trusts(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range t.prefixes {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Resolve returns the client's address and the connecting peer's.
//
// The peer is RemoteAddr's host. When the peer is trusted, X-Forwarded-For is
// read from the right and the first address that is not itself trusted is the
// client: the left of the header is whatever the client sent, and each trusted
// hop appends on the right. A hop that cannot be read ends the chain, and a
// chain of only trusted hops ends at its left-most.
func (t Trusted) Resolve(r *http.Request) (source, peer string) {
	peer = hostOf(r.RemoteAddr)
	source = peer
	pa, err := netip.ParseAddr(peer)
	if err != nil || !t.trusts(pa) {
		return source, peer
	}
	var hops []string
	for _, h := range r.Header.Values("X-Forwarded-For") {
		hops = append(hops, strings.Split(h, ",")...)
	}
	for i := len(hops) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(hops[i]))
		if err != nil {
			// What is to the left of an unreadable hop was not appended by
			// anything Pando trusts.
			return source, peer
		}
		a = a.Unmap()
		if !t.trusts(a) {
			return a.String(), peer
		}
		source = a.String()
	}
	return source, peer
}

func hostOf(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// Request is what the audit log records about where a request came from.
type Request struct {
	SourceIP  string
	PeerIP    string
	UserAgent string
}

// maxUserAgent bounds what a client can put in every audit row it causes.
const maxUserAgent = 512

// Of describes a request, as t resolves it.
func (t Trusted) Of(r *http.Request) Request {
	source, peer := t.Resolve(r)
	ua := r.UserAgent()
	if len(ua) > maxUserAgent {
		ua = ua[:maxUserAgent]
	}
	return Request{SourceIP: source, PeerIP: peer, UserAgent: ua}
}

type ctxKey struct{}

// With returns ctx carrying req.
func With(ctx context.Context, req Request) context.Context {
	return context.WithValue(ctx, ctxKey{}, req)
}

// From returns the request ctx carries, if any. A system process's context
// carries none, and its audit events record no source.
func From(ctx context.Context) (Request, bool) {
	req, ok := ctx.Value(ctxKey{}).(Request)
	return req, ok
}

// Middleware records each request's addresses on its context.
func (t Trusted) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(With(r.Context(), t.Of(r))))
	})
}
