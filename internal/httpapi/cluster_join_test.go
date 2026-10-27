package httpapi_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	corehosts "github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/secret"
)

func post(t *testing.T, h http.Handler, path, from, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.RemoteAddr = from
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestR404_FailedJoinsAreRateLimitedPerAddress asserts R-404 applied to
// R-415: the cluster listener takes no session, so refused joins from one
// client address are limited as wrong passwords are, and a caller over the
// limit is told how long to wait while another address is not held to it.
func TestR404_FailedJoinsAreRateLimitedPerAddress(t *testing.T) {
	s := &httpapi.Server{Hosts: &corehosts.Service{Store: refusingStore{}}, Logger: zap.NewNop()}
	h := s.ClusterRoutes()
	bad := `{"secret":"jt_1.x","name":"edge-2","address":"edge:7443"}`

	for i := 0; i < 20; i++ {
		got := post(t, h, corehosts.JoinPath, "203.0.113.7:4000", bad)
		require.Equal(t, http.StatusUnauthorized, got.Code, "attempt %d: %s", i, got.Body.String())
	}
	got := post(t, h, corehosts.JoinPath, "203.0.113.7:4001", bad)
	require.Equal(t, http.StatusTooManyRequests, got.Code, got.Body.String())
	require.NotEmpty(t, got.Header().Get("Retry-After"))
	require.Contains(t, got.Body.String(), "Too many refused joins from this network address.")

	got = post(t, h, corehosts.JoinPath, "198.51.100.9:4000", "not json")
	require.Equal(t, http.StatusBadRequest, got.Code, "another address is not held to it")
}

// refusingStore refuses every join token, as a wrong, expired or revoked one
// is refused.
type refusingStore struct{ corehosts.Store }

func (refusingStore) Join(context.Context, secret.Value, func(state.JoinToken) (state.Host, error)) (state.JoinToken, state.Host, error) {
	return state.JoinToken{}, state.Host{}, errs.New(errs.AuthTokenInvalid, "This join token is not valid.")
}

// TestR404_OnlyARefusedSecretCountsAgainstAJoinAddress asserts R-404 applied
// to R-415: what counts toward an address's limit is a refusal that tested a
// secret, a join token or a setup token, and not a request refused for its
// shape, such as a name, an address or a body Pando cannot use, which tests
// none. A refused name is not echoed back.
func TestR404_OnlyARefusedSecretCountsAgainstAJoinAddress(t *testing.T) {
	s := &httpapi.Server{Hosts: &corehosts.Service{Store: refusingStore{}}, Logger: zap.NewNop()}
	h := s.ClusterRoutes()
	for i := 0; i < 30; i++ {
		for _, body := range []string{
			`{"secret":"jt_1.x","name":"` + strings.Repeat("n", 60<<10) + `","address":"edge:7443"}`,
			`{"secret":"jt_1.x","name":"control","address":"edge:7443"}`,
			`{"secret":"jt_1.x","name":"edge-2","address":"edge"}`,
			`{"secret":"jt_1.x","name":"edge-2","address":"edge:7443","version":"1 2"}`,
			"not json",
		} {
			got := post(t, h, corehosts.JoinPath, "203.0.113.10:4000", body)
			require.Equal(t, http.StatusBadRequest, got.Code, "attempt %d: %s", i, got.Body.String())
			require.Less(t, got.Body.Len(), 1024, "the refusal does not echo the name")
		}
	}
	got := post(t, h, corehosts.JoinPath, "203.0.113.10:4000", `{"secret":"jt_1.x","name":"edge-2","address":"edge:7443"}`)
	require.Equal(t, http.StatusUnauthorized, got.Code, "a token is still tried: %s", got.Body.String())

	setup := &httpapi.Server{Logger: zap.NewNop(), Joiner: &corehosts.Joiner{
		SetupToken: func(context.Context, secret.Value) error { return nil },
	}}
	sh := setup.Routes()
	for i := 0; i < 30; i++ {
		got := post(t, sh, "/api/v1/setup/join", "203.0.113.11:4000", `{"setup_token":"right","token":"pj1.x","name":"edge-2"}`)
		require.Equal(t, http.StatusBadRequest, got.Code, "attempt %d: %s", i, got.Body.String())
	}
}

// TestR404_FailedSetupJoinsAreRateLimitedPerAddress asserts R-404 applied
// to POST /setup/join, which is public: a wrong setup token counts, and the
// limit holds before the token is checked again.
func TestR404_FailedSetupJoinsAreRateLimitedPerAddress(t *testing.T) {
	checked := 0
	s := &httpapi.Server{Logger: zap.NewNop(), Joiner: &corehosts.Joiner{
		SetupToken: func(context.Context, secret.Value) error { checked++; return state.ErrSetupTokenWrong },
	}}
	h := s.Routes()
	for i := 0; i < 20; i++ {
		got := post(t, h, "/api/v1/setup/join", "203.0.113.8:4000", `{"setup_token":"wrong","token":"pj1.x","name":"edge-2"}`)
		require.NotEqual(t, http.StatusTooManyRequests, got.Code, "attempt %d", i)
	}
	got := post(t, h, "/api/v1/setup/join", "203.0.113.8:4000", `{"setup_token":"wrong","token":"pj1.x","name":"edge-2"}`)
	require.Equal(t, http.StatusTooManyRequests, got.Code, got.Body.String())
	require.Equal(t, 20, checked, "over the limit the token is not checked")

	got = post(t, h, "/api/v1/setup/join", "203.0.113.9:4000", "not json")
	require.Equal(t, http.StatusBadRequest, got.Code)
}

// TestTheClusterListenerRefusesARenewalWithoutACertificate asserts that a
// renewal needs the host's own certificate, and that the routes answer
// plainly when no cluster is set up.
func TestTheClusterListenerRefusesARenewalWithoutACertificate(t *testing.T) {
	s := &httpapi.Server{Hosts: &corehosts.Service{}, Logger: zap.NewNop()}
	got := post(t, s.ClusterRoutes(), corehosts.RenewPath, "203.0.113.7:4000", `{"csr":""}`)
	require.Equal(t, http.StatusUnauthorized, got.Code, got.Body.String())

	off := (&httpapi.Server{Logger: zap.NewNop()}).ClusterRoutes()
	got = post(t, off, corehosts.RenewPath, "203.0.113.7:4000", `{}`)
	require.Equal(t, http.StatusInternalServerError, got.Code)
}
