package egress

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/log"
)

// DefaultPort is where the gateway listens, and what a restricted app's
// HTTP_PROXY and HTTPS_PROXY name (R-187). 3128 because it is the port every
// forward proxy has listened on since Squid, and some clients guess it.
const DefaultPort = 3128

// Gateway is the egress gateway: an HTTP forward proxy that lets a restricted
// app's traffic out only where its rules allow (R-185 – R-187).
//
// It answers two kinds of request, the two that HTTP_PROXY and HTTPS_PROXY
// produce: CONNECT host:port, which opens a tunnel (HTTPS, and anything else a
// client chooses to tunnel), and a plain HTTP request in absolute form
// (GET http://host/path). Everything else is refused.
//
// Each connection is decided the same way:
//
//  1. The name the app asked for is checked on its own (Compiled.AllowsName),
//     before anything is resolved — a denylisted name is never looked up.
//  2. The name is resolved once, or taken as it is when it is an address.
//  3. Only the addresses the rules allow (Compiled.Allows) are kept, and the
//     connection is dialed to one of those addresses. Never to the name again:
//     resolving a second time would let a name that answered with a public
//     address when it was checked answer with 169.254.169.254 when it was
//     dialed, which is how DNS rebinding walks around private-range blocking.
//
// A refusal is a 403 whose body says why, in words an app owner can act on
// (R-105), and one structured log line.
type Gateway struct {
	// Rules decide every connection. Required.
	Rules *Compiled

	// Logger receives one line per refusal. Nil logs nothing.
	Logger *zap.Logger

	// Resolver looks names up. Nil is the system resolver.
	Resolver Resolver

	// DialTimeout bounds one attempt to connect to one address, and
	// ResolveTimeout one name lookup. Zero is ten seconds for each.
	DialTimeout    time.Duration
	ResolveTimeout time.Duration

	// dial connects to an address the rules allowed. Nil is a net.Dialer.
	// Replaced only by tests.
	dial func(ctx context.Context, network, address string) (net.Conn, error)

	once      sync.Once
	transport *http.Transport
	proxy     *httputil.ReverseProxy

	// stopping ends when Serve's context does, and closes every tunnel.
	stopping context.Context
	stop     context.CancelFunc
}

// Resolver is the part of net.Resolver the gateway uses.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// allowedKey carries the addresses a plain HTTP request was allowed to reach
// from ServeHTTP to the transport's dialer.
type allowedKey struct{}

func (g *Gateway) init() {
	g.once.Do(func() {
		g.stopping, g.stop = context.WithCancel(context.Background()) //nolint:gosec // G118: stop is called by Serve when its context ends.
		g.transport = &http.Transport{
			// Never another proxy. The gateway is the last hop out, and the
			// environment it runs in is not a place to pick one up from.
			Proxy:                 nil,
			DialContext:           g.dialAllowed,
			MaxIdleConns:          64,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 0,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     false,
		}
		g.proxy = &httputil.ReverseProxy{
			Transport: g.transport,
			// The request already names where it goes; it is sent on as it
			// is. Hop-by-hop headers, Proxy-Authorization among them, are
			// removed by ReverseProxy. No X-Forwarded-For is added: where a
			// request came from inside the app is nobody's business outside.
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.Out.URL = pr.In.URL
				pr.Out.Host = pr.In.Host
				pr.Out.RequestURI = ""
			},
			// Streamed, never buffered whole: a response of any size passes
			// through a fixed buffer, and a server-sent event arrives as it
			// is sent.
			FlushInterval: -1,
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				g.fail(w, http.StatusBadGateway, fmt.Sprintf(
					"Pando's egress gateway could not reach %s: %s.", r.URL.Host, plainError(err)))
			},
		}
	})
}

func (g *Gateway) logger() *zap.Logger {
	if g.Logger == nil {
		return zap.NewNop()
	}
	return g.Logger
}

// ServeHTTP handles one request to the gateway.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	g.init()
	switch {
	case r.Method == http.MethodConnect:
		g.tunnel(w, r)
	case r.URL.IsAbs():
		g.forward(w, r)
	case r.URL.Path == "/healthz" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	default:
		g.fail(w, http.StatusBadRequest,
			"This is Pando's egress gateway, which only forwards requests sent to it as a proxy. "+
				"Set HTTP_PROXY and HTTPS_PROXY to its address rather than requesting it directly.")
	}
}

