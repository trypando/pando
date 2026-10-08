package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/logstream"
	"github.com/trypando/pando/internal/errs"
)

// appLogSource is which runtime log a request reads.
type appLogSource struct {
	runtime api.RuntimeAdapter
	key     logstream.Key
}

// appLogSource authorizes app.logs.read and resolves the runtime and the part
// whose log the request names — `workload`, defaulting to the spec's primary.
func (s *Server) appLogSource(w http.ResponseWriter, r *http.Request) (appLogSource, bool) {
	app, ok := s.requireControl(w, r, authz.AppLogsRead)
	if !ok {
		return appLogSource{}, false
	}
	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app is not running yet."))
		return appLogSource{}, false
	}

	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return appLogSource{}, false
	}
	ref := rev.Body.Runtime.AdapterRef
	runtime, ok := s.Registry.Runtime(ref)
	if !ok {
		Error(w, r, errs.New(errs.AdapterUnavailable, "The app's runtime is not configured."))
		return appLogSource{}, false
	}

	workload := r.URL.Query().Get("workload")
	if workload == "" {
		if primary, ok := rev.Body.PrimaryWorkload(); ok {
			workload = primary.Name
		}
	}
	return appLogSource{runtime: runtime, key: logstream.Key{Runtime: ref, AppID: app.ID, Workload: workload}}, true
}

// handleAppLogStream is the app's own output as server-sent events, read from
// this replica's one shared stream for the part (O-51).
//
// The stream opens with `event: reset` followed by the most recent lines the
// shared stream holds, then each new line as a `data:` event. `event: notice`
// is Pando saying something about the stream — that the runtime's stream ended
// (a restart) and is being reconnected, or has been. It ends with
// `event: revoked` when the viewer's access no longer holds, after which a
// client must not reconnect, or `event: lagged` when the viewer fell too far
// behind, after which reconnecting starts again from the recent lines.
func (s *Server) handleAppLogStream(w http.ResponseWriter, r *http.Request) {
	src, ok := s.appLogSource(w, r)
	if !ok {
		return
	}
	s.followAppLogs(w, r, src, &sseViewer{w: w, rc: http.NewResponseController(w)})
}

// endingViewer is a logstream.Viewer that knows how to start and how to say
// why Pando ended it.
type endingViewer interface {
	logstream.Viewer
	start(w http.ResponseWriter)
	end(e *logstream.Ended)
}

// followAppLogs joins the shared stream and serves it to v until the viewer
// leaves or Pando ends it. Everything that decides anything is in logstream;
// this formats.
func (s *Server) followAppLogs(w http.ResponseWriter, r *http.Request, src appLogSource, v endingViewer) {
	if s.LogStreams == nil {
		Error(w, r, errs.New(errs.AdapterUnavailable,
			"Live logs are not available on this Pando server. Read the most recent lines without following instead."))
		return
	}
	sub, err := s.LogStreams.Subscribe(r.Context(), src.runtime, src.key)
	if err != nil {
		Error(w, r, err)
		return
	}

	v.start(w)
	err = s.LogStreams.Serve(r.Context(), sub, s.stillAllowed(r, src.key.AppID, authz.AppLogsRead), v)
	if ended := (*logstream.Ended)(nil); errors.As(err, &ended) {
		v.end(ended)
	}
}

// stillAllowed checks a long-lived request's access again, the way a fresh
// request is checked: the credentials it presented are authenticated again —
// a session revoked or a user suspended since is anonymous now (R-048,
// R-049) — and the verb is checked against what they resolve to. Design 06
// §4.2, as the proxy's long-lived connections do.
func (s *Server) stillAllowed(r *http.Request, appID string, verb authz.Verb) logstream.Authorize {
	return func(ctx context.Context) error {
		p := PrincipalFrom(ctx)
		if s.Authent != nil {
			var err error
			p, err = s.Authent.Authenticate(r)
			if err != nil {
				if errs.CodeOf(err) == errs.Internal {
					// An unknown answer is not an allow.
					return err
				}
				p = authz.Anonymous()
			}
		}
		return s.Authz.CheckControl(ctx, p, appID, verb)
	}
}

// sseViewer writes a log as server-sent events.
type sseViewer struct {
	w  io.Writer
	rc *http.ResponseController
}

// sseRetryMillis is how long a browser waits before reconnecting a stream that
// closed: after `lagged`, or a replica going away.
const sseRetryMillis = 3000

func (v *sseViewer) start(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(v.w, "retry: %d\nevent: reset\ndata: \n\n", sseRetryMillis)
}

// sseData makes text safe as one SSE data field. A lone carriage return is a
// line ending to an SSE parser and would split the line in two, or end the
// event early.
func sseData(text string) string {
	return strings.NewReplacer("\r", "", "\n", " ").Replace(text)
}

// Line writes one line of output. G705: not an HTML context — the response is
// text/event-stream and the console renders these lines as text.
func (v *sseViewer) Line(text string) error {
	_, err := fmt.Fprintf(v.w, "data: %s\n\n", sseData(text)) //nolint:gosec
	return err
}

func (v *sseViewer) Notice(text string) error {
	_, err := fmt.Fprintf(v.w, "event: notice\ndata: %s\n\n", sseData(text))
	return err
}

func (v *sseViewer) Flush() error { return v.rc.Flush() }

func (v *sseViewer) end(e *logstream.Ended) {
	fmt.Fprintf(v.w, "event: %s\ndata: %s\n\n", e.Reason, sseData(e.Message))
	_ = v.rc.Flush()
}

// plainViewer writes a log as text, a line at a time, for `pando logs
// --follow`. Pando's own notices are marked so they cannot be mistaken for
// something the app printed.
type plainViewer struct {
	w  io.Writer
	rc *http.ResponseController
}

func (v plainViewer) start(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
}

func (v plainViewer) Line(text string) error {
	_, err := io.WriteString(v.w, text+"\n")
	return err
}

func (v plainViewer) Notice(text string) error {
	_, err := io.WriteString(v.w, "[pando] "+text+"\n")
	return err
}

func (v plainViewer) Flush() error { return v.rc.Flush() }

func (v plainViewer) end(e *logstream.Ended) {
	_ = v.Notice(e.Message)
	_ = v.rc.Flush()
}
