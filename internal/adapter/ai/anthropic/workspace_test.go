package anthropic_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	anthropicadapter "github.com/trypando/pando/internal/adapter/ai/anthropic"
	"github.com/trypando/pando/internal/adapter/api"
)

// headerRecorder answers a models lookup and a Messages call, and keeps the
// anthropic-workspace-id header of every request it saw — present or not.
type headerRecorder struct {
	mu   sync.Mutex
	seen []http.Header
}

func (h *headerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.seen = append(h.seen, r.Header.Clone())
	h.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet {
		_, _ = io.WriteString(w, `{"id":"claude-opus-5-5","type":"model","display_name":"Claude Opus 5.5","created_at":"2026-01-01T00:00:00Z"}`)
		return
	}
	_, _ = io.WriteString(w, message("tool_use", toolUse("t1", "submit", `{"summary":"Nothing happened."}`)))
}

// callEveryWay configures an adapter against the recorder and makes both
// kinds of request it makes: the health check's models lookup and a Messages
// call.
func callEveryWay(t *testing.T, extra map[string]any) []http.Header {
	t.Helper()
	rec := &headerRecorder{}
	server := httptest.NewServer(rec)
	t.Cleanup(server.Close)

	cfg := map[string]any{
		"credentials": map[string]string{"api_key": "sk-ant-test"},
		"base_url":    server.URL,
	}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, err := json.Marshal(cfg)
	require.NoError(t, err)

	a := anthropicadapter.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	require.NoError(t, a.HealthCheck(context.Background()))
	_, err = a.SummarizeAudit(context.Background(), api.AuditSummaryRequest{Question: "Anything?"})
	require.NoError(t, err)

	require.Len(t, rec.seen, 2)
	return rec.seen
}

// TestAWorkspaceIDIsSentOnEveryRequest asserts that a configured workspace ID
// reaches the API as anthropic-workspace-id on the models lookup and on a
// Messages call alike — a key created for several workspaces is refused on
// any request without it.
func TestAWorkspaceIDIsSentOnEveryRequest(t *testing.T) {
	for _, h := range callEveryWay(t, map[string]any{"workspace_id": " wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ "}) {
		require.Equal(t, "wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ", h.Get("anthropic-workspace-id"),
			"sent, with the whitespace a paste brings trimmed")
	}
}

// TestNoWorkspaceIDSendsNoHeader asserts that leaving the setting empty
// behaves as before it existed: no anthropic-workspace-id header at all, which
// is what a key created for one workspace expects.
func TestNoWorkspaceIDSendsNoHeader(t *testing.T) {
	for _, h := range callEveryWay(t, nil) {
		_, present := h["Anthropic-Workspace-Id"]
		require.False(t, present, "an empty setting sends no header, not an empty one")
	}
}

// TestAMalformedWorkspaceIDIsRefusedAtConfigure asserts a value that is not a
// workspace's tagged ID fails when it is saved, with a message that says where
// the right value is, rather than as a 400 on the first screening.
func TestAMalformedWorkspaceIDIsRefusedAtConfigure(t *testing.T) {
	for _, bad := range []string{"default", "01JwQvzr7rXLA5AGx3HKfFUJ", "wrkspc_", "wrkspc_abc\r\nX-Other: 1", "Production"} {
		raw, err := json.Marshal(map[string]any{
			"credentials":  map[string]string{"api_key": "sk-ant-test"},
			"workspace_id": bad,
		})
		require.NoError(t, err)
		err = anthropicadapter.New().Configure(context.Background(), raw)
		require.Error(t, err, bad)
		require.Contains(t, err.Error(), "wrkspc_")
		require.Contains(t, err.Error(), "Workspaces page")
	}
}

// TestInfoOffersTheWorkspaceID asserts the form asks for the workspace ID as
// plain, optional configuration with no default.
func TestInfoOffersTheWorkspaceID(t *testing.T) {
	for _, f := range anthropicadapter.Info().Fields {
		if f.Key != "workspace_id" {
			continue
		}
		require.False(t, f.Credential, "a workspace ID is not a secret")
		require.Empty(t, f.Default, "blank unless the key needs it")
		require.True(t, strings.HasPrefix(f.Placeholder, "wrkspc_"))
		return
	}
	t.Fatal("no workspace_id field")
}
