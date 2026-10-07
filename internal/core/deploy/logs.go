package deploy

import (
	"io"
	"sync"
)

// LogStore holds build and deploy output for streaming.
//
// In memory and bounded. Logs on disk under a size cap are R-222/R-223's
// concern and belong to the reconciler's garbage collection; this is the live
// tail a console watches while a deploy runs.
type LogStore struct {
	mu       sync.RWMutex
	streams  map[string]*stream
	maxLines int
}

func NewLogStore() *LogStore {
	return &LogStore{streams: map[string]*stream{}, maxLines: 2000}
}

type stream struct {
	mu        sync.RWMutex
	lines     []string
	done      bool
	listeners []chan string
	maxLines  int
}

// Writer returns a sink for a deployment's output.
func (s *LogStore) Writer(deploymentID string) *Sink {
	s.mu.Lock()
	defer s.mu.Unlock()

	st, ok := s.streams[deploymentID]
	if !ok {
		st = &stream{maxLines: s.maxLines}
		s.streams[deploymentID] = st
	}
	return &Sink{stream: st}
}

// Follow returns the lines so far plus a channel of new ones.
//
// The backlog is returned with the channel rather than replayed through it, so
// a client that connects late still sees the whole build instead of joining
// mid-stream.
func (s *LogStore) Follow(deploymentID string) ([]string, <-chan string, func()) {
	s.mu.Lock()
	st, ok := s.streams[deploymentID]
	if !ok {
		st = &stream{maxLines: s.maxLines}
		s.streams[deploymentID] = st
	}
	s.mu.Unlock()

	st.mu.Lock()
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
				return
			}
		}
	}
	return backlog, ch, cancel
}

// Sink is an io.Writer over one deployment's output.
// Has reports whether this process holds any of a deployment's log: whether it
// ran the deploy (since it last started) or someone is already following it
// here.
func (s *LogStore) Has(deploymentID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.streams[deploymentID]
	return ok
}

type Sink struct {
	stream *stream
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
	w.stream.finish()
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

func (s *stream) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
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
