package oauth_test

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clisignin"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/oauth"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

type fakeClients struct {
	rows map[string]state.OAuthClient
	err  error
}

func (f *fakeClients) Create(_ context.Context, c state.OAuthClient) error {
	if f.err != nil {
		return f.err
	}
	f.rows[c.ID] = c
	return nil
}

func (f *fakeClients) ByID(_ context.Context, id string) (state.OAuthClient, bool, error) {
	if f.err != nil {
		return state.OAuthClient{}, false, f.err
	}
	c, ok := f.rows[id]
	return c, ok, nil
}

type fakeCodes struct {
	rows map[string]state.OAuthCode
	err  error
}

func (f *fakeCodes) Create(_ context.Context, c state.OAuthCode) error {
	if f.err != nil {
		return f.err
	}
	f.rows[c.CodeHash] = c
	return nil
}

func (f *fakeCodes) Take(_ context.Context, hash string) (state.OAuthCode, bool, error) {
	if f.err != nil {
		return state.OAuthCode{}, false, f.err
	}
	c, ok := f.rows[hash]
	delete(f.rows, hash)
	return c, ok, nil
}

type fakeUsers map[string]state.User

func (f fakeUsers) ByID(_ context.Context, id string) (state.User, bool, error) {
	if id == "usr_broken" {
		return state.User{}, false, errors.New("database down")
	}
	u, ok := f[id]
	return u, ok, nil
}

type minted struct {
	kind, name, owner, createdBy string
	expires                      *time.Time
}

type fakeTokens struct {
	made []minted
	err  error
}

func (f *fakeTokens) Create(_ context.Context, kind, name, owner, createdBy string, expires *time.Time) (state.Issued, error) {
	if f.err != nil {
		return state.Issued{}, f.err
	}
	f.made = append(f.made, minted{kind, name, owner, createdBy, expires})
	return state.Issued{Token: state.Token{ID: "tok_1", Name: name, ExpiresAt: expires}, Secret: secret.New("s3cret")}, nil
}

type fakePolicy struct {
	doc policy.Document
	err error
}

func (f fakePolicy) Load(context.Context) (policy.Document, error) { return f.doc, f.err }

const (
	verifier = "a-verifier-that-is-at-least-forty-three-characters-long"
	issuer   = "https://pando.example.com"
	redirect = "https://claude.ai/api/mcp/auth_callback"
)

var resources = []string{issuer + "/api/v1/mcp", issuer}

type rig struct {
	svc     *oauth.Service
	clients *fakeClients
	codes   *fakeCodes
	tokens  *fakeTokens
	clock   *clock.Fake
	policy  *fakePolicy
}

func newRig() *rig {
	r := &rig{
		clients: &fakeClients{rows: map[string]state.OAuthClient{}},
		codes:   &fakeCodes{rows: map[string]state.OAuthCode{}},
		tokens:  &fakeTokens{},
		clock:   clock.NewFake(time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)),
		policy:  &fakePolicy{},
	}
	r.svc = &oauth.Service{
		Clients: r.clients, Codes: r.codes, Tokens: r.tokens, Policy: r.policy, Clock: r.clock,
		Users: fakeUsers{
			"usr_ada":  {ID: "usr_ada", Status: "active"},
			"usr_new":  {ID: "usr_new", Status: "active", MustChangePassword: true},
			"usr_gone": {ID: "usr_gone", Status: "suspended"},
		},
	}
	return r
}

func (r *rig) register(t *testing.T, uris ...string) string {
	t.Helper()
	got, err := r.svc.Register(context.Background(), oauth.Registration{ClientName: "Claude", RedirectURIs: uris})
	require.Nil(t, err)
	return got.ClientID
}

func request(clientID string) oauth.Request {
	return oauth.Request{
		ClientID: clientID, RedirectURI: redirect, ResponseType: "code",
		CodeChallenge: clisignin.Challenge(verifier), CodeChallengeMethod: "S256",
		State: "xyz", Resource: issuer + "/api/v1/mcp",
	}
}

