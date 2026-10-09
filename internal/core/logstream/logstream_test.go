package logstream_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/logstream"
	"github.com/trypando/pando/internal/errs"
)

// fakeRuntime counts Logs calls and hands each one a pipe the test writes to.
type fakeRuntime struct {
	api.RuntimeAdapter

	mu      sync.Mutex
	opens   []api.LogOptions
	writers []*io.PipeWriter
	closed  int
	fail    error // returned by Logs when set
}

func (f *fakeRuntime) Logs(ctx context.Context, _ api.WorkloadRef, opts api.LogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, opts)
	if f.fail != nil {
		return nil, f.fail
	}
	pr, pw := io.Pipe()
	f.writers = append(f.writers, pw)
	return &countingCloser{PipeReader: pr, f: f}, nil
}

type countingCloser struct {
	*io.PipeReader
	f    *fakeRuntime
	once sync.Once
}

func (c *countingCloser) Close() error {
	c.once.Do(func() {
		c.f.mu.Lock()
		c.f.closed++
		c.f.mu.Unlock()
	})
	return c.PipeReader.Close()
}

func (f *fakeRuntime) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.opens)
}

func (f *fakeRuntime) closes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// print writes lines to the newest open stream.
func (f *fakeRuntime) print(t *testing.T, lines ...string) {
	t.Helper()
	f.mu.Lock()
	w := f.writers[len(f.writers)-1]
	f.mu.Unlock()
	_, err := io.WriteString(w, strings.Join(lines, "\n")+"\n")
	require.NoError(t, err)
}

// end ends the newest open stream, as a container restart does.
func (f *fakeRuntime) end() {
	f.mu.Lock()
	w := f.writers[len(f.writers)-1]
	f.mu.Unlock()
	_ = w.Close()
}

var key = logstream.Key{Runtime: "rt_fake", AppID: "app_1", Workload: "web"}

func next(t *testing.T, sub *logstream.Subscription) logstream.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		require.True(t, ok, "the stream closed")
		return ev
	case <-time.After(5 * time.Second):
		t.Fatal("no event arrived")
		return logstream.Event{}
	}
}

// TestO51_ManyViewersShareOneRuntimeStream asserts O-51: runtime load grows
// with watched parts, not with viewers.
func TestO51_ManyViewersShareOneRuntimeStream(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New()

	var subs []*logstream.Subscription
	for range 50 {
		sub, err := hub.Subscribe(t.Context(), rt, key)
		require.NoError(t, err)
		subs = append(subs, sub)
	}
	require.Equal(t, 1, rt.calls(), "fifty viewers, one runtime stream")
	require.True(t, rt.opens[0].Follow)
	require.Equal(t, logstream.DefaultBackfill, rt.opens[0].Tail)

	rt.print(t, "hello")
	for _, sub := range subs {
		require.Equal(t, logstream.Event{Kind: logstream.KindLine, Text: "hello"}, next(t, sub))
	}

	// Another part of the same app is another stream.
	other, err := hub.Subscribe(t.Context(), rt, logstream.Key{Runtime: "rt_fake", AppID: "app_1", Workload: "worker"})
	require.NoError(t, err)
	require.Equal(t, 2, rt.calls())
	require.Equal(t, 2, hub.Active())

	other.Close()
	for _, sub := range subs {
		sub.Close()
		sub.Close() // twice is harmless
	}
}

func TestO51_ALateViewerGetsTheBackfill(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithBackfill(3))

	first, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer first.Close()
	require.Empty(t, first.Backfill)

	rt.print(t, "one", "two", "three", "four")
	for range 4 {
		next(t, first)
	}

	late, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer late.Close()
	require.Equal(t, []string{"two", "three", "four"}, late.Backfill, "the newest lines, bounded by the ring")
	require.Equal(t, 1, rt.calls())

	rt.print(t, "five")
	require.Equal(t, "five", next(t, late).Text)
}

func TestO51_TheRuntimeStreamClosesAfterTheGracePeriod(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithGrace(50 * time.Millisecond))

	a, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	b, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)

	a.Close()
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 0, rt.closes(), "a viewer remains")

	// Leaving and coming back within the grace period reuses the stream.
	b.Close()
	c, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, rt.calls())
	require.Equal(t, 0, rt.closes())

	c.Close()
	require.Eventually(t, func() bool { return rt.closes() == 1 && hub.Active() == 0 },
		5*time.Second, 10*time.Millisecond)

	// The next viewer opens a new one.
	d, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer d.Close()
	require.Equal(t, 2, rt.calls())
}

