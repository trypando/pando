package source

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"

	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/client"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
)

// gitHTTPClient is the client every git fetch over HTTP uses.
//
// go-git's default has no timeouts of any kind. After a while in use, the first
// request of every clone — `GET …/info/refs` — hung until the ten-minute
// detection deadline, while the same URL answered in a quarter of a second from
// inside the same container, and restarting Pando cleared it (issue #55). That
// is a pooled connection that died without closing: every clone to the host is
// multiplexed onto one HTTP/2 connection, and with no health check nothing ever
// notices it is gone.
//
// So: HTTP/2 pings a connection that has gone quiet and drops it when the ping
// is not answered, a server that accepts a request and never starts answering
// is given up on, and idle connections are not kept long enough to go stale.
// None of these bound a clone that is making progress — a large repository
// still streams for as long as it takes, under the caller's context.
//
// Every connection it opens, a redirect's included, is checked against the
// guard on the address dialed (guard.go) — and through a proxy, on the
// repository's host as well (guardedProxy).
var gitHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 guardedProxy,
		DialContext:           guardedDialer.DialContext,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   30 * time.Second,
		ResponseHeaderTimeout: 2 * time.Minute,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   4,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: 30 * time.Second,
			PingTimeout:     15 * time.Second,
		},
	},
}

// proxyFor is the proxy a request goes through, from HTTPS_PROXY, HTTP_PROXY
// and NO_PROXY. A variable so a test can give one without the environment,
// which net/http reads once per process.
var proxyFor = http.ProxyFromEnvironment

// guardedProxy is the proxy a fetch's request goes through, if any, after the
// repository's host has been checked against the guard.
//
// Behind a proxy the dial the guard sees is to the proxy, which the host's
// operator chose and which is inside what Pando trusts its operator with
// (R-087); refusing proxies would break every install behind one. So the
// repository's host is resolved here and refused if any address it has is
// refused, an IP literal included. Best effort, and said so: the proxy
// resolves the name again for itself, and may get another answer. The
// transport asks this for every request, a redirect's included.
func guardedProxy(req *http.Request) (*url.URL, error) {
	p, err := proxyFor(req)
	if err != nil || p == nil {
		return p, err
	}
	def := uint16(443)
	if req.URL.Scheme == "http" {
		def = 80
	}
	if err := checkHost(req.Context(), req.URL.Hostname(), urlPort(req.URL.Port(), def)); err != nil {
		return nil, err
	}
	return p, nil
}

// The protocols a repository is fetched over: https, http and ssh, and nothing
// else (2026-10-16 assessment, finding 3). go-git also registers file and
// git by default, and a file:// URL — or a bare local path, which go-git reads
// as one — would read the disk Pando runs on. git is removed, and file
// refuses unless a test allowed it, here rather than only at validation, so
// no address that slipped past it can use them.
func init() {
	c := githttp.NewClient(gitHTTPClient)
	client.InstallProtocol("https", guarded{Transport: c, defaultPort: 443})
	client.InstallProtocol("http", guarded{Transport: c, defaultPort: 80})
	client.InstallProtocol("ssh", guarded{Transport: gitssh.DefaultClient, defaultPort: 22, viaGuardedDialer: true})

	// go-git's SSH transport takes no dialer, but it does dial through an
	// endpoint's proxy URL with golang.org/x/net/proxy, whose schemes a
	// program may add to. This one is the guarded dialer itself, so an SSH
	// connection is checked on the socket exactly as an HTTP one is —
	// including an address an ssh_config Hostname alias points it at.
	proxy.RegisterDialerType(guardedDialScheme, func(*url.URL, proxy.Dialer) (proxy.Dialer, error) {
		return guardedDialer, nil
	})
	for scheme := range client.Protocols {
		switch scheme {
		case "https", "http", "ssh":
		case "file":
			client.InstallProtocol(scheme, localOnly{client.Protocols[scheme]})
		default:
			client.InstallProtocol(scheme, nil)
		}
	}
}

var localRepositories atomic.Bool

// AllowLocalRepositories lets a repository on this machine's disk be fetched,
// for tests that build one in a temporary directory. Pando never calls it:
// a local path is not a source a person can give.
func AllowLocalRepositories() { localRepositories.Store(true) }

// localOnly refuses file:// and bare local paths unless a test allowed them.
type localOnly struct{ transport.Transport }

func (l localOnly) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	if !localRepositories.Load() {
		return nil, errLocalRepository
	}
	return l.Transport.NewUploadPackSession(ep, auth)
}

func (l localOnly) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	if !localRepositories.Load() {
		return nil, errLocalRepository
	}
	return l.Transport.NewReceivePackSession(ep, auth)
}

// guardedDialScheme names the guarded dialer as a proxy URL go-git's SSH
// transport dials through. Not a proxy anyone runs: the "proxy" is the dial.
const guardedDialScheme = "pando-guarded"

// guardedDialer is the dialer every repository connection is made with, HTTP
// and SSH alike: the guard is checked on the address dialed (guard.go).
var guardedDialer = &net.Dialer{
	Timeout:        30 * time.Second,
	KeepAlive:      30 * time.Second,
	ControlContext: guardedControl,
}

// guarded checks the endpoint's host against the guard before a session is
// opened, which is the readable refusal; the dial is checked again on the
// socket. For SSH, viaGuardedDialer points the endpoint at the guarded
// dialer, since that is the only way into go-git's SSH dial.
type guarded struct {
	transport.Transport
	defaultPort      uint16
	viaGuardedDialer bool
}

// endpoint is ep as the inner transport is given it. Pando sets no SSH proxy
// of its own, so the guarded dialer always takes the proxy's place.
func (g guarded) endpoint(ep *transport.Endpoint) *transport.Endpoint {
	if !g.viaGuardedDialer {
		return ep
	}
	e := *ep
	e.Proxy = transport.ProxyOptions{URL: guardedDialScheme + "://dial"}
	return &e
}

// sessionCheckTimeout bounds the lookup made before a session opens: go-git
// gives no context here.
const sessionCheckTimeout = 10 * time.Second

func (g guarded) check(ep *transport.Endpoint) error {
	ctx, cancel := context.WithTimeout(context.Background(), sessionCheckTimeout)
	defer cancel()
	return checkHost(ctx, ep.Host, portOr(ep.Port, g.defaultPort))
}

func (g guarded) NewUploadPackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.UploadPackSession, error) {
	if err := g.check(ep); err != nil {
		return nil, err
	}
	return g.Transport.NewUploadPackSession(g.endpoint(ep), auth)
}

func (g guarded) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	if err := g.check(ep); err != nil {
		return nil, err
	}
	return g.Transport.NewReceivePackSession(g.endpoint(ep), auth)
}
