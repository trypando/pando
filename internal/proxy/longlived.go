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

// reauthorizing wraps a ResponseWriter so that a request that stays open is
// re-authorized for as long as it does: a hijacked connection — a websocket,
// almost always — and a response that streams, such as server-sent events or a
// long download.
//
// O-13. The per-request CheckData that enforces access never fires again once a
// request is under way, so without this a websocket or an event stream would be
// a way to hold access indefinitely after revocation. That is precisely the
// property an attacker looks for.
type reauthorizing struct {
	http.ResponseWriter

	proxy *Proxy
	appID string

	// cancel ends the request on its way to the app, which ends a response
	// that is still streaming. Called when a re-check fails.
	cancel context.CancelFunc

	// mu guards timer and hijacked: the timer fires on its own goroutine.
	mu       sync.Mutex
	timer    *time.Timer
	hijacked bool
	revoked  bool

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
	// From here the connection's own loop re-authorizes it, and closes it
	// with a frame a client can read; the request's timer stands down.
	w.stopStreaming(true)

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

// watch re-authorizes the connection every interval and closes it on failure,
// until it closes some other way.
func (w *reauthorizing) watch(ctx context.Context, r *http.Request, conn *closeWatch) {
	ticker := time.NewTicker(w.proxy.interval())
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

// interval is how often an open request is re-authorized: assertion.Lifetime
// itself, not a copy of its value, so the revocation window, the assertion's
// life and this timer stay one number.
func (p *Proxy) interval() time.Duration {
	if p.reauthEvery > 0 {
		return p.reauthEvery
	}
	return assertion.Lifetime
}

// startStreaming re-authorizes the request every interval while it is being
// served, and cancels it when a check fails (M5 of the 2026-10 assessment; R-048,
// R-049). A request answered within one interval — nearly all of them — never
// runs a check: the timer is stopped when the request ends.
//
// A timer rather than a goroutine per request, because this is on every
// request's path and nearly every timer is stopped unfired.
func (w *reauthorizing) startStreaming() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timer = time.AfterFunc(w.proxy.interval(), w.recheck)
}

// stopStreaming stops the timer, for good. hijacked says the connection's own
// loop has taken over.
func (w *reauthorizing) stopStreaming(hijacked bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if hijacked {
		w.hijacked = true
	}
	if w.timer != nil {
		w.timer.Stop()
		w.timer = nil
	}
}

// recheck authenticates the request's credentials again and asks CheckData,
// and on a refusal cancels the request: the stream ends where it is, and the
// client's next request meets the proxy's front door like any other.
func (w *reauthorizing) recheck() {
	w.mu.Lock()
	if w.timer == nil || w.hijacked {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()

	ctx := w.request.Context()
	principal, err := w.proxy.reauthenticate(w.request)
	if err == nil {
		err = w.proxy.Authz.CheckData(ctx, principal, w.appID)
	}

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.timer == nil || w.hijacked {
		return
	}
	if err == nil {
		w.timer.Reset(w.proxy.interval())
		return
	}
	w.timer = nil
	w.revoked = true
	log.From(ctx).Info("ending a streaming response after revocation",
		zap.String("app_id", w.appID),
		zap.String("principal_id", principal.ID))
	w.cancel()
}

// wasRevoked reports whether a re-check refused the request and ended it.
func (w *reauthorizing) wasRevoked() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.revoked
}

// WriteHeader passes a status on, and for an informational response (103 Early
// Hints, 100 Continue) only its Link headers. ReverseProxy copies a 1xx
// response's headers straight to the client, past ModifyResponse, so a
// Set-Cookie on one would reach the browser without stripSetCookies seeing it
// (R-173). Link is the one header a 1xx is sent for.
func (w *reauthorizing) WriteHeader(code int) {
	if code >= 100 && code < 200 && code != http.StatusSwitchingProtocols {
		h := w.Header()
		links := h.Values("Link")
		clear(h)
		for _, v := range links {
			h.Add("Link", v)
		}
	}
	w.ResponseWriter.WriteHeader(code)
}

// reauthenticate resolves a long-lived connection's credentials again, the
// way ServeHTTP does for a fresh request: a credential that no longer works —
// a revoked session, a token revoked or expired — is anonymous, and CheckData
// decides whether anonymous is enough. A failure to look the credential up is
// returned, and closes the connection: an unknown answer is not an allow.
func (p *Proxy) reauthenticate(r *http.Request) (authz.Principal, error) {
	principal, err := p.authenticate(r)
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
