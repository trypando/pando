package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
)

func TestTheClusterAddressNamesTheListener(t *testing.T) {
	cfg := &config.Config{Server: config.Server{ClusterAddr: ":9443", ExternalURL: "https://pando.example.com"}}
	require.Equal(t, "https://pando.example.com:9443", clusterAddress(cfg))

	cfg.Server = config.Server{ClusterAddr: "bogus", AdvertiseURL: "http://10.0.0.5:8080"}
	require.Equal(t, "https://10.0.0.5:7443", clusterAddress(cfg), "the default port, and the advertise address's host")

	cfg.Server = config.Server{ClusterAddr: ":7443"}
	name, _ := os.Hostname()
	require.Equal(t, name, clusterHostname(cfg))
}

func joinCfg(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	return &config.Config{Server: config.Server{
		ClusterAddr: "127.0.0.1:0", ClusterDir: filepath.Join(dir, "cluster"),
		TokenKeyPath: filepath.Join(dir, "token.key"), ClusterAuthorityPath: filepath.Join(dir, "ca.pem"),
	}}
}

// TestR415_PANDO_JOIN_TOKENJoinsOnceAtFirstStart asserts R-415's unattended
// join: without the variable nothing happens; with it a failed join stops
// startup with the reason; once joined, the variable is ignored.
func TestR415_PANDO_JOIN_TOKENJoinsOnceAtFirstStart(t *testing.T) {
	cfg := joinCfg(t)
	j := newJoiner(cfg, nil, nil)
	j.Fresh = func(context.Context) (bool, bool, error) { return true, false, nil }
	require.Equal(t, cfg.Server.ClusterDir, j.Files.Dir)
	require.Contains(t, j.Platform, "/")

	joined, err := joinFromEnvironment(context.Background(), cfg, j, zap.NewNop())
	require.NoError(t, err)
	require.False(t, joined)

	cfg.Bootstrap.JoinToken = "pj1.not-a-token"
	_, err = joinFromEnvironment(context.Background(), cfg, j, zap.NewNop())
	require.ErrorContains(t, err, "PANDO_JOIN_TOKEN is set and the join failed")

	require.NoError(t, os.MkdirAll(cfg.Server.ClusterDir, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cfg.Server.ClusterDir, hosts.JoinedFile), []byte(`{"roles":["app"]}`), 0o600))
	joined, err = joinFromEnvironment(context.Background(), cfg, j, zap.NewNop())
	require.NoError(t, err)
	require.False(t, joined, "already joined: nothing to do")
}

func TestTheClusterListenerServesTLSFromTheAuthority(t *testing.T) {
	cfg := joinCfg(t)
	authority, err := hosts.LoadOrCreateAuthority(cfg.Server.ClusterAuthorityPath)
	require.NoError(t, err)
	srv, err := startClusterListener(cfg, authority, http.NotFoundHandler(), zap.NewNop())
	require.NoError(t, err)
	require.NoError(t, srv.Close())

	cfg.Server.ClusterAddr = "256.0.0.1:1"
	_, err = startClusterListener(cfg, authority, http.NotFoundHandler(), zap.NewNop())
	require.ErrorContains(t, err, "server.cluster_addr")
}

func TestAnAppHostWaitsAsItsAgent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, runAppHost(ctx, hosts.Joined{Name: "edge"}, zap.NewNop()))
}
