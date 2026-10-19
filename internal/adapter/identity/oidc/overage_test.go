package oidc_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/identity/oidc"
	"github.com/trypando/pando/internal/errs"
)

// fakeGraph answers /v1.0/me/getMemberObjects as Microsoft Graph does, for
// the access token the fake provider issues.
type fakeGraph struct {
	srv    *httptest.Server
	calls  int
	status int
	groups []string
}

func newGraph(t *testing.T) *fakeGraph {
	t.Helper()
	g := &fakeGraph{status: http.StatusOK}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g.calls++
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/v1.0/me/getMemberObjects", r.URL.Path)
		require.Equal(t, "Bearer at", r.Header.Get("Authorization"))
		var body map[string]bool
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.True(t, body["securityEnabledOnly"])
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(g.status)
		if g.status != http.StatusOK {
			_, _ = w.Write([]byte(`{"error":{"code":"Authorization_RequestDenied","message":"Insufficient privileges"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"value": g.groups})
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func overageClaims() map[string]any {
	return map[string]any{"sub": "s1", "oid": "o1",
		"_claim_names":   map[string]any{"groups": "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{"endpoint": "https://graph.microsoft.com/v1.0/users/o1/getMemberObjects"}}}
}

// TestR078_OIDCEntraGroupsOverageReadsGroupsFromGraph asserts that when Entra
// ID leaves out the groups of a person in more than 200, Pando reads them from
// Microsoft Graph instead of signing them in with none.
func TestR078_OIDCEntraGroupsOverageReadsGroupsFromGraph(t *testing.T) {
	f, g := newFake(t), newGraph(t)
	g.groups = []string{"6f1c-group-a", "9d2e-group-b"}
	a := configured(t, f, map[string]any{"subject_claim": "oid"}).WithGraphURL(g.srv.URL)

	f.claims = overageClaims()
	s, err := signIn(t, f, a, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"6f1c-group-a", "9d2e-group-b"}, s.Groups)
	require.Equal(t, 1, g.calls)

	// The implicit flow's hasgroups means the same.
	f.claims = map[string]any{"sub": "s1", "oid": "o1", "hasgroups": true}
	s, err = signIn(t, f, a, nil)
	require.NoError(t, err)
	require.Len(t, s.Groups, 2)

	// Groups in the token are used as they are, and Graph is not asked.
	f.claims = map[string]any{"sub": "s1", "oid": "o1", "groups": []any{"g1"}}
	s, err = signIn(t, f, a, nil)
	require.NoError(t, err)
	require.Equal(t, []string{"g1"}, s.Groups)
	require.Equal(t, 2, g.calls)
}

// TestR078_OIDCEntraGroupsOverageWithoutGraphRefusesTheSignIn asserts that a
// person whose groups cannot be read is refused, with the permission to grant,
// rather than signed in as a member of nothing.
func TestR078_OIDCEntraGroupsOverageWithoutGraphRefusesTheSignIn(t *testing.T) {
	f, g := newFake(t), newGraph(t)
	g.status = http.StatusForbidden
	f.claims = overageClaims()

	_, err := signIn(t, f, configured(t, f, nil).WithGraphURL(g.srv.URL), nil)
	require.Error(t, err)
	e := errs.As(err)
	require.Equal(t, errs.AuthInvalid, e.Code)
	require.Contains(t, e.Message, "more than 200")
	require.Contains(t, e.Message, "HTTP 403 (Authorization_RequestDenied)")
	require.Contains(t, e.Remedy, "User.Read")

	// Graph unreachable.
	g.srv.Close()
	_, err = signIn(t, f, configured(t, f, nil).WithGraphURL(g.srv.URL), nil)
	require.Error(t, err)
	require.Equal(t, errs.AdapterUnavailable, errs.As(err).Code)
}

// TestR078_OIDCDistributedGroupsFromAnotherProviderAreRefused asserts that the
// access token is only sent to Microsoft Graph, and only for Entra ID: another
// provider's distributed groups claim refuses the sign-in.
func TestR078_OIDCDistributedGroupsFromAnotherProviderAreRefused(t *testing.T) {
	f := newFake(t)
	f.claims = overageClaims()
	_, err := signIn(t, f, configured(t, f, nil), nil)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "left this person's groups out of the ID token")
}

// TestOIDCRefusesAProviderThatSignsWithTheClientSecret asserts that a provider
// signing ID tokens with HS256 — Authentik with no signing key — is named as
// the problem, at the health check and at sign-in, with what to choose.
func TestOIDCRefusesAProviderThatSignsWithTheClientSecret(t *testing.T) {
	f := newFake(t)
	f.algs = []string{"HS256"}
	a := configured(t, f, nil)
	err := a.HealthCheck(context.Background())
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "HS256")
	require.Contains(t, errs.As(err).Remedy, "Signing Key")

	// Advertised alongside RS256, but used: refused at sign-in.
	f.algs = []string{"RS256", "HS256"}
	f.hsSecret = []byte("secret-secret-secret-secret-secret")
	f.claims = map[string]any{"sub": "s1"}
	_, err = signIn(t, f, configured(t, f, nil), nil)
	require.Error(t, err)
	require.Equal(t, errs.AuthInvalid, errs.As(err).Code)
	require.Contains(t, errs.As(err).Message, "HS256")
}

func TestOIDCPresetsSayWhatEachProviderNeeds(t *testing.T) {
	presets := map[string]map[string]string{}
	help := map[string]string{}
	for _, p := range oidc.Info().Presets {
		presets[p.ID], help[p.ID] = p.Values, p.Help
	}
	require.Contains(t, strings.Fields(presets["entra"]["scopes"]), "User.Read", "Graph is read for a groups overage")
	require.Contains(t, help["google"], "Groups are not synced from Google")
	require.Contains(t, help["authentik"], "Signing Key")
}
