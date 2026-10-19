package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/identity/oidc"
	"github.com/trypando/pando/internal/errs"
)

// fakeProvider is a minimal OpenID Connect provider: discovery, JWKS, and a
// token endpoint that checks the PKCE verifier and returns an ID token with
// whatever claims the test sets.
type fakeProvider struct {
	t         *testing.T
	srv       *httptest.Server
	key       *rsa.PrivateKey
	claims    map[string]any
	nonce     string
	challenge string
	userinfo  map[string]any
	noIDToken bool
	algs      []string // discovery's id_token_signing_alg_values_supported; RS256 when nil
	hsSecret  []byte   // sign the ID token with HS256 and this key, as Authentik does without a signing key
}

func newFake(t *testing.T) *fakeProvider {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	f := &fakeProvider{t: t, key: key}
	mux := http.NewServeMux()
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": f.srv.URL, "authorization_endpoint": f.srv.URL + "/authorize",
			"token_endpoint": f.srv.URL + "/token", "jwks_uri": f.srv.URL + "/jwks",
			"userinfo_endpoint":                     f.srv.URL + "/userinfo",
			"id_token_signing_alg_values_supported": algsOr(f.algs),
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	mux.HandleFunc("/userinfo", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(f.userinfo)
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, r.ParseForm())
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != f.challenge {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant", "error_description": "PKCE verification failed"})
			return
		}
		user, pass, _ := r.BasicAuth()
		if user != "pando" || pass != "secret" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_client", "error_description": "bad secret"})
			return
		}
		claims := map[string]any{"iss": f.srv.URL, "aud": "pando", "exp": time.Now().Add(time.Hour).Unix(),
			"iat": time.Now().Unix(), "nonce": f.nonce}
		for k, v := range f.claims {
			claims[k] = v
		}
		signingKey := jose.SigningKey{Algorithm: jose.RS256, Key: key}
		if f.hsSecret != nil {
			signingKey = jose.SigningKey{Algorithm: jose.HS256, Key: f.hsSecret}
		}
		signer, err := jose.NewSigner(signingKey, (&jose.SignerOptions{}).WithHeader("kid", "k1"))
		require.NoError(t, err)
		payload, _ := json.Marshal(claims)
		jws, err := signer.Sign(payload)
		require.NoError(t, err)
		idToken, _ := jws.CompactSerialize()
		w.Header().Set("Content-Type", "application/json")
		reply := map[string]any{"access_token": "at", "token_type": "Bearer", "id_token": idToken}
		if f.noIDToken {
			delete(reply, "id_token")
		}
		_ = json.NewEncoder(w).Encode(reply)
	})
	return f
}

func algsOr(algs []string) []string {
	if algs == nil {
		return []string{"RS256"}
	}
	return algs
}

func configured(t *testing.T, f *fakeProvider, extra map[string]any) *oidc.Adapter {
	t.Helper()
	cfg := map[string]any{"issuer": f.srv.URL, "client_id": "pando", "credentials": map[string]string{"client_secret": "secret"}}
	for k, v := range extra {
		cfg[k] = v
	}
	raw, _ := json.Marshal(cfg)
	a := oidc.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// signIn runs Begin, plays the provider's part, and calls Authenticate.
func signIn(t *testing.T, f *fakeProvider, a *oidc.Adapter, mutate func(url.Values, *[]byte)) (api.Subject, error) {
	t.Helper()
	ctx := context.Background()
	endpoints := api.Endpoints{CallbackURL: "https://pando.example.com/api/v1/auth/providers/idp_x/callback"}
	r, err := a.Begin(ctx, api.BeginRequest{Endpoints: endpoints, State: "st4te"})
	require.NoError(t, err)
	u, err := url.Parse(r.URL)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, "S256", q.Get("code_challenge_method"), "PKCE is always on")
	require.Equal(t, "st4te", q.Get("state"))
	require.Equal(t, endpoints.CallbackURL, q.Get("redirect_uri"))
	require.Contains(t, q.Get("scope"), "openid")
	f.nonce, f.challenge = q.Get("nonce"), q.Get("code_challenge")
	require.NotEmpty(t, f.nonce)

	cb := url.Values{"code": {"c0de"}, "state": {"st4te"}}
	flow := r.Flow
	if mutate != nil {
		mutate(cb, &flow)
	}
	return a.Authenticate(ctx, api.Credential{Callback: cb, Flow: flow, Endpoints: endpoints})
}

