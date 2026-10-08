package httpapi

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/auditstream"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/errs"
)

// fakeStream is an AuditStream in memory: a page to serve, lines to export,
// and an error for each call that should fail.
type fakeStream struct {
	resolveErr error
	waitErr    error
	exportErr  error
	oldestErr  error

	page      audit.StreamPage
	lines     []json.RawMessage
	oldest    time.Time
	hasOldest bool

	gotQuery audit.StreamQuery
	gotWait  time.Duration
}

func (f *fakeStream) ResolveCursor(_ context.Context, s string) (audit.Cursor, error) {
	if f.resolveErr != nil {
		return audit.Cursor{}, f.resolveErr
	}
	return audit.ParseCursor(s)
}

func (f *fakeStream) Wait(_ context.Context, q audit.StreamQuery, wait time.Duration, _ clock.Clock) (audit.StreamPage, error) {
	f.gotQuery, f.gotWait = q, wait
	return f.page, f.waitErr
}

func (f *fakeStream) Export(_ context.Context, q audit.StreamQuery, enc audit.Encoder, write func([]byte) error) (int, error) {
	f.gotQuery = q
	n := 0
	for _, line := range f.lines {
		out := line
		if enc != nil {
			var err error
			if out, err = enc(line); err != nil {
				return n, err
			}
		}
		if err := write(out); err != nil {
			return n, err
		}
		n++
	}
	return n, f.exportErr
}

func (f *fakeStream) Oldest(context.Context) (time.Time, bool, error) {
	return f.oldest, f.hasOldest, f.oldestErr
}

type fakeSinks struct {
	list []auditstream.Status
	err  error
}

func (f fakeSinks) Statuses(context.Context) ([]auditstream.Status, error) { return f.list, f.err }

func streamServer(st AuditStream) *Server {
	return &Server{
		Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil), AuditStream: st,
		AuditEncoders: map[string]audit.Encoder{api.AuditFormatOCSF: func(line json.RawMessage) (json.RawMessage, error) {
			return json.RawMessage(`{"class_uid":6003}`), nil
		}},
	}
}

// TestR381_StreamHandlerRefusesWhatItCannotServe asserts the stream's
// refusals: an installation with no stream, a cursor that does not resolve,
// an unknown format, and limits and waits out of range, each saying what is
// valid.
func TestR381_StreamHandlerRefusesWhatItCannotServe(t *testing.T) {
	w := asSystem((&Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil)}).handleAuditStream,
		http.MethodGet, "/api/v1/audit/stream", "", nil)
	require.Equal(t, http.StatusInternalServerError, w.Code)
	assert.Contains(t, requireCode(t, w, errs.Internal)["message"], "not readable")

	w = serve(streamServer(&fakeStream{}).handleAuditStream, authz.Anonymous(), http.MethodGet, "/api/v1/audit/stream", "", nil)
	requireCode(t, w, errs.AuthRequired)

	w = asSystem(streamServer(&fakeStream{resolveErr: errs.New(errs.ValidInvalid, "That cursor is not one.")}).handleAuditStream,
		http.MethodGet, "/api/v1/audit/stream?after=nope", "", nil)
	requireCode(t, w, errs.ValidInvalid)

	for _, target := range []string{
		"/api/v1/audit/stream?format=cef",
		"/api/v1/audit/stream?limit=x",
		"/api/v1/audit/stream?limit=0",
		"/api/v1/audit/stream?wait=-1",
		"/api/v1/audit/stream?wait=61",
		"/api/v1/audit/stream?wait=soon",
	} {
		w = asSystem(streamServer(&fakeStream{}).handleAuditStream, http.MethodGet, target, "", nil)
		requireCode(t, w, errs.ValidInvalid)
	}

	// OCSF named with no encoder on the installation is not a format.
	noEnc := streamServer(&fakeStream{})
	noEnc.AuditEncoders = nil
	w = asSystem(noEnc.handleAuditStream, http.MethodGet, "/api/v1/audit/stream?format=ocsf", "", nil)
	assert.Contains(t, requireCode(t, w, errs.ValidInvalid)["message"], "Valid answers: native, ocsf.")
}

