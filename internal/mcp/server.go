package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"sync"
)

// A minimal MCP server over JSON-RPC 2.0 on stdio.
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

const protocolVersion = "2024-11-05"

// Server speaks MCP on a pair of streams.
type Server struct {
	// Call performs one API call on the agent's behalf. Injected so the
	// transport can be tested without a server, and so this package cannot
	// reach anything the API does not expose.
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

	out  *json.Encoder
	mu   sync.Mutex
	name string
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
	s.name = "pando"

	scanner := bufio.NewScanner(in)
	// MCP messages are one JSON object per line, and a tools/call result can
	// carry a whole app spec. The default 64 KiB would truncate one.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var req request
		if err := json.Unmarshal(line, &req); err != nil {
			s.reply(response{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: "parse error"}})
			continue
		}
		s.dispatch(ctx, req)
	}
	return scanner.Err()
}

func (s *Server) dispatch(ctx context.Context, req request) {
	switch req.Method {
	case "initialize":
		s.reply(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "pando", "version": s.version()},
		}})

	case "notifications/initialized", "initialized":
		// A notification: no id, no reply.

	case "tools/list":
		s.reply(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"tools": s.tools(),
		}})

	case "tools/call":
		s.callTool(ctx, req)

	case "ping":
		s.reply(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{}})

	default:
		s.reply(response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32601, Message: "unknown method: " + req.Method}})
	}
}

func (s *Server) callTool(ctx context.Context, req request) {
	var params struct {
		Name      string         `json:"name"`
		Arguments map[string]any `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.reply(response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "invalid params"}})
		return
	}

	tool, ok := toolsByName[params.Name]
	if !ok || s.ToolsDisabled[params.Name] {
		s.reply(response{JSONRPC: "2.0", ID: req.ID,
			Error: &rpcError{Code: -32602, Message: "unknown tool: " + params.Name}})
		return
	}

	method, path, body, err := tool.request(params.Arguments)
	if err != nil {
		s.toolError(req.ID, err.Error())
		return
	}

	var result json.RawMessage
	if err := s.Call(ctx, method, path, body, &result); err != nil {
		// The API's own message, which is held to the R-105 standard: self
		// contained, actionable, and written to be pasted into an assistant.
		// An agent is exactly that reader, so rewriting it here would discard
		// the thing it was written for.
		s.toolError(req.ID, err.Error())
		return
	}

	s.reply(response{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(result)}},
	}})
}

// toolError reports a failure as a tool result rather than a protocol error.
//
// An agent can read and act on a tool result; a JSON-RPC error is a transport
// failure and most clients surface it as one. "You do not have permission to do
// this, it requires app.deploy" is information the agent should get, not a
// broken connection.
func (s *Server) toolError(id json.RawMessage, message string) {
	s.reply(response{JSONRPC: "2.0", ID: id, Result: map[string]any{
		"isError": true,
		"content": []map[string]any{{"type": "text", "text": message}},
	}})
}

func (s *Server) reply(r response) {
	if r.ID == nil && r.Error == nil && r.Result == nil {
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
