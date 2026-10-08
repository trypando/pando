//go:build integration

package httpapi_test

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func (i *install) auditCount(s *session, action string) int {
	i.t.Helper()
	var page struct {
		Events []struct {
			Action string `json:"action"`
		} `json:"events"`
	}
	got := i.do(s, http.MethodGet, "/audit?limit=500&action="+url.QueryEscape(action), nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	got.JSON(i.t, &page)
	n := 0
	for _, e := range page.Events {
		if e.Action == action {
			n++
		}
	}
	return n
}

var sinkBody = map[string]any{
	"id": "as_siem", "category": "audit_sink", "kind": "https", "name": "SIEM",
	"config":      map[string]any{"url": "https://siem.example.com/ingest", "exclude": "app.use"},
	"credentials": map[string]any{"token": "hec-token-value"},
}

// TestR385_AnAuditSinkNeedsInstallAuditExport asserts R-385: managing adapters
// is not enough to send the audit log somewhere new.
func TestR385_AnAuditSinkNeedsInstallAuditExport(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	ops := i.user("adapter-ops")
	role := i.customRole(admin, "adapter operator", "install", "install.view", "install.adapters.manage")
	require.Equal(t, http.StatusOK, i.do(admin, http.MethodPut, "/users/"+i.userID(ops)+"/role", map[string]any{"role_id": role}).Code)

	refused := i.do(ops, http.MethodPost, "/adapters", sinkBody)
	require.Equal(t, http.StatusForbidden, refused.Code, refused.String())
	assert.Contains(t, refused.String(), "install.audit.export")

	saved := i.do(admin, http.MethodPost, "/adapters", sinkBody)
	require.Equal(t, http.StatusCreated, saved.Code, saved.String())
	assert.Contains(t, saved.String(), "in use from now on")

	// Saving over it under another category is changing it too.
	overwrite := map[string]any{"id": "as_siem", "category": "ai", "kind": "anthropic", "name": "x"}
	refused = i.do(ops, http.MethodPost, "/adapters", overwrite)
	require.Equal(t, http.StatusForbidden, refused.Code, refused.String())

	// Refused when its adapter refuses it, before anything is saved.
	bad := map[string]any{"id": "as_bad", "category": "audit_sink", "kind": "https", "name": "bad",
		"config": map[string]any{"url": "http://siem.example.com/ingest"}}
	got := i.do(admin, http.MethodPost, "/adapters", bad)
	require.GreaterOrEqual(t, got.Code, 400, got.String())

	// The sink is listed for anyone who reads the log, with what it sends
	// where, and the token is nowhere in it.
	var sinks struct {
		AuditSinks []struct {
			ID         string `json:"id"`
			Endpoint   string `json:"endpoint"`
			Disclosure string `json:"disclosure"`
			Backlog    int    `json:"backlog"`
		} `json:"audit_sinks"`
	}
	list := i.do(admin, http.MethodGet, "/audit/sinks", nil)
	require.Equal(t, http.StatusOK, list.Code, list.String())
	list.JSON(t, &sinks)
	require.Len(t, sinks.AuditSinks, 1)
	assert.Equal(t, "Every audit event except app.use is sent to siem.example.com over HTTPS.", sinks.AuditSinks[0].Disclosure)
	assert.NotContains(t, list.String(), "hec-token-value")
	assert.NotContains(t, i.do(admin, http.MethodGet, "/audit?action=adapter.configure", nil).String(), "hec-token-value")
}

// TestR381_TheStreamReadsInOrderFromACursor asserts R-381 through the API, and
// R-388: reading the stream is audited, at most once an hour per reader.
func TestR381_TheStreamReadsInOrderFromACursor(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	type page struct {
		Events   []map[string]any `json:"events"`
		Cursor   string           `json:"cursor"`
		CaughtUp bool             `json:"caught_up"`
	}
	read := func(after string, extra string) page {
		got := i.do(admin, http.MethodGet, "/audit/stream?after="+url.QueryEscape(after)+extra, nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		var p page
		got.JSON(t, &p)
		return p
	}

	// The horizon is the oldest transaction running anywhere in the cluster
	// (design 12 §2), and other tests share this one, so a read waits for
	// events as a collector would rather than expecting them at once.
	seen := map[float64]bool{}
	take := func(p page) {
		for _, e := range p.Events {
			id := e["id"].(float64)
			assert.False(t, seen[id], "event %v was read twice", id)
			seen[id] = true
		}
		assertCommitOrder(t, p.Events)
	}

	first := read("", "&wait=30")
	require.NotEmpty(t, first.Events, "the install's own setup is in the log")
	take(first)

	i.appWithSpec(admin, "streamed")
	next := read(first.Cursor, "&action=app.&wait=30")
	require.NotEmpty(t, next.Events)
	for _, e := range next.Events {
		assert.Contains(t, e["action"], "app.")
	}
	take(next)
	// Whatever is past the second cursor is new: nothing already read comes
	// back.
	take(read(next.Cursor, ""))

	ocsf := read("", "&limit=1&format=ocsf&wait=30")
	require.Len(t, ocsf.Events, 1)
	assert.Contains(t, ocsf.Events[0], "class_uid")

	bad := i.do(admin, http.MethodGet, "/audit/stream?after=42", nil)
	require.Equal(t, http.StatusBadRequest, bad.Code, bad.String())

	assert.Equal(t, 1, countVia(t, i, admin, "stream"), "several stream reads in an hour are recorded once")
	assert.Positive(t, countVia(t, i, admin, "list"), "listing the log is recorded every time")
}

// assertCommitOrder checks lines are in the stream's order: by transaction,
// then by id (design 12 §2). Not by id alone, which is the order the stream
// exists to avoid promising.
func assertCommitOrder(t *testing.T, events []map[string]any) {
	t.Helper()
	var lastTx, lastID float64 = -1, -1
	for _, e := range events {
		tx, err := strconv.ParseFloat(fmt.Sprint(e["txid"]), 64)
		require.NoError(t, err, "a native line carries its txid: %v", e)
		id := e["id"].(float64)
		if tx == lastTx {
			assert.Greater(t, id, lastID, "within a transaction, by id")
		} else {
			assert.Greater(t, tx, lastTx, "by transaction")
		}
		lastTx, lastID = tx, id
	}
}

func countVia(t *testing.T, i *install, s *session, via string) int {
	t.Helper()
	var p struct {
		Events []struct {
			Action string         `json:"action"`
			Detail map[string]any `json:"detail"`
		} `json:"events"`
	}
	i.do(s, http.MethodGet, "/audit?limit=500&action=audit.read", nil).JSON(t, &p)
	n := 0
	for _, e := range p.Events {
		if e.Action == "audit.read" && e.Detail["via"] == via {
			n++
		}
	}
	return n
}

// TestR387_AnyRangeExportsAsGzippedLines asserts R-387 and R-388: an export is
// the archive's line format, gzipped, in commit order, and is itself audited.
func TestR387_AnyRangeExportsAsGzippedLines(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	i.appWithSpec(admin, "exported")

	// An export reads up to the horizon, which other tests on this cluster
	// hold back (design 12 §2): asked again until the app's events are past it.
	var lines []map[string]any
	require.Eventually(t, func() bool {
		got := i.do(admin, http.MethodGet, "/audit/export?action=app.", nil)
		require.Equal(t, http.StatusOK, got.Code, got.String())
		assert.Equal(t, "application/gzip", got.Hdr.Get("Content-Type"))
		zr, err := gzip.NewReader(bytes.NewReader(got.Body))
		require.NoError(t, err)
		sc := bufio.NewScanner(zr)
		lines = nil
		for sc.Scan() {
			var line map[string]any
			require.NoError(t, json.Unmarshal(sc.Bytes(), &line))
			lines = append(lines, line)
		}
		require.NoError(t, sc.Err())
		return len(lines) > 0
	}, 30*time.Second, 250*time.Millisecond)
	for _, line := range lines {
		require.Contains(t, line, "occurred_at")
		require.Contains(t, line, "outcome", "every column, as the archive writes it")
	}
	assertCommitOrder(t, lines)
	assert.GreaterOrEqual(t, i.auditCount(admin, "audit.export"), 1, "every export is recorded")
}
