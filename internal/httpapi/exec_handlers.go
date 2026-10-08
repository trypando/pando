package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/coder/websocket"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// handleExec opens a terminal inside a running workload (design 04 §2.4).
//
// Routed as GET, though the design writes it `POST /api/v1/apps/{id}/exec`. A
// WebSocket handshake is a GET by protocol — RFC 6455 requires it, and a
// browser's `new WebSocket()` cannot issue anything else — so the design's verb
// is not implementable from the console it exists for. Everything else about
// the line holds.
//
// The ordering is a requirement, not an implementation detail: check
// `app.exec`, then host policy (R-085, which surfaces as POLICY_EXEC_DISABLED),
// then **write the audit event, and only then open the session**. Audit before
// access, so a session that is aborted between authorization and the first byte
// is still recorded.
//
// R-086 is the reason this is the most carefully written handler in the package.
// Exec is the highest-privilege action in the system: a holder can read the
// database directly, read injected environment including secrets, and modify a
// running workload in ways that never appear in the spec. The verb list is not
// a security boundary against someone holding `app.exec`, and the documentation
// says so plainly rather than implying otherwise.
//
// R-086 also concedes what is *not* recorded: the command, not the stream. A
// captured PTY stream would be a durable, searchable store of every secret an
// operator ever typed, sitting in the one table deliberately readable by anyone
// with audit access — and it could not be redacted, because `secret.Value`
// protects values Pando handles and a stream is bytes Pando never parses (O-7).
func (s *Server) handleExec(w http.ResponseWriter, r *http.Request) {
	// Verb and host policy. CheckControl evaluates policy before grants
	// (design 06 §2), so an install with exec disabled denies the owner too.
	app, ok := s.requireControl(w, r, authz.AppExec)
	if !ok {
		return
	}

	runtime, workload, ok := s.execTarget(w, r, app.ID)
	if !ok {
		return
	}

	command := execCommand(r)
	p := PrincipalFrom(r.Context())

	// Audit before access. Written synchronously and its failure is fatal to
	// the request: an exec session that is not recorded is worse than one that
	// does not happen.
	if err := s.Auditor.Write(r.Context(), audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        "app.exec",
		AppID:         app.ID,
		TargetKind:    "workload",
		TargetID:      workload,
		Detail: map[string]any{
			// The command, not the stream (R-086, O-7).
			"command":  command,
			"workload": workload,
		},
	}); err != nil {
		Error(w, r, errs.Wrap(errs.Internal,
			"Pando couldn't record this session, so it didn't start it.", err).
			WithRemedy("Try again. If it keeps happening, the audit log may be unwritable."))
		return
	}

	// The session's end is audited too (R-228): how long it ran and how it
	// ended, whatever ended it. Detached from the request, which may already
	// be gone when the session is.
	began := time.Now()
	ended := "error"
	var exitCode *int
	defer func(ctx context.Context) {
		detail := map[string]any{
			"command":     command,
			"workload":    workload,
			"duration_ms": time.Since(began).Milliseconds(),
			"reason":      ended,
		}
		if exitCode != nil {
			detail["exit_code"] = *exitCode
		}
		s.audit(r.WithContext(ctx), audit.Event{
			PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
			Action: "app.exec.end", AppID: app.ID, TargetKind: "workload", TargetID: workload,
			Detail: detail,
		})
	}(context.WithoutCancel(r.Context()))

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Same-origin only. The console is served from this origin, and an exec
		// socket reachable cross-origin would be a terminal any page could open
		// with the user's cookies.
		OriginPatterns: nil,
	})
	if err != nil {
		// Accept has already written a response.
		log.From(r.Context()).Warn("exec websocket upgrade failed", zap.Error(err))
		return
	}
	defer func() { _ = conn.CloseNow() }()

	// Detached from the request context: an exec session outlives the handler's
	// notion of a request, and canceling on the first idle period would drop
	// the terminal mid-command.
	ctx, cancel := context.WithCancel(context.WithoutCancel(r.Context()))
	defer cancel()

	session, err := runtime.Exec(ctx, api.WorkloadRef{BundleID: app.ID, Workload: workload},
		api.ExecRequest{Command: command, TTY: true})
	if err != nil {
		closeWith(conn, websocket.StatusInternalError, errs.As(err))
		return
	}
	defer func() { _ = session.Close() }()

	ended, exitCode = pump(ctx, cancel, conn, session, log.From(r.Context()))
}

