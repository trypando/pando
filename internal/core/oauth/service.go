// Package oauth signs MCP clients in (R-413, design 06 §3.6).
//
// Pando serves MCP itself, and an MCP client that has no token gets one the
// way the MCP specification's authorization section says: OAuth 2.1, with
// Pando as the authorization server and the console as its consent page.
//
//  1. The client registers itself (RFC 7591): a name and the addresses it
//     may be sent back to. No secret: every client is public.
//  2. It sends the person's browser to the console with a PKCE challenge
//     (RFC 7636, S256 only). The person, signed in however their account signs
//     in, sees the client's name and where it will be sent back to, and
//     approves. The browser goes back to the client with a one-time code.
//  3. The client trades the code and the verifier for a delegated token
//     (R-058), named for the client, under host policy's lifetime (R-061).
//
// It is R-406's flow (package clisignin) with the client's own redirect
// address in place of the CLI's loopback port, and the same binding: a code
// is worth nothing without the verifier, which never left the client.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/id"
	"github.com/trypando/pando/internal/secret"
)

// CodeLifetime is how long an approval waits for the client, as a CLI
// sign-in's (clisignin.CodeLifetime).
const CodeLifetime = clisignin.CodeLifetime

const (
	maxRedirectURIs = 10
	maxNameLen      = 100
	maxURILen       = 2000
	maxStateLen     = 1000
)

// ClientStore keeps registered clients (state.OAuthClients).
type ClientStore interface {
	Create(ctx context.Context, c state.OAuthClient) error
	ByID(ctx context.Context, id string) (state.OAuthClient, bool, error)
}

// CodeStore keeps approvals (state.OAuthCodes).
type CodeStore interface {
	Create(ctx context.Context, c state.OAuthCode) error
	Take(ctx context.Context, codeHash string) (state.OAuthCode, bool, error)
}

// AccountReader reads accounts (state.Users).
type AccountReader interface {
	ByID(ctx context.Context, userID string) (state.User, bool, error)
}

// TokenMinter mints tokens (state.Tokens).
type TokenMinter interface {
	Create(ctx context.Context, kind, name, ownerUserID, createdBy string, expiresAt *time.Time) (state.Issued, error)
}

// Service registers clients, approves their sign-ins and trades them for
// tokens. Like clisignin, it decides nothing about who is asking: the caller
// has established that a signed-in person, not a token, approved, and audits.
type Service struct {
	Clients ClientStore
	Codes   CodeStore
	Users   AccountReader
	Tokens  TokenMinter
	Policy  interface {
		Load(ctx context.Context) (policy.Document, error)
	}
	Clock clock.Clock
}

// ErrorResponse is an OAuth error response (RFC 6749 §5.2, RFC 7591 §3.2.2): what
// registration and the token endpoint answer, since a client reads those, not
// Pando's error envelope.
type ErrorResponse struct {
	Status      int    `json:"-"`
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *ErrorResponse) Error() string { return e.Code + ": " + e.Description }

func oauthError(status int, code, description string) *ErrorResponse {
	return &ErrorResponse{Status: status, Code: code, Description: description}
}

// --- registration ----------------------------------------------------------

// Registration is a client's registration request (RFC 7591 §2). Fields Pando
// does not use are ignored; the answer says what was registered.
type Registration struct {
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types,omitempty"`
	ResponseTypes           []string `json:"response_types,omitempty"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method,omitempty"`
}

