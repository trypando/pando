package httpapi_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/httpapi"
)

type challenges map[string]string

func (c challenges) KeyAuthorization(_ context.Context, token string) (string, bool, error) {
	ka, ok := c[token]
	return ka, ok, nil
}

// TestR169_APendingHTTP01ChallengeIsAnsweredOnEveryHostname asserts the
// leader's pending answer is served by whichever replica the CA reaches, on an
// app's hostname and Pando's own, and that a token nobody is waiting on is the
// app's path as before.
func TestR169_APendingHTTP01ChallengeIsAnsweredOnEveryHostname(t *testing.T) {
	const token = "abcdefghijklmnopqrstuvwx_-12"
	r := newRouted(t, func(s *httpapi.Server) {
		s.Console = marker("console")
		s.AppProxy = marker("proxy")
		s.AppHosts = hosts{app: map[string]bool{"notes.example.com": true}}
		s.ACMEChallenges = challenges{token: token + ".thumbprint"}
	})
	for _, host := range []string{"notes.example.com", "pando.example.com"} {
		got := r.get(host, httpapi.ACMEChallengePrefix+token)
		require.Equal(t, http.StatusOK, got.Code, host)
		require.Equal(t, token+".thumbprint", got.Body.String(), host)
		require.Empty(t, got.Header().Get("X-Handled-By"), host)
	}
	got := r.get("notes.example.com", httpapi.ACMEChallengePrefix+"nobody-is-waiting-on-this")
	require.Equal(t, "proxy", got.Header().Get("X-Handled-By"))
}
