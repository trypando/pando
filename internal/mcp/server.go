package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"sync"
)

// A minimal MCP server over JSON-RPC 2.0, on stdio (pando mcp) or HTTP (http.go).
//
// Written rather than pulled in: the protocol surface Pando needs is
// initialize, tools/list and tools/call, and an SDK for three methods is a
// dependency to track for no gain. The wire format is JSON-RPC 2.0, which is
// old and stable.
//
// Every tool is a call to the REST API with the agent's own token (R-262). That
// is the whole design: an agent is a principal like any other, no tool bypasses
// authorization, and every action lands in the audit log under the token's
// owner. A tool that reached into the service layer directly would be a second
// path to the same actions with its own idea of who is allowed to take them.

// protocolVersions are the MCP revisions this server speaks, latest first.
// The three differ in what this server does not use — batching, streams,
// structured tool output — so one implementation answers all of them.
var protocolVersions = []string{"2025-06-18", "2025-03-26", "2024-11-05"}

// Server speaks MCP on a pair of streams.
type Server struct {
	// Call performs one API call on the agent's behalf. Injected so the
	// transport can be tested without a server, and so this package cannot
	// reach anything the API does not expose.
	//
	// out is a *[]byte, and Call stores the response body in it as sent. Not
	// decoded: the body goes to the agent unchanged, and an app's logs are
	// plain text rather than JSON.
	Call func(ctx context.Context, method, path string, body any, out any) error

	// ToolsDisabled are excluded from tools/list.
	//
	// A courtesy, not a boundary. The enforcement is host policy scoped to
	// token principals (O-12), because an agent holding a token can call the
	// REST API directly and would simply route around a list kept here. What
	// this does is stop the agent wasting a turn on a call it cannot make.
	ToolsDisabled map[string]bool

	// Version is reported as serverInfo.version. Empty reports "dev".
	Version string

	out *json.Encoder
	mu  sync.Mutex
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads requests until the stream closes.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = json.NewEncoder(out)

	scanner := bufio.NewScanner(in)
	// MCP messages are one JSON object per line, and a tools/call result can
	// carry a whole app spec. The default 64 KiB would truncate one.
	scanner.Buffer(make([]byte, 0, 64*1024), maxMessage)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(&response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		s.reply(s.handle(ctx, req))
	}
	return scanner.Err()
}

// maxMessage is the largest message either transport reads.
const maxMessage = 8 * 1024 * 1024

// handle answers one message. Nil for a notification, which has no answer.
func (s *Server) handle(ctx context.Context, req request) *response {
	switch req.Method {
	case "initialize":
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": negotiate(req.Params),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pando", "version": s.version()},
		}}

	case "notifications/initialized", "initialized":
		// A notification: no id, no reply.
		return nil

	case "tools/list":
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"tools": s.tools(),
		}}

	case "tools/call":
		return s.callTool(ctx, req)

	case "ping":
		return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}}

	default:
		if req.ID == nil {
			// Any other notification, such as notifications/cancelled, is
			// one this server has nothing to do about.
			return nil
		}
		return &response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "unknown method: " + req.Method}}
	}
}

// negotiate answers initialize's protocolVersion: the client's, when this
// server speaks it, and otherwise the latest this server does, which the
// client may then decline (MCP lifecycle, "Version Negotiation").
func negotiate(params json.RawMessage) string {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(params, &p)
	if slices.Contains(protocolVersions, p.ProtocolVersion) {
		return p.ProtocolVersion
	}
	return protocolVersions[0]
}

func (s *Server) callTool(ctx context.Context, req request) *response {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		return &response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "invalid params"}}
	}

	tool, ok := toolsByName[params.Name]
	if !ok || s.ToolsDisabled[params.Name] {
		return &response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}}
	}

	method, path, body, err := tool.request(params.Arguments)
	if err != nil {
		return toolError(req.ID, err.Error())
	}

	var result []byte
	if err := s.Call(ctx, method, path, body, &result); err != nil {
		// The API's own message, which is held to the R-105 standard: self
		// contained, actionable, and written to be pasted into an assistant.
		// An agent is exactly that reader, so rewriting it here would discard
		// the thing it was written for.
		return toolError(req.ID, err.Error())
	}

	// An endpoint that answers 204 has no body. An empty result reads to an
	// agent as if nothing happened, so it gets a word saying something did.
	text := string(result)
	if strings.TrimSpace(text) == "" {
		text = "Done."
	}
	return &response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}}
}

// toolError reports a failure as a tool result rather than a protocol error.
//
// An agent can read and act on a tool result; a JSON-RPC error is a transport
// failure and most clients surface it as one. "You do not have permission to do
// this, it requires app.deploy" is information the agent should get, not a
// broken connection.
func toolError(id json.RawMessage, message string) *response {
	return &response{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": message}},
	}}
}

func (s *Server) reply(r *response) {
	if r == nil || (r.ID == nil && r.Error == nil && r.Result == nil) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_ = s.out.Encode(r)
}

func (s *Server) tools() []map[string]any {
	out := make([]map[string]any, 0, len(toolList))
	for _, t := range toolList {
		if s.ToolsDisabled[t.Name] {
			continue
		}
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Description,
			"inputSchema": t.Schema,
		})
	}
	return out
}

func (s *Server) version() string {
	if s.Version == "" {
		return "dev"
	}
	return s.Version
}