func TestR043_OIDCMapsClaimsToASubject(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "00u1", "email": "dana@example.com", "email_verified": true,
		"name": "Dana Scully", "preferred_username": "dana", "groups": []any{"eng", "ops"}}
	s, err := signIn(t, f, configured(t, f, nil), nil)
	require.NoError(t, err)
	require.Equal(t, "00u1", s.ExternalID)
	require.Equal(t, "dana@example.com", s.Email)
	require.True(t, s.EmailVerified)
	require.Equal(t, "Dana Scully", s.DisplayName)
	require.Equal(t, "dana", s.Username)
	require.Equal(t, []string{"eng", "ops"}, s.Groups)
	require.Equal(t, []string{"00u1"}, s.Attributes["sub"])
}

// Entra: the stable ID is oid, and the email is not vouched for.
func TestR043_OIDCEntraMatchesOnOidAndNeverTrustsItsEmail(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "pairwise-per-app", "oid": "0a21f0f2", "email": "x@corp.com", "groups": "g1"}
	s, err := signIn(t, f, configured(t, f, map[string]any{"subject_claim": "oid"}), nil)
	require.NoError(t, err)
	require.Equal(t, "0a21f0f2", s.ExternalID)
	require.False(t, s.EmailVerified, "no email_verified claim, no linking by email")
	require.Equal(t, []string{"g1"}, s.Groups, "a single group sent as a string")
}

func TestR043_OIDCFillsMissingClaimsFromUserinfo(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "u1"}
	f.userinfo = map[string]any{"sub": "u1", "email": "u1@example.com", "groups": []any{"from-userinfo"}}
	s, err := signIn(t, f, configured(t, f, nil), nil)
	require.NoError(t, err)
	require.Equal(t, "u1@example.com", s.Email)
	require.Equal(t, []string{"from-userinfo"}, s.Groups)

	// Userinfo about someone else is ignored.
	f.userinfo = map[string]any{"sub": "someone-else", "email": "evil@example.com"}
	s, err = signIn(t, f, configured(t, f, nil), nil)
	require.NoError(t, err)
	require.Empty(t, s.Email)
}

func TestR043_OIDCRefusesWhatItDidNotAskFor(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "u1"}
	a := configured(t, f, nil)

	// A token minted for another sign-in (nonce mismatch).
	_, err := signIn(t, f, a, func(_ url.Values, flow *[]byte) {
		*flow = []byte(`{"v":"` + "x" + `","n":"other"}`)
	})
	require.Error(t, err, "a different verifier fails PKCE")

	_, err = signIn(t, f, a, func(_ url.Values, flow *[]byte) { *flow = nil })
	require.Error(t, err, "OIDC sign-in must start at Pando")
	require.Contains(t, errs.As(err).Message, "did not start at Pando")

	_, err = signIn(t, f, a, func(cb url.Values, _ *[]byte) {
		cb.Del("code")
		cb.Set("error", "access_denied")
		cb.Set("error_description", "User is not assigned to the client application.")
	})
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "not assigned to the client application")

	_, err = signIn(t, f, a, func(cb url.Values, _ *[]byte) { cb.Set("iss", "https://other.example.com") })
	require.Error(t, err, "RFC 9207: the response names a different issuer")
}

func TestR043_OIDCWithoutTheRightSecretSaysSo(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "u1"}
	raw, _ := json.Marshal(map[string]any{"issuer": f.srv.URL, "client_id": "pando",
		"credentials": map[string]string{"client_secret": "wrong"}})
	a := oidc.New()
	require.NoError(t, a.Configure(context.Background(), raw))
	_, err := signIn(t, f, a, nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "client credentials")
}

func TestR043_OIDCGoogleHostedDomain(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "g1", "hd": "other.com"}
	_, err := signIn(t, f, configured(t, f, map[string]any{"hosted_domain": "example.com"}), nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "only accepts accounts from example.com")

	f.claims = map[string]any{"sub": "g1", "hd": "example.com"}
	_, err = signIn(t, f, configured(t, f, map[string]any{"hosted_domain": "example.com"}), nil)
	require.NoError(t, err)
}

func TestOIDCConfigurationIsCheckedWhenSaved(t *testing.T) {
	for name, cfg := range map[string]string{
		"no issuer":      `{"client_id":"x"}`,
		"http issuer":    `{"issuer":"http://idp.example.com","client_id":"x"}`,
		"placeholder":    `{"issuer":"https://login.microsoftonline.com/{tenant-id}/v2.0","client_id":"x"}`,
		"no client":      `{"issuer":"https://idp.example.com"}`,
		"bad lifetime":   `{"issuer":"https://idp.example.com","client_id":"x","session_max_lifetime":"forever"}`,
		"unknown prompt": `{"issuer":"https://idp.example.com","client_id":"x","prompt":"please"}`,
	} {
		err := oidc.New().Configure(context.Background(), json.RawMessage(cfg))
		require.Error(t, err, name)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
	}
	a := oidc.New()
	require.NoError(t, a.Configure(context.Background(),
		json.RawMessage(`{"issuer":"https://idp.example.com","client_id":"x","session_max_lifetime":"1h"}`)))
	require.Equal(t, time.Hour, a.SessionPolicy().MaxLifetime)
	require.Equal(t, api.RevocationExpiryOnly, a.SessionPolicy().RevocationMode, "R-050 without SCIM")
	require.True(t, a.SupportsPush())
}

