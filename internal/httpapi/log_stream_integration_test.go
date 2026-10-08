//go:build integration

package httpapi_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/logstream"
	"github.com/trypando/pando/internal/httpapi"
)

// streamRuntime is a runtime whose logs are pipes the test writes to, and
// which counts how often it is asked for them.
type streamRuntime struct {
	adapterapi.RuntimeAdapter

	mu      sync.Mutex
	opens   []adapterapi.LogOptions
	writers []*io.PipeWriter
}

func (f *streamRuntime) Category() adapterapi.Category { return adapterapi.CategoryRuntime }

// Observe reports a provisioned database the spec does not declare, as a
// runtime running one would.
func (f *streamRuntime) Observe(context.Context, adapterapi.BundleRef) (adapterapi.ObservedBundle, error) {
	return adapterapi.ObservedBundle{Workloads: []adapterapi.ObservedWorkload{
		{Name: "web", Present: true, Running: true},
		{Name: "db", Present: true, Running: true},
	}}, nil
}

func (f *streamRuntime) Logs(_ context.Context, _ adapterapi.WorkloadRef, opts adapterapi.LogOptions) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opens = append(f.opens, opts)
	if !opts.Follow {
		return io.NopCloser(strings.NewReader("one-shot\n")), nil
	}
	pr, pw := io.Pipe()
	f.writers = append(f.writers, pw)
	return pr, nil
}

func (f *streamRuntime) follows() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.opens {
		if o.Follow {
			n++
		}
	}
	return n
}

func (f *streamRuntime) print(t *testing.T, line string) {
	t.Helper()
	f.mu.Lock()
	w := f.writers[len(f.writers)-1]
	f.mu.Unlock()
	_, err := io.WriteString(w, line+"\n")
	require.NoError(t, err)
}

// appOnStreamRuntime is a pinned app whose spec names a fake runtime.
func appOnStreamRuntime(t *testing.T, i *install, s *session) (string, *streamRuntime) {
	t.Helper()
	rt := &streamRuntime{}
	require.NoError(t, i.Server.Registry.Register("rt_stream", rt))

	id := i.createApp(s, "chatty")
	body := minimalSpec()
	body["runtime"] = map[string]any{"adapter_ref": "rt_stream"}
	i.pinSpec(s, id, i.writeSpec(s, id, body))
	return id, rt
}

// sse reads one server-sent events response.
type sse struct {
	resp *http.Response
	r    *bufio.Reader
}

type sseEvent struct{ name, data string }

func openSSE(t *testing.T, base string, s *session, path string) *sse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/api/v1"+path, nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", httpapi.SessionCookie+"="+s.cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	return &sse{resp: resp, r: bufio.NewReader(resp.Body)}
}

// next returns the next event, skipping the retry field.
func (s *sse) next(t *testing.T) sseEvent {
	t.Helper()
	var ev sseEvent
	for {
		line, err := s.r.ReadString('\n')
		require.NoError(t, err)
		line = strings.TrimSuffix(line, "\n")
		switch {
		case line == "":
			if ev.name != "" || ev.data != "" {
				return ev
			}
		case strings.HasPrefix(line, "event: "):
			ev.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			ev.data = strings.TrimPrefix(line, "data: ")
		}
	}
}

// TestO51_EveryViewerOfAPartSharesOneRuntimeStream asserts O-51 end to end:
// two people watching one part of an app are one runtime log stream, and a
// late one starts from the recent lines.
func TestO51_EveryViewerOfAPartSharesOneRuntimeStream(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id, rt := appOnStreamRuntime(t, i, admin)

	srv := httptest.NewServer(i.handler)
	t.Cleanup(srv.Close) // registered first, so it runs after the streams close

	first := openSSE(t, srv.URL, admin, "/apps/"+id+"/logs/stream")
	require.Equal(t, http.StatusOK, first.resp.StatusCode)
	require.Equal(t, "text/event-stream", first.resp.Header.Get("Content-Type"))
	require.Equal(t, sseEvent{name: "reset"}, first.next(t))

	rt.print(t, "started")
	require.Equal(t, sseEvent{data: "started"}, first.next(t))

	second := openSSE(t, srv.URL, admin, "/apps/"+id+"/logs/stream?workload=web")
	require.Equal(t, sseEvent{name: "reset"}, second.next(t))
	require.Equal(t, sseEvent{data: "started"}, second.next(t), "the backfill")

	rt.print(t, "for both")
	require.Equal(t, "for both", first.next(t).data)
	require.Equal(t, "for both", second.next(t).data)

	// The CLI's follow reads the same stream as text.
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet,
		srv.URL+"/api/v1/apps/"+id+"/logs?follow=true", nil)
	require.NoError(t, err)
	req.Header.Set("Cookie", httpapi.SessionCookie+"="+admin.cookie)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	text := bufio.NewReader(resp.Body)
	line, err := text.ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "started\n", line)

	require.Equal(t, 1, rt.follows(), "three viewers, one runtime stream")
}

