//go:build integration

package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/httpapi"
)

const (
	mcpRedirect = "https://claude.ai/api/mcp/auth_callback"

	// httptest.NewRequest's host.
	testIssuer = "http://example.com"
)

// get is a GET outside /api/v1, for the well-known documents.
func (i *install) get(path string) reply {
	i.t.Helper()
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return reply{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

// form posts a form-encoded body, as the token endpoint takes one.
func (i *install) form(path string, values url.Values) reply {
	i.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1"+path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	return reply{Code: rec.Code, Body: rec.Body.Bytes(), Hdr: rec.Header()}
}

// mcp posts one MCP message as s.
func (i *install) mcp(s *session, message string) reply {
	i.t.Helper()
	return i.rawJSON(s, http.MethodPost, "/mcp", message)
}

// mcpText is a tool call's text, and whether it was an error.
func mcpText(t *testing.T, r reply) (string, bool) {
	t.Helper()
	require.Equal(t, http.StatusOK, r.Code, r.String())
	var msg struct {
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	r.JSON(t, &msg)
	require.Len(t, msg.Result.Content, 1, r.String())
	return msg.Result.Content[0].Text, msg.Result.IsError
}

func authorizeQuery(clientID string) url.Values {
	return url.Values{
		"client_id": {clientID}, "redirect_uri": {mcpRedirect}, "response_type": {"code"},
		"code_challenge": {clisignin.Challenge(cliVerifier)}, "code_challenge_method": {"S256"},
		"state": {"st-1"}, "resource": {testIssuer + "/api/v1/mcp"},
	}
}

// connectMCP runs the whole OAuth sign-in for s and returns the client's
// token and its ID.
func connectMCP(t *testing.T, i *install, s *session) (secretToken string, clientID string) {
	t.Helper()
	reg := i.anon(http.MethodPost, "/oauth/register", map[string]any{
		"client_name": "Claude", "redirect_uris": []string{mcpRedirect},
		"token_endpoint_auth_method": "none",
	})
	require.Equal(t, http.StatusCreated, reg.Code, reg.String())
	var client struct {
		ClientID string `json:"client_id"`
	}
	reg.JSON(t, &client)

	q := authorizeQuery(client.ClientID)
	check := i.do(s, http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
	require.Equal(t, http.StatusOK, check.Code, check.String())
	var consent struct {
		ClientName string `json:"client_name"`
		ReturnsTo  string `json:"returns_to"`
	}
	check.JSON(t, &consent)
	require.Equal(t, "Claude", consent.ClientName)
	require.Equal(t, "claude.ai", consent.ReturnsTo)

	body := map[string]any{"approve": true}
	for k := range q {
		body[k] = q.Get(k)
	}
	approved := i.do(s, http.MethodPost, "/oauth/authorize", body)
	require.Equal(t, http.StatusCreated, approved.Code, approved.String())
	require.Equal(t, "no-store", approved.Hdr.Get("Cache-Control"))
	var a struct {
		RedirectURL string `json:"redirect_url"`
	}
	approved.JSON(t, &a)
	to, err := url.Parse(a.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "claude.ai", to.Host)
	require.Equal(t, "st-1", to.Query().Get("state"))
	require.Equal(t, testIssuer, to.Query().Get("iss"))

	token := i.form("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")},
		"redirect_uri": {mcpRedirect}, "client_id": {client.ClientID}, "code_verifier": {cliVerifier},
	})
	require.Equal(t, http.StatusOK, token.Code, token.String())
	require.Equal(t, "no-store", token.Hdr.Get("Cache-Control"))
	var tok struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
	}
	token.JSON(t, &tok)
	require.Equal(t, "Bearer", tok.TokenType)
	require.NotEmpty(t, tok.AccessToken)

	again := i.form("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")},
		"client_id": {client.ClientID}, "code_verifier": {cliVerifier},
	})
	require.Equal(t, http.StatusBadRequest, again.Code, "a code works once")
	require.Contains(t, again.String(), "invalid_grant")
	return tok.AccessToken, client.ClientID
}

