//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/httpapi"
)

// execRuntime is a runtime that can open a terminal, into a session the test
// scripts.
type execRuntime struct {
	adapterapi.RuntimeAdapter

	open func() (adapterapi.ExecSession, error)
}

func (*execRuntime) Category() adapterapi.Category { return adapterapi.CategoryRuntime }
func (*execRuntime) Capabilities(context.Context) (adapterapi.RuntimeCapabilities, error) {
	return adapterapi.RuntimeCapabilities{SupportsExec: true}, nil
}
func (f *execRuntime) Exec(context.Context, adapterapi.WorkloadRef, adapterapi.ExecRequest) (adapterapi.ExecSession, error) {
	return f.open()
}

// fakeExecSession prints output, then either ends by itself with an exit code
// (the shell exiting) or waits until it is closed (the terminal going away).
type fakeExecSession struct {
	output   string
	selfEnds bool
	exitCode int
	hasCode  bool

	mu      sync.Mutex
	read    bool
	input   []byte
	resized [2]uint16
	closed  chan struct{}
	once    sync.Once
}

func newExecSession(output string, selfEnds bool, code int, hasCode bool) *fakeExecSession {
	return &fakeExecSession{output: output, selfEnds: selfEnds, exitCode: code, hasCode: hasCode, closed: make(chan struct{})}
}

func (s *fakeExecSession) Read(p []byte) (int, error) {
	s.mu.Lock()
	if !s.read {
		s.read = true
		s.mu.Unlock()
		return copy(p, s.output), nil
	}
	s.mu.Unlock()
	if !s.selfEnds {
		<-s.closed
	}
	return 0, io.EOF
}

func (s *fakeExecSession) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.input = append(s.input, p...)
	return len(p), nil
}

func (s *fakeExecSession) Resize(rows, cols uint16) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resized = [2]uint16{rows, cols}
	return nil
}

func (s *fakeExecSession) ExitCode() (int, bool) { return s.exitCode, s.hasCode }

func (s *fakeExecSession) Close() error {
	s.once.Do(func() { close(s.closed) })
	return nil
}

// appOnExecRuntime is a pinned app whose spec names an exec-capable runtime.
func appOnExecRuntime(t *testing.T, i *install, s *session, rt *execRuntime) string {
	t.Helper()
	require.NoError(t, i.Server.Registry.Register("rt_exec", rt))
	id := i.createApp(s, "shell")
	body := minimalSpec()
	body["runtime"] = map[string]any{"adapter_ref": "rt_exec"}
	i.pinSpec(s, id, i.writeSpec(s, id, body))
	return id
}

func dialExecSocket(t *testing.T, base string, s *session, appID string) *websocket.Conn {
	t.Helper()
	header := http.Header{}
	header.Set("Cookie", httpapi.SessionCookie+"="+s.cookie)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	t.Cleanup(cancel)
	conn, resp, err := websocket.Dial(ctx, strings.Replace(base, "http://", "ws://", 1)+
		"/api/v1/apps/"+appID+"/exec?command=sh&command=-l", &websocket.DialOptions{HTTPHeader: header})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.CloseNow() })
	return conn
}

type auditEvent struct {
	Action     string         `json:"action"`
	AppID      string         `json:"app_id"`
	TargetID   string         `json:"target_id"`
	PrincipalI string         `json:"principal_id"`
	Detail     map[string]any `json:"detail"`
}