func TestO51_AnOpenFailureIsReturnedAndNotKept(t *testing.T) {
	rt := &fakeRuntime{fail: errs.New(errs.NotFound, `There is nothing running called "web".`)}
	hub := logstream.New()

	_, err := hub.Subscribe(t.Context(), rt, key)
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	require.Equal(t, 0, hub.Active())

	rt.mu.Lock()
	rt.fail = nil
	rt.mu.Unlock()
	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	sub.Close()
}

func TestO51_ARestartIsReconnectedAndAnnounced(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithRetry(time.Millisecond, 4*time.Millisecond))

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer sub.Close()

	rt.print(t, "before")
	require.Equal(t, "before", next(t, sub).Text)

	rt.end()
	ended := next(t, sub)
	require.Equal(t, logstream.KindNotice, ended.Kind)
	require.Contains(t, ended.Text, "reconnecting")

	// A viewer joining while it is down is told so.
	late, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer late.Close()
	require.Equal(t, logstream.KindNotice, next(t, late).Kind)

	require.Eventually(t, func() bool { return rt.calls() >= 2 }, 5*time.Second, time.Millisecond)
	rt.mu.Lock()
	reopened := rt.opens[1]
	rt.mu.Unlock()
	require.True(t, reopened.Follow)
	require.False(t, reopened.Since.IsZero(), "reopened from when the stream ended, not from the start")

	rt.print(t, "after")
	resumed := next(t, sub)
	require.Equal(t, logstream.KindNotice, resumed.Kind)
	require.Contains(t, resumed.Text, "Reconnected")
	require.Equal(t, "after", next(t, sub).Text)
}

func TestO51_AReopenThatFailsIsRetried(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithRetry(time.Millisecond, 2*time.Millisecond))

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)

	rt.mu.Lock()
	rt.fail = errors.New("container is gone")
	rt.mu.Unlock()
	rt.end()
	require.Equal(t, logstream.KindNotice, next(t, sub).Kind)
	require.Eventually(t, func() bool { return rt.calls() >= 4 }, 5*time.Second, time.Millisecond)

	rt.mu.Lock()
	rt.fail = nil
	rt.mu.Unlock()
	n := rt.calls()
	require.Eventually(t, func() bool { return rt.calls() > n }, 5*time.Second, time.Millisecond)
	sub.Close()
}

// TestO51_ASlowViewerDoesNotHoldUpOthers asserts the shared stream never
// waits on one viewer.
func TestO51_ASlowViewerDoesNotHoldUpOthers(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithBuffer(4))

	slow, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	fast, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer fast.Close()

	for i := range 10 {
		rt.print(t, fmt.Sprint("line ", i))
		require.Equal(t, fmt.Sprint("line ", i), next(t, fast).Text)
	}

	// The slow one was cut off after its buffer filled, and says why.
	var got int
	for range slow.Events() {
		got++
	}
	require.Equal(t, 4, got)
	require.NotNil(t, slow.Ended())
	require.Equal(t, logstream.ReasonLagged, slow.Ended().Reason)
	slow.Close() // after being dropped, harmless
}

// recorder is a Viewer writing into a buffer.
type recorder struct {
	mu      sync.Mutex
	lines   []string
	notices []string
	flushes int
	failOn  string
}

func (r *recorder) Line(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s == r.failOn {
		return errors.New("broken pipe")
	}
	r.lines = append(r.lines, s)
	return nil
}

func (r *recorder) Notice(s string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notices = append(r.notices, s)
	return nil
}

func (r *recorder) Flush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.flushes++
	return nil
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines...)
}

// TestR048_RevokedAccessEndsALogStream asserts R-048 for log streams: access
// is checked again while a viewer stays connected, and the stream ends when it
// no longer holds.
func TestR048_RevokedAccessEndsALogStream(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithReauthEvery(5 * time.Millisecond))

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)

	var mu sync.Mutex
	allowed, checks := true, 0
	authorize := func(context.Context) error {
		mu.Lock()
		defer mu.Unlock()
		checks++
		if allowed {
			return nil
		}
		return errs.New(errs.PermDenied, "no")
	}

	done := make(chan error, 1)
	v := &recorder{}
	go func() { done <- hub.Serve(t.Context(), sub, authorize, v) }()

	rt.print(t, "visible")
	require.Eventually(t, func() bool { return len(v.seen()) == 1 }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool { mu.Lock(); defer mu.Unlock(); return checks >= 2 },
		5*time.Second, time.Millisecond)

	mu.Lock()
	allowed = false
	mu.Unlock()

	select {
	case err := <-done:
		var ended *logstream.Ended
		require.ErrorAs(t, err, &ended)
		require.Equal(t, logstream.ReasonRevoked, ended.Reason)
		require.Contains(t, ended.Error(), "access")
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived the access that opened it")
	}
	require.Eventually(t, func() bool { return hub.Active() == 1 }, time.Second, time.Millisecond)
}

