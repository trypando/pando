package mcp_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/mcp"
)

func post(t *testing.T, srv *mcp.Server, body string, header ...string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, mcp.HTTPPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func httpServer() (*mcp.Server, *[]string) {
	var paths []string
	return &mcp.Server{
		Version: "v0.16.0",
		Call: func(_ context.Context, method, path string, _, out any) error {
			paths = append(paths, method+" "+path)
			*out.(*[]byte) = []byte(`{"apps":[]}`)
			return nil
		},
	}, &paths
}

// TestR413_MCPIsServedOverHTTP asserts R-413: a message posted to the
// endpoint is answered in the response, as JSON, by the same server and
// tools as stdio.
func TestR413_MCPIsServedOverHTTP(t *testing.T) {
	srv, paths := httpServer()

	rec := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	var init map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &init))
	res := result(t, init)
	require.Equal(t, "2025-06-18", res["protocolVersion"])
	require.Equal(t, "v0.16.0", res["serverInfo"].(map[string]any)["version"])

	rec = post(t, srv, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, "MCP-Protocol-Version", "2025-06-18")
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Empty(t, rec.Body.String())

	rec = post(t, srv, call(2, "pando_list_apps", `{}`))
	require.Equal(t, http.StatusOK, rec.Code)
	var reply map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reply))
	require.Equal(t, `{"apps":[]}`, text(t, reply))
	require.Equal(t, []string{"GET /apps"}, *paths)
}

func TestHTTPTransportEdges(t *testing.T) {
	srv, _ := httpServer()

	get := httptest.NewRecorder()
	srv.ServeHTTP(get, httptest.NewRequest(http.MethodGet, mcp.HTTPPath, nil))
	require.Equal(t, http.StatusMethodNotAllowed, get.Code, "no event stream is offered")
	require.Equal(t, http.MethodPost, get.Header().Get("Allow"))

	rec := post(t, srv, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, "MCP-Protocol-Version", "1999-01-01")
	require.Equal(t, http.StatusBadRequest, rec.Code)

	rec = post(t, srv, `{not json`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "-32700")

	// A client's response to a request this server never sends.
	rec = post(t, srv, `{"jsonrpc":"2.0","id":9,"result":{}}`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	// An unknown notification is accepted and ignored; an unknown request is
	// answered with an error.
	rec = post(t, srv, `{"jsonrpc":"2.0","method":"notifications/cancelled","params":{}}`)
	require.Equal(t, http.StatusAccepted, rec.Code)
	rec = post(t, srv, `{"jsonrpc":"2.0","id":3,"method":"resources/list"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), "-32601")

	rec = post(t, srv, strings.Repeat(" ", 8*1024*1024+1))
	require.Equal(t, http.StatusRequestEntityTooLarge, rec.Code)
}

// A batch, which the 2025-03-26 revision allows, is answered as one.
func TestHTTPTransportAnswersABatch(t *testing.T) {
	srv, _ := httpServer()

	rec := post(t, srv, `[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},`+
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`)
	require.Equal(t, http.StatusOK, rec.Code)
	var replies []map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &replies))
	require.Len(t, replies, 2)

	rec = post(t, srv, `[{"jsonrpc":"2.0","method":"notifications/initialized"}]`)
	require.Equal(t, http.StatusAccepted, rec.Code)

	rec = post(t, srv, `[]`)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}