// execTarget resolves which workload to open a terminal in.
func (s *Server) execTarget(w http.ResponseWriter, r *http.Request, appID string) (api.RuntimeAdapter, string, bool) {
	app, found, err := s.Apps.ByID(r.Context(), appID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return nil, "", false
	}
	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app has never been deployed, so there is nothing to open a terminal in.").
			WithRemedy("Deploy the app first."))
		return nil, "", false
	}

	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return nil, "", false
	}

	runtime, ok := s.Registry.Runtime(rev.Body.Runtime.AdapterRef)
	if !ok {
		Error(w, r, errs.Newf(errs.PlanAdapterNotConfigured,
			"This app runs on %q, which is not configured on this installation.",
			rev.Body.Runtime.AdapterRef))
		return nil, "", false
	}

	caps, err := runtime.Capabilities(r.Context())
	if err != nil {
		Error(w, r, err)
		return nil, "", false
	}
	if !caps.SupportsExec {
		// Capability, not a type assertion (R-254): this produces a readable
		// refusal rather than a nil dereference.
		Error(w, r, errs.New(errs.PlanCapabilityUnsupported,
			"The runtime this app uses can't open a terminal inside it.").
			WithRemedy("Nothing to change on the app — this is a limit of the runtime it runs on."))
		return nil, "", false
	}

	// Default to the primary workload: it is the one the app's URL resolves to,
	// so it is the one someone means by "this app" (R-026).
	workload := r.URL.Query().Get("workload")
	if workload == "" {
		if primary, found := rev.Body.PrimaryWorkload(); found {
			workload = primary.Name
		}
	}
	if workload == "" {
		Error(w, r, errs.New(errs.ValidInvalid, "This app has no workload to open a terminal in."))
		return nil, "", false
	}

	// A workload the spec does not declare is not this app's to open.
	declared := false
	for _, candidate := range rev.Body.Workloads {
		if candidate.Name == workload {
			declared = true
		}
	}
	if !declared {
		Error(w, r, errs.Newf(errs.NotFound, "This app has no part called %q.", workload).
			WithDetail("workload", workload))
		return nil, "", false
	}

	return runtime, workload, true
}

// execCommand returns the command to run, defaulting to an interactive shell.
//
// `sh` rather than `bash`: a minimal image has the first and often not the
// second, and failing to open a terminal because the image is small would be a
// confusing way to learn that.
func execCommand(r *http.Request) []string {
	if command := r.URL.Query()["command"]; len(command) > 0 {
		return command
	}
	return []string{"sh"}
}

// control is a message from the client that is not terminal input.
//
// Only resize, for now. It travels as a text frame while terminal bytes travel
// as binary frames, so the two can never be confused — a resize arriving as
// input would print JSON into the user's shell.
type control struct {
	Rows uint16 `json:"rows"`
	Cols uint16 `json:"cols"`
}

// pump copies bytes between the socket and the session until either ends.
// pump copies between the socket and the session until either ends. It
// returns how the session ended — server_closed when the shell ended first,
// client_closed when the terminal did — and the shell's exit code if it has
// one, for app.exec.end (R-228).
func pump(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, session api.ExecSession, logger *zap.Logger) (string, *int) {
	// Session to socket.
	go func() {
		defer cancel()

		buf := make([]byte, 32<<10)
		for {
			n, err := session.Read(buf)
			if n > 0 {
				if writeErr := conn.Write(ctx, websocket.MessageBinary, buf[:n]); writeErr != nil {
					return
				}
			}
			if err != nil {
				if !errors.Is(err, io.EOF) {
					logger.Debug("exec session read ended", zap.Error(err))
				}
				return
			}
		}
	}()

	// Socket to session.
	for {
		kind, data, err := conn.Read(ctx)
		if err != nil {
			break
		}

		switch kind {
		case websocket.MessageBinary:
			if _, err := session.Write(data); err != nil {
				break
			}

		case websocket.MessageText:
			var message control
			if err := json.Unmarshal(data, &message); err != nil {
				continue
			}
			if message.Rows > 0 && message.Cols > 0 {
				_ = session.Resize(message.Rows, message.Cols)
			}
		}
	}

	// The session goroutine cancels ctx when the shell ends; a socket read
	// that failed with ctx still live is the terminal going away.
	ended := "client_closed"
	if ctx.Err() != nil {
		ended = "server_closed"
	}

	// The exit code, so the console can say how the shell ended rather than
	// just going blank.
	status := websocket.StatusNormalClosure
	reason := "session ended"
	var exitCode *int
	if code, ok := session.ExitCode(); ok {
		exitCode = &code
		if code != 0 {
			reason = "exited with status " + itoa(code)
		}
	}

	shutdown, stop := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer stop()
	_ = conn.Close(status, reason)
	<-shutdown.Done()
	return ended, exitCode
}

// closeWith reports a failure through the socket, since the HTTP response is
// already committed by the upgrade.
func closeWith(conn *websocket.Conn, status websocket.StatusCode, e *errs.Error) {
	reason := "the terminal could not be opened"
	if e != nil && e.Message != "" {
		reason = e.Message
	}
	// A close reason is capped at 123 bytes by the protocol.
	if len(reason) > 120 {
		reason = reason[:120]
	}
	_ = conn.Close(status, reason)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