func codeFrom(t *testing.T, to string) string {
	t.Helper()
	u, err := url.Parse(to)
	require.NoError(t, err)
	return u.Query().Get("code")
}

func exchange(clientID, code string) oauth.TokenRequest {
	return oauth.TokenRequest{
		GrantType: "authorization_code", Code: code, RedirectURI: redirect, ClientID: clientID, CodeVerifier: verifier,
	}
}

// TestR413_AnMCPClientSignsInWithOAuth asserts R-413: a client registers
// itself, a signed-in person's approval sends the browser back to the
// client's own address with a code, and the code and its verifier are traded
// once for a delegated token named for the client, acting as that person.
func TestR413_AnMCPClientSignsInWithOAuth(t *testing.T) {
	r := newRig()
	ctx := context.Background()

	reg, oerr := r.svc.Register(ctx, oauth.Registration{
		ClientName: "Claude", RedirectURIs: []string{redirect},
		GrantTypes: []string{"authorization_code", "refresh_token"}, TokenEndpointAuthMethod: "client_secret_basic",
	})
	require.Nil(t, oerr)
	require.True(t, strings.HasPrefix(reg.ClientID, "oac_"), reg.ClientID)
	require.Equal(t, "none", reg.TokenEndpointAuthMethod, "every client is public")
	require.Equal(t, []string{"authorization_code"}, reg.GrantTypes)

	consent, err := r.svc.Check(ctx, request(reg.ClientID), resources)
	require.NoError(t, err)
	require.Equal(t, "Claude", consent.ClientName)
	require.Equal(t, "claude.ai", consent.ReturnsTo)

	approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(reg.ClientID), resources, issuer)
	require.NoError(t, err)
	to, err := url.Parse(approved.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "claude.ai", to.Host)
	require.Equal(t, "/api/mcp/auth_callback", to.Path)
	require.Equal(t, "xyz", to.Query().Get("state"))
	require.Equal(t, issuer, to.Query().Get("iss"))
	code := to.Query().Get("code")
	require.NotEmpty(t, code)

	got, oerr := r.svc.Exchange(ctx, exchange(reg.ClientID, code))
	require.Nil(t, oerr)
	require.Equal(t, "usr_ada", got.UserID)
	require.Equal(t, "Claude", got.ClientName)
	require.Equal(t, []minted{{state.TokenDelegated, "Claude", "usr_ada", "ses_1", nil}}, r.tokens.made)
	tok := oauth.TokenFor(got, r.clock.Now())
	require.Equal(t, "Bearer", tok.TokenType)
	require.Equal(t, "s3cret", tok.AccessToken.Reveal())
	require.Zero(t, tok.ExpiresIn, "no lifetime in policy, no expiry")

	_, oerr = r.svc.Exchange(ctx, exchange(reg.ClientID, code))
	require.NotNil(t, oerr)
	require.Equal(t, "invalid_grant", oerr.Code, "a code works once")
}

// TestR413_TheTokenLivesAsLongAsPolicyLets asserts R-413 with R-061: the
// token's expiry is host policy's, and the answer says when.
func TestR413_TheTokenLivesAsLongAsPolicyLets(t *testing.T) {
	r := newRig()
	r.policy.doc = policy.Document{MaxTokenLifetimeDays: 30}
	id := r.register(t, redirect)
	approved, err := r.svc.Approve(context.Background(), "usr_ada", "ses_1", request(id), resources, issuer)
	require.NoError(t, err)

	got, oerr := r.svc.Exchange(context.Background(), exchange(id, codeFrom(t, approved.RedirectURL)))
	require.Nil(t, oerr)
	require.NotNil(t, r.tokens.made[0].expires)
	require.Equal(t, int64(30*24*3600), oauth.TokenFor(got, r.clock.Now()).ExpiresIn)
}

