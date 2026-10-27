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
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/httpapi"
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

// recordInstallHost records this machine as the install's first host, once
// (R-414). A failure is logged and does not stop startup: until the row is
// made the cluster still counts as one host (hosts.Service.Count).
func recordInstallHost(ctx context.Context, cfg *config.Config, store hosts.InstallHostStore, logger *zap.Logger) {
	machine, _ := os.Hostname()
	address := localAgentAddress(cfg.Server.AdvertiseURL)
	if address == "" {
		address = net.JoinHostPort(clusterHostname(cfg), strconv.Itoa(hostagent.DefaultPort))
	}
	h, recorded, err := hosts.RecordInstallHost(ctx, store, hosts.InstallHostInfo{
		Hostname: machine, Address: address, Platform: runtime.GOOS + "/" + runtime.GOARCH, Version: buildVersion,
	})
	switch {
	case err != nil:
		logger.Warn("could not record this machine as the cluster's first host", zap.Error(err))
	case recorded:
		logger.Info("recorded this machine as the cluster's first host", zap.String("host_id", h.ID), zap.String("name", h.Name))
	}
}

// startClusterListener serves the cluster listener on server.cluster_addr,
// over TLS from the cluster authority (R-415), answering the cluster's
// liveness checks for this host with pings (R-421). The returned server is
// shut down with the API's.
func startClusterListener(cfg *config.Config, authority *hosts.Authority, handler http.Handler, pings *controlPings, logger *zap.Logger) (*http.Server, error) {
	tlsConfig, err := authority.ListenerTLS([]string{clusterHostname(cfg), "localhost", "127.0.0.1", "::1"}, time.Now())
	if err != nil {
		return nil, fmt.Errorf("pando could not issue the cluster listener's certificate: %w", err)
	}
	srv := &http.Server{
		Addr: cfg.Server.ClusterAddr, Handler: handler, TLSConfig: tlsConfig,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second,
	}
	pings.attach(srv)
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

// startingHandler is the cluster listener's HTTP before the API is built: a
// listener started early, to answer pings while Pando waits for its database
// (R-421), refuses joins until set is called.
type startingHandler struct{ h atomic.Pointer[http.Handler] }

func (s *startingHandler) set(h http.Handler) { s.h.Store(&h) }

func (s *startingHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h := s.h.Load(); h != nil {
		(*h).ServeHTTP(w, r)
		return
	}
	httpapi.Error(w, r, errs.New(errs.Internal, "This control host is starting and cannot take a join yet.").
		WithRemedy("Try again in a minute."))
}

// clusterSizer counts the cluster's hosts.
type clusterSizer interface {
	Count(ctx context.Context) (int, error)
}

// requireClusterListener decides what a cluster listener that could not
// start means (R-414): nothing for a single host that never joined, which
// needs it only to be joined by another, so it runs without it and says
// so; for a host of a cluster, a stop, since the other hosts join and check
// it there.
func requireClusterListener(ctx context.Context, listenErr error, hasJoined bool, cluster clusterSizer, logger *zap.Logger) error {
	if listenErr == nil {
		return nil
	}
	if !hasJoined {
		if n, err := cluster.Count(ctx); err == nil && n <= 1 {
			logger.Warn("the cluster listener did not start; this single host runs without it, and no host can join it until it does",
				zap.Error(listenErr),
				zap.String("remedy", "free the port in server.cluster_addr, or set server.cluster_addr to a free one, then restart Pando"))
			return nil
		}
	}
	return listenErr
}
