// Package logstream shares one live log stream per app part between everyone
// watching it on a replica (O-51).
//
// The console's Logs tab used to ask the runtime for the last 500 lines every
// five seconds, from every tab that had an app open. Each ask is a container
// log read (Docker) or a pod log request (Kubernetes), so the runtime's load
// grew with the number of people looking rather than with the number of apps.
// A Hub holds one following runtime.Logs stream per (runtime, app, part) while
// at least one viewer watches it, keeps the most recent lines in a bounded ring
// for whoever joins next, and fans each new line out to every viewer. The
// runtime stream closes a short grace period after the last viewer leaves, so a
// page reload does not reopen it.
//
// Only what the runtime printed is shared here. Whether a viewer may read it is
// decided by the caller when the viewer connects, and decided again on an
// interval while the viewer stays connected (Serve, R-048, design 06 §4.2): a
// shared stream never carries an authorization decision.
//
// Design notes: docs/design/notes-shared-log-streams-o51.md.
package logstream

import (
	"bufio"
	"context"
	"errors"
	"io"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/clock"
)

// Defaults. Each is a [P] default recorded in the design note.
const (
	// Backfill is how many recent lines a stream holds for a viewer who joins
	// after it opened, and how many it asks the runtime for when it opens.
	DefaultBackfill = 1000

	// DefaultTail is what a one-shot read returns when no tail is given, and
	// MaxTail is the most it returns whatever is asked for. A larger tail is
	// cut to MaxTail rather than refused: a person asking for "everything" is
	// better served by the most Pando will send than by an error.
	DefaultTail = 200
	MaxTail     = 5000

	// DefaultGrace is how long a stream with no viewers stays open, so a page
	// reload or a switch between parts and back does not reopen it.
	DefaultGrace = 30 * time.Second

	// DefaultBuffer is how many events one viewer may fall behind by before
	// it is disconnected. Large enough to take a full backfill arriving in one
	// burst when a stream opens.
	DefaultBuffer = 2048

	// MaxLineBytes is the longest line passed on; the rest of a longer line is
	// dropped and the line ends with an ellipsis. One unbroken megabyte of
	// output would otherwise be one megabyte per viewer per buffer slot.
	MaxLineBytes = 16 << 10

	// Reconnect backoff after the runtime's stream ends.
	DefaultRetryMin = time.Second
	DefaultRetryMax = 30 * time.Second
)

// DefaultReauthEvery is how often a connected viewer's access is checked again.
// It is assertion.Lifetime itself rather than a copy of its value, as the
// proxy's long-lived connections are: design 06 §3.1 sets one revocation
// window, and every long-lived path re-checks on exactly that interval.
const DefaultReauthEvery = assertion.Lifetime

// Messages viewers are sent. Each stands alone (R-105).
const (
	noticeEnded = "The log stream from the runtime ended, usually because this part of the app " +
		"restarted, stopped or was replaced. Pando is reconnecting and will show new output when " +
		"there is some."
	noticeResumed = "Reconnected to this part's log. The lines that follow were printed after the " +
		"previous stream ended."
	messageLagged = "This log stream fell too far behind the app's output and Pando disconnected it, " +
		"so the other people reading the same log were not held up. Reconnecting starts again " +
		"from the most recent lines."
	messageRevoked = "Your access to this app's logs has ended, so Pando closed the log stream. " +
		"Someone with permission to manage the app's access can grant app.logs.read again."
)

// ClampTail is the tail a one-shot read uses: DefaultTail for none (or a
// nonsensical negative), and never more than MaxTail.
func ClampTail(n int) int {
	switch {
	case n <= 0:
		return DefaultTail
	case n > MaxTail:
		return MaxTail
	default:
		return n
	}
}

// Key names one shared stream. The same app on two runtimes is two streams.
type Key struct {
	Runtime  string // the spec's runtime adapter ref
	AppID    string
	Workload string
}

// Kind is what an Event carries.
type Kind uint8

const (
	// KindLine is one line the app printed, without its newline.
	KindLine Kind = iota + 1
	// KindNotice is Pando saying something about the stream itself — that it
	// ended and is being reconnected, or that it has been.
	KindNotice
)

// Event is one thing sent to a viewer.
type Event struct {
	Kind Kind
	Text string
}

// Reason says why Pando, rather than the viewer, ended a viewer's stream.
type Reason string

const (
	// ReasonRevoked: the viewer's access no longer holds. A client must not
	// reconnect on its own.
	ReasonRevoked Reason = "revoked"
	// ReasonLagged: the viewer fell DefaultBuffer events behind. Reconnecting
	// is fine and starts again from the backfill.
	ReasonLagged Reason = "lagged"
)

