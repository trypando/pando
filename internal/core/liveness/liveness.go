// Package liveness decides, from what the cluster's hosts can reach, when a
// host must stop its own apps and when the leader may take a host for lost
// (R-421, docs/design/notes-cluster.md "Deciding a host is gone, and
// fencing").
//
// The decisions are pure and live in Tracker (one host's own view) and Ledger
// (the leader's collection of every host's view). Monitor, Watch and Service
// run them: probing, stopping, recording and starting standbys.
//
// The rule, on both sides:
//
//   - A host stops its own apps only when it has reached fewer than half the
//     cluster's hosts, itself included, for Period.
//   - The leader takes a host for lost only when more than half the hosts
//     have not reached it for three times Period, so the lost host has
//     stopped its apps before its standbys start.
//   - With fewer than three hosts neither happens by itself.
//
// Everything here leans one way. A contact anywhere in a window counts the
// peer as reached; a gap in this host's own probing (a suspended machine, a
// clock that jumped) restarts the window rather than counting as silence; a
// report the leader has not heard lately counts for nothing. Each of those can
// only delay a fence or a failover, never cause one: a host that is not cut
// off must never stop its apps.
//
// Durations are measured on each host's own monotonic clock. No host compares
// its clock with another's: what crosses the wire is how long, never when.
package liveness

import (
	"errors"
	"sync"
	"time"

	"github.com/trypando/pando/internal/core/clock"
)

// DefaultPeriod is T [P: 30 s] (R-421).
const DefaultPeriod = 30 * time.Second

// MinHosts is the smallest cluster in which anything stops or fails over by
// itself (R-421). Two hosts cannot tell a dead peer from a cut link.
const MinHosts = 3

// lostFactor is how many Periods a majority must not have reached a host
// before it is lost (R-421).
const lostFactor = 3

// Config holds the timings. Zero values take the defaults.
type Config struct {
	// Period is T: how long a host must have reached fewer than half the
	// cluster before it stops its apps. DefaultPeriod when zero.
	Period time.Duration

	// Interval is how often a host probes every other. Period/6 when zero,
	// and never more than that.
	Interval time.Duration

	// Timeout bounds one probe. Interval/2 when zero, and never more than
	// Interval.
	Timeout time.Duration

	// PeerWait is how long a round waits for the list of peers before it
	// probes the last one known. The list is read beside the round, never
	// in it, so a database that hangs never keeps a host from probing, or
	// from fencing. The smaller of Timeout and 2 s when zero.
	PeerWait time.Duration
}

// maxPeerWait bounds PeerWait.
const maxPeerWait = 2 * time.Second

// ErrContactOnly is wrapped by a probe's error when the peer's machine
// answered but gave no view: it turned the connection away, failed the
// handshake, or refused the ping. That is contact (R-421): the machine is up
// and on the network, though its Pando process may be down, restarting or
// waiting for its database, and its apps may be running. Only silence — a
// probe that timed out or found no route — is not contact.
var ErrContactOnly = errors.New("the host's machine answered without its view")

// contact reports whether a probe that returned err reached the peer.
func contact(err error) bool { return err == nil || errors.Is(err, ErrContactOnly) }

// Normalize fills in the defaults and holds the timings to each other.
func (c Config) Normalize() Config {
	if c.Period <= 0 {
		c.Period = DefaultPeriod
	}
	if c.Interval <= 0 || c.Interval > c.Period/6 {
		c.Interval = c.Period / 6
	}
	if c.Timeout <= 0 || c.Timeout > c.Interval {
		c.Timeout = c.Interval / 2
	}
	if c.PeerWait <= 0 || c.PeerWait > c.Timeout {
		c.PeerWait = min(c.Timeout, maxPeerWait)
	}
	return c
}

// MaxGap is the longest pause between two of a host's own probe rounds that
// still counts as probing without a break. A longer one — the machine was
// suspended, the process stalled, the clock jumped — starts the host's
// window again.
func (c Config) MaxGap() time.Duration { return 3 * c.Normalize().Interval }

// Fresh is how recently the leader must have heard a host's report for it to
// count. A host the leader has not heard from lately has said nothing.
func (c Config) Fresh() time.Duration { return 3 * c.Normalize().Interval }

// LostAfter is how long a majority must not have reached a host before the
// leader takes it for lost: three times Period (R-421).
func (c Config) LostAfter() time.Duration { return lostFactor * c.Normalize().Period }

// Elapsed returns a reader of c's monotonic time since the call: a duration
// that never goes backward.
//
// clock.System's Now is UTC, and UTC drops Go's monotonic reading, so for the
// system clock this reads time.Since on a monotonic start instead: a wall
// clock stepped by NTP or by hand moves nothing here. For any other clock —
// a test's Fake — it reads Now, and a reading earlier than the last is held
// at the last, so a clock set backward stops time rather than reversing it.
func Elapsed(c clock.Clock) func() time.Duration {
	var read func() time.Duration
	if _, ok := c.(clock.System); ok {
		start := time.Now()
		read = func() time.Duration { return time.Since(start) }
	} else {
		start := c.Now()
		read = func() time.Duration { return c.Now().Sub(start) }
	}
	var mu sync.Mutex
	var last time.Duration
	return func() time.Duration {
		mu.Lock()
		defer mu.Unlock()
		if r := read(); r > last {
			last = r
		}
		return last
	}
}

// belowHalf reports whether reached is fewer than half of n.
func belowHalf(reached, n int) bool { return 2*reached < n }

// moreThanHalf reports whether count is more than half of n.
func moreThanHalf(count, n int) bool { return 2*count > n }
