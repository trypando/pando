package source

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"time"

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
// guard on the address dialed (guard.go).
var gitHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:        30 * time.Second,
			KeepAlive:      30 * time.Second,
			ControlContext: guardedControl,
		}).DialContext,
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

// The protocols a repository is fetched over: https, http and ssh, and nothing
// else (2026-10-16 assessment, finding 3). go-git also registers file and
// git by default, and a file:// URL — or a bare local path, which go-git reads
// as one — would read the disk Pando runs on. git is removed, and file
// refuses unless a test allowed it, here rather than only at validation, so
// no address that slipped past it can use them.
func init() {
	c := githttp.NewClient(gitHTTPClient)
	client.InstallProtocol("https", guarded{c, 443})
	client.InstallProtocol("http", guarded{c, 80})
	client.InstallProtocol("ssh", guarded{gitssh.DefaultClient, 22})
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

// guarded checks the endpoint's host against the guard before a session is
// opened: the readable refusal, and for SSH the only check there is before the
// dial (guard.go's checkHost says why that is enough). HTTP is checked again
// on every connection it dials.
type guarded struct {
	transport.Transport
	defaultPort int
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
	return g.Transport.NewUploadPackSession(ep, auth)
}

func (g guarded) NewReceivePackSession(ep *transport.Endpoint, auth transport.AuthMethod) (transport.ReceivePackSession, error) {
	if err := g.check(ep); err != nil {
		return nil, err
	}
	return g.Transport.NewReceivePackSession(ep, auth)
}