func TestO51_ServeWritesBackfillThenLiveLinesAndEndsWithTheViewer(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithGrace(time.Millisecond))

	first, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	rt.print(t, "old")
	next(t, first)

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	first.Close()

	ctx, cancel := context.WithCancel(t.Context())
	v := &recorder{}
	done := make(chan error, 1)
	go func() { done <- hub.Serve(ctx, sub, func(context.Context) error { return nil }, v) }()

	rt.print(t, "new", "newer")
	rt.end()
	require.Eventually(t, func() bool { return len(v.seen()) == 3 }, 5*time.Second, time.Millisecond)
	require.Equal(t, []string{"old", "new", "newer"}, v.seen())
	require.Eventually(t, func() bool { v.mu.Lock(); defer v.mu.Unlock(); return len(v.notices) == 1 },
		5*time.Second, time.Millisecond)

	cancel()
	require.NoError(t, <-done, "a viewer leaving is not an error")
	require.Eventually(t, func() bool { return hub.Active() == 0 }, 5*time.Second, time.Millisecond)
}

func TestO51_ServeReportsALaggedViewer(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New(logstream.WithBuffer(2))

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	other, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer other.Close()

	// One line at a time, each read by other before the next is printed:
	// printed together, all three could arrive before other was read, and
	// other — buffered like sub — would lag and be closed too.
	for _, line := range []string{"a", "b", "c"} {
		rt.print(t, line)
		next(t, other)
	}

	err = hub.Serve(t.Context(), sub, func(context.Context) error { return nil }, &recorder{})
	var ended *logstream.Ended
	require.ErrorAs(t, err, &ended)
	require.Equal(t, logstream.ReasonLagged, ended.Reason)
}

func TestO51_ServeStopsWhenAWriteFails(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New()

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() {
		done <- hub.Serve(t.Context(), sub, func(context.Context) error { return nil }, &recorder{failOn: "boom"})
	}()
	rt.print(t, "fine", "boom")
	require.NoError(t, <-done)
}

func TestO51_AViewerWaitingOnAnOpenCanGiveUp(t *testing.T) {
	block := make(chan struct{})
	rt := &blockingRuntime{release: block}
	hub := logstream.New()

	opened := make(chan *logstream.Subscription, 1)
	go func() {
		sub, _ := hub.Subscribe(context.Background(), rt, key)
		opened <- sub
	}()
	require.Eventually(t, func() bool { return hub.Active() == 1 }, 5*time.Second, time.Millisecond)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := hub.Subscribe(ctx, rt, key)
	require.ErrorIs(t, err, context.Canceled)

	close(block)
	sub := <-opened
	require.NotNil(t, sub)
	sub.Close()
}

type blockingRuntime struct {
	api.RuntimeAdapter
	release chan struct{}
}

func (b *blockingRuntime) Logs(context.Context, api.WorkloadRef, api.LogOptions) (io.ReadCloser, error) {
	<-b.release
	pr, _ := io.Pipe()
	return pr, nil
}

func TestClampTail(t *testing.T) {
	require.Equal(t, logstream.DefaultTail, logstream.ClampTail(0))
	require.Equal(t, logstream.DefaultTail, logstream.ClampTail(-5))
	require.Equal(t, 42, logstream.ClampTail(42))
	require.Equal(t, logstream.MaxTail, logstream.ClampTail(logstream.MaxTail+1))
}

func TestO51_LongLinesAreCutAndCarriageReturnsDropped(t *testing.T) {
	rt := &fakeRuntime{}
	hub := logstream.New()

	sub, err := hub.Subscribe(t.Context(), rt, key)
	require.NoError(t, err)
	defer sub.Close()

	long := strings.Repeat("x", logstream.MaxLineBytes+5000)
	rt.print(t, long, "crlf\r", "")
	got := next(t, sub).Text
	require.Len(t, got, logstream.MaxLineBytes+len("…"))
	require.True(t, strings.HasSuffix(got, "…"))
	require.Equal(t, "crlf", next(t, sub).Text)
	require.Equal(t, "", next(t, sub).Text, "an empty line is a line")

	// A final line without a newline is still delivered when the stream ends.
	rt.mu.Lock()
	w := rt.writers[0]
	rt.mu.Unlock()
	bw := bufio.NewWriter(w)
	_, _ = bw.WriteString("unterminated")
	_ = bw.Flush()
	_ = w.Close()
	require.Equal(t, "unterminated", next(t, sub).Text)
}
