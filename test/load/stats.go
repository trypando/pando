package main

import (
	"math"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Histogram records latencies in logarithmic buckets, each about 2% wider
// than the last, from 100 µs to about two minutes. A quantile read from it is
// within 2% of the true one, and its size does not grow with the number of
// requests, which at the cluster tier is millions per step.
type Histogram struct {
	Counts []uint64 `json:"counts"`
	Total  uint64   `json:"total"`
	// Max is kept exactly; the top bucket would otherwise hide a timeout.
	Max time.Duration `json:"max"`
}

const (
	histMin    = 100 * time.Microsecond
	histGrowth = 1.02
	histSize   = 720 // histMin * 1.02^720 ≈ 155 s
)

func NewHistogram() *Histogram { return &Histogram{Counts: make([]uint64, histSize)} }

func bucketOf(d time.Duration) int {
	if d <= histMin {
		return 0
	}
	b := int(math.Log(float64(d)/float64(histMin))/math.Log(histGrowth)) + 1
	if b >= histSize {
		return histSize - 1
	}
	return b
}

// bucketUpper is the largest latency bucket b holds.
func bucketUpper(b int) time.Duration {
	return time.Duration(float64(histMin) * math.Pow(histGrowth, float64(b)))
}

func (h *Histogram) Add(d time.Duration) {
	h.Counts[bucketOf(d)]++
	h.Total++
	if d > h.Max {
		h.Max = d
	}
}

// Merge adds o's samples to h.
func (h *Histogram) Merge(o *Histogram) {
	for i, c := range o.Counts {
		h.Counts[i] += c
	}
	h.Total += o.Total
	if o.Max > h.Max {
		h.Max = o.Max
	}
}

// Quantile returns the latency q of the samples were at or under: 0.95 is
// p95. Zero with no samples.
func (h *Histogram) Quantile(q float64) time.Duration {
	if h.Total == 0 {
		return 0
	}
	rank := uint64(math.Ceil(q * float64(h.Total)))
	if rank < 1 {
		rank = 1
	}
	var seen uint64
	for b, c := range h.Counts {
		seen += c
		if seen >= rank {
			if u := bucketUpper(b); b < histSize-1 && u < h.Max {
				return u
			}
			return h.Max
		}
	}
	return h.Max
}

// ClassStats is one endpoint class's numbers for one step.
type ClassStats struct {
	Class    string            `json:"class"`
	Surface  string            `json:"surface"`
	Requests uint64            `json:"requests"`
	Errors   uint64            `json:"errors"`
	Statuses map[string]uint64 `json:"statuses"`
	Latency  *Histogram        `json:"latency"`
	// Seconds is how long the step recorded for; Requests/Seconds is the
	// achieved rate.
	Seconds float64 `json:"seconds"`
}

func (c *ClassStats) ErrorRate() float64 {
	if c.Requests == 0 {
		return 0
	}
	return float64(c.Errors) / float64(c.Requests)
}

func (c *ClassStats) Rate() float64 {
	if c.Seconds <= 0 {
		return 0
	}
	return float64(c.Requests) / c.Seconds
}

// Recorder collects outcomes while a step holds, and nothing outside it: a
// ramp's sign-ins and warm-up are not the step's steady state. Sign-ins are
// the exception, recorded always, because the ramp is where they happen.
type Recorder struct {
	mu        sync.Mutex
	recording bool
	classes   map[string]*ClassStats
}

func NewRecorder() *Recorder { return &Recorder{classes: map[string]*ClassStats{}} }

// Outcome is one request's result. Status is 0 for a transport error.
type Outcome struct {
	Surface string
	Class   string
	Status  int
	Err     error
	OK      bool
	Latency time.Duration
	// Always records the outcome whether or not the step is holding.
	Always bool
}

func (r *Recorder) Record(o Outcome) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.recording && !o.Always {
		return
	}
	c, ok := r.classes[o.Class]
	if !ok {
		c = &ClassStats{Class: o.Class, Surface: o.Surface, Statuses: map[string]uint64{}, Latency: NewHistogram()}
		r.classes[o.Class] = c
	}
	c.Requests++
	if !o.OK {
		c.Errors++
	}
	c.Statuses[statusKey(o)]++
	c.Latency.Add(o.Latency)
}

func statusKey(o Outcome) string {
	if o.Status == 0 {
		if o.Err != nil && isTimeout(o.Err) {
			return "timeout"
		}
		return "transport error"
	}
	return strconv.Itoa(o.Status)
}

// Hold starts recording.
func (r *Recorder) Hold() {
	r.mu.Lock()
	r.recording = true
	r.mu.Unlock()
}

// Take stops recording and returns what was recorded since the last Take,
// stamped with how long the hold lasted, sorted by surface then class.
func (r *Recorder) Take(held time.Duration) []*ClassStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.recording = false
	out := make([]*ClassStats, 0, len(r.classes))
	for _, c := range r.classes {
		c.Seconds = held.Seconds()
		out = append(out, c)
	}
	r.classes = map[string]*ClassStats{}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Surface != out[j].Surface {
			return out[i].Surface < out[j].Surface
		}
		return out[i].Class < out[j].Class
	})
	return out
}
