//go:build integration

package httpapi_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corehosts "github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// clusterListener serves the install's cluster routes over TLS from its
// authority, as server.cluster_addr does, with the keys a control host is
// handed on disk.
func clusterListener(t *testing.T, i *install) *httptest.Server {
	t.Helper()
	svc := i.Server.Hosts
	svc.Version = "test"
	keyPath := filepath.Join(t.TempDir(), "token.key")
	require.NoError(t, os.WriteFile(keyPath, testTokenKey, 0o600))
	svc.Keys = corehosts.KeySource{DatabaseURL: secret.New("postgres://cluster-db/pando"), TokenKeyPath: keyPath}

	srv := httptest.NewUnstartedServer(i.Server.ClusterRoutes())
	tlsConfig, err := svc.Authority.ListenerTLS([]string{"127.0.0.1"}, time.Now())
	require.NoError(t, err)
	srv.TLS = tlsConfig
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// mintJoin mints a join token for roles through the API, naming the listener.
func mintJoin(t *testing.T, i *install, srv *httptest.Server, roles ...string) string {
	t.Helper()
	got := i.do(i.admin(), http.MethodPost, "/cluster/join-tokens", map[string]any{"roles": roles, "address": srv.URL})
	require.Equal(t, http.StatusCreated, got.Code, got.String())
	var out struct {
		Join string `json:"join"`
	}
	got.JSON(t, &out)
	return out.Join
}

// freshHost is a new Pando, with nothing in it, about to join.
func freshHost(t *testing.T) *corehosts.Joiner {
	t.Helper()
	dir := t.TempDir()
	return &corehosts.Joiner{
		Fresh: func(context.Context) (bool, bool, error) { return false, false, nil },
		Files: corehosts.JoinFiles{
			Dir: filepath.Join(dir, "cluster"), DatabaseURLPath: filepath.Join(dir, "cluster", "database-url"),
			TokenKeyPath: filepath.Join(dir, "token.key"), AuthorityPath: filepath.Join(dir, "cluster-ca.pem"),
		},
		Version: "test", AgentAddress: "edge:7443",
	}
}

// TestR415_AHostJoinsThroughTheClusterListener asserts R-415 end to end
// against the database: an app host joins with a single-use token and is
// given no keys, the same token cannot join a second host, a control host is
// given the keys, and every join and refusal is audited.
func TestR415_AHostJoinsThroughTheClusterListener(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	ctx := context.Background()

	appToken := mintJoin(t, i, srv, "app")
	app := freshHost(t)
	joined, err := app.Join(ctx, appToken, "edge-1")
	require.NoError(t, err)
	require.Equal(t, []string{"app"}, joined.Roles)
	_, err = os.Stat(app.Files.DatabaseURLPath)
	require.True(t, os.IsNotExist(err), "an app host is given no database address")

	_, err = freshHost(t).Join(ctx, appToken, "edge-2")
	require.Equal(t, errs.AuthTokenInvalid, errs.CodeOf(err), "a single-use token joins one host")

	control := freshHost(t)
	_, err = control.Join(ctx, mintJoin(t, i, srv, "control", "app"), "core-2")
	require.NoError(t, err)
	url, err := os.ReadFile(control.Files.DatabaseURLPath)
	require.NoError(t, err)
	require.Equal(t, "postgres://cluster-db/pando", string(url))
	key, err := os.ReadFile(control.Files.TokenKeyPath)
	require.NoError(t, err)
	require.Equal(t, testTokenKey, key)

	listed := i.do(i.admin(), http.MethodGet, "/cluster/hosts", nil)
	require.Contains(t, listed.String(), "edge-1")
	require.Contains(t, listed.String(), "core-2")
	require.NotContains(t, listed.String(), "edge-2")

	audit := i.do(i.admin(), http.MethodGet, "/audit?action=cluster.", nil)
	require.Contains(t, audit.String(), "cluster.host.join")
	require.Contains(t, audit.String(), "cluster.join.denied")
}

func mustGet(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	return req
}

// TestR415_TheClusterListenerServesItsAuthority asserts R-415: the
// authority's certificate, public, to a client that pins it.
func TestR415_TheClusterListenerServesItsAuthority(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: corehosts.PinnedTLS(i.Server.Hosts.Authority.Hash())}}
	resp, err := client.Do(mustGet(t, srv.URL+corehosts.AuthorityPath))
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, string(i.Server.Hosts.Authority.CertPEM()), string(body))

	bad, err := client.Post(srv.URL+corehosts.JoinPath, "application/json", nil)
	require.NoError(t, err)
	_ = bad.Body.Close()
	require.Equal(t, http.StatusBadRequest, bad.StatusCode)
}

// TestR415_APandoWithAnAccountCannotJoinAtSetup asserts R-415 on
// POST /setup/join: this install has an account, so it refuses, and says why.
func TestR415_APandoWithAnAccountCannotJoinAtSetup(t *testing.T) {
	t.Parallel()
	i := newInstall(t)

	off := i.anon(http.MethodPost, "/setup/join", map[string]any{"token": "pj1.x", "name": "edge"})
	require.Equal(t, string(errs.Internal), off.ErrorCode(), off.String())

	joiner := freshHost(t)
	joiner.Fresh = state.NewCluster(i.db, testTokenKey).Fresh
	i.Server.Joiner = joiner
	srv := clusterListener(t, i)
	got := i.anon(http.MethodPost, "/setup/join", map[string]any{"token": mintJoin(t, i, srv, "app"), "name": "edge"})
	require.Equal(t, string(errs.StateInvalid), got.ErrorCode(), got.String())
	require.Contains(t, got.String(), "already has accounts or apps")

	audit := i.do(i.admin(), http.MethodGet, "/audit?action=cluster.join.denied", nil)
	require.Contains(t, audit.String(), "cluster.join.denied")
}

// TestR415_SetupJoinAnswersAndRestarts asserts R-415's setup contract: 202
// with the roles, then a restart into the cluster.
func TestR415_SetupJoinAnswersAndRestarts(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	joiner := freshHost(t)
	restarted := make(chan struct{}, 1)
	joiner.Restart = func() { restarted <- struct{}{} }
	i.Server.Joiner = joiner

	got := i.anon(http.MethodPost, "/setup/join", map[string]any{"token": mintJoin(t, i, srv, "app"), "name": "edge-9"})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	var out struct {
		Roles  []string `json:"roles"`
		Status string   `json:"status"`
	}
	got.JSON(t, &out)
	require.Equal(t, []string{"app"}, out.Roles)
	require.Equal(t, "joined", out.Status)
	<-restarted

	unreadable := i.anon(http.MethodPost, "/setup/join", "not an object")
	require.Equal(t, string(errs.ValidInvalid), unreadable.ErrorCode(), unreadable.String())
}