// Ended is returned by Serve when Pando ended the stream.
type Ended struct {
	Reason  Reason
	Message string
}

func (e *Ended) Error() string { return e.Message }

// Option configures a Hub.
type Option func(*Hub)

// WithClock sets the time source.
func WithClock(c clock.Clock) Option { return func(h *Hub) { h.clock = c } }

// WithGrace sets how long a stream with no viewers stays open.
func WithGrace(d time.Duration) Option { return func(h *Hub) { h.grace = d } }

// WithBackfill sets how many recent lines a stream keeps.
func WithBackfill(n int) Option { return func(h *Hub) { h.backfill = n } }

// WithBuffer sets how far one viewer may fall behind.
func WithBuffer(n int) Option { return func(h *Hub) { h.buffer = n } }

// WithRetry sets the reconnect backoff.
func WithRetry(lo, hi time.Duration) Option {
	return func(h *Hub) { h.retryMin, h.retryMax = lo, hi }
}

// WithReauthEvery sets how often Serve checks a viewer's access again.
func WithReauthEvery(d time.Duration) Option { return func(h *Hub) { h.reauthEvery = d } }

// WithLogger sets where the hub logs a stream it could not reopen.
func WithLogger(l *zap.Logger) Option { return func(h *Hub) { h.log = l } }

// Hub is one replica's set of shared log streams. Use New.
type Hub struct {
	clock       clock.Clock
	grace       time.Duration
	backfill    int
	buffer      int
	retryMin    time.Duration
	retryMax    time.Duration
	reauthEvery time.Duration
	log         *zap.Logger

	mu      sync.Mutex
	streams map[Key]*stream
}

// New returns a Hub with the defaults above.
func New(opts ...Option) *Hub {
	h := &Hub{
		clock:       clock.System{},
		grace:       DefaultGrace,
		backfill:    DefaultBackfill,
		buffer:      DefaultBuffer,
		retryMin:    DefaultRetryMin,
		retryMax:    DefaultRetryMax,
		reauthEvery: DefaultReauthEvery,
		log:         zap.NewNop(),
		streams:     map[Key]*stream{},
	}
	for _, o := range opts {
		o(h)
	}
	return h
}

// stream is one shared runtime log stream. Every field after ready is guarded
// by Hub.mu.
type stream struct {
	key Key
	rt  api.RuntimeAdapter

	// ready closes once the first open has succeeded or failed; err is the
	// failure. Viewers that arrive while the stream is opening wait on it.
	ready chan struct{}
	err   error

	cancel context.CancelFunc

	ring  []string // a circular buffer of the most recent lines
	start int
	count int

	subs map[*Subscription]struct{}

	// down is set while the runtime's stream has ended and not yet produced
	// a line again, so a viewer joining then is told.
	down bool

	// idle counts the times the stream has been left with no viewers. A grace
	// timer acts only if no viewer has come and gone since it started.
	idle int
}

// Active reports how many streams the hub holds open, for tests and metrics.
func (h *Hub) Active() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.streams)
}

// Subscribe joins the shared stream for key, opening it on rt if this replica
// has none. The caller has already authorized the viewer. The open's error —
// nothing running under that name, the runtime unreachable — is returned
// here, before anything has been sent, so it can be answered as an error.
//
// The subscription must be closed.
func (h *Hub) Subscribe(ctx context.Context, rt api.RuntimeAdapter, key Key) (*Subscription, error) {
	for {
		s := h.join(ctx, key, rt)

		select {
		case <-s.ready:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		if s.err != nil {
			return nil, s.err
		}

		if sub, ok := h.attach(s); ok {
			return sub, nil
		}
		// Closed by its grace timer between the open and now. The next pass
		// opens a fresh one.
	}
}

// join returns the stream for key, creating and opening it if there is none.
func (h *Hub) join(ctx context.Context, key Key, rt api.RuntimeAdapter) *stream {
	h.mu.Lock()
	s, ok := h.streams[key]
	if ok {
		h.mu.Unlock()
		return s
	}
	s = &stream{
		key:   key,
		rt:    rt,
		ready: make(chan struct{}),
		ring:  make([]string, h.backfill),
		subs:  map[*Subscription]struct{}{},
	}
	h.streams[key] = s
	h.mu.Unlock()
	h.open(ctx, s)
	return s
}

// attach adds a viewer to an open stream, unless the stream has been closed.
func (h *Hub) attach(s *stream) (*Subscription, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.streams[s.key] != s {
		return nil, false
	}
	sub := &Subscription{
		hub:      h,
		stream:   s,
		events:   make(chan Event, h.buffer),
		Backfill: s.lines(),
	}
	s.subs[sub] = struct{}{}
	s.idle++ // a pending grace timer no longer applies
	if s.down {
		sub.events <- Event{Kind: KindNotice, Text: noticeEnded}
	}
	return sub, true
}

// open makes the stream's first runtime call and, on success, starts reading.
// Detached from the first viewer's context: one viewer closing their tab must
// not end the stream for the rest.
func (h *Hub) open(ctx context.Context, s *stream) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	ref := api.WorkloadRef{BundleID: s.key.AppID, Workload: s.key.Workload}
	rc, err := s.rt.Logs(ctx, ref, api.LogOptions{Follow: true, Tail: h.backfill})
	if err != nil {
		cancel()
		h.mu.Lock()
		delete(h.streams, s.key)
		h.mu.Unlock()
		s.err = err
		close(s.ready)
		return
	}
	s.cancel = cancel
	close(s.ready)
	go h.run(ctx, s, ref, rc)
}

