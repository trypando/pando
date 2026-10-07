package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"net"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// reauthorizing wraps a ResponseWriter so that a hijacked connection — a
// websocket, almost always — is re-authorized for as long as it stays open.
//
// O-13. The per-request CheckData that enforces access never fires again once a
// connection is upgraded, so without this a websocket would be the one way to
// hold access indefinitely after revocation. That is precisely the property an
// attacker looks for.
type reauthorizing struct {
	http.ResponseWriter

	proxy *Proxy
	appID string

	// request is the request that opened the connection. Its credentials —
	// the session cookie or bearer token, and any passcode unlock — are
	// authenticated again on every check, not the principal they resolved to
	// when the connection opened. That principal is a snapshot: a session
	// revoked or a user suspended since would still pass as it was (R-048,
	// R-049).
	request *http.Request
}

// Hijack takes over the connection and starts the re-authorization loop.
func (w *reauthorizing) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}

	conn, rw, err := hijacker.Hijack()
	if err != nil {
		return nil, nil, err
	}

	// Detached from the request context: the request is over the moment the
	// connection is hijacked, and a canceled context would end the loop
	// immediately, which is the opposite of what this is for. Cloned now,
	// while the request is still whole, so later checks read its credentials
	// and nothing the server does to it after.
	ctx := context.WithoutCancel(w.request.Context())
	watched := &closeWatch{Conn: conn, done: make(chan struct{})}
	go w.watch(ctx, w.request.Clone(ctx), watched)
	return watched, rw, nil
}

// watch re-authorizes the connection on the assertion lifetime and closes it
// on failure, until it closes some other way.
//
// The interval is assertion.Lifetime itself, not a copy of its value: the
// revocation window, the assertion's life and this timer are one number, and
// referencing the constant is what keeps them from drifting apart.
func (w *reauthorizing) watch(ctx context.Context, r *http.Request, conn *closeWatch) {
	every := w.proxy.reauthEvery
	if every <= 0 {
		every = assertion.Lifetime
	}
	ticker := time.NewTicker(every)
	defer ticker.Stop()

	for {
		select {
		case <-conn.done:
			// Closed by either end. Nothing left to authorize.
			return
		case <-ticker.C:
		}

		principal, err := w.proxy.reauthenticate(r)
		if err == nil {
			err = w.proxy.Authz.CheckData(ctx, principal, w.appID)
		}
		if err == nil {
			continue
		}

		log.From(ctx).Info("closing a long-lived connection after revocation",
			zap.String("app_id", w.appID),
			zap.String("principal_id", principal.ID))

		// A policy-violation close frame rather than an abrupt reset, so a
		// client can tell revocation from a network fault and does not simply
		// reconnect in a loop.
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_, _ = conn.Write(closeFrame(closePolicyViolation, "access revoked"))
		_ = conn.Close()
		return
	}
}

// reauthenticate resolves a long-lived connection's credentials again, the
// way ServeHTTP does for a fresh request: a credential that no longer works —
// a revoked session, a token revoked or expired — is anonymous, and CheckData
// decides whether anonymous is enough. A failure to look the credential up is
// returned, and closes the connection: an unknown answer is not an allow.
func (p *Proxy) reauthenticate(r *http.Request) (authz.Principal, error) {
	principal, err := p.Authenticator.Authenticate(r)
	if err != nil {
		if errs.CodeOf(err) == errs.Internal {
			return authz.Anonymous(), err
		}
		principal = authz.Anonymous()
	}
	principal.Passcodes = passcodesFrom(r)
	return principal, nil
}

// closeWatch is a hijacked connection that says when it has been closed, so
// the loop watching it ends with it rather than outliving it.
type closeWatch struct {
	net.Conn
	once sync.Once
	done chan struct{}
}

func (c *closeWatch) Close() error {
	c.once.Do(func() { close(c.done) })
	return c.Conn.Close()
}

// WebSocket close codes.
const closePolicyViolation = 1008

// closeFrame builds an unmasked server-to-client close frame (RFC 6455 §5.5.1).
//
// Written by hand because the proxy does not otherwise speak the websocket
// protocol — it hands bytes along — and pulling in a websocket library to send
// two frames' worth of bytes would be a dependency for nothing.
func closeFrame(code uint16, reason string) []byte {
	payload := make([]byte, 2+len(reason))
	binary.BigEndian.PutUint16(payload, code)
	copy(payload[2:], reason)

	// Close payloads are capped at 125 bytes, which is also the boundary below
	// which the length fits in the header's 7 bits — so no extended length.
	if len(payload) > 125 {
		payload = payload[:125]
	}

	frame := make([]byte, 0, 2+len(payload))
	frame = append(frame, 0x88) // FIN + opcode 8 (close)
	// G115: payload was truncated to 125 bytes immediately above, which is the
	// reason the truncation is there.
	frame = append(frame, byte(len(payload))) //nolint:gosec
	return append(frame, payload...)
}

// Unwrap lets http.ResponseController reach Flush on the underlying writer.
// Without it, wrapping would break SSE — the very thing R-170 requires.
func (w *reauthorizing) Unwrap() http.ResponseWriter { return w.ResponseWriter }
