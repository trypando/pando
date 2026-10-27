//go:build integration

package httpapi_test

import (
	"context"
	"crypto/tls"
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
		SetupToken: func(_ context.Context, token secret.Value) error {
			if token.Reveal() != testSetupToken {
				return state.ErrSetupTokenWrong
			}
			return nil
		},
	}
}

// testSetupToken is the fresh host's setup token (R-046).
const testSetupToken = "s3tup-token"

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
	got := i.anon(http.MethodPost, "/setup/join", map[string]any{"setup_token": testSetupToken, "token": mintJoin(t, i, srv, "app"), "name": "edge"})
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

	got := i.anon(http.MethodPost, "/setup/join", map[string]any{"setup_token": testSetupToken, "token": mintJoin(t, i, srv, "app"), "name": "edge-9"})
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

// TestR415_JoiningAtSetupNeedsTheSetupToken asserts R-046 on R-415's setup
// join: POST /setup/join without this Pando's setup token, or with another,
// is refused before the cluster is reached, audited, and spends no use of the
// join token; with it, the host joins.
func TestR415_JoiningAtSetupNeedsTheSetupToken(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	joiner := freshHost(t)
	joiner.Restart = func() {}
	i.Server.Joiner = joiner
	token := mintJoin(t, i, srv, "app")

	for name, body := range map[string]map[string]any{
		"none":    {"token": token, "name": "edge-3"},
		"another": {"setup_token": "guess", "token": token, "name": "edge-3"},
	} {
		got := i.anon(http.MethodPost, "/setup/join", body)
		require.Equal(t, string(errs.AuthInvalid), got.ErrorCode(), name+": "+got.String())
		require.Contains(t, got.String(), "setup token")
	}
	listed := i.do(i.admin(), http.MethodGet, "/cluster/hosts", nil)
	require.NotContains(t, listed.String(), "edge-3", "the cluster was never reached")

	got := i.anon(http.MethodPost, "/setup/join", map[string]any{"setup_token": " " + testSetupToken + " ", "token": token, "name": "edge-3"})
	require.Equal(t, http.StatusAccepted, got.Code, got.String())

	audit := i.do(i.admin(), http.MethodGet, "/audit?action=cluster.join.denied", nil)
	require.Contains(t, audit.String(), string(errs.AuthInvalid))
}

// TestR421_AJoinedHostReadsThePeerListWithItsCertificate asserts R-421's peer
// list on the cluster listener: a host that joined reads every joined host
// with the certificate it joined with, and a client without one is refused.
func TestR421_AJoinedHostReadsThePeerListWithItsCertificate(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	ctx := context.Background()
	app := freshHost(t)
	joined, err := app.Join(ctx, mintJoin(t, i, srv, "app"), "edge-4")
	require.NoError(t, err)

	cert, err := tls.LoadX509KeyPair(filepath.Join(app.Files.Dir, corehosts.HostCertFile), filepath.Join(app.Files.Dir, corehosts.HostKeyFile))
	require.NoError(t, err)
	ca, err := os.ReadFile(filepath.Join(app.Files.Dir, corehosts.AuthorityCertFile))
	require.NoError(t, err)
	cfg, err := corehosts.JoinedTLS(ca, cert)
	require.NoError(t, err)
	sync := &corehosts.PeerSync{Address: joined.Cluster, TLS: cfg, File: filepath.Join(app.Files.Dir, corehosts.PeersFile)}
	peers, err := sync.Peers(ctx)
	require.NoError(t, err)
	require.Contains(t, peers, corehosts.Peer{ID: joined.HostID, Name: "edge-4", Address: "edge:7443"})

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: corehosts.PinnedTLS(i.Server.Hosts.Authority.Hash())}}
	resp, err := client.Do(mustGet(t, srv.URL+corehosts.PeersPath))
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