func TestRegistrationRefusesWhatCannotBeARedirect(t *testing.T) {
	r := newRig()
	for _, uri := range []string{
		"http://claude.ai/callback",
		"javascript:alert(1)",
		"data:text/html,hi",
		"file:///etc/passwd",
		"https://claude.ai/cb#frag",
		"https://user:pw@claude.ai/cb",
		"https:///no-host",
		"/relative",
		"not a url at all%",
		"https://claude.ai/" + strings.Repeat("a", 2001),
	} {
		_, oerr := r.svc.Register(context.Background(), oauth.Registration{RedirectURIs: []string{uri}})
		require.NotNil(t, oerr, uri)
		require.Equal(t, "invalid_redirect_uri", oerr.Code, uri)
		require.Equal(t, http.StatusBadRequest, oerr.Status)
	}

	_, oerr := r.svc.Register(context.Background(), oauth.Registration{})
	require.Equal(t, "invalid_redirect_uri", oerr.Code)
	_, oerr = r.svc.Register(context.Background(), oauth.Registration{RedirectURIs: make([]string, 11)})
	require.Equal(t, "invalid_redirect_uri", oerr.Code)
}

func TestRegistrationAcceptsLoopbackAndNativeApps(t *testing.T) {
	r := newRig()
	for _, uri := range []string{
		"http://127.0.0.1:33418/callback",
		"http://localhost:6274/oauth/callback",
		"http://[::1]:8000/cb",
		"cursor://anysphere.cursor-mcp/oauth/callback",
		"com.example.app:/oauth",
	} {
		_, oerr := r.svc.Register(context.Background(), oauth.Registration{ClientName: "x", RedirectURIs: []string{uri}})
		require.Nil(t, oerr, uri)
	}
}

func TestRegistrationChecksItsMetadata(t *testing.T) {
	r := newRig()
	ctx := context.Background()

	_, oerr := r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}, GrantTypes: []string{"client_credentials"}})
	require.Equal(t, "invalid_client_metadata", oerr.Code)
	_, oerr = r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}, ResponseTypes: []string{"token"}})
	require.Equal(t, "invalid_client_metadata", oerr.Code)
	_, oerr = r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}, ClientName: strings.Repeat("n", 101)})
	require.Equal(t, "invalid_client_metadata", oerr.Code)
	_, oerr = r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}, ClientName: "bad\x00name"})
	require.Equal(t, "invalid_client_metadata", oerr.Code)

	// No name is named for where it returns to.
	got, oerr := r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{"cursor://anysphere/cb"}})
	require.Nil(t, oerr)
	require.Equal(t, "cursor:", got.ClientName)
	got, oerr = r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}})
	require.Nil(t, oerr)
	require.Equal(t, "claude.ai", got.ClientName)

	r.clients.err = errors.New("database down")
	_, oerr = r.svc.Register(ctx, oauth.Registration{RedirectURIs: []string{redirect}})
	require.Equal(t, "server_error", oerr.Code)
	require.Equal(t, http.StatusInternalServerError, oerr.Status)
	require.Contains(t, oerr.Error(), "server_error")
}

// TestR413_AnApprovalGoesOnlyWhereTheClientRegistered asserts R-413: the
// browser is sent only to an address the client registered, so neither
// Approve nor Cancel can be made to send it anywhere else.
func TestR413_AnApprovalGoesOnlyWhereTheClientRegistered(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	id := r.register(t, redirect)

	elsewhere := request(id)
	elsewhere.RedirectURI = "https://attacker.example/cb"
	_, err := r.svc.Approve(ctx, "usr_ada", "ses_1", elsewhere, resources, issuer)
	require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
	_, err = r.svc.Deny(ctx, elsewhere, resources, issuer)
	require.Error(t, err)
	require.Empty(t, r.codes.rows)

	denied, err := r.svc.Deny(ctx, request(id), resources, issuer)
	require.NoError(t, err)
	to, err := url.Parse(denied)
	require.NoError(t, err)
	require.Equal(t, "claude.ai", to.Host)
	require.Equal(t, "access_denied", to.Query().Get("error"))
	require.Empty(t, to.Query().Get("code"))
}

