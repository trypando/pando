package deploy

import (
	"io"
	"sync"
	"time"
)

// LogRetention is how long a deploy's log stays in memory once nothing holds
// it: the deploy has finished (or never started writing here) and no one is
// following it. Long enough for a console that opens a deploy just after it
// ends to see the whole log; short enough that a busy install does not keep
// every deploy's output for the life of the process.
const LogRetention = 5 * time.Minute

// LogStore holds build and deploy output for streaming.
//
// In memory and bounded. Logs on disk under a size cap are R-222/R-223's
// concern and belong to the reconciler's garbage collection; this is the live
// tail a console watches while a deploy runs.
//
// A stream is dropped LogRetention after it was last held (see stream.idle).
// Eviction is lazy and amortized: Writer and Follow sweep the map at most once
// per quarter of the retention, and Has reports an expired stream as gone even
// before a sweep removes it. No goroutine is involved, so there is nothing to
// stop.
type LogStore struct {
	mu        sync.RWMutex
	streams   map[string]*stream
	maxLines  int
	retention time.Duration
	now       func() time.Time
	lastSweep time.Time
}

func NewLogStore() *LogStore {
	return &LogStore{
		streams:   map[string]*stream{},
		maxLines:  2000,
		retention: LogRetention,
		now:       time.Now,
	}
}

type stream struct {
	mu        sync.RWMutex
	lines     []string
	done      bool
	writing   bool // a Writer was opened; until a Sink closes, the deploy is running
	listeners []chan string
	maxLines  int
	// idleSince is when the stream last stopped being held: created,
	// finished, or left by its last listener. Meaningful only while idle.
	idleSince time.Time
}

// idle reports whether nothing holds the stream: no running writer and no
// listener. A stream Follow created for a deploy that never writes here counts
// as idle once its follower leaves, so a request for an unknown deploy does
// not leave an entry behind for good. Callers hold st.mu.
func (st *stream) idle() bool {
	return (st.done || !st.writing) && len(st.listeners) == 0
}

// expired reports whether a stream may be dropped. Callers hold st.mu.
func (s *LogStore) expired(st *stream, now time.Time) bool {
	return st.idle() && now.Sub(st.idleSince) >= s.retention
}

// sweepLocked drops expired streams, at most once per quarter retention so the
// cost is amortized over many calls. Callers hold s.mu for writing.
func (s *LogStore) sweepLocked(now time.Time) {
	if now.Sub(s.lastSweep) < s.retention/4 {
		return
	}
	s.lastSweep = now
	for id, st := range s.streams {
		st.mu.RLock()
		gone := s.expired(st, now)
		st.mu.RUnlock()
		if gone {
			delete(s.streams, id)
		}
	}
}

// openLocked returns a deployment's stream, creating it if need be. An expired
// stream is replaced rather than revived. Callers hold s.mu for writing.
func (s *LogStore) openLocked(deploymentID string, now time.Time) *stream {
	s.sweepLocked(now)
	if st, ok := s.streams[deploymentID]; ok {
		st.mu.RLock()
		gone := s.expired(st, now)
		st.mu.RUnlock()
		if !gone {
			return st
		}
	}
	st := &stream{maxLines: s.maxLines, idleSince: now}
	s.streams[deploymentID] = st
	return st
}

// Writer returns a sink for a deployment's output.
func (s *LogStore) Writer(deploymentID string) *Sink {
	s.mu.Lock()
	defer s.mu.Unlock()

	st := s.openLocked(deploymentID, s.now())
	st.mu.Lock()
	st.writing = true
	st.mu.Unlock()
	return &Sink{stream: st, now: s.now}
}

// Follow returns the lines so far plus a channel of new ones.
//
// The backlog is returned with the channel rather than replayed through it, so
// a client that connects late still sees the whole build instead of joining
// mid-stream.
func (s *LogStore) Follow(deploymentID string) ([]string, <-chan string, func()) {
	s.mu.Lock()
	st := s.openLocked(deploymentID, s.now())
	// Taken before s.mu is released, so a sweep cannot drop the stream between
	// finding it and registering the listener.
	st.mu.Lock()
	s.mu.Unlock()
	defer st.mu.Unlock()

	backlog := append([]string(nil), st.lines...)
	if st.done {
		closed := make(chan string)
		close(closed)
		return backlog, closed, func() {}
	}

	ch := make(chan string, 256)
	st.listeners = append(st.listeners, ch)

	cancel := func() {
		st.mu.Lock()
		defer st.mu.Unlock()
		for i, listener := range st.listeners {
			if listener == ch {
				st.listeners = append(st.listeners[:i], st.listeners[i+1:]...)
				close(ch)
				if len(st.listeners) == 0 {
					st.idleSince = s.now()
				}
				return
			}
		}
	}
	return backlog, ch, cancel
}

// Has reports whether this process holds any of a deployment's log: whether it
// is running the deploy, ran it within LogRetention, or someone is following
// it here. A stream past its retention is reported gone even before a sweep
// removes it.
func (s *LogStore) Has(deploymentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	st, ok := s.streams[deploymentID]
	if !ok {
		return false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return !s.expired(st, s.now())
}

// Sink is an io.Writer over one deployment's output.
type Sink struct {
	stream *stream
	now    func() time.Time
	buf    []byte
}

func (w *Sink) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := indexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.stream.emit(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Close flushes any partial line and ends the stream.
func (w *Sink) Close() error {
	if len(w.buf) > 0 {
		w.stream.emit(string(w.buf))
		w.buf = nil
	}
	w.stream.finish(w.now())
	return nil
}

func (s *stream) emit(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.lines = append(s.lines, line)
	if len(s.lines) > s.maxLines {
		s.lines = s.lines[len(s.lines)-s.maxLines:]
	}

	for _, listener := range s.listeners {
		select {
		case listener <- line:
		default:
			// A listener that cannot keep up loses lines rather than blocking
			// the build. A slow console must never slow a deploy.
		}
	}
}

func (s *stream) finish(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	s.idleSince = now
	for _, listener := range s.listeners {
		close(listener)
	}
	s.listeners = nil
}

func indexByte(b []byte, c byte) int {
	for i, got := range b {
		if got == c {
			return i
		}
	}
	return -1
}

var _ io.WriteCloser = (*Sink)(nil)
