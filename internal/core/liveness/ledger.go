package liveness

import "time"

// Ledger is the leader's collection of what every host reports it has not
// reached, and the one place a host is taken for lost (R-421). Times given
// to it are readings of the leader's own monotonic clock; the durations in a
// report are the observer's own.
//
// It is kept in memory by whichever replica leads, not in the database. A
// decision needs only the last three Periods of reports, and a new leader
// that starts empty has only to wait those three Periods again: slower, never
// wrong. Keeping it in Postgres would put the database on the path of a
// decision that must not depend on it.
//
// A Ledger is not safe for concurrent use; Watch holds it under one goroutine.
type Ledger struct {
	cfg     Config
	reports map[string]report
}

type report struct {
	at        time.Duration
	unreached map[string]time.Duration
}

// NewLedger returns an empty Ledger.
func NewLedger(cfg Config) *Ledger {
	return &Ledger{cfg: cfg.Normalize(), reports: map[string]report{}}
}

// Record keeps observer's report, received at now: for each peer, how long
// the observer has watched it without contact.
func (l *Ledger) Record(now time.Duration, observer string, unreached map[string]time.Duration) {
	l.reports[observer] = report{at: now, unreached: unreached}
}

// Forget drops what observer reported, as when it leaves the cluster.
func (l *Ledger) Forget(observer string) { delete(l.reports, observer) }

// Silent is how many of hosts, x excepted, report having watched x without
// contact for at least three Periods, in a report the leader heard lately.
func (l *Ledger) Silent(now time.Duration, hosts []string, x string) int {
	need := l.cfg.LostAfter()
	count := 0
	for _, y := range hosts {
		if y == x {
			continue
		}
		r, ok := l.reports[y]
		if !ok || now < r.at || now-r.at > l.cfg.Fresh() {
			// Unheard, or heard too long ago: it has said nothing.
			continue
		}
		if d, ok := r.unreached[x]; ok && d >= need {
			count++
		}
	}
	return count
}

// Lost reports whether x is lost (R-421): the cluster, x included, has at
// least three hosts, and more than half of them have not reached x for three
// Periods. x reaches itself, so it is never one of them.
func (l *Ledger) Lost(now time.Duration, hosts []string, x string) bool {
	n := len(hosts)
	if n < MinHosts {
		return false
	}
	return moreThanHalf(l.Silent(now, hosts, x), n)
}