func TestCheckRefusesAMalformedRequest(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	id := r.register(t, redirect, "https://claude.ai/other")

	cases := map[string]func(*oauth.Request){
		"no client":        func(q *oauth.Request) { q.ClientID = "" },
		"unknown client":   func(q *oauth.Request) { q.ClientID = "oac_nobody" },
		"ambiguous":        func(q *oauth.Request) { q.RedirectURI = "" },
		"response type":    func(q *oauth.Request) { q.ResponseType = "token" },
		"plain challenge":  func(q *oauth.Request) { q.CodeChallengeMethod = "plain" },
		"short challenge":  func(q *oauth.Request) { q.CodeChallenge = "abc" },
		"long state":       func(q *oauth.Request) { q.State = strings.Repeat("s", 1001) },
		"another resource": func(q *oauth.Request) { q.Resource = "https://elsewhere.example/mcp" },
	}
	for name, mutate := range cases {
		q := request(id)
		mutate(&q)
		_, err := r.svc.Check(ctx, q, resources)
		require.Error(t, err, name)
		require.Equal(t, errs.ValidInvalid, errs.As(err).Code, name)
	}

	r.clients.err = errors.New("database down")
	_, err := r.svc.Check(ctx, request(id), resources)
	require.Error(t, err)
}

// A client with one address may leave redirect_uri out; a loopback address
// takes any port (RFC 8252 §7.3), and keeps the query it registered.
func TestRedirectResolution(t *testing.T) {
	r := newRig()
	ctx := context.Background()

	one := r.register(t, redirect)
	q := request(one)
	q.RedirectURI = ""
	consent, err := r.svc.Check(ctx, q, resources)
	require.NoError(t, err)
	require.Equal(t, "claude.ai", consent.ReturnsTo)

	local := r.register(t, "http://127.0.0.1/callback?app=1")
	q = request(local)
	q.RedirectURI = "http://127.0.0.1:51234/callback?app=1"
	approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", q, resources, issuer)
	require.NoError(t, err)
	to, err := url.Parse(approved.RedirectURL)
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:51234", to.Host)
	require.Equal(t, "1", to.Query().Get("app"))

	q.RedirectURI = "http://127.0.0.1:51234/elsewhere"
	_, err = r.svc.Check(ctx, q, resources)
	require.Error(t, err)

	got, oerr := r.svc.Exchange(ctx, oauth.TokenRequest{
		GrantType: "authorization_code", Code: codeFrom(t, approved.RedirectURL), ClientID: local, CodeVerifier: verifier,
	})
	require.Nil(t, oerr, "redirect_uri may be left out of the exchange")
	require.Equal(t, local, got.ClientID)
}

func TestApproveRefusesAnAccountThatCannotSignIn(t *testing.T) {
	r := newRig()
	ctx := context.Background()
	id := r.register(t, redirect)

	_, err := r.svc.Approve(ctx, "usr_new", "ses_1", request(id), resources, issuer)
	require.Equal(t, errs.PermDenied, errs.As(err).Code, "R-046 comes first")
	_, err = r.svc.Approve(ctx, "usr_gone", "ses_1", request(id), resources, issuer)
	require.Equal(t, errs.AuthInvalid, errs.As(err).Code)
	_, err = r.svc.Approve(ctx, "usr_broken", "ses_1", request(id), resources, issuer)
	require.Error(t, err)

	r.codes.err = errors.New("database down")
	_, err = r.svc.Approve(ctx, "usr_ada", "ses_1", request(id), resources, issuer)
	require.Error(t, err)
	require.Empty(t, r.tokens.made)
}

