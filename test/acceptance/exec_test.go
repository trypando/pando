//go:build integration

package acceptance_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"
)

// Exec, against the shipped stack (design 04 §2.4).
//
// R-086 is why these tests exist in this shape rather than "the terminal
// works": exec is the highest-privilege action in the system, and what matters
// is the order of the checks and that the audit event is written before any
// access, so a session aborted between authorization and the first byte is
// still recorded.

// dialExec opens the exec socket for an app.
func dialExec(t *testing.T, c *client, appID string, query string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	url := strings.Replace(baseURL(), "http://", "ws://", 1) + "/apps/" + appID + "/exec"
	if query != "" {
		url += "?" + query
	}

	header := http.Header{}
	header.Set("Cookie", "pando_session="+c.cookie)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)

	return websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: header})
}

// A terminal in a running app: type a command, read its output.
func TestR086_ExecOpensATerminalInTheRunningWorkload(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-"+stamp())

	conn, _, err := dialExec(t, c, app, "")
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Terminal bytes travel as binary frames.
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary,
		[]byte("echo pando-exec-works\n")))

	require.Eventually(t, func() bool {
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		kind, data, err := conn.Read(readCtx)
		if err != nil {
			return false
		}
		return kind == websocket.MessageBinary && strings.Contains(string(data), "pando-exec-works")
	}, 25*time.Second, time.Second, "the shell never echoed the command's output back")
}

// A resize is a text frame, so it can never be mistaken for input.
//
// Getting this wrong prints JSON into the user's shell, which is the kind of
// bug that looks like the terminal is haunted.
func TestExecResizeIsControlNotInput(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-resize-"+stamp())

	conn, _, err := dialExec(t, c, app, "")
	require.NoError(t, err)
	defer func() { _ = conn.CloseNow() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resize, err := json.Marshal(map[string]int{"rows": 40, "cols": 120})
	require.NoError(t, err)
	require.NoError(t, conn.Write(ctx, websocket.MessageText, resize))

	// The shell is still usable, and the resize did not land in its input.
	require.NoError(t, conn.Write(ctx, websocket.MessageBinary, []byte("echo after-resize\n")))

	var seen strings.Builder
	require.Eventually(t, func() bool {
		readCtx, stop := context.WithTimeout(ctx, 5*time.Second)
		defer stop()
		_, data, err := conn.Read(readCtx)
		if err != nil {
			return false
		}
		seen.Write(data)
		return strings.Contains(seen.String(), "after-resize")
	}, 25*time.Second, time.Second)

	require.NotContains(t, seen.String(), `"rows"`,
		"the resize message was written into the shell instead of resizing it")
}

// R-086: the command is recorded, the stream is not (O-7).
//
// A captured stream would be a durable, searchable store of every secret an
// operator typed, in the one table deliberately readable by anyone with audit
// access — and it could not be redacted, because secret.Value protects values
// Pando handles and a stream is bytes Pando never parses.
func TestR086_ExecRecordsTheCommandAndNotTheStream(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-audit-"+stamp())

	conn, _, err := dialExec(t, c, app, "command=sh&command=-c&command=id")
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	secretish := "hunter2-not-in-the-audit-log"
	_ = conn.Write(ctx, websocket.MessageBinary, []byte("echo "+secretish+"\n"))
	time.Sleep(2 * time.Second)
	_ = conn.CloseNow()

	events := auditEvents(t, "app.exec")
	require.NotEmpty(t, events, "the session was not recorded")

	whole := strings.Join(events, "\n")
	require.Contains(t, whole, `"id"`, "the command is recorded")
	require.NotContains(t, whole, secretish,
		"the stream must not be recorded — it would be a searchable store of every secret typed")
}

// Audit before access: the event exists even for a session that opens and is
// abandoned without a byte being sent.
func TestR086_AnAbandonedSessionIsStillRecorded(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-abandon-"+stamp())

	before := len(auditEvents(t, "app.exec"))

	conn, _, err := dialExec(t, c, app, "")
	require.NoError(t, err)
	_ = conn.CloseNow() // nothing sent, nothing read

	require.Eventually(t, func() bool {
		return len(auditEvents(t, "app.exec")) > before
	}, 15*time.Second, time.Second,
		"a session authorized and then abandoned must still appear in the audit log")
}

// TestR228_AnExecSessionsEndIsRecorded asserts R-228's "start and end": when
// the terminal closes, app.exec.end records how long the session ran and how
// it ended.
func TestR228_AnExecSessionsEndIsRecorded(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-end-"+stamp())

	before := len(auditEvents(t, "app.exec.end"))
	conn, _, err := dialExec(t, c, app, "")
	require.NoError(t, err)
	time.Sleep(time.Second)
	_ = conn.Close(websocket.StatusNormalClosure, "done")

	require.Eventually(t, func() bool {
		return len(auditEvents(t, "app.exec.end")) > before
	}, 15*time.Second, time.Second, "the session's end was not recorded")
	last := auditEvents(t, "app.exec.end")[0]
	require.Contains(t, last, `"duration_ms"`)
	require.Contains(t, last, `"reason"`)
}

// R-085: host policy may disable exec install-wide, and it denies the owner too.
func TestR085_HostPolicyCanDisableExecInstallWide(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-policy-"+stamp())

	setHostPolicy(t, `{"allow_anonymous_grants": true, "min_build_isolation": 10, `+
		`"min_runtime_isolation": 10, "disabled_verbs": ["app.exec"]}`)
	t.Cleanup(func() {
		setHostPolicy(t, `{"allow_anonymous_grants": true, "min_build_isolation": 10, `+
			`"min_runtime_isolation": 10}`)
	})

	_, response, err := dialExec(t, c, app, "")
	require.Error(t, err, "the upgrade must be refused")
	require.NotNil(t, response)
	require.Equal(t, http.StatusForbidden, response.StatusCode)

	body, status := c.do(t, http.MethodGet, fmt.Sprintf("/apps/%s/exec", app), "")
	require.Equal(t, http.StatusForbidden, status)
	require.Contains(t, body, "POLICY_EXEC_DISABLED")
	require.Contains(t, body, "administrator",
		"the message says who can turn it back on, because this is a posture and not a mistake")
}

// A workload the spec does not declare is not this app's to open.
func TestExecRefusesAWorkloadTheSpecDoesNotDeclare(t *testing.T) {
	c := login(t)
	app := deployedApp(t, c, "exec-unknown-"+stamp())

	body, status := c.do(t, http.MethodGet,
		fmt.Sprintf("/apps/%s/exec?workload=not-a-workload", app), "")
	require.Equal(t, http.StatusNotFound, status, body)
}

// auditEvents returns the audit rows for an action, newest first.
func auditEvents(t *testing.T, action string) []string {
	t.Helper()
	out, err := execCompose("exec", "-T", "postgres", "psql", "-U", "pando", "-d", "pando",
		"-t", "-A", "-c",
		"SELECT coalesce(detail::text, '') FROM audit_events WHERE action = "+quoteSQL(action)+
			" ORDER BY occurred_at DESC LIMIT 50")
	require.NoError(t, err, out)

	var rows []string
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			rows = append(rows, line)
		}
	}
	return rows
}