// run reads the runtime's stream and, when it ends while viewers remain,
// reopens it from the moment it ended — a container restarting ends a
// followed Docker log, and a pod being replaced ends a Kubernetes one.
func (h *Hub) run(ctx context.Context, s *stream, ref api.WorkloadRef, rc io.ReadCloser) {
	wait := h.retryMin
	for {
		got := h.pump(ctx, s, rc)
		if ctx.Err() != nil {
			return
		}
		endedAt := h.clock.Now()

		h.mu.Lock()
		if !s.down {
			s.down = true
			h.broadcast(s, Event{Kind: KindNotice, Text: noticeEnded})
		}
		h.mu.Unlock()

		// A stream that printed something earns a quick retry; one that ended
		// with nothing (a stopped container's log ends at once) backs off, so
		// a part that stays down is not asked about every second.
		if got {
			wait = h.retryMin
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-h.clock.After(wait):
			}
			wait = min(wait*2, h.retryMax)
			var err error
			rc, err = s.rt.Logs(ctx, ref, api.LogOptions{Follow: true, Since: endedAt})
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			h.log.Debug("could not reopen an app's log stream",
				zap.String("app_id", s.key.AppID), zap.String("workload", s.key.Workload), zap.Error(err))
		}
	}
}

// pump copies lines from rc to the stream's viewers until rc ends or ctx is
// canceled, and reports whether it read any.
func (h *Hub) pump(ctx context.Context, s *stream, rc io.ReadCloser) bool {
	// Closing the reader is what unblocks a read waiting on a quiet app.
	stop := context.AfterFunc(ctx, func() { _ = rc.Close() })
	defer func() {
		stop()
		_ = rc.Close()
	}()

	got := false
	r := bufio.NewReaderSize(rc, 4096)
	for {
		line, err := readLine(r)
		if line != nil {
			got = true
			h.mu.Lock()
			if s.down {
				s.down = false
				h.broadcast(s, Event{Kind: KindNotice, Text: noticeResumed})
			}
			s.push(string(line))
			h.broadcast(s, Event{Kind: KindLine, Text: string(line)})
			h.mu.Unlock()
		}
		if err != nil {
			return got
		}
	}
}

// readLine returns the next line without its line ending, cut to
// MaxLineBytes. A final line with no newline is returned with the error.
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	cut := false
	for {
		frag, isPrefix, err := r.ReadLine()
		if err != nil {
			if errors.Is(err, io.EOF) && line != nil {
				return finish(line, cut), err
			}
			return nil, err
		}
		if !cut {
			room := MaxLineBytes - len(line)
			if len(frag) > room {
				frag, cut = frag[:room], true
			}
			line = append(line, frag...)
		}
		if line == nil {
			line = []byte{}
		}
		if !isPrefix {
			return finish(line, cut), nil
		}
	}
}

func finish(line []byte, cut bool) []byte {
	if cut {
		return append(line, "…"...)
	}
	return line
}

// push keeps a line in the ring. Hub.mu held.
func (s *stream) push(line string) {
	if len(s.ring) == 0 {
		return
	}
	i := (s.start + s.count) % len(s.ring)
	s.ring[i] = line
	if s.count < len(s.ring) {
		s.count++
	} else {
		s.start = (s.start + 1) % len(s.ring)
	}
}