// tunnel opens a CONNECT tunnel to a destination the rules allow.
func (g *Gateway) tunnel(w http.ResponseWriter, r *http.Request) {
	host, port, err := splitTarget(r.Host, 0)
	if err != nil {
		g.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	// Taken over before the destination is decided and dialed, not after.
	// Until then the server reads ahead on the connection to notice a client
	// that hangs up, and a client that sends its first bytes without waiting
	// for the reply — or closes its sending side once it has — had the
	// server parse those bytes as a second request and cancel the dial as
	// abandoned. After Hijack they wait in buf, and the answer is written
	// here by hand.
	hj, ok := w.(http.Hijacker)
	if !ok {
		g.fail(w, http.StatusInternalServerError, "Pando's egress gateway cannot open a tunnel over this connection.")
		return
	}
	client, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	defer func() { _ = client.Close() }()

	// Not the request's context from here on. The server cancels that when
	// it sees the client stop sending, which for a tunnel is no reason to
	// stop: a client that has sent everything still reads the answer. The
	// tunnel ends when either side closes, or when the gateway stops.
	ctx := g.stopping
	upstream, status, reason := g.connect(ctx, host, port) //nolint:contextcheck // Deliberately not the request's context; see above.
	if upstream == nil {
		writeStatus(client, status, reason)
		return
	}
	defer func() { _ = upstream.Close() }()

	if _, err := io.WriteString(client, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}

	// The context ends when the gateway shuts down. A tunnel can be open for
	// hours — a websocket, a database connection — so it is closed then
	// rather than left to finish on its own.
	stop := context.AfterFunc(ctx, func() { //nolint:contextcheck // The gateway's lifetime, not the request's; see above.
		_ = client.Close()
		_ = upstream.Close()
	})
	defer stop()

	done := make(chan struct{}, 2)
	go func() {
		// Anything the client sent after its CONNECT line and before the
		// reply, which the server has already read into buf, goes first.
		_, _ = io.Copy(upstream, buf)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	<-done
}

// closeWrite ends one direction of a tunnel and leaves the other open, so a
// client that finished sending still reads the rest of the answer.
func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// forward sends a plain HTTP request on to a destination the rules allow.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request) {
	if r.URL.Scheme != "http" {
		g.fail(w, http.StatusBadRequest, fmt.Sprintf(
			"Pando's egress gateway forwards only http:// URLs as plain requests, and this one is %s://. "+
				"Clients send https:// through a proxy with CONNECT, which the gateway supports.", r.URL.Scheme))
		return
	}
	host, port, err := splitTarget(r.URL.Host, 80)
	if err != nil {
		g.fail(w, http.StatusBadRequest, err.Error())
		return
	}

	addrs, status, reason := g.decide(r.Context(), host, port)
	if len(addrs) == 0 {
		g.fail(w, status, reason)
		return
	}
	ctx := context.WithValue(r.Context(), allowedKey{}, allowed{host: host, port: port, addrs: addrs})
	g.proxy.ServeHTTP(w, r.WithContext(ctx))
}

type allowed struct {
	host  string
	port  int
	addrs []netip.Addr
}

// dialAllowed is the transport's dialer. It connects only to the addresses
// ServeHTTP decided on for this request, and refuses anything else.
func (g *Gateway) dialAllowed(ctx context.Context, network, address string) (net.Conn, error) {
	a, ok := ctx.Value(allowedKey{}).(allowed)
	if !ok {
		return nil, errors.New("no destination was allowed for this connection")
	}
	host, port, err := splitTarget(address, 80)
	if err != nil || host != a.host || port != a.port {
		return nil, fmt.Errorf("%s is not the destination that was allowed", address)
	}
	conn, _, err := g.dialAny(ctx, a.addrs, port)
	return conn, err
}

// connect decides a destination and dials it.
func (g *Gateway) connect(ctx context.Context, host string, port int) (net.Conn, int, string) {
	addrs, status, reason := g.decide(ctx, host, port)
	if len(addrs) == 0 {
		return nil, status, reason
	}
	conn, _, err := g.dialAny(ctx, addrs, port)
	if err != nil {
		return nil, http.StatusBadGateway, fmt.Sprintf(
			"Pando's egress gateway could not connect to %s: %s.", hostPort(host, port), plainError(err))
	}
	return conn, 0, ""
}

// decide returns the addresses a connection to host:port may be dialed to,
// or none, an HTTP status and the reason.
func (g *Gateway) decide(ctx context.Context, host string, port int) ([]netip.Addr, int, string) {
	if g.Rules == nil {
		return nil, http.StatusInternalServerError, "Pando's egress gateway has no rules, so it lets nothing out."
	}
	target := hostPort(host, port)

	var candidates []netip.Addr
	if a, err := netip.ParseAddr(host); err == nil {
		candidates = []netip.Addr{a.Unmap()}
	} else {
		if d := g.Rules.AllowsName(host, port); !d.Allowed {
			return nil, http.StatusForbidden, g.refuse(host, port, d.Reason)
		}
		timeout := g.ResolveTimeout
		if timeout <= 0 {
			timeout = 10 * time.Second
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		resolved, err := g.resolver().LookupNetIP(rctx, "ip", host)
		if err != nil || len(resolved) == 0 {
			g.logger().Info("egress gateway could not resolve a destination",
				log.Untrusted("host", host), zap.Int("port", port), zap.Error(err))
			return nil, http.StatusBadGateway, fmt.Sprintf(
				"Pando's egress gateway could not find an address for %s. Check the name is spelled correctly and exists.", host)
		}
		for _, a := range resolved {
			candidates = append(candidates, a.Unmap())
		}
	}

	var kept []netip.Addr
	first := ""
	for _, a := range candidates {
		d := g.decideAddr(host, port, a)
		if d.Allowed {
			kept = append(kept, a)
		} else if first == "" {
			first = d.Reason
		}
	}
	if len(kept) == 0 {
		return nil, http.StatusForbidden, g.refuse(host, port, first)
	}
	g.logger().Debug("egress allowed", log.Untrusted("destination", target))
	return kept, 0, ""
}

// decideAddr is Compiled.Allows, and one refusal of the gateway's own.
//
// The gateway's loopback and unspecified addresses are its own container, not
// a destination the app has: "localhost" in the app is the app, and its
// NO_PROXY keeps that from reaching here. Refused whatever the rules say, so
// no rule can open the gateway to connections to itself.
func (g *Gateway) decideAddr(host string, port int, a netip.Addr) Decision {
	if a.IsLoopback() || a.IsUnspecified() {
		return Decision{Reason: fmt.Sprintf("%s is the gateway's own address, which is never a destination", a)}
	}
	return g.Rules.Allows(host, port, a)
}

// refuse logs a refusal and returns what the app is told.
func (g *Gateway) refuse(host string, port int, reason string) string {
	g.logger().Info("egress refused",
		log.Untrusted("host", host), zap.Int("port", port), log.Untrusted("reason", reason))
	return fmt.Sprintf("Pando's egress gateway refused a connection to %s: %s. "+
		"An app's egress rules come from host policy and the app's own settings, and take effect at its next deploy.",
		hostPort(host, port), reason)
}

// dialAny connects to the first address that answers.
func (g *Gateway) dialAny(ctx context.Context, addrs []netip.Addr, port int) (net.Conn, netip.Addr, error) {
	timeout := g.DialTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	dial := g.dial
	if dial == nil {
		d := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	var last error
	for _, a := range addrs {
		dctx, cancel := context.WithTimeout(ctx, timeout)
		conn, err := dial(dctx, "tcp", netip.AddrPortFrom(a, uint16(port)).String()) //nolint:gosec // G115: port is checked to be 1–65535 by splitTarget.
		cancel()
		if err == nil {
			return conn, a, nil
		}
		last = err
		if ctx.Err() != nil {
			break
		}
	}
	return nil, netip.Addr{}, last
}

func (g *Gateway) resolver() Resolver {
	if g.Resolver != nil {
		return g.Resolver
	}
	return net.DefaultResolver
}

// fail writes a short plain-text answer.
func (g *Gateway) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, msg+"\n") //nolint:gosec // G705: text/plain with nosniff, read by an HTTP client, never rendered as HTML.
}

// writeStatus answers on a hijacked connection what fail answers through a
// ResponseWriter, and closes it: a refused tunnel is over.
func writeStatus(conn net.Conn, status int, msg string) {
	msg += "\n"
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, _ = fmt.Fprintf(conn,
		"HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nX-Content-Type-Options: nosniff\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, http.StatusText(status), len(msg), msg)
}

// splitTarget reads host[:port], with IPv6 in brackets. defaultPort zero
// means the port is required, as it is in CONNECT.
func splitTarget(s string, defaultPort int) (string, int, error) {
	host, portText, err := net.SplitHostPort(s)
	if err != nil {
		if defaultPort == 0 {
			return "", 0, fmt.Errorf("%q is not a destination Pando's egress gateway can connect to. Write it as host:port, such as api.example.com:443", s)
		}
		host, portText = strings.Trim(s, "[]"), strconv.Itoa(defaultPort)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", 0, fmt.Errorf("%q does not have a port from 1 to 65535", s)
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return "", 0, fmt.Errorf("%q names no host", s)
	}
	return host, port, nil
}

func hostPort(host string, port int) string {
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// plainError is an error's text without the Go plumbing in front of it:
// "dial tcp 203.0.113.7:443: connect: connection refused" becomes
// "connection refused".
func plainError(err error) string {
	var op *net.OpError
	if errors.As(err, &op) && op.Err != nil {
		err = op.Err
	}
	msg := err.Error()
	if i := strings.LastIndex(msg, ": "); i >= 0 {
		msg = msg[i+2:]
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout") {
		return "the connection timed out"
	}
	return msg
}

// ListenAndServe runs the gateway on addr until ctx ends.
func (g *Gateway) ListenAndServe(ctx context.Context, addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return g.Serve(ctx, ln)
}

// Serve runs the gateway on a listener until ctx ends, then stops accepting,
// closes open tunnels and returns.
func (g *Gateway) Serve(ctx context.Context, ln net.Listener) error {
	g.init()
	defer context.AfterFunc(ctx, g.stop)()
	srv := &http.Server{
		Handler:           g,
		ReadHeaderTimeout: 30 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ErrorLog:          zap.NewStdLog(g.logger()),
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// The serving context has ended; shutting down needs one that has not.
	shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdown)
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