// TestR381_StreamHandlerServesAPage asserts a page as the stream serves it:
// the query it read with, each line through the encoder asked for, and the
// cursor to read from next.
func TestR381_StreamHandlerServesAPage(t *testing.T) {
	st := &fakeStream{page: audit.StreamPage{
		Events:   []json.RawMessage{json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)},
		Cursor:   audit.Cursor{TxID: 9, ID: 2},
		CaughtUp: true,
	}}
	srv := streamServer(st)
	srv.AuditReads = &audit.ReadThrottle{}

	w := asSystem(srv.handleAuditStream, http.MethodGet,
		"/api/v1/audit/stream?after=c1.3.4&action=app.&exclude=app.use&limit=7&wait=5", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	require.JSONEq(t, `{"events":[{"id":1},{"id":2}],"cursor":"c1.9.2","caught_up":true}`, w.Body.String())
	assert.Equal(t, audit.StreamQuery{After: audit.Cursor{TxID: 3, ID: 4}, Limit: 7,
		Actions: []string{"app."}, Exclude: []string{"app.use"}}, st.gotQuery)
	assert.Equal(t, 5*time.Second, st.gotWait)

	w = asSystem(srv.handleAuditStream, http.MethodGet, "/api/v1/audit/stream?format=ocsf", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	assert.JSONEq(t, `{"events":[{"class_uid":6003},{"class_uid":6003}],"cursor":"c1.9.2","caught_up":true}`, w.Body.String())

	// An empty page is an empty list, not null.
	st.page = audit.StreamPage{}
	w = asSystem(srv.handleAuditStream, http.MethodGet, "/api/v1/audit/stream?format=native", "", nil)
	assert.JSONEq(t, `{"events":[],"cursor":"c1.0.0","caught_up":false}`, w.Body.String())
}

// TestR381_StreamHandlerReportsAFailedRead asserts a read that fails, or a
// line its encoder refuses, is an error rather than a partial page.
func TestR381_StreamHandlerReportsAFailedRead(t *testing.T) {
	st := &fakeStream{waitErr: errors.New("connection reset")}
	w := asSystem(streamServer(st).handleAuditStream, http.MethodGet, "/api/v1/audit/stream", "", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)

	st = &fakeStream{page: audit.StreamPage{Events: []json.RawMessage{json.RawMessage(`{}`)}}}
	srv := streamServer(st)
	srv.AuditEncoders[api.AuditFormatOCSF] = func(json.RawMessage) (json.RawMessage, error) {
		return nil, errs.New(errs.Internal, "This event could not be written as OCSF.")
	}
	w = asSystem(srv.handleAuditStream, http.MethodGet, "/api/v1/audit/stream?format=ocsf", "", nil)
	assert.Contains(t, requireCode(t, w, errs.Internal)["message"], "OCSF")
}

func gunzipLines(t *testing.T, b []byte) string {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	require.NoError(t, err)
	out, err := io.ReadAll(zr)
	require.NoError(t, err)
	return string(out)
}

// TestR387_ExportHandlerWritesARangeAsGzippedLines asserts the export's
// shape: the range it read, the file it names, gzipped lines, and the header
// that says where the live log begins when the range reaches before it.
func TestR387_ExportHandlerWritesARangeAsGzippedLines(t *testing.T) {
	oldest := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	st := &fakeStream{
		lines:  []json.RawMessage{json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)},
		oldest: oldest, hasOldest: true,
	}
	srv := streamServer(st)

	w := asSystem(srv.handleAuditExport, http.MethodGet,
		"/api/v1/audit/export?since=2026-08-01T00:00:00Z&until=2026-10-01T00:00:00Z&action=app.&exclude=app.use", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/gzip", w.Header().Get("Content-Type"))
	assert.Equal(t, `attachment; filename="audit-20260801T000000Z-20261001T000000Z.jsonl.gz"`, w.Header().Get("Content-Disposition"))
	assert.Equal(t, "nosniff", w.Header().Get("X-Content-Type-Options"))
	assert.Equal(t, oldest.Format(time.RFC3339Nano), w.Header().Get("Pando-Audit-Live-From"),
		"a range from before the live log says where the archives take over")
	assert.Equal(t, "{\"id\":1}\n{\"id\":2}\n", gunzipLines(t, w.Body.Bytes()))
	assert.Equal(t, time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC), st.gotQuery.Since)
	assert.Equal(t, time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), st.gotQuery.Until)
	assert.Equal(t, []string{"app."}, st.gotQuery.Actions)
	assert.Equal(t, []string{"app.use"}, st.gotQuery.Exclude)

	// From inside the live log, no header; with no range, the plain name.
	w = asSystem(srv.handleAuditExport, http.MethodGet, "/api/v1/audit/export?since=2026-09-15T00:00:00Z&format=ocsf", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Pando-Audit-Live-From"))
	assert.Equal(t, "{\"class_uid\":6003}\n{\"class_uid\":6003}\n", gunzipLines(t, w.Body.Bytes()))

	w = asSystem(srv.handleAuditExport, http.MethodGet, "/api/v1/audit/export", "", nil)
	assert.Equal(t, `attachment; filename="audit.jsonl.gz"`, w.Header().Get("Content-Disposition"))
	assert.NotEmpty(t, w.Header().Get("Pando-Audit-Live-From"), "no start is from the beginning")

	// An oldest that cannot be read leaves the header off rather than failing.
	st.oldestErr = errors.New("timeout")
	w = asSystem(srv.handleAuditExport, http.MethodGet, "/api/v1/audit/export", "", nil)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, w.Header().Get("Pando-Audit-Live-From"))
}