// TestR413_AnMCPClientConnectsToPandoByItsAddress asserts R-413 end to end:
// an MCP client with nothing but Pando's address is pointed at how to sign in,
// registers, is approved by a signed-in person, and calls tools as that
// person, each one an API call made with its token.
func TestR413_AnMCPClientConnectsToPandoByItsAddress(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	// No token: told where to find out how to get one.
	bare := i.mcp(nil, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)
	require.Equal(t, http.StatusUnauthorized, bare.Code, bare.String())
	require.Equal(t,
		`Bearer resource_metadata="`+testIssuer+`/.well-known/oauth-protected-resource/api/v1/mcp"`,
		bare.Hdr.Get("WWW-Authenticate"))

	resource := i.get("/.well-known/oauth-protected-resource/api/v1/mcp")
	require.Equal(t, http.StatusOK, resource.Code, resource.String())
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	resource.JSON(t, &prm)
	require.Equal(t, testIssuer+"/api/v1/mcp", prm.Resource)
	require.Equal(t, []string{testIssuer}, prm.AuthorizationServers)
	require.Equal(t, http.StatusOK, i.get("/.well-known/oauth-protected-resource").Code)

	server := i.get("/.well-known/oauth-authorization-server")
	require.Equal(t, http.StatusOK, server.Code, server.String())
	var asm map[string]any
	server.JSON(t, &asm)
	require.Equal(t, testIssuer, asm["issuer"])
	require.Equal(t, testIssuer+httpapi.AuthorizePath, asm["authorization_endpoint"])
	require.Equal(t, testIssuer+"/api/v1/oauth/token", asm["token_endpoint"])
	require.Equal(t, testIssuer+"/api/v1/oauth/register", asm["registration_endpoint"])
	require.Equal(t, []any{"S256"}, asm["code_challenge_methods_supported"])

	dana := cliUser(i, "dana")
	danaID, _ := meTwoFactor(t, i, dana)
	secret, clientID := connectMCP(t, i, dana)
	require.Equal(t, 1, auditCount(t, i, "oauth.approve", danaID))

	agent := &session{token: secret}
	asAgent, _ := meTwoFactor(t, i, agent)
	require.Equal(t, danaID, asAgent, "the token acts as the person who approved (R-058)")

	var listed struct {
		Tokens []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"tokens"`
	}
	list := i.do(dana, http.MethodGet, "/tokens", nil)
	require.Equal(t, http.StatusOK, list.Code, list.String())
	list.JSON(t, &listed)
	tokens := listed.Tokens
	require.Len(t, tokens, 1)
	require.Equal(t, "Claude", tokens[0].Name, "named for the client, and revoked like any other")
	require.Equal(t, 1, auditCount(t, i, "token.create", tokens[0].ID))

	init := i.mcp(agent, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	require.Equal(t, http.StatusOK, init.Code, init.String())
	require.Contains(t, init.String(), `"protocolVersion":"2025-06-18"`)

	// A tool is an API call as dana: the section is hers.
	text, isErr := mcpText(t, i.mcp(agent,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"pando_create_section","arguments":{"name":"From Claude"}}}`))
	require.False(t, isErr, text)
	mine := i.do(dana, http.MethodGet, "/me/apps", nil)
	require.Contains(t, mine.String(), "From Claude")

	// A refusal reaches the agent as the API wrote it, as a tool result.
	text, isErr = mcpText(t, i.mcp(agent,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"pando_get_app","arguments":{"app_id":"app_01JZZZZZZZZZZZZZZZZZZZZZZZ"}}}`))
	require.True(t, isErr)
	require.NotEmpty(t, text)

	// A browser session is not a way in: host policy for tokens has to hold
	// for every agent (O-12).
	asDana := i.mcp(dana, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusUnauthorized, asDana.Code, asDana.String())

	// Revoked, the token is told to sign in again.
	require.Equal(t, http.StatusNoContent, i.do(dana, http.MethodDelete, "/tokens/"+tokens[0].ID, nil).Code)
	revoked := i.mcp(agent, `{"jsonrpc":"2.0","id":1,"method":"ping"}`)
	require.Equal(t, http.StatusUnauthorized, revoked.Code, revoked.String())
	require.Contains(t, revoked.Hdr.Get("WWW-Authenticate"), "resource_metadata=")

	// A real approval spent with the wrong verifier is recorded against the
	// account that approved it; a guess is not recorded at all.
	q := authorizeQuery(clientID)
	body := map[string]any{"approve": true}
	for k := range q {
		body[k] = q.Get(k)
	}
	var a struct {
		RedirectURL string `json:"redirect_url"`
	}
	i.do(dana, http.MethodPost, "/oauth/authorize", body).JSON(t, &a)
	to, err := url.Parse(a.RedirectURL)
	require.NoError(t, err)
	stolen := i.form("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {to.Query().Get("code")},
		"client_id": {clientID}, "code_verifier": {cliVerifier + "-not-the-one"},
	})
	require.Equal(t, http.StatusBadRequest, stolen.Code)
	guess := i.form("/oauth/token", url.Values{
		"grant_type": {"authorization_code"}, "code": {"a-guess"}, "client_id": {clientID}, "code_verifier": {cliVerifier},
	})
	require.Equal(t, http.StatusBadRequest, guess.Code)
	require.Equal(t, 1, auditCount(t, i, "oauth.denied", danaID))
}

// TestR413_OnlyASignedInPersonApprovesAnMCPClient asserts R-413: nobody
// signed out, and no token, can see or approve a client's sign-in.
func TestR413_OnlyASignedInPersonApprovesAnMCPClient(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := cliUser(i, "dana")
	secret, clientID := connectMCP(t, i, dana)
	agent := &session{token: secret}

	q := authorizeQuery(clientID)
	body := map[string]any{"approve": true}
	for k := range q {
		body[k] = q.Get(k)
	}

	for _, s := range []*session{nil, agent} {
		check := i.do(s, http.MethodGet, "/oauth/authorize?"+q.Encode(), nil)
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, check.Code, check.String())
		approve := i.do(s, http.MethodPost, "/oauth/authorize", body)
		require.Contains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, approve.Code, approve.String())
	}

	// Cancel sends the browser back to the client, and only there.
	body["approve"] = false
	canceled := i.do(dana, http.MethodPost, "/oauth/authorize", body)
	require.Equal(t, http.StatusOK, canceled.Code, canceled.String())
	require.Contains(t, canceled.String(), "error=access_denied")
	body["redirect_uri"] = "https://attacker.example/cb"
	elsewhere := i.do(dana, http.MethodPost, "/oauth/authorize", body)
	require.Equal(t, http.StatusBadRequest, elsewhere.Code, elsewhere.String())
}

// The token endpoint and registration answer in OAuth's own format, which is
// what a client reads.
func TestOAuthEndpointsAnswerInOAuthsFormat(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	reg := i.anon(http.MethodPost, "/oauth/register", map[string]any{"redirect_uris": []string{"http://evil.example/cb"}})
	require.Equal(t, http.StatusBadRequest, reg.Code)
	require.Contains(t, reg.String(), `"error":"invalid_redirect_uri"`)

	bad := i.rawJSON(nil, http.MethodPost, "/oauth/register", `not json`)
	require.Equal(t, http.StatusBadRequest, bad.Code)
	require.Contains(t, bad.String(), "invalid_client_metadata")

	asJSON := i.rawJSON(nil, http.MethodPost, "/oauth/token", `{"grant_type":"authorization_code"}`)
	require.Equal(t, http.StatusBadRequest, asJSON.Code)
	require.Contains(t, asJSON.String(), `"error":"invalid_request"`)

	refresh := i.form("/oauth/token", url.Values{"grant_type": {"refresh_token"}})
	require.Equal(t, http.StatusBadRequest, refresh.Code)
	require.Contains(t, refresh.String(), "unsupported_grant_type")
}

// Without OAuth set up, each endpoint says so rather than half-working.
func TestOAuthUnavailable(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.OAuth = nil
	dana := cliUser(i, "dana")

	require.Equal(t, http.StatusNotFound, i.get("/.well-known/oauth-authorization-server").Code)
	require.Equal(t, http.StatusNotFound, i.get("/.well-known/oauth-protected-resource").Code)
	require.Equal(t, http.StatusNotFound, i.anon(http.MethodPost, "/oauth/register", map[string]any{}).Code)
	require.Equal(t, http.StatusNotFound, i.form("/oauth/token", url.Values{}).Code)
	require.Equal(t, http.StatusNotFound, i.do(dana, http.MethodGet, "/oauth/authorize", nil).Code)
	require.Equal(t, http.StatusNotFound, i.do(dana, http.MethodPost, "/oauth/authorize", map[string]any{}).Code)
}

// An API token made by hand works too, for a client that takes one.
func TestR413_AHandMadeTokenReachesTheMCPEndpoint(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	dana := cliUser(i, "dana")

	made := i.do(dana, http.MethodPost, "/tokens", map[string]any{"name": "agent"})
	require.Equal(t, http.StatusCreated, made.Code, made.String())
	var tok struct {
		Secret string `json:"secret"`
	}
	made.JSON(t, &tok)

	var buf bytes.Buffer
	require.NoError(t, json.NewEncoder(&buf).Encode(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}))
	got := i.mcp(&session{token: tok.Secret}, buf.String())
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.Contains(t, got.String(), "pando_list_apps")
}
