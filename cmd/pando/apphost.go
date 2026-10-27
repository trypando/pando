package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/dockernet"
	"github.com/trypando/pando/internal/hostagent"
)

// runAppHost is what a host that joined with the app role alone runs in
// place of the server: its agent, in this process, with what the join gave it
// (R-416, R-421). It needs no database: the control plane being away changes
// nothing here.
func runAppHost(ctx context.Context, dir string, joined hosts.Joined, logger *zap.Logger) error {
	agent, monitor, err := appHostAgent(dir, joined, logger)
	if err != nil {
		return err
	}
	logger.Info("this host joined a cluster as an app host and runs its agent",
		zap.String("host_id", joined.HostID), zap.String("name", joined.Name), zap.String("cluster", joined.Cluster))
	return serveAgent(ctx, agent, monitor, agentListen())
}

// appHostAgent is a joined app host's agent and its Monitor: the agent serves
// with the host's own certificate, named hostagent.ServerName(<name>), checks
// every connection against the cluster authority, and carries the control
// plane's runtime calls to the host's rootless runtime (R-416). The Monitor
// checks the other hosts with the pando-host certificate the join issued,
// keeping its peer list current from the cluster listener (R-421).
func appHostAgent(dir string, joined hosts.Joined, logger *zap.Logger) (*hostagent.Agent, *liveness.Monitor, error) {
	files, err := readClusterFiles(dir, hosts.HostCertFile, hosts.HostKeyFile, hosts.AuthorityCertFile,
		hosts.HostClientCertFile, hosts.HostClientKeyFile)
	if err != nil {
		return nil, nil, err
	}
	certPEM, keyPEM, caPEM := files[0], files[1], files[2]
	serverTLS, err := hostagent.ServerTLS(certPEM, keyPEM, caPEM)
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	pool, err := appHostPool(joined)
	if err != nil {
		return nil, nil, err
	}
	socket, err := appHostRuntimeSocket()
	if err != nil {
		return nil, nil, err
	}
	agent := &hostagent.Agent{TLS: serverTLS, Pool: pool, Logger: logger, RuntimeSocket: socket}

	client509, err := tls.X509KeyPair(files[3], files[4])
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	host509, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	clusterTLS, err := hosts.JoinedTLS(caPEM, host509)
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	docker, err := client.New(client.WithHost("unix://" + socket))
	if err != nil {
		return nil, nil, fmt.Errorf("this app host could not reach its container runtime at %s, which it stops apps through when it is cut off: %w", socket, err)
	}
	sync := &hosts.PeerSync{Address: joined.Cluster, TLS: clusterTLS, File: filepath.Join(dir, hosts.PeersFile)}
	return agent, newMonitor(joined.HostID, sync.Peers, client509, roots, docker, logger), nil
}

// readClusterFiles reads names from the cluster directory, in order.
func readClusterFiles(dir string, names ...string) ([][]byte, error) {
	out := make([][]byte, len(names))
	for i, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n)) // #nosec G304 -- the configured cluster directory
		if err != nil {
			return nil, appHostError(dir, err)
		}
		out[i] = data
	}
	return out, nil
}

func appHostError(dir string, err error) error {
	return fmt.Errorf("this app host cannot run its agent: what it was given when it joined, in %s (server.cluster_dir), is missing or unreadable (%w). Remove the host from the cluster, then join it again with a new join token", dir, err)
}

// agentListen is where the agent listens: PANDO_AGENT_LISTEN, else every
// address on the agent port.
func agentListen() string {
	if listen := os.Getenv(multidocker.EnvAgentListen); listen != "" {
		return listen
	}
	return ":" + strconv.Itoa(hostagent.DefaultPort)
}

// appHostPool is the app network range the agent forwards into:
// PANDO_AGENT_NETWORK_POOL, else the runtime's when the host joined, else the
// runtime's default.
func appHostPool(joined hosts.Joined) (netip.Prefix, error) {
	raw := os.Getenv(multidocker.EnvAgentPool)
	if raw == "" {
		raw = joined.NetworkPool
	}
	if raw == "" {
		raw = dockernet.DefaultPool
	}
	pool, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("this app host cannot run its agent: %q is not an address range. Valid answer: an IPv4 range such as %s, in %s", raw, dockernet.DefaultPool, multidocker.EnvAgentPool)
	}
	return pool.Masked(), nil
}

// appHostRuntimeSocket is the host's rootless container runtime socket
// (R-402, R-416): PANDO_AGENT_RUNTIME_SOCKET, else DOCKER_HOST when it names
// a socket, else $XDG_RUNTIME_DIR/docker.sock, where rootless Docker puts it.
func appHostRuntimeSocket() (string, error) {
	if s := os.Getenv(multidocker.EnvAgentRuntimeSocket); s != "" {
		return s, nil
	}
	if s, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && s != "" {
		return s, nil
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "docker.sock"), nil
	}
	return "", errors.New("this app host cannot run its agent: it does not know where its container runtime's socket is. " +
		"Set " + multidocker.EnvAgentRuntimeSocket + " to the rootless runtime's socket, such as /run/user/1000/docker.sock")
}