// lines copies the ring out, oldest first. Hub.mu held.
func (s *stream) lines() []string {
	out := make([]string, s.count)
	for i := range s.count {
		out[i] = s.ring[(s.start+i)%len(s.ring)]
	}
	return out
}

// broadcast sends ev to every viewer without waiting on any of them. A viewer
// whose buffer is full is disconnected rather than waited for: one slow
// connection must not hold up everyone else reading the same log. Hub.mu held.
func (h *Hub) broadcast(s *stream, ev Event) {
	for sub := range s.subs {
		select {
		case sub.events <- ev:
		default:
			sub.ended = &Ended{Reason: ReasonLagged, Message: messageLagged}
			h.detach(s, sub)
		}
	}
}

// detach removes a viewer and, if it was the last, starts the grace timer.
// Hub.mu held.
func (h *Hub) detach(s *stream, sub *Subscription) {
	if _, ok := s.subs[sub]; !ok {
		return
	}
	delete(s.subs, sub)
	close(sub.events)
	if len(s.subs) > 0 {
		return
	}
	s.idle++
	idle := s.idle
	go func() {
		<-h.clock.After(h.grace)
		h.mu.Lock()
		defer h.mu.Unlock()
		if s.idle != idle || len(s.subs) > 0 || h.streams[s.key] != s {
			return
		}
		delete(h.streams, s.key)
		s.cancel()
	}()
}

// Subscription is one viewer of a shared stream.
type Subscription struct {
	// Backfill is the most recent lines the stream held when the viewer
	// joined, oldest first. Lines arriving after it are on Events.
	Backfill []string

	hub    *Hub
	stream *stream
	events chan Event
	ended  *Ended // set, under Hub.mu, before events is closed by the hub
}

// Events delivers what arrives after the backfill. It is closed when the
// viewer is closed or disconnected; Ended then says which.
func (s *Subscription) Events() <-chan Event { return s.events }

// Ended is why Pando closed Events, or nil if the viewer closed it.
func (s *Subscription) Ended() *Ended {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	return s.ended
}

// Close leaves the stream. Safe to call more than once.
func (s *Subscription) Close() {
	s.hub.mu.Lock()
	defer s.hub.mu.Unlock()
	s.hub.detach(s.stream, s)
}

// Viewer is where Serve writes one viewer's stream: the HTTP handler's
// server-sent events, or plain text for the CLI.
type Viewer interface {
	Line(text string) error
	Notice(text string) error
	// Flush is called when there is nothing more to write for now, so a
	// burst of lines is sent together rather than one network write each.
	Flush() error
}

// Authorize decides again whether the viewer may still read the log. A nil
// error allows; any error ends the stream, an unknown answer included.
type Authorize func(ctx context.Context) error

// Serve writes sub's backfill and then its events to v until the viewer goes
// (ctx ends, or a write fails), the viewer falls too far behind, or authorize
// stops allowing. It closes sub. It returns nil when the viewer went, and an
// *Ended when Pando ended the stream.
//
// authorize runs every DefaultReauthEvery: the check that let the viewer in
// fires once, and without this a log stream would outlive the access that
// opened it (R-048, design 06 §3.1, §4.2).
func (h *Hub) Serve(ctx context.Context, sub *Subscription, authorize Authorize, v Viewer) error {
	defer sub.Close()

	for _, line := range sub.Backfill {
		if err := v.Line(line); err != nil {
			return nil
		}
	}
	if err := v.Flush(); err != nil {
		return nil
	}

	recheck := h.clock.After(h.reauthEvery)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-recheck:
			if err := authorize(ctx); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				return &Ended{Reason: ReasonRevoked, Message: messageRevoked}
			}
			recheck = h.clock.After(h.reauthEvery)
		case ev, open := <-sub.events:
			if !open {
				if e := sub.Ended(); e != nil {
					return e
				}
				return nil
			}
			if err := write(v, ev); err != nil {
				return nil
			}
			// Drain what is already waiting before flushing.
			for drained := false; !drained; {
				select {
				case ev, open := <-sub.events:
					if !open {
						_ = v.Flush()
						if e := sub.Ended(); e != nil {
							return e
						}
						return nil
					}
					if err := write(v, ev); err != nil {
						return nil
					}
				default:
					drained = true
				}
			}
			if err := v.Flush(); err != nil {
				return nil
			}
		}
	}
}

func write(v Viewer, ev Event) error {
	if ev.Kind == KindNotice {
		return v.Notice(ev.Text)
	}
	return v.Line(ev.Text)
}
