package audit

import (
	"sync"
	"time"
)

// ReadThrottle decides when a read of the audit stream is itself audited
// (R-388): at most once per reader per Every, per replica. A collector
// long-polling every two seconds would otherwise put tens of thousands of
// events a day about itself into the log it is reading. Listing, exporting
// and downloading an archive are audited every time and do not use it.
type ReadThrottle struct {
	Every time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

// DefaultReadEvery is how often one reader's stream reads are recorded.
const DefaultReadEvery = time.Hour

// Due reports whether reader's read at now should be audited, and records it
// if so.
func (t *ReadThrottle) Due(reader string, now time.Time) bool {
	if t == nil {
		// No throttle records every read: too many events beats none.
		return true
	}
	every := t.Every
	if every <= 0 {
		every = DefaultReadEvery
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.last == nil {
		t.last = map[string]time.Time{}
	}
	if at, ok := t.last[reader]; ok && now.Sub(at) < every {
		return false
	}
	t.last[reader] = now
	// Readers come and go; a map that only grows is a leak on a busy
	// install, so anything older than the window is forgotten.
	if len(t.last) > 10_000 {
		for k, at := range t.last {
			if now.Sub(at) >= every {
				delete(t.last, k)
			}
		}
	}
	return true
}