// TestR387_ExportHandlerRefusesWhatItCannotServe asserts the export's
// refusals, each before anything is sent.
func TestR387_ExportHandlerRefusesWhatItCannotServe(t *testing.T) {
	w := asSystem((&Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil)}).handleAuditExport,
		http.MethodGet, "/api/v1/audit/export", "", nil)
	assert.Contains(t, requireCode(t, w, errs.Internal)["message"], "not readable")

	w = serve(streamServer(&fakeStream{}).handleAuditExport, authz.Anonymous(), http.MethodGet, "/api/v1/audit/export", "", nil)
	requireCode(t, w, errs.AuthRequired)

	w = asSystem(streamServer(&fakeStream{}).handleAuditExport, http.MethodGet, "/api/v1/audit/export?format=leef", "", nil)
	requireCode(t, w, errs.ValidInvalid)

	for _, target := range []string{"/api/v1/audit/export?since=yesterday", "/api/v1/audit/export?until=2026-13-01"} {
		w = asSystem(streamServer(&fakeStream{}).handleAuditExport, http.MethodGet, target, "", nil)
		assert.Contains(t, requireCode(t, w, errs.ValidInvalid)["message"], "RFC 3339")
	}
}

// failingWriter is a response the client stopped reading.
type failingWriter struct {
	h      http.Header
	status int
}

func (f *failingWriter) Header() http.Header       { return f.h }
func (f *failingWriter) WriteHeader(code int)      { f.status = code }
func (f *failingWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }

// TestR387_AnExportThatEndsEarlyIsTruncated asserts an export that fails part
// way leaves a gzip stream that does not decompress, which is the client's
// signal, and that a client gone away stops the export.
func TestR387_AnExportThatEndsEarlyIsTruncated(t *testing.T) {
	st := &fakeStream{lines: []json.RawMessage{json.RawMessage(`{"id":1}`)}, exportErr: errors.New("connection reset")}
	w := asSystem(streamServer(st).handleAuditExport, http.MethodGet, "/api/v1/audit/export", "", nil)
	require.Equal(t, http.StatusOK, w.Code, "the status is already sent")
	zr, err := gzip.NewReader(bytes.NewReader(w.Body.Bytes()))
	if err == nil {
		_, err = io.ReadAll(zr)
	}
	assert.Error(t, err, "no gzip trailer: the export is visibly incomplete")

	st = &fakeStream{lines: []json.RawMessage{json.RawMessage(`{"id":1}`), json.RawMessage(`{"id":2}`)}}
	r := httptest.NewRequest(http.MethodGet, "/api/v1/audit/export", nil)
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyPrincipal{}, authz.System()))
	fw := &failingWriter{h: http.Header{}}
	streamServer(st).handleAuditExport(fw, r)
	assert.Equal(t, http.StatusOK, fw.status)
}

// TestR383_SinksHandlerListsEverySink asserts the listing: none configured is
// an empty list, each sink carries its disclosure, and a failed read is an
// error.
func TestR383_SinksHandlerListsEverySink(t *testing.T) {
	srv := &Server{Logger: zap.NewNop(), Authz: authz.New(nil, nil, nil)}
	w := asSystem(srv.handleListAuditSinks, http.MethodGet, "/api/v1/audit/sinks", "", nil)
	require.JSONEq(t, `{"audit_sinks":[]}`, w.Body.String())

	w = serve(srv.handleListAuditSinks, authz.Anonymous(), http.MethodGet, "/api/v1/audit/sinks", "", nil)
	requireCode(t, w, errs.AuthRequired)

	srv.AuditSinks = fakeSinks{list: []auditstream.Status{{ID: "as_siem", Kind: "https", Name: "SIEM", Enabled: true,
		Transport: "https", Endpoint: "siem.example.com"}}}
	w = asSystem(srv.handleListAuditSinks, http.MethodGet, "/api/v1/audit/sinks", "", nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var got struct {
		AuditSinks []struct {
			ID         string `json:"id"`
			Disclosure string `json:"disclosure"`
		} `json:"audit_sinks"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Len(t, got.AuditSinks, 1)
	assert.Equal(t, "as_siem", got.AuditSinks[0].ID)
	assert.Equal(t, auditstream.Describe(auditstream.Status{ID: "as_siem", Kind: "https", Name: "SIEM", Enabled: true,
		Transport: "https", Endpoint: "siem.example.com"}), got.AuditSinks[0].Disclosure)

	srv.AuditSinks = fakeSinks{err: errors.New("database is down")}
	w = asSystem(srv.handleListAuditSinks, http.MethodGet, "/api/v1/audit/sinks", "", nil)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
