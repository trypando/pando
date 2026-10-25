package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// Pando's own MCP endpoint (R-413, design 04 §3).
//
// The same server `pando mcp` runs, on another transport: every tool is a call
// to this API with the caller's token, made in-process through the same router
// and every middleware on it, so a tool reaches exactly what the token reaches
// by HTTP and is audited the same way (R-262). Nothing here decides access.

// handleMCP answers MCP for a token, and only a token. A browser session is
// refused: host policy limits what tokens may do (agent_disabled_verbs,
// O-12), and an agent arriving on a person's session would be outside it.
func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	p := PrincipalFrom(r.Context())
	if p.Kind != authz.KindToken {
		s.challenge(w, r)
		message := "Pando's MCP endpoint needs a token."
		if p.Kind != authz.KindAnonymous {
			message = "Pando's MCP endpoint takes a token, not a browser session."
		}
		Error(w, r, errs.New(errs.AuthRequired, message).
			WithRemedy("Add Pando to the MCP client by this address and approve it in the browser, or send an API token in the Authorization header."))
		return
	}
	srv := &mcp.Server{Version: s.Version, Call: s.mcpCall(mcpRoot(r.Context()), r)}
	srv.ServeHTTP(w, r)
}

// rootKey carries the router a request came in through, for the MCP endpoint
// to make its tools' calls through the same one.
type rootKey struct{}

// withRoot puts the router in each request's context.
func withRoot(root http.Handler) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), rootKey{}, root)))
		})
	}
}

func mcpRoot(ctx context.Context) http.Handler {
	root, _ := ctx.Value(rootKey{}).(http.Handler)
	return root
}

// challenge points a client without a token at where to get one (RFC 9728
// §5.1, MCP authorization).
func (s *Server) challenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate",
		fmt.Sprintf(`Bearer resource_metadata="%s"`, s.issuer(r)+protectedResourcePath+mcp.HTTPPath))
}

// protectedResourcePath is where RFC 9728 puts protected resource metadata.
const protectedResourcePath = "/.well-known/oauth-protected-resource"

// mcpChallenges adds the challenge to every 401 the MCP endpoint answers,
// including one Authenticate gives an expired or revoked token before the
// handler runs, so a client knows to sign in again.
func (s *Server) mcpChallenges(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != mcp.HTTPPath {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(&challengeWriter{ResponseWriter: w, challenge: func() { s.challenge(w, r) }}, r)
	})
}

type challengeWriter struct {
	http.ResponseWriter
	challenge func()
}

func (c *challengeWriter) WriteHeader(status int) {
	if status == http.StatusUnauthorized {
		c.challenge()
	}
	c.ResponseWriter.WriteHeader(status)
}

// innerHeaders are copied from the MCP request to each API call it makes:
// the credential, and what says where the caller is, so the audit log records
// the agent's address rather than Pando's own.
var innerHeaders = []string{
	"Authorization", "User-Agent", "Forwarded", "X-Forwarded-For", "X-Forwarded-Proto",
	"X-Forwarded-Host", "X-Real-Ip",
}

// mcpCall makes a tool's API call through root as the MCP request's caller.
func (s *Server) mcpCall(root http.Handler, outer *http.Request) func(ctx context.Context, method, path string, body, out any) error {
	return func(ctx context.Context, method, path string, body, out any) error {
		var reader io.Reader = http.NoBody
		contentType := ""
		switch b := body.(type) {
		case nil:
		case mcp.Bytes:
			reader, contentType = bytes.NewReader(b.Data), b.ContentType
		default:
			encoded, err := json.Marshal(b)
			if err != nil {
				return err
			}
			reader, contentType = bytes.NewReader(encoded), "application/json"
		}

		// The outer request's route context would make the router treat
		// this as a subrouter's request and skip routing; a fresh one routes
		// it from the top.
		ctx = context.WithValue(ctx, chi.RouteCtxKey, nil)
		req, err := http.NewRequestWithContext(ctx, method, "/api/v1"+path, reader)
		if err != nil {
			return err
		}
		req.Host, req.RemoteAddr, req.TLS = outer.Host, outer.RemoteAddr, outer.TLS
		for _, h := range innerHeaders {
			if v := outer.Header.Values(h); len(v) > 0 {
				req.Header[h] = v
			}
		}
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}

		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		root.ServeHTTP(rec, req)

		if rec.status >= http.StatusBadRequest {
			return envelopeError(rec.status, rec.body.Bytes())
		}
		if raw, ok := out.(*[]byte); ok {
			*raw = rec.body.Bytes()
			return nil
		}
		if out == nil || rec.body.Len() == 0 {
			return nil
		}
		return json.Unmarshal(rec.body.Bytes(), out)
	}
}

// envelopeError is a refusal as an agent reads it: the API's message and its
// remedy, as `pando mcp` reports them (cli.APIError).
func envelopeError(status int, raw []byte) error {
	var e struct {
		Message string `json:"message"`
		Remedy  string `json:"remedy"`
	}
	if json.Unmarshal(raw, &e) != nil || e.Message == "" {
		return fmt.Errorf("the server returned %d", status)
	}
	if e.Remedy != "" {
		return fmt.Errorf("%s\n%s", e.Message, e.Remedy)
	}
	return fmt.Errorf("%s", e.Message)
}

// bufferedResponse holds an in-process API response for the tool that asked.
type bufferedResponse struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.status, b.wrote = status, true
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	b.wrote = true
	return b.body.Write(p)
}

// Flush is a no-op: the response goes to the tool whole.
func (b *bufferedResponse) Flush() {}
