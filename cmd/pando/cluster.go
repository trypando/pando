package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

// Joining a cluster (R-415, docs/design/notes-cluster.md): the files a joined
// host keeps, the joining side, and the listener a control host answers joins
// on.

func joinFiles(cfg *config.Config) hosts.JoinFiles {
	return hosts.JoinFiles{
		Dir:             cfg.Server.ClusterDir,
		DatabaseURLPath: cfg.Server.ClusterDatabaseURLPath(),
		TokenKeyPath:    cfg.Server.TokenKeyPath,
		AuthorityPath:   cfg.Server.ClusterAuthorityPath,
	}
}

// clusterPort is the port of server.cluster_addr.
func clusterPort(cfg *config.Config) int {
	_, port, err := net.SplitHostPort(cfg.Server.ClusterAddr)
	if n, perr := strconv.Atoi(port); err == nil && perr == nil && n > 0 {
		return n
	}
	return hosts.DefaultClusterPort
}

// clusterHostname is the name other hosts reach this one by: the external
// URL's host, the advertise address's, or the machine's.
func clusterHostname(cfg *config.Config) string {
	for _, raw := range []string{cfg.Server.ExternalURL, cfg.Server.AdvertiseURL} {
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			return u.Hostname()
		}
	}
	name, _ := os.Hostname()
	return name
}

// clusterAddress is the cluster listener's address, which a join token minted
// without one names (R-415).
func clusterAddress(cfg *config.Config) string {
	return "https://" + net.JoinHostPort(clusterHostname(cfg), strconv.Itoa(clusterPort(cfg)))
}

// newJoiner is the joining side, for POST /setup/join and PANDO_JOIN_TOKEN.
func newJoiner(cfg *config.Config, cluster *state.Cluster, restart func()) *hosts.Joiner {
	machine, _ := os.Hostname()
	return &hosts.Joiner{
		Fresh:        cluster.Fresh,
		Files:        joinFiles(cfg),
		Version:      buildVersion,
		Platform:     runtime.GOOS + "/" + runtime.GOARCH,
		AgentAddress: net.JoinHostPort(clusterHostname(cfg), strconv.Itoa(hostagent.DefaultPort)),
		DefaultName:  machine,
		Restart:      restart,
	}
}

// joinFromEnvironment joins with PANDO_JOIN_TOKEN at first start, as the
// setup screen does (R-415). It reports whether it joined, in which case
// Pando restarts into the cluster. Once joined, the variable is ignored.
func joinFromEnvironment(ctx context.Context, cfg *config.Config, joiner *hosts.Joiner, logger *zap.Logger) (bool, error) {
	if cfg.Bootstrap.JoinToken == "" {
		return false, nil
	}
	if _, done, err := joiner.Files.Read(); err != nil || done {
		return false, err
	}
	joined, err := joiner.Setup(ctx, cfg.Bootstrap.JoinToken, cfg.Bootstrap.JoinName)
	if err != nil {
		return false, fmt.Errorf("PANDO_JOIN_TOKEN is set and the join failed: %w", err)
	}
	logger.Info("joined a cluster", zap.String("host_id", joined.HostID), zap.String("name", joined.Name),
		zap.Strings("roles", joined.Roles), zap.String("cluster", joined.Cluster))
	return true, nil
}

// startClusterListener serves the cluster listener on server.cluster_addr,
// over TLS from the cluster authority (R-415). The returned server is shut
// down with the API's.
func startClusterListener(cfg *config.Config, authority *hosts.Authority, handler http.Handler, logger *zap.Logger) (*http.Server, error) {
	tlsConfig, err := authority.ListenerTLS([]string{clusterHostname(cfg), "localhost", "127.0.0.1", "::1"}, time.Now())
	if err != nil {
		return nil, fmt.Errorf("pando could not issue the cluster listener's certificate: %w", err)
	}
	srv := &http.Server{
		Addr: cfg.Server.ClusterAddr, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	ln, err := net.Listen("tcp", cfg.Server.ClusterAddr)
	if err != nil {
		return nil, fmt.Errorf("pando could not listen for joining hosts on %s (server.cluster_addr): %w", cfg.Server.ClusterAddr, err)
	}
	go func() {
		logger.Info("cluster listener", zap.String("addr", ln.Addr().String()))
		if err := srv.ServeTLS(ln, "", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("the cluster listener stopped", zap.Error(err))
		}
	}()
	return srv, nil
}

// runAppHost is what a host that joined with the app role alone runs in
// place of the server: its agent, with the certificate it was issued (R-416).
//
// The agent that carries the docker-hosts runtime's calls is another slice of
// #347's work; until it lands this says what the host is and waits, rather
// than serving an API it has no database for.
func runAppHost(ctx context.Context, joined hosts.Joined, logger *zap.Logger) error {
	logger.Info("this host joined a cluster as an app host and runs its agent",
		zap.String("host_id", joined.HostID), zap.String("name", joined.Name), zap.String("cluster", joined.Cluster))
	<-ctx.Done()
	return nil
}
