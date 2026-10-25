package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"slices"
)

// MCP over HTTP (R-413): the Streamable HTTP transport, the part of it this
// server needs.
//
// Each POST carries a message and is answered in its own response, as JSON.
// No stream is offered: GET is refused, as the transport allows, because no
// tool sends anything unasked — a long read such as a detection's progress is
// a long poll, which a plain response carries. No session is kept either: a
// tool call is an API call with the caller's token, so there is nothing
// between messages to remember. Who the caller is, is the HTTP layer's to
// settle before a message gets here (internal/httpapi).

// HTTPPath is where Pando answers MCP. Under /api/v1, which is Pando's on
// every installation, so no app's path can be taken by it.
const HTTPPath = "/api/v1/mcp"

// ServeHTTP answers one MCP message, or a batch of them, posted to it.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeRPC(w, http.StatusMethodNotAllowed, &response{JSONRPC: "2.0",
			Error: &rpcError{Code: -32000, Message: "Pando's MCP endpoint takes POST only; it offers no event stream."}})
		return
	}
	if v := r.Header.Get("MCP-Protocol-Version"); v != "" && !slices.Contains(protocolVersions, v) {
		writeRPC(w, http.StatusBadRequest, &response{JSONRPC: "2.0",
			Error: &rpcError{Code: -32600, Message: "unsupported MCP-Protocol-Version: " + v}})
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxMessage+1))
	if err != nil || len(raw) > maxMessage {
		writeRPC(w, http.StatusRequestEntityTooLarge, &response{JSONRPC: "2.0",
			Error: &rpcError{Code: -32600, Message: "message too large"}})
		return
	}
	raw = bytes.TrimSpace(raw)

	// A batch, which the 2025-03-26 revision allows and later ones dropped.
	if len(raw) > 0 && raw[0] == '[' {
		var batch []request
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 {
			writeRPC(w, http.StatusBadRequest, &response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			return
		}
		var out []*response
		for _, req := range batch {
			if resp := s.answer(r, req); resp != nil {
				out = append(out, resp)
			}
		}
		if len(out) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		writeRPC(w, http.StatusOK, out)
		return
	}

	var req request
	if err := json.Unmarshal(raw, &req); err != nil {
		writeRPC(w, http.StatusBadRequest, &response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
		return
	}
	resp := s.answer(r, req)
	if resp == nil {
		// A notification, or a client's response to a request this server
		// never sends: accepted, with nothing to say.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

// answer is handle for a message that may be a client's response rather than a
// request: it has no method, and nothing answers it.
func (s *Server) answer(r *http.Request, req request) *response {
	if req.Method == "" {
		return nil
	}
	return s.handle(r.Context(), req)
}

func writeRPC(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