func TestOIDCPresetsNameOnlyRealFields(t *testing.T) {
	info := oidc.Info()
	keys := map[string]bool{}
	for _, f := range info.Fields {
		keys[f.Key] = true
	}
	for _, p := range info.Presets {
		for k := range p.Values {
			require.True(t, keys[k], "preset %s sets %s, which is not a field", p.ID, k)
		}
	}
}

// TestR050_WithoutPushTheSessionLifetimeIsTheRevocationWindow asserts R-050:
// an adapter that cannot learn of a revocation declares expiry-only, with the
// session lifetime it was configured with as the window core reports. Core
// upgrades it to push only when SCIM is on (TestR048_ASCIMTokenIsOnlyASCIMToken).
func TestR050_WithoutPushTheSessionLifetimeIsTheRevocationWindow(t *testing.T) {
	a := oidc.New()
	require.NoError(t, a.Configure(context.Background(),
		json.RawMessage(`{"issuer":"https://idp.example.com","client_id":"x","session_max_lifetime":"4h"}`)))
	p := a.SessionPolicy()
	require.Equal(t, api.RevocationExpiryOnly, p.RevocationMode)
	require.Equal(t, 4*time.Hour, p.MaxLifetime)
}

// A provider that is not acting as OpenID Connect, or cannot be reached, is
// named as the problem, with what to check.
func TestR043_OIDCSaysWhatIsWrongWithTheProvider(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "u1"}
	f.noIDToken = true
	_, err := signIn(t, f, configured(t, f, nil), nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "no ID token")

	down := oidc.New()
	require.NoError(t, down.Configure(context.Background(), json.RawMessage(`{"issuer":"https://127.0.0.1:1","client_id":"x"}`)))
	err = down.HealthCheck(context.Background())
	require.Error(t, err)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	_, err = down.Begin(context.Background(), api.BeginRequest{State: "s", Endpoints: api.Endpoints{CallbackURL: "https://p/cb"}})
	require.Error(t, err)
	_, err = configured(t, f, nil).Begin(context.Background(), api.BeginRequest{})
	require.Error(t, err, "a sign-in needs core's state and callback")

	md, err := configured(t, f, nil).ServiceMetadata(context.Background(), api.Endpoints{})
	require.NoError(t, err)
	require.Nil(t, md, "OpenID Connect has no metadata to give a provider")
}

// The prompt and Google's domain hint reach the provider; userinfo is not
// asked when turned off; claims arrive in the shapes providers send them.
func TestR043_OIDCSendsHintsAndReadsClaimsAsSent(t *testing.T) {
	f := newFake(t)
	f.claims = map[string]any{"sub": "u-1", "employee_id": 12345.0, "email": "a@example.com", "email_verified": "true",
		"groups": "a, b", "given_name": "Ann", "family_name": "Lee"}
	f.userinfo = map[string]any{"sub": "u-1", "name": "From Userinfo"}
	a := configured(t, f, map[string]any{"subject_claim": "employee_id", "prompt": "select_account", "hosted_domain": "example.com",
		"disable_userinfo": true, "scopes": "openid,email"})

	r, err := a.Begin(context.Background(), api.BeginRequest{State: "s", Endpoints: api.Endpoints{CallbackURL: "https://p/cb"}})
	require.NoError(t, err)
	u, _ := url.Parse(r.URL)
	require.Equal(t, "select_account", u.Query().Get("prompt"))
	require.Equal(t, "example.com", u.Query().Get("hd"))
	require.Equal(t, "openid email", u.Query().Get("scope"))

	f.claims["hd"] = "example.com"
	s, err := signIn(t, f, a, nil)
	require.NoError(t, err)
	require.Equal(t, "12345", s.ExternalID, "a numeric subject is read as its digits")
	require.True(t, s.EmailVerified, `"true" as a string counts`)
	require.Equal(t, []string{"a", "b"}, s.Groups)
	require.Equal(t, "Ann Lee", s.DisplayName, "userinfo was not asked, so the name comes from its parts")
	require.Equal(t, []string{"12345"}, s.Attributes["employee_id"])

	f.claims = map[string]any{"email": "x@example.com"}
	_, err = signIn(t, f, configured(t, f, nil), nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, `"sub"`)
}