// TestR048_ALogStreamEndsWhenTheViewersSessionIsRevoked asserts R-048 for the
// live log: a session ended while its log stream is open ends the stream, and
// says why.
func TestR048_ALogStreamEndsWhenTheViewersSessionIsRevoked(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.LogStreams = logstream.New(logstream.WithReauthEvery(20 * time.Millisecond))
	admin := i.admin()
	id, _ := appOnStreamRuntime(t, i, admin)

	srv := httptest.NewServer(i.handler)
	t.Cleanup(srv.Close) // registered first, so it runs after the streams close

	viewer := i.admin()
	stream := openSSE(t, srv.URL, viewer, "/apps/"+id+"/logs/stream")
	require.Equal(t, sseEvent{name: "reset"}, stream.next(t))

	got := i.do(viewer, http.MethodDelete, "/sessions", nil)
	require.Less(t, got.Code, 300, got.String())

	ended := stream.next(t)
	require.Equal(t, "revoked", ended.name)
	require.Contains(t, ended.data, "access")
}

// TestR048_ADeployLogStreamEndsWhenTheViewersSessionIsRevoked asserts R-048
// for a deploy's live log: a stream checked only when it opened kept showing
// a long build's output to someone whose session had since been revoked.
func TestR048_ADeployLogStreamEndsWhenTheViewersSessionIsRevoked(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.DeployLogReauthEvery = 20 * time.Millisecond
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.deploymentFor(appID, false)
	sink := i.Server.Logs.Writer(depID)
	t.Cleanup(func() { _ = sink.Close() })
	_, err := fmt.Fprintln(sink, "=> Building")
	require.NoError(t, err)

	srv := httptest.NewServer(i.handler)
	t.Cleanup(srv.Close)

	viewer := i.admin()
	stream := openSSE(t, srv.URL, viewer, "/apps/"+appID+"/deployments/"+depID+"/logs")
	require.Equal(t, sseEvent{data: "=> Building"}, stream.next(t))

	got := i.do(viewer, http.MethodDelete, "/sessions", nil)
	require.Less(t, got.Code, 300, got.String())

	told := stream.next(t)
	require.Contains(t, told.data, "access to this app's logs has ended")
	require.Equal(t, "end", stream.next(t).name)
}

// TestO51_OnlyAPartTheAppHasIsStreamed asserts that the log endpoints read
// only a part the spec declares or the runtime runs for the app: a made-up
// name opens no runtime stream (O-51 shares one per real part) and never
// reaches a log line as the request wrote it (CodeQL go/log-injection).
func TestO51_OnlyAPartTheAppHasIsStreamed(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id, rt := appOnStreamRuntime(t, i, admin)

	for _, made := range []string{"nope", "web\nforged=1"} {
		got := i.do(admin, http.MethodGet, "/apps/"+id+"/logs?workload="+url.QueryEscape(made), nil)
		require.Equal(t, http.StatusNotFound, got.Code, got.String())
		require.Contains(t, got.String(), "This app has no part called")
		got = i.do(admin, http.MethodGet, "/apps/"+id+"/logs/stream?workload="+url.QueryEscape(made), nil)
		require.Equal(t, http.StatusNotFound, got.Code, got.String())
	}
	rt.mu.Lock()
	require.Empty(t, rt.opens, "no runtime stream for a part the app does not have")
	rt.mu.Unlock()

	// The spec's part, and one only the runtime reports, are both read.
	for _, part := range []string{"web", "db"} {
		got := i.do(admin, http.MethodGet, "/apps/"+id+"/logs?workload="+part, nil)
		require.Equal(t, http.StatusOK, got.Code, part+": "+got.String())
	}
}

func TestLogsTailIsCapped(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	id, rt := appOnStreamRuntime(t, i, admin)

	got := i.do(admin, http.MethodGet, "/apps/"+id+"/logs?tail=1000000", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Equal(t, "one-shot\n", got.String())

	got = i.do(admin, http.MethodGet, "/apps/"+id+"/logs", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())

	rt.mu.Lock()
	defer rt.mu.Unlock()
	require.Equal(t, logstream.MaxTail, rt.opens[0].Tail)
	require.Equal(t, logstream.DefaultTail, rt.opens[1].Tail)
}

func TestLogStreamRefusesWhatTheLogsEndpointRefuses(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	// Not deployed: no pinned spec.
	notes := i.createApp(admin, "notes")
	got := i.do(admin, http.MethodGet, "/apps/"+notes+"/logs/stream", nil)
	require.Equal(t, "STATE_INVALID", got.ErrorCode(), got.String())

	// Someone with no access is told there is no such app.
	id, _ := appOnStreamRuntime(t, i, admin)
	other := i.user("stranger")
	got = i.do(other, http.MethodGet, "/apps/"+id+"/logs/stream", nil)
	require.Equal(t, http.StatusNotFound, got.Code, got.String())

	// A replica with no hub says so rather than following nothing.
	i.Server.LogStreams = nil
	got = i.do(admin, http.MethodGet, "/apps/"+id+"/logs/stream", nil)
	require.Equal(t, "ADAPTER_UNAVAILABLE", got.ErrorCode(), got.String())
}