// eventsFor is every event with this action, newest first.
func (i *install) eventsFor(s *session, action string) []auditEvent {
	i.t.Helper()
	var page struct {
		Events []auditEvent `json:"events"`
	}
	got := i.do(s, http.MethodGet, "/audit?limit=500&action="+url.QueryEscape(action), nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	got.JSON(i.t, &page)
	var out []auditEvent
	for _, e := range page.Events {
		if e.Action == action {
			out = append(out, e)
		}
	}
	return out
}

// waitForEvent waits for the nth event with this action: a session's end is
// recorded as the handler returns, which can be after the client sees the
// socket close.
func (i *install) waitForEvent(t *testing.T, s *session, action string, n int) []auditEvent {
	t.Helper()
	var got []auditEvent
	require.Eventually(t, func() bool {
		got = i.eventsFor(s, action)
		return len(got) >= n
	}, 15*time.Second, 50*time.Millisecond, "waiting for %d %s event(s)", n, action)
	return got
}

// TestR228_AnExecSessionsEndIsAudited asserts R-228 for the end of a
// terminal: a session the shell ends is recorded with how long it ran, that
// the server ended it, and the shell's exit code; one the terminal ends is
// recorded as client_closed; and one that never opened is still recorded as
// ending in an error, after the app.exec that preceded it.
func TestR228_AnExecSessionsEndIsAudited(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	rt := &execRuntime{}
	var next func() (adapterapi.ExecSession, error)
	var nextMu sync.Mutex
	rt.open = func() (adapterapi.ExecSession, error) {
		nextMu.Lock()
		defer nextMu.Unlock()
		return next()
	}
	setNext := func(f func() (adapterapi.ExecSession, error)) {
		nextMu.Lock()
		defer nextMu.Unlock()
		next = f
	}
	appID := appOnExecRuntime(t, i, admin, rt)

	srv := httptest.NewServer(i.handler)
	t.Cleanup(srv.Close)

	// The shell prints, then exits with status 3.
	exited := newExecSession("hello\n", true, 3, true)
	setNext(func() (adapterapi.ExecSession, error) { return exited, nil })
	conn := dialExecSocket(t, srv.URL, admin, appID)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	kind, data, err := conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, websocket.MessageBinary, kind)
	assert.Equal(t, "hello\n", string(data))
	// The socket ends. How it closes is not asserted here: what is under test
	// is the audit event, and the exit code is in it.
	_, _, err = conn.Read(ctx)
	require.Error(t, err)

	ends := i.waitForEvent(t, admin, "app.exec.end", 1)
	end := ends[0]
	assert.Equal(t, appID, end.AppID)
	assert.Equal(t, "web", end.TargetID)
	assert.Equal(t, "server_closed", end.Detail["reason"])
	assert.EqualValues(t, 3, end.Detail["exit_code"])
	assert.Equal(t, "web", end.Detail["workload"])
	assert.Equal(t, []any{"sh", "-l"}, end.Detail["command"])
	assert.Contains(t, end.Detail, "duration_ms")
	require.Len(t, i.eventsFor(admin, "app.exec"), 1, "the start was recorded before the session opened")

	// The terminal goes away while the shell is still running: typed input
	// and a resize reach the session first.
	open := newExecSession("$ ", false, 0, false)
	setNext(func() (adapterapi.ExecSession, error) { return open, nil })
	conn = dialExecSocket(t, srv.URL, admin, appID)
	_, data, err = conn.Read(ctx)
	require.NoError(t, err)
	assert.Equal(t, "$ ", string(data))
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte("ls\n")))
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`{"rows":40,"cols":120}`)))
	require.NoError(t, conn.Write(ctx, websocket.MessageText, []byte(`not a control message`)))
	require.Eventually(t, func() bool {
		open.mu.Lock()
		defer open.mu.Unlock()
		return string(open.input) == "ls\n" && open.resized == [2]uint16{40, 120}
	}, 10*time.Second, 20*time.Millisecond)
	require.NoError(t, conn.Close(websocket.StatusNormalClosure, "bye"))

	ends = i.waitForEvent(t, admin, "app.exec.end", 2)
	end = ends[0]
	assert.Equal(t, "client_closed", end.Detail["reason"])
	assert.NotContains(t, end.Detail, "exit_code", "a shell still running has no exit code")

	// The runtime cannot open the session: the socket says why, and the end
	// is recorded as an error.
	setNext(func() (adapterapi.ExecSession, error) {
		return nil, errs.New(errs.Internal, "The container is not running.")
	})
	conn = dialExecSocket(t, srv.URL, admin, appID)
	_, _, err = conn.Read(ctx)
	assert.Equal(t, websocket.StatusInternalError, websocket.CloseStatus(err))
	assert.Contains(t, err.Error(), "The container is not running.")

	ends = i.waitForEvent(t, admin, "app.exec.end", 3)
	assert.Equal(t, "error", ends[0].Detail["reason"])

	// A request that is not a WebSocket at all is refused by the upgrade,
	// and still recorded at both ends.
	got := i.do(admin, http.MethodGet, "/apps/"+appID+"/exec", nil)
	require.GreaterOrEqual(t, got.Code, 400, got.String())
	ends = i.waitForEvent(t, admin, "app.exec.end", 4)
	assert.Equal(t, "error", ends[0].Detail["reason"])
	assert.Equal(t, []any{"sh"}, ends[0].Detail["command"])
	assert.Len(t, i.eventsFor(admin, "app.exec"), 4, "every end has its start")
}

// TestR391_AnsweringDetectionIsAudited asserts R-391 for detection answers:
// who answered which questions is recorded, and not what they answered,
// since an answer can be a value somebody typed.
func TestR391_AnsweringDetectionIsAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")

	workloads := []spec.Workload{{Name: "web", Primary: true, Exposed: true}}
	proposal := detect.Proposal{
		Status: detect.StatusNeedsAnswers,
		Winner: detect.Candidate{Detector: "dockerfile", Strategy: spec.BuildDockerfile,
			Draft: detect.Draft{Workloads: workloads}},
		DraftSpec: spec.AppSpec{SchemaVersion: spec.SchemaVersion, AppID: appID, Workloads: workloads},
		Questions: []detect.Question{
			{Key: detect.KeyPrimaryPort, Kind: "port", Prompt: "Which port does this app serve HTTP on?"},
			{Key: detect.KeyStartCommand, Kind: "text", Prompt: "What command starts this app?"},
		},
	}
	require.NoError(t, state.NewDetections(i.db).Save(ctx, appID, detect.StatusNeedsAnswers, proposal, ""))

	got := i.do(admin, http.MethodPost, "/apps/"+appID+"/detection/answers", map[string]any{
		"answers": map[string]string{detect.KeyStartCommand: "node secret-server.js", detect.KeyPrimaryPort: "3000"},
	})
	require.Equal(t, http.StatusOK, got.Code, got.String())

	events := i.eventsFor(admin, "detection.answer")
	require.Len(t, events, 1)
	assert.Equal(t, appID, events[0].AppID)
	assert.Equal(t, appID, events[0].TargetID)
	assert.Equal(t, i.AdminID, events[0].PrincipalI)
	assert.Equal(t, []any{detect.KeyPrimaryPort, detect.KeyStartCommand}, events[0].Detail["questions"],
		"which questions, sorted")
	assert.NotContains(t, i.do(admin, http.MethodGet, "/audit?action=detection.answer", nil).String(), "secret-server",
		"never what was answered")
}
