package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/trypando/pando/internal/clientaddr"
	"github.com/trypando/pando/internal/errs"
)

// FailedAttempts counts wrong guesses at a passcode (R-075a) or a password
// (R-404), by a key that says which and whose.
//
// Shared by every replica in production (state.FailedAttempts): a count kept
// in one process's memory gave an attacker one allowance per replica behind
// the load balancer, and a fresh one whenever a replica restarted (issue #72).
type FailedAttempts interface {
	Recent(ctx context.Context, key string, window time.Duration) (int, error)
	// Wait is how long until the oldest failure within window leaves it.
	Wait(ctx context.Context, key string, window time.Duration) (time.Duration, error)
	Record(ctx context.Context, key string, window time.Duration) error
	Clear(ctx context.Context, key string) error
}

// memoryFailedAttempts is the count when no store is wired, as in a test that
// builds a Server by hand. Correct for one process and only one, and one per
// Server, so that tests in one process do not lock each other out.
type memoryFailedAttempts struct {
	mu   sync.Mutex
	seen map[string][]time.Time
}

// within keeps key's failures inside window and returns them, oldest first.
// The caller holds mu.
func (l *memoryFailedAttempts) within(key string, window time.Duration) []time.Time {
	now := time.Now()
	recent := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if now.Sub(t) < window {
			recent = append(recent, t)
		}
	}
	l.seen[key] = recent
	return recent
}

func (l *memoryFailedAttempts) Recent(_ context.Context, key string, window time.Duration) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.within(key, window)), nil
}

func (l *memoryFailedAttempts) Wait(_ context.Context, key string, window time.Duration) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.within(key, window)
	if len(recent) == 0 {
		return 0, nil
	}
	return max(time.Until(recent[0].Add(window)), 0), nil
}

func (l *memoryFailedAttempts) Record(_ context.Context, key string, _ time.Duration) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.seen[key] = append(l.seen[key], time.Now())
	return nil
}

func (l *memoryFailedAttempts) Clear(_ context.Context, key string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.seen, key)
	return nil
}

func (s *Server) failedAttempts() FailedAttempts {
	if s.FailedAttempts != nil {
		return s.FailedAttempts
	}
	s.attemptsOnce.Do(func() { s.attempts = &memoryFailedAttempts{seen: map[string][]time.Time{}} })
	return s.attempts
}

// attemptLimit is how many failures a key may have within a window before
// another attempt is refused without being checked.
type attemptLimit struct {
	key    string
	max    int
	window time.Duration
}

// exceeded reports how long until the limit lets another attempt through, or
// zero when it already does. An unreadable count is an error, never a free
// attempt: the limit fails closed.
func (l attemptLimit) exceeded(ctx context.Context, f FailedAttempts) (time.Duration, error) {
	n, err := f.Recent(ctx, l.key, l.window)
	if err != nil || n < l.max {
		return 0, err
	}
	wait, err := f.Wait(ctx, l.key, l.window)
	if err != nil {
		return 0, err
	}
	// Never zero once over the limit, so the caller can tell the two apart.
	return max(wait, time.Second), nil
}

// tooManyAttempts refuses an attempt over a limit, saying when to try again:
// in a Retry-After header for a client and in the message for a person.
func tooManyAttempts(w http.ResponseWriter, r *http.Request, wait time.Duration, what string) {
	secs := int(math.Ceil(wait.Seconds()))
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	Error(w, r, errs.Newf(errs.RateLimited, "%s Try again in %s.", what, waitPhrase(wait)).
		WithDetail("retry_after_seconds", secs))
}

// waitPhrase says how long to wait in words: "1 minute", "12 minutes",
// "30 seconds".
func waitPhrase(d time.Duration) string {
	if d < time.Minute {
		s := int(math.Ceil(d.Seconds()))
		if s == 1 {
			return "1 second"
		}
		return fmt.Sprintf("%d seconds", s)
	}
	m := int(math.Ceil(d.Minutes()))
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// limitAddr is the client address a limit on guessing is kept per (R-075a,
// R-404): the one the audit log records, which is the TCP peer unless the peer
// is a listed trusted proxy (design 12 §3.2). Keying on the bare peer made one
// count of the whole install behind Traefik or a tunnel, so anyone could lock
// everyone out.
func limitAddr(r *http.Request) string {
	if req, ok := clientaddr.From(r.Context()); ok && req.SourceIP != "" {
		return req.SourceIP
	}
	return clientIP(r)
}
