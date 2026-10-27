package httpapi

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/trypando/pando/internal/clientaddr"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// FailedAttempts counts wrong guesses at a passcode (R-075a), a password
// (R-404) or a two-factor code (R-405), by a key that says which and whose.
//
// Shared by every replica in production (state.FailedAttempts): a count kept
// in one process's memory gave an attacker one allowance per replica behind
// the load balancer, and a fresh one whenever a replica restarted (issue #72).
//
// An attempt is counted as a pending failure before it is checked, atomically
// with reading the count; a wrong guess settles it as a failure and a right
// one releases it. Read first and recorded after, a burst of concurrent
// attempts all saw the same count and every one was checked. A limit reached
// only by pending attempts waits for them rather than refusing, so concurrent
// right guesses are not refused because of each other.
type FailedAttempts interface {
	Reserve(ctx context.Context, limits ...state.AttemptLimit) (state.Reservation, error)
	Fail(ctx context.Context, res state.Reservation) error
	Release(ctx context.Context, res state.Reservation) error
	Clear(ctx context.Context, key string) error
}

// memoryFailedAttempts is the count when no store is wired, as in a test that
// builds a Server by hand. Correct for one process and only one, and one per
// Server, so that tests in one process do not lock each other out.
type memoryFailedAttempts struct {
	mu   sync.Mutex
	seen map[string][]memoryAttempt
}

// memoryAttempt is one counted attempt, pending until settled.
type memoryAttempt struct {
	at      time.Time
	pending bool
}

// within keeps key's attempts inside window and returns them, oldest first.
// The caller holds mu.
func (l *memoryFailedAttempts) within(key string, window time.Duration) []memoryAttempt {
	now := time.Now()
	recent := l.seen[key][:0]
	for _, a := range l.seen[key] {
		if now.Sub(a.at) < window {
			recent = append(recent, a)
		}
	}
	l.seen[key] = recent
	return recent
}

// memoryPendingPoll is how often a waiting Reserve looks again.
const memoryPendingPoll = 5 * time.Millisecond

func (l *memoryFailedAttempts) Reserve(ctx context.Context, limits ...state.AttemptLimit) (state.Reservation, error) {
	deadline := time.Now().Add(state.PendingWait)
	for {
		res, waiting := l.reserveOnce(limits)
		if !waiting || !time.Now().Before(deadline) {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return state.Reservation{}, ctx.Err()
		case <-time.After(memoryPendingPoll):
		}
	}
}

// reserveOnce is one try at Reserve. Waiting reports a refusal only because
// of attempts still pending.
func (l *memoryFailedAttempts) reserveOnce(limits []state.AttemptLimit) (state.Reservation, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	blocked := state.Reservation{Over: -1}
	for i, limit := range limits {
		recent := l.within(limit.Key, limit.Window)
		failed := settled(recent)
		if len(recent) == 0 {
			continue
		}
		over := state.Reservation{Over: i, Wait: max(time.Until(recent[0].at.Add(limit.Window)), time.Second)}
		if failed >= limit.Max {
			return over, false
		}
		if len(recent) >= limit.Max && !blocked.Refused() {
			blocked = over
		}
	}
	if blocked.Refused() {
		return blocked, true
	}
	return l.holdAll(limits), false
}

// settled counts the attempts that count as failures: those no longer
// pending, and pending ones too old to still be in flight.
func settled(attempts []memoryAttempt) int {
	n := 0
	for _, a := range attempts {
		if !a.pending || time.Since(a.at) >= state.PendingStale {
			n++
		}
	}
	return n
}

// holdAll counts a pending failure against every limit. The caller holds mu.
func (l *memoryFailedAttempts) holdAll(limits []state.AttemptLimit) state.Reservation {
	res := state.Reservation{Over: -1}
	for _, limit := range limits {
		at := time.Now()
		l.seen[limit.Key] = append(l.seen[limit.Key], memoryAttempt{at: at, pending: true})
		res.Held = append(res.Held, state.HeldAttempt{Key: limit.Key, At: at})
	}
	return res
}

// held is the index of h among its key's attempts, or -1. The caller holds mu.
func (l *memoryFailedAttempts) held(h state.HeldAttempt) int {
	return slices.IndexFunc(l.seen[h.Key], func(a memoryAttempt) bool { return a.at.Equal(h.At) && a.pending })
}

func (l *memoryFailedAttempts) Fail(_ context.Context, res state.Reservation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range res.Held {
		if i := l.held(h); i >= 0 {
			l.seen[h.Key][i].pending = false
		}
	}
	return nil
}

func (l *memoryFailedAttempts) Release(_ context.Context, res state.Reservation) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, h := range res.Held {
		if i := l.held(h); i >= 0 {
			l.seen[h.Key] = slices.Delete(l.seen[h.Key], i, i+1)
		}
	}
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
	s.attemptsOnce.Do(func() { s.attempts = &memoryFailedAttempts{seen: map[string][]memoryAttempt{}} })
	return s.attempts
}

// limited is a limit and what a refusal over it says.
type limited struct {
	state.AttemptLimit
	what string
}

// reserveAttempt counts an attempt as a failure against every limit before it
// is checked (R-075a, R-404, R-405). Over one, it answers with how long to
// wait, calls refused first when given (to audit the refusal), and reports
// false. An unreadable count is an error, never a free attempt: the limit
// fails closed.
func (s *Server) reserveAttempt(w http.ResponseWriter, r *http.Request, refused func(), limits ...limited) (state.Reservation, bool) {
	var over func(state.AttemptLimit)
	if refused != nil {
		over = func(state.AttemptLimit) { refused() }
	}
	return s.reserveLimited(w, r, over, limits...)
}

// reserveLimited is reserveAttempt, telling refused which limit the attempt
// was over.
func (s *Server) reserveLimited(w http.ResponseWriter, r *http.Request, refused func(over state.AttemptLimit), limits ...limited) (state.Reservation, bool) {
	plain := make([]state.AttemptLimit, len(limits))
	for i, l := range limits {
		plain[i] = l.AttemptLimit
	}
	res, err := s.failedAttempts().Reserve(r.Context(), plain...)
	if err != nil {
		Error(w, r, err)
		return res, false
	}
	if res.Refused() {
		if refused != nil {
			refused(plain[res.Over])
		}
		tooManyAttempts(w, r, res.Wait, limits[res.Over].what)
		return res, false
	}
	return res, true
}

// settle ends a reserved attempt by how its check came out. A wrong guess
// (AUTH_INVALID) settles the failures it was counted as. Anything else — a right
// guess, or a check that could not be made — takes them back, and a right one
// also forgets the failures under each key in forget.
func (s *Server) settle(r *http.Request, res state.Reservation, err error, forget ...string) {
	ctx := context.WithoutCancel(r.Context())
	f := s.failedAttempts()
	if err != nil && errs.CodeOf(err) == errs.AuthInvalid {
		_ = f.Fail(ctx, res)
		return
	}
	_ = f.Release(ctx, res)
	if err == nil {
		for _, key := range forget {
			_ = f.Clear(ctx, key)
		}
	}
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
