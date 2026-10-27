package hostagent

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httputil"
	"regexp"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// A joined host never exposes its container runtime's API (R-416). Its agent,
// on the one port the host publishes, carries the runtime calls a control
// host's runtime adapter makes to the host's own rootless socket, beside the
// proxy traffic it forwards. Only for a control host: an app host's
// certificate, or an agent's own, is refused before the socket is touched.

// runtimeTarget is the header line's word for the host's container runtime.
const runtimeTarget = "docker"

// isRuntimeRequest reports whether the header line asks for the runtime.
func isRuntimeRequest(line string) bool {
	fields := strings.Fields(line)
	return len(fields) == 2 && fields[0] == protocolVersion && fields[1] == runtimeTarget
}

// Fence is whether this host is fencing (R-421): it decided it is cut off
// from most of the cluster, and has not reached it again since, whether or
// not its apps are stopped yet. core's liveness.Monitor is one. From the
// decision on, through the stop, the agent refuses every runtime call that
// would create or start a container, so the control plane cannot undo the
// fence through it, nor start a container the stop has already passed.
type Fence interface {
	Fencing() bool
}

// startCall is a Docker API call that creates or starts a container, with or
// without the API version in front.
var startCall = regexp.MustCompile(`^(/v[0-9.]+)?/containers/(create|[^/]+/(start|restart|unpause))$`)

// fencedMessage is what a refused call answers, in Docker's error shape.
const fencedMessage = `{"message":"This host is cut off from most of the cluster and stops its apps, so it starts none until it reaches the cluster again."}`

// openRuntime connects to the host's runtime for a control host's
// certificate. With a Fence set, the stream is served call by call
// (fenceable), so a call made while the host is fencing is refused even on a
// connection opened before the fence began.
func (a *Agent) openRuntime(ctx context.Context, cs tls.ConnectionState) (net.Conn, error) {
	if a.RuntimeSocket == "" && a.DialRuntime == nil {
		return nil, errors.New("this agent does not carry container runtime calls")
	}
	if len(cs.VerifiedChains) == 0 || len(cs.VerifiedChains[0]) == 0 || !IsControl(cs.VerifiedChains[0][0]) {
		return nil, errors.New("container runtime calls are carried only for a control host of this cluster")
	}
	ctx, cancel := context.WithTimeout(ctx, headerTimeout)
	defer cancel()
	conn, err := a.dialRuntime(ctx)
	if err != nil {
		return nil, errors.New("the host's container runtime is not answering")
	}
	if a.Fence == nil {
		return conn, nil
	}
	ours, err := a.fenceable(conn)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ours, nil
}

// fenceable serves the runtime calls of one stream through an HTTP proxy that
// asks Fence before each one, and returns the stream's other end. first is a
// connection to the runtime already made, used for the first call.
//
// The two ends close for writing one direction at a time (streamPair), so
// the end of a helper container's stdin reaches the runtime through an
// upgraded attach or exec while its output is still to come (R-420).
func (a *Agent) fenceable(first net.Conn) (net.Conn, error) {
	ours, theirs, err := streamPair()
	if err != nil {
		return nil, err
	}
	var once sync.Once
	dial := func(ctx context.Context, _, _ string) (net.Conn, error) {
		var c net.Conn
		once.Do(func() { c = first })
		if c != nil {
			return c, nil
		}
		return a.dialRuntime(ctx)
	}
	transport := &http.Transport{DialContext: dial, DisableCompression: true}
	proxy := &httputil.ReverseProxy{
		Rewrite:   func(r *httputil.ProxyRequest) { r.Out.URL.Scheme, r.Out.URL.Host = "http", "docker" },
		Transport: transport,
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost && startCall.MatchString(r.URL.Path) && a.Fence.Fencing() {
				a.logger().Info("refused a runtime call while this host is fencing", zap.String("path", r.URL.Path))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(fencedMessage))
				return
			}
			proxy.ServeHTTP(w, r)
		}),
		ReadHeaderTimeout: headerTimeout,
	}
	ln := &oneConn{conn: theirs, closed: make(chan struct{})}
	srv.ConnState = func(_ net.Conn, st http.ConnState) {
		if st == http.StateClosed || st == http.StateHijacked {
			ln.Close()
		}
	}
	go func() {
		_ = srv.Serve(ln)
		// first, when no call took it; a stream a call upgraded stays open.
		once.Do(func() { _ = first.Close() })
		transport.CloseIdleConnections()
	}()
	return ours, nil
}

// oneConn is a listener that accepts one connection, then waits until it is
// closed.
type oneConn struct {
	mu     sync.Mutex
	conn   net.Conn
	closed chan struct{}
	once   sync.Once
}

func (l *oneConn) Accept() (net.Conn, error) {
	l.mu.Lock()
	c := l.conn
	l.conn = nil
	l.mu.Unlock()
	if c != nil {
		return c, nil
	}
	<-l.closed
	return nil, net.ErrClosed
}

func (l *oneConn) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *oneConn) Addr() net.Addr { return pipeAddr{} }

type pipeAddr struct{}

func (pipeAddr) Network() string { return "pipe" }
func (pipeAddr) String() string  { return "runtime" }

func (a *Agent) dialRuntime(ctx context.Context) (net.Conn, error) {
	if a.DialRuntime != nil {
		return a.DialRuntime(ctx)
	}
	var d net.Dialer
	return d.DialContext(ctx, "unix", a.RuntimeSocket)
}

// DialRuntime opens a stream through the agent at addr to its host's
// container runtime API, as the client tlsConfig authenticates: a control
// host's (IsControl). A Docker client dials one per connection it keeps.
func DialRuntime(ctx context.Context, addr string, tlsConfig *tls.Config) (net.Conn, error) {
	return dialAgent(ctx, addr, tlsConfig, protocolVersion+" "+runtimeTarget)
}