// Registered is the answer to a registration (RFC 7591 §3.2.1).
type Registered struct {
	ClientID                string   `json:"client_id"`
	ClientIDIssuedAt        int64    `json:"client_id_issued_at"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// Register records a client. Open to anyone (R-413): what a client can do is
// ask a person to approve it, and the person sees its name and where it will
// be sent back to. A client nobody approves is removed after a day.
func (s *Service) Register(ctx context.Context, reg Registration) (Registered, *ErrorResponse) {
	if len(reg.RedirectURIs) == 0 || len(reg.RedirectURIs) > maxRedirectURIs {
		return Registered{}, oauthError(http.StatusBadRequest, "invalid_redirect_uri",
			fmt.Sprintf("A client registers 1 to %d redirect_uris.", maxRedirectURIs))
	}
	for _, u := range reg.RedirectURIs {
		if err := checkRedirectURI(u); err != "" {
			return Registered{}, oauthError(http.StatusBadRequest, "invalid_redirect_uri", err)
		}
	}
	if len(reg.GrantTypes) > 0 && !slices.Contains(reg.GrantTypes, "authorization_code") {
		return Registered{}, oauthError(http.StatusBadRequest, "invalid_client_metadata",
			"Pando signs clients in with the authorization_code grant only.")
	}
	if len(reg.ResponseTypes) > 0 && !slices.Contains(reg.ResponseTypes, "code") {
		return Registered{}, oauthError(http.StatusBadRequest, "invalid_client_metadata",
			"Pando answers response_type code only.")
	}

	name := strings.TrimSpace(reg.ClientName)
	if name == "" {
		name = returnsTo(reg.RedirectURIs[0])
	}
	if len(name) > maxNameLen || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return Registered{}, oauthError(http.StatusBadRequest, "invalid_client_metadata",
			fmt.Sprintf("client_name must be at most %d characters with no control characters.", maxNameLen))
	}

	c := state.OAuthClient{ID: id.New(id.OAuthClient), Name: name, RedirectURIs: reg.RedirectURIs}
	if err := s.Clients.Create(ctx, c); err != nil {
		return Registered{}, oauthError(http.StatusInternalServerError, "server_error", "Could not record the client.")
	}
	// Every client is public: whatever token endpoint authentication it asked
	// for, it gets none, and the answer says so (RFC 7591 §3.2.1 lets the
	// server replace a value).
	return Registered{
		ClientID: c.ID, ClientIDIssuedAt: s.Clock.Now().Unix(), ClientName: name,
		RedirectURIs: reg.RedirectURIs, GrantTypes: []string{"authorization_code"},
		ResponseTypes: []string{"code"}, TokenEndpointAuthMethod: "none",
	}, nil
}

// schemeShape is a URI scheme (RFC 3986 §3.1).
var schemeShape = regexp.MustCompile(`^[a-z][a-z0-9+.-]*$`)

// refusedSchemes run in or read from the browser rather than handing a code
// to a client, or are plain-text network schemes.
var refusedSchemes = map[string]bool{
	"javascript": true, "data": true, "file": true, "blob": true, "about": true,
	"vbscript": true, "filesystem": true, "ftp": true, "ws": true, "wss": true,
}

// checkRedirectURI says what is wrong with a redirect address, or "".
//
// HTTPS; HTTP only on the loopback address, for a client on the person's own
// machine (RFC 8252 §7.3); or a private-use scheme a native app has claimed
// (§7.1). Never a fragment (RFC 6749 §3.1.2).
func checkRedirectURI(raw string) string {
	if len(raw) > maxURILen {
		return fmt.Sprintf("A redirect_uri is at most %d characters.", maxURILen)
	}
	u, err := url.Parse(raw)
	if err != nil || !u.IsAbs() {
		return fmt.Sprintf("%q is not an absolute URL.", raw)
	}
	if u.Fragment != "" || strings.Contains(raw, "#") {
		return fmt.Sprintf("%q has a fragment, which a redirect_uri may not.", raw)
	}
	if u.User != nil {
		return fmt.Sprintf("%q has a user name or password in it, which a redirect_uri may not.", raw)
	}
	switch scheme := strings.ToLower(u.Scheme); {
	case scheme == "https":
		if u.Hostname() == "" {
			return fmt.Sprintf("%q has no host.", raw)
		}
	case scheme == "http":
		if !loopback(u.Hostname()) {
			return fmt.Sprintf("%q uses http on a host other than the loopback address. Use https, or http://127.0.0.1 for a client on this machine.", raw)
		}
	case refusedSchemes[scheme] || !schemeShape.MatchString(scheme):
		return fmt.Sprintf("%q uses a scheme a redirect_uri may not.", raw)
	}
	return ""
}

func loopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// returnsTo is where a redirect address sends the browser, as a person
// approving it is shown: the host, or a native app's scheme.
func returnsTo(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	switch strings.ToLower(u.Scheme) {
	case "https", "http":
		return u.Host
	default:
		return u.Scheme + ":"
	}
}

// --- authorization ---------------------------------------------------------

// Request is an authorization request (RFC 6749 §4.1.1, RFC 7636 §4.3, RFC
// 8707 §2), as the console's approval page sends it on from its address.
type Request struct {
	ClientID            string `json:"client_id"`
	RedirectURI         string `json:"redirect_uri"`
	ResponseType        string `json:"response_type"`
	CodeChallenge       string `json:"code_challenge"`
	CodeChallengeMethod string `json:"code_challenge_method"`
	State               string `json:"state"`
	Resource            string `json:"resource"`
	Scope               string `json:"scope"`
}

// Consent is what the person approving is shown.
type Consent struct {
	ClientID   string `json:"client_id"`
	ClientName string `json:"client_name"`

	// ReturnsTo is the host, or native app scheme, the browser goes back to.
	ReturnsTo string `json:"returns_to"`

	// redirectURI is the registered address the request resolved to.
	redirectURI string
}

func invalid(message string) error {
	return errs.New(errs.ValidInvalid, message).
		WithRemedy("Start connecting again from the MCP client.")
}

// Check validates an authorization request against its client and says what
// to show the person. resources are the values RFC 8707's resource parameter
// may name: Pando's MCP endpoint and Pando itself.
//
// Every refusal is shown on the approval page rather than sent back to the
// client [P]: one with an unknown client or redirect address must be (RFC 6749
// §4.1.2.1), and drawing the rest the same way keeps one path.
func (s *Service) Check(ctx context.Context, req Request, resources []string) (Consent, error) {
	if req.ClientID == "" {
		return Consent{}, invalid("This sign-in names no client_id.")
	}
	client, found, err := s.Clients.ByID(ctx, req.ClientID)
	if err != nil {
		return Consent{}, err
	}
	if !found {
		return Consent{}, errs.New(errs.ValidInvalid,
			"This sign-in names an MCP client Pando does not know. A client nobody approves is forgotten after a day.").
			WithRemedy("Remove Pando from the MCP client and add it again.")
	}
	redirect, ok := resolveRedirect(client, req.RedirectURI)
	if !ok {
		return Consent{}, invalid("This sign-in's redirect_uri is not one its client registered.")
	}
	if req.ResponseType != "code" {
		return Consent{}, invalid("This sign-in asks for a response_type other than code, the only one Pando answers.")
	}
	if req.CodeChallengeMethod != "S256" {
		return Consent{}, invalid("This sign-in asks for a code challenge method other than S256, the only one Pando accepts.")
	}
	if !challengeShape.MatchString(req.CodeChallenge) {
		return Consent{}, invalid("This sign-in's code challenge is not an unpadded base64url SHA-256 digest.")
	}
	if len(req.State) > maxStateLen {
		return Consent{}, invalid(fmt.Sprintf("This sign-in's state is longer than %d characters.", maxStateLen))
	}
	if req.Resource != "" && !slices.Contains(resources, strings.TrimSuffix(req.Resource, "/")) {
		return Consent{}, invalid(fmt.Sprintf("This sign-in asks for a token for %s, which is not this Pando installation.", req.Resource))
	}
	return Consent{
		ClientID: client.ID, ClientName: client.Name, ReturnsTo: returnsTo(redirect), redirectURI: redirect,
	}, nil
}

// challengeShape is an unpadded base64url SHA-256: 43 characters.
var challengeShape = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

// resolveRedirect is the registered address a request's redirect_uri names:
// exactly one of them, or any port of a loopback one (RFC 8252 §7.3), since a
// client on the person's machine listens wherever it can. Empty names the one
// address of a client that registered only one (RFC 6749 §3.1.2.3).
func resolveRedirect(c state.OAuthClient, asked string) (string, bool) {
	if asked == "" {
		if len(c.RedirectURIs) == 1 {
			return c.RedirectURIs[0], true
		}
		return "", false
	}
	for _, registered := range c.RedirectURIs {
		if asked == registered || sameLoopbackIgnoringPort(registered, asked) {
			return asked, true
		}
	}
	return "", false
}

func sameLoopbackIgnoringPort(registered, asked string) bool {
	r, err1 := url.Parse(registered)
	a, err2 := url.Parse(asked)
	if err1 != nil || err2 != nil || r.Scheme != "http" || a.Scheme != "http" || !loopback(r.Hostname()) {
		return false
	}
	return r.Hostname() == a.Hostname() && r.Path == a.Path && r.RawQuery == a.RawQuery
}

// Approved is where the browser goes with the code.
type Approved struct {
	RedirectURL string
	ClientName  string
	ExpiresAt   time.Time
}

// Approve records that userID, signed in as approvedBy, lets the client
// sign in as them, and says where to send the browser. issuer goes on the
// redirect as iss (RFC 9207), so a client can tell which server answered.
func (s *Service) Approve(ctx context.Context, userID, approvedBy string, req Request, resources []string, issuer string) (Approved, error) {
	consent, err := s.Check(ctx, req, resources)
	if err != nil {
		return Approved{}, err
	}
	if err := s.usable(ctx, userID); err != nil {
		return Approved{}, err
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Approved{}, errs.Wrap(errs.Internal, "Could not make a sign-in code.", err)
	}
	code := base64.RawURLEncoding.EncodeToString(raw)
	expires := s.Clock.Now().Add(CodeLifetime)
	if err := s.Codes.Create(ctx, state.OAuthCode{
		CodeHash: clisignin.Digest(code), ClientID: consent.ClientID, UserID: userID, ApprovedBy: approvedBy,
		CodeChallenge: req.CodeChallenge, RedirectURI: consent.redirectURI, ExpiresAt: expires,
	}); err != nil {
		return Approved{}, err
	}
	return Approved{
		RedirectURL: withQuery(consent.redirectURI, url.Values{"code": {code}, "state": {req.State}, "iss": {issuer}}),
		ClientName:  consent.ClientName, ExpiresAt: expires,
	}, nil
}

// Deny says where to send the browser when the person cancels: the client's
// address with access_denied (RFC 6749 §4.1.2.1), once the request is shown to
// be a real client's, so Cancel cannot be used to send a browser anywhere else.
func (s *Service) Deny(ctx context.Context, req Request, resources []string, issuer string) (string, error) {
	consent, err := s.Check(ctx, req, resources)
	if err != nil {
		return "", err
	}
	return withQuery(consent.redirectURI, url.Values{
		"error": {"access_denied"}, "state": {req.State}, "iss": {issuer},
	}), nil
}

// withQuery adds values to an address, keeping any query it already has
// (RFC 6749 §3.1.2). An empty state is left out rather than sent empty.
func withQuery(raw string, add url.Values) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	q := u.Query()
	for k, vs := range add {
		if len(vs) == 1 && vs[0] == "" {
			continue
		}
		q[k] = vs
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// --- the token endpoint ----------------------------------------------------

// TokenRequest is a token request (RFC 6749 §4.1.3, RFC 7636 §4.5).
type TokenRequest struct {
	GrantType    string
	Code         string
	RedirectURI  string
	ClientID     string
	CodeVerifier string
}

// Exchanged is a token made for an approved client.
type Exchanged struct {
	Issued state.Issued

	// UserID is the account the token acts as (R-058).
	UserID     string
	ApprovedBy string
	ClientID   string
	ClientName string
}

// verifierShape is RFC 7636 §4.1's: 43 to 128 unreserved characters.
var verifierShape = regexp.MustCompile(`^[A-Za-z0-9._~-]{43,128}$`)

func invalidGrant() *ErrorResponse {
	return oauthError(http.StatusBadRequest, "invalid_grant",
		"This code is not valid. It may have expired, been used already, or been issued to another client.")
}

// Exchange trades a code and its verifier for a delegated token. The approval
// is gone whatever the outcome. When a code that named an approval is
// refused, the Exchanged returned with the error says whose it was, so the
// caller can audit that and only that, as clisignin.Exchange.
func (s *Service) Exchange(ctx context.Context, req TokenRequest) (Exchanged, *ErrorResponse) {
	if req.GrantType != "authorization_code" {
		return Exchanged{}, oauthError(http.StatusBadRequest, "unsupported_grant_type",
			"Pando issues tokens for the authorization_code grant only.")
	}
	// A public client names itself at the exchange (RFC 6749 §4.1.3), so a
	// code is bound to the client it was issued to as well as to its verifier.
	if req.ClientID == "" {
		return Exchanged{}, oauthError(http.StatusBadRequest, "invalid_request", "The token request names no client_id.")
	}
	if req.Code == "" || !verifierShape.MatchString(req.CodeVerifier) {
		return Exchanged{}, invalidGrant()
	}
	approval, found, err := s.Codes.Take(ctx, clisignin.Digest(req.Code))
	if err != nil {
		return Exchanged{}, oauthError(http.StatusInternalServerError, "server_error", "Could not read the sign-in.")
	}
	if !found {
		return Exchanged{}, invalidGrant()
	}

	refused := Exchanged{UserID: approval.UserID, ApprovedBy: approval.ApprovedBy, ClientID: approval.ClientID}
	if !s.Clock.Now().Before(approval.ExpiresAt) ||
		req.ClientID != approval.ClientID ||
		(req.RedirectURI != "" && req.RedirectURI != approval.RedirectURI) ||
		subtle.ConstantTimeCompare([]byte(clisignin.Challenge(req.CodeVerifier)), []byte(approval.CodeChallenge)) != 1 {
		return refused, invalidGrant()
	}

	// A store that cannot be read is Pando's failure, not a bad code: it is
	// answered as one and, with no refusal returned, not audited as one.
	client, found, err := s.Clients.ByID(ctx, approval.ClientID)
	if err != nil {
		return Exchanged{}, oauthError(http.StatusInternalServerError, "server_error", "Could not read the client.")
	}
	if !found {
		return refused, invalidGrant()
	}
	refused.ClientName = client.Name

	// Approved minutes ago, but the account may have been suspended since.
	user, found, err := s.Users.ByID(ctx, approval.UserID)
	if err != nil {
		return Exchanged{}, oauthError(http.StatusInternalServerError, "server_error", "Could not read the account.")
	}
	if !found || user.Status != "active" {
		return refused, invalidGrant()
	}

	doc, err := s.Policy.Load(ctx)
	if err != nil {
		return Exchanged{}, oauthError(http.StatusInternalServerError, "server_error", "Could not read host policy.")
	}
	issued, err := s.Tokens.Create(ctx, state.TokenDelegated, client.Name, user.ID, approval.ApprovedBy,
		doc.TokenExpiry(s.Clock.Now()))
	if err != nil {
		return Exchanged{}, oauthError(http.StatusInternalServerError, "server_error", "Could not make the token.")
	}
	return Exchanged{
		Issued: issued, UserID: user.ID, ApprovedBy: approval.ApprovedBy,
		ClientID: client.ID, ClientName: client.Name,
	}, nil
}

// Token is the token endpoint's answer (RFC 6749 §5.1). No refresh token: the
// token lives as long as host policy lets any token live, and a client signs
// in again after that.
type Token struct {
	AccessToken secret.Value `json:"-"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int64        `json:"expires_in,omitempty"`
}

// TokenFor is the answer for an exchanged token, at now.
func TokenFor(e Exchanged, now time.Time) Token {
	t := Token{AccessToken: e.Issued.Secret, TokenType: "Bearer"}
	if exp := e.Issued.Token.ExpiresAt; exp != nil {
		t.ExpiresIn = int64(exp.Sub(now).Seconds())
	}
	return t
}

func (s *Service) usable(ctx context.Context, userID string) error {
	user, found, err := s.Users.ByID(ctx, userID)
	if err != nil {
		return err
	}
	if !found || user.Status != "active" {
		return errs.New(errs.AuthInvalid, "This account is not active, so it cannot sign in an MCP client.").
			WithRemedy("Ask an administrator of this Pando installation to reactivate it.")
	}
	// R-046: the generated password is changed before anything else, and a
	// token made around it would outlive the change.
	if user.MustChangePassword {
		return errs.New(errs.PermDenied, "This account must change its password before it can sign in an MCP client.").
			WithRemedy("Choose a new password in the console, then connect the client again.")
	}
	return nil
}