// TestR413_ACodeIsWorthNothingWithoutItsVerifier asserts R-413: every way a
// code can be wrong is the same answer, and a refusal that spent a real
// approval says whose it was so it can be audited.
func TestR413_ACodeIsWorthNothingWithoutItsVerifier(t *testing.T) {
	ctx := context.Background()

	cases := map[string]func(*rig, *oauth.TokenRequest){
		"wrong verifier":  func(_ *rig, q *oauth.TokenRequest) { q.CodeVerifier = verifier + "-not-the-one" },
		"another client":  func(_ *rig, q *oauth.TokenRequest) { q.ClientID = "oac_other" },
		"another address": func(_ *rig, q *oauth.TokenRequest) { q.RedirectURI = "https://claude.ai/elsewhere" },
		"expired":         func(r *rig, _ *oauth.TokenRequest) { r.clock.Advance(oauth.CodeLifetime) },
	}
	for name, mutate := range cases {
		r := newRig()
		id := r.register(t, redirect)
		approved, err := r.svc.Approve(ctx, "usr_ada", "ses_1", request(id), resources, issuer)
		require.NoError(t, err)
		q := exchange(id, codeFrom(t, approved.RedirectURL))
		mutate(r, &q)

		got, oerr := r.svc.Exchange(ctx, q)
		require.NotNil(t, oerr, name)
		require.Equal(t, "invalid_grant", oerr.Code, name)
		require.Equal(t, "usr_ada", got.UserID, name)
		require.Empty(t, r.codes.rows, "%s: the attempt spent the code", name)
		require.Empty(t, r.tokens.made, name)
	}
}

func TestExchangeRefusals(t *testing.T) {
	ctx := context.Background()
	r := newRig()
	id := r.register(t, redirect)

	_, oerr := r.svc.Exchange(ctx, oauth.TokenRequest{GrantType: "refresh_token"})
	require.Equal(t, "unsupported_grant_type", oerr.Code)

	got, oerr := r.svc.Exchange(ctx, exchange(id, "not-a-code"))
	require.Equal(t, "invalid_grant", oerr.Code)
	require.Empty(t, got.UserID, "a guess names nobody, so nothing is audited")

	_, oerr = r.svc.Exchange(ctx, oauth.TokenRequest{GrantType: "authorization_code", Code: "c", ClientID: id, CodeVerifier: "short"})
	require.Equal(t, "invalid_grant", oerr.Code)

	approve := func(user string) string {
		require.NoError(t, r.codes.Create(ctx, state.OAuthCode{
			CodeHash: clisignin.Digest("code-" + user), ClientID: id, UserID: user, ApprovedBy: "ses_1",
			CodeChallenge: clisignin.Challenge(verifier), RedirectURI: redirect, ExpiresAt: r.clock.Now().Add(time.Minute),
		}))
		return "code-" + user
	}

	// A client names itself, and a request that does not spends nothing.
	unnamed := approve("usr_ada")
	_, oerr = r.svc.Exchange(ctx, oauth.TokenRequest{GrantType: "authorization_code", Code: unnamed, CodeVerifier: verifier})
	require.Equal(t, "invalid_request", oerr.Code)
	require.Len(t, r.codes.rows, 1, "the code was not spent")
	delete(r.codes.rows, clisignin.Digest(unnamed))

	// A store that cannot be read is Pando's failure, with no refusal to audit.
	got, oerr = r.svc.Exchange(ctx, exchange(id, approve("usr_broken")))
	require.Equal(t, "server_error", oerr.Code)
	require.Empty(t, got.UserID)
	r.clients.err = errors.New("database down")
	got, oerr = r.svc.Exchange(ctx, exchange(id, approve("usr_ada")))
	require.Equal(t, "server_error", oerr.Code)
	require.Empty(t, got.UserID)
	r.clients.err = nil

	// Suspended since approving.
	_, oerr = r.svc.Exchange(ctx, exchange(id, approve("usr_gone")))
	require.Equal(t, "invalid_grant", oerr.Code)

	// The client is gone.
	code := approve("usr_ada")
	delete(r.clients.rows, id)
	_, oerr = r.svc.Exchange(ctx, exchange(id, code))
	require.Equal(t, "invalid_grant", oerr.Code)
	id = r.register(t, redirect)

	r.policy.err = errors.New("database down")
	_, oerr = r.svc.Exchange(ctx, exchange(id, approve("usr_ada")))
	require.Equal(t, "server_error", oerr.Code)
	r.policy.err = nil

	r.tokens.err = errors.New("database down")
	_, oerr = r.svc.Exchange(ctx, exchange(id, approve("usr_ada")))
	require.Equal(t, "server_error", oerr.Code)

	r.codes.err = errors.New("database down")
	_, oerr = r.svc.Exchange(ctx, exchange(id, "anything"))
	require.Equal(t, "server_error", oerr.Code)
}
