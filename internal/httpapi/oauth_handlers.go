package httpapi

import (
	"encoding/json"
	"mime"
	"net/http"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/oauth"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// Signing an MCP client in with OAuth (R-413, design 06 §3.6). The logic is
// package oauth's; these publish where things are, check who is asking, and
// audit.

// AuthorizePath is the console's approval page, OAuth's authorization
// endpoint. Under /admin, which no app slug can take (consoleRoutes).
const AuthorizePath = "/admin/authorize"

// headerCacheControl is the header every answer here sets.
const headerCacheControl = "Cache-Control"

const (
	oauthTokenPath    = "/api/v1/oauth/token"
	oauthRegisterPath = "/api/v1/oauth/register"
)

// issuer is Pando as an authorization server: the address it is reached at.
func (s *Server) issuer(r *http.Request) string { return s.externalBase(r) }

// oauthResources are the values a client may name as the resource it wants a
// token for (RFC 8707): the MCP endpoint, or Pando as a whole. The token is a
// delegated token either way, good for the API as the CLI's is.
func (s *Server) oauthResources(r *http.Request) []string {
	base := s.issuer(r)
	return []string{base + mcp.HTTPPath, base}
}

func (s *Server) oauthUnavailable(w http.ResponseWriter, r *http.Request) bool {
	if s.OAuth != nil {
		return false
	}
	Error(w, r, errs.New(errs.NotFound, "This installation does not sign MCP clients in with OAuth.").
		WithRemedy("Give the MCP client an API token in its Authorization header, or run pando mcp on your machine."))
	return true
}

// handleProtectedResource is the MCP endpoint's protected resource metadata
// (RFC 9728): which authorization server issues tokens for it. Public, and
// where a client is pointed by the 401 it gets without a token.
func (s *Server) handleProtectedResource(w http.ResponseWriter, r *http.Request) {
	if s.oauthUnavailable(w, r) {
		return
	}
	w.Header().Set(headerCacheControl, "public, max-age=300")
	JSON(w, http.StatusOK, map[string]any{
		"resource":                 s.issuer(r) + mcp.HTTPPath,
		"resource_name":            "Pando",
		"authorization_servers":    []string{s.issuer(r)},
		"bearer_methods_supported": []string{"header"},
	})
}

// handleAuthorizationServer is Pando's authorization server metadata (RFC
// 8414). Public.
func (s *Server) handleAuthorizationServer(w http.ResponseWriter, r *http.Request) {
	if s.oauthUnavailable(w, r) {
		return
	}
	base := s.issuer(r)
	w.Header().Set(headerCacheControl, "public, max-age=300")
	JSON(w, http.StatusOK, map[string]any{
		"issuer":                                         base,
		"authorization_endpoint":                         base + AuthorizePath,
		"token_endpoint":                                 base + oauthTokenPath,
		"registration_endpoint":                          base + oauthRegisterPath,
		"response_types_supported":                       []string{"code"},
		"grant_types_supported":                          []string{"authorization_code"},
		"code_challenge_methods_supported":               []string{"S256"},
		"token_endpoint_auth_methods_supported":          []string{"none"},
		"authorization_response_iss_parameter_supported": true,
	})
}

// handleRegisterOAuthClient is dynamic client registration (RFC 7591).
// Public, and not audited: registering grants nothing, and a row per request
// would be a row anybody can add. A client nobody approves goes after a day.
func (s *Server) handleRegisterOAuthClient(w http.ResponseWriter, r *http.Request) {
	if s.oauthUnavailable(w, r) {
		return
	}
	var reg oauth.Registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil {
		oauthJSON(w, http.StatusBadRequest, &oauth.ErrorResponse{Code: "invalid_client_metadata", Description: "The request body is not a JSON object."})
		return
	}
	got, oerr := s.OAuth.Register(r.Context(), reg)
	if oerr != nil {
		oauthFailed(w, oerr)
		return
	}
	oauthJSON(w, http.StatusCreated, got)
}

// oauthRequest reads an authorization request from the approval page's query.
func oauthRequest(r *http.Request) oauth.Request {
	q := r.URL.Query()
	return oauth.Request{
		ClientID: q.Get("client_id"), RedirectURI: q.Get("redirect_uri"), ResponseType: q.Get("response_type"),
		CodeChallenge: q.Get("code_challenge"), CodeChallengeMethod: q.Get("code_challenge_method"),
		State: q.Get("state"), Resource: q.Get("resource"), Scope: q.Get("scope"),
	}
}

// signedInPerson is the session approving a sign-in, or false once it has
// answered. A session only, as a CLI sign-in (handleApproveCLISignIn): a
// token approving would be a token minting a token.
func signedInPerson(w http.ResponseWriter, r *http.Request) (authz.Principal, bool) {
	p := PrincipalFrom(r.Context())
	if p.Kind == authz.KindAnonymous || p.UserID == "" {
		Error(w, r, errs.New(errs.AuthRequired, "Sign in to Pando in this browser to connect an MCP client."))
		return p, false
	}
	if p.Kind == authz.KindToken {
		Error(w, r, errs.New(errs.PermDenied, "A token cannot sign in an MCP client.").
			WithRemedy("Open the address the MCP client opened in a browser signed in to Pando."))
		return p, false
	}
	return p, true
}

// handleCheckOAuthAuthorization is what the approval page shows: which client
// is asking, and where the browser will go back to.
func (s *Server) handleCheckOAuthAuthorization(w http.ResponseWriter, r *http.Request) {
	if _, ok := signedInPerson(w, r); !ok || s.oauthUnavailable(w, r) {
		return
	}
	consent, err := s.OAuth.Check(r.Context(), oauthRequest(r), s.oauthResources(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	JSON(w, http.StatusOK, consent)
}

// handleDecideOAuthAuthorization is the approval page's Approve or Cancel.
// Either way the answer is where to send the browser: the client's own
// address, with a code or with access_denied.
func (s *Server) handleDecideOAuthAuthorization(w http.ResponseWriter, r *http.Request) {
	p, ok := signedInPerson(w, r)
	if !ok || s.oauthUnavailable(w, r) {
		return
	}
	var body struct {
		oauth.Request
		Approve bool `json:"approve"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		Error(w, r, errs.New(errs.ValidInvalid, "The request body could not be read."))
		return
	}

	if !body.Approve {
		to, err := s.OAuth.Deny(r.Context(), body.Request, s.oauthResources(r), s.issuer(r))
		if err != nil {
			Error(w, r, err)
			return
		}
		JSON(w, http.StatusOK, map[string]any{"redirect_url": to})
		return
	}

	approved, err := s.OAuth.Approve(r.Context(), p.UserID, p.ID, body.Request, s.oauthResources(r), s.issuer(r))
	if err != nil {
		Error(w, r, err)
		return
	}
	s.audit(r, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: "oauth.approve", TargetKind: "user", TargetID: p.UserID,
		Detail: map[string]any{"client_id": body.ClientID, "client_name": approved.ClientName},
	})
	// The code rides in the address, worth nothing without the client's
	// verifier, but still not for a cache to keep.
	w.Header().Set(headerCacheControl, "no-store")
	JSON(w, http.StatusCreated, map[string]any{
		"redirect_url": approved.RedirectURL,
		"expires_at":   approved.ExpiresAt,
	})
}

// handleOAuthToken is the token endpoint (RFC 6749 §3.2): a client trading
// its code and verifier for a token. Public: the verifier is the proof.
// Answers in OAuth's own format, which is what a client reads.
func (s *Server) handleOAuthToken(w http.ResponseWriter, r *http.Request) {
	if s.oauthUnavailable(w, r) {
		return
	}
	req, ok := tokenRequest(r)
	if !ok {
		oauthJSON(w, http.StatusBadRequest, &oauth.ErrorResponse{Code: "invalid_request",
			Description: "The token request must be form-encoded (application/x-www-form-urlencoded)."})
		return
	}

	got, oerr := s.OAuth.Exchange(r.Context(), req)
	if oerr != nil {
		// Only a code that named an approval is recorded, against the account
		// that approved it, as a CLI sign-in's (handleExchangeCLISignIn).
		if oerr.Code == "invalid_grant" && got.UserID != "" {
			s.audit(r, audit.Event{
				PrincipalKind: audit.KindAnonymous, Action: "oauth.denied",
				TargetKind: "user", TargetID: got.UserID,
				Detail: map[string]any{"client_id": got.ClientID, "approved_by": got.ApprovedBy},
			})
		}
		oauthFailed(w, oerr)
		return
	}

	s.audit(r, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: got.UserID, OnBehalfOf: got.UserID,
		Action: "token.create", TargetKind: "token", TargetID: got.Issued.Token.ID,
		Detail: map[string]any{
			"name": got.ClientName, "via": "oauth", "client_id": got.ClientID, "approved_by": got.ApprovedBy,
		},
	})

	t := oauth.TokenFor(got, s.clock().Now())
	body := map[string]any{"access_token": t.AccessToken.Reveal(), "token_type": t.TokenType}
	if t.ExpiresIn > 0 {
		body["expires_in"] = t.ExpiresIn
	}
	oauthJSON(w, http.StatusOK, body)
}

// tokenRequest reads a token request: form-encoded, as RFC 6749 §4.1.3 says.
func tokenRequest(r *http.Request) (oauth.TokenRequest, bool) {
	mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "application/x-www-form-urlencoded" {
		return oauth.TokenRequest{}, false
	}
	if err := r.ParseForm(); err != nil {
		return oauth.TokenRequest{}, false
	}
	f := r.PostForm
	return oauth.TokenRequest{
		GrantType: f.Get("grant_type"), Code: f.Get("code"), RedirectURI: f.Get("redirect_uri"),
		ClientID: f.Get("client_id"), CodeVerifier: f.Get("code_verifier"),
	}, true
}

// oauthFailed answers an OAuth error: 400, or 500 when Pando itself failed.
func oauthFailed(w http.ResponseWriter, e *oauth.ErrorResponse) {
	if e.Status == http.StatusInternalServerError {
		oauthJSON(w, http.StatusInternalServerError, e)
		return
	}
	oauthJSON(w, http.StatusBadRequest, e)
}

// oauthJSON answers in OAuth's format, never cached (RFC 6749 §5.1).
func oauthJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set(headerCacheControl, "no-store")
	w.Header().Set("Pragma", "no-cache")
	JSON(w, status, body)
}
