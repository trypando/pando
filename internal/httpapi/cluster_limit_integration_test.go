//go:build integration

package httpapi_test

import (
	"context"
	"crypto/tls"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	corehosts "github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
)

// actionCount counts the audit events with action.
func actionCount(t *testing.T, i *install, action string) int {
	t.Helper()
	var n int
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_events WHERE action = $1`, action).Scan(&n))
	return n
}

// TestR347_ARateLimitedCallerAddsNoMoreAuditRows asserts R-347 and R-348
// with R-404 and R-415: a caller without a session can add only a bounded
// number of rows to the audit log, which keeps every row for three months.
// Joins refused within the limit are each audited; once an address is over
// it, its further requests are answered 429 and add no row but one
// cluster.join.limited for the window, on the cluster listener and on
// POST /setup/join alike.
func TestR347_ARateLimitedCallerAddsNoMoreAuditRows(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.FailedAttempts = state.NewFailedAttempts(i.db)
	listener := i.Server.ClusterRoutes()
	bad := `{"secret":"jt_1.x","name":"edge-2","address":"edge:7443"}`

	for n := 0; n < 20; n++ {
		got := post(t, listener, corehosts.JoinPath, "203.0.113.20:4000", bad)
		require.Equal(t, http.StatusUnauthorized, got.Code, "attempt %d: %s", n, got.Body.String())
	}
	require.Equal(t, 20, actionCount(t, i, "cluster.join.denied"), "each refusal within the limit")
	for n := 0; n < 50; n++ {
		got := post(t, listener, corehosts.JoinPath, "203.0.113.20:4000", bad)
		require.Equal(t, http.StatusTooManyRequests, got.Code, "attempt %d: %s", n, got.Body.String())
		require.NotEmpty(t, got.Header().Get("Retry-After"))
	}
	require.Equal(t, 20, actionCount(t, i, "cluster.join.denied"), "nothing more once limited")
	require.Equal(t, 1, actionCount(t, i, "cluster.join.limited"), "one event for the window")

	i.Server.Joiner = freshHost(t)
	wrong := `{"setup_token":"wrong","token":"pj1.x","name":"edge-2"}`
	for n := 0; n < 20; n++ {
		got := post(t, i.handler, "/api/v1/setup/join", "203.0.113.21:4000", wrong)
		require.Equal(t, http.StatusUnauthorized, got.Code, "attempt %d: %s", n, got.Body.String())
	}
	for n := 0; n < 50; n++ {
		got := post(t, i.handler, "/api/v1/setup/join", "203.0.113.21:4000", wrong)
		require.Equal(t, http.StatusTooManyRequests, got.Code, "attempt %d: %s", n, got.Body.String())
	}
	require.Equal(t, 40, actionCount(t, i, "cluster.join.denied"))
	require.Equal(t, 2, actionCount(t, i, "cluster.join.limited"), "one per address")
}

// TestR347_ARevokedHostCannotFloodTheLogThroughRenewal asserts R-347 and
// R-404 with R-415 on POST /cluster/renew: a removed host's certificate,
// which still chains to the authority for up to 90 days, is refused before
// anything else is done, and however often it asks the log gains one event
// for the window. A certificate from the authority that is no host's is
// limited per certificate and per address, and audited only within the
// limit.
func TestR347_ARevokedHostCannotFloodTheLogThroughRenewal(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.FailedAttempts = state.NewFailedAttempts(i.db)
	srv := clusterListener(t, i)
	ctx := context.Background()
	app := freshHost(t)
	joined, err := app.Join(ctx, mintJoin(t, i, srv, "app"), "edge-1")
	require.NoError(t, err)
	dir := app.Files.Dir
	host, err := tls.LoadX509KeyPair(filepath.Join(dir, corehosts.HostCertFile), filepath.Join(dir, corehosts.HostKeyFile))
	require.NoError(t, err)
	probe, err := tls.LoadX509KeyPair(filepath.Join(dir, corehosts.HostClientCertFile), filepath.Join(dir, corehosts.HostClientKeyFile))
	require.NoError(t, err)

	renew := func(cert tls.Certificate) int {
		cfg := corehosts.PinnedTLS(i.Server.Hosts.Authority.Hash())
		cfg.Certificates = []tls.Certificate{cert}
		client := &http.Client{Transport: &http.Transport{TLSClientConfig: cfg}}
		defer client.CloseIdleConnections()
		resp, err := client.Post(srv.URL+corehosts.RenewPath, "application/json", strings.NewReader(`{"csr":"x"}`))
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// A certificate the authority issued that is no host's: limited.
	codes := map[int]int{}
	for n := 0; n < 30; n++ {
		codes[renew(probe)]++
	}
	require.Positive(t, codes[http.StatusUnauthorized])
	require.Positive(t, codes[http.StatusTooManyRequests], "limited per certificate")
	denied := actionCount(t, i, "cluster.host.renew.denied")
	require.Equal(t, codes[http.StatusUnauthorized], denied, "each refusal within the limit")
	require.Equal(t, 1, actionCount(t, i, "cluster.host.renew.limited"))

	removed := i.do(i.admin(), http.MethodDelete, "/cluster/hosts/"+joined.HostID, nil)
	require.Equal(t, http.StatusOK, removed.Code, removed.String())
	for n := 0; n < 50; n++ {
		require.Equal(t, http.StatusUnauthorized, renew(host), "attempt %d", n)
	}
	require.Equal(t, denied+1, actionCount(t, i, "cluster.host.renew.denied"), "one event for the revoked certificate's window")
}
