package proxy

import (
	"net"
	"net/http"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/trypando/pando/internal/clientaddr"
)

// Anonymous requests are rate-limited (R-075), per app and client address
// [P]: a sustained rate, and a burst above it so that a page and its assets
// load at once. Generous on purpose: this is a guard against one address
// flooding a public app, not a quota, and a person browsing never meets it.
const (
	anonymousRate  = rate.Limit(50)
	anonymousBurst = 300

	// anonymousIdle is how long a bucket goes unused before it is forgotten.
	// Longer than a full bucket takes to refill, so forgetting one never
	// gives anyone a fresh allowance they would not already have had.
	anonymousIdle = time.Minute
)

// anonymousLimiter holds a token bucket per app and client address.
//
// In each replica's memory rather than in the database, unlike the limits on
// guessing a passcode or password (issue #72): with N replicas a flood gets N
// times the rate, which still bounds it, and a database write per anonymous
// request would make the limit the cheapest way to load Postgres.
type anonymousLimiter struct {
	mu        sync.Mutex
	buckets   map[string]*anonymousBucket
	lastSweep time.Time
}

type anonymousBucket struct {
	lim  *rate.Limiter
	seen time.Time
}

func newAnonymousLimiter() *anonymousLimiter {
	return &anonymousLimiter{buckets: map[string]*anonymousBucket{}}
}

// allow takes one request from key's bucket at now, reporting whether there
// was one to take.
func (l *anonymousLimiter) allow(key string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.lastSweep) > anonymousIdle {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > anonymousIdle {
				delete(l.buckets, k)
			}
		}
		l.lastSweep = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &anonymousBucket{lim: rate.NewLimiter(anonymousRate, anonymousBurst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.lim.AllowN(now, 1)
}

// sourceAddr is the client address the audit log records for r: the TCP peer
// unless the peer is a listed trusted proxy (design 12 §3.2). Keyed on the
// bare peer, every visitor behind Traefik or a tunnel would share one bucket.
func sourceAddr(r *http.Request) string {
	if req, ok := clientaddr.From(r.Context()); ok && req.SourceIP != "" {
		return req.SourceIP
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
