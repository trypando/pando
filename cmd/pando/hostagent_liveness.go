package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/hostagent"
)

// What a joined host's agent is given to take part in R-421. Set by whoever
// starts the agent on a joined host; an agent started without PANDO_HOST_ID
// — a multi-host Docker install's forwarding agent — takes no part, and
// refuses pings.
const (
	// envHostID is this host's ID (host_…).
	envHostID = "PANDO_HOST_ID"
	// envPeersFile is a JSON file listing the cluster's hosts, [{"id",
	// "name", "address"}], read again every round so that it can change
	// without a restart. A file that cannot be read keeps the last list.
	envPeersFile = "PANDO_CLUSTER_PEERS_FILE"
	// envHostCert and envHostKey are the PEM certificate and key this host
	// checks others with, issued from the cluster authority with
	// hostagent.Authorities.IssueHost.
	envHostCert = "PANDO_HOST_CLIENT_CERT"
	envHostKey  = "PANDO_HOST_CLIENT_KEY"
)

type peerEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
}

// agentLiveness is this host's Monitor (R-421), or nil when the agent takes
// no part. It probes the other hosts with the host certificate, answers
// their pings through agent.Liveness, and fences through the host's own
// container runtime. It needs no database: Postgres being away changes
// nothing here.
func agentLiveness(authoritiesPEM []byte, logger *zap.Logger) (*liveness.Monitor, error) {
	self := os.Getenv(envHostID)
	if self == "" {
		return nil, nil
	}
	peersFile := os.Getenv(envPeersFile)
	if peersFile == "" {
		return nil, fmt.Errorf("the host agent was given %s but not %s, so it cannot check the cluster's other hosts", envHostID, envPeersFile)
	}
	cert, err := tls.X509KeyPair([]byte(os.Getenv(envHostCert)), []byte(os.Getenv(envHostKey)))
	if err != nil {
		return nil, fmt.Errorf("the host agent could not read the certificate it checks other hosts with (%s, %s): %w", envHostCert, envHostKey, err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(authoritiesPEM) {
		return nil, errors.New("the host agent was given no authority to check other hosts' agents against")
	}
	docker, err := client.New(client.FromEnv)
	if err != nil {
		return nil, fmt.Errorf("the host agent could not reach this host's container runtime, which it stops apps through when the host is cut off: %w", err)
	}

	return &liveness.Monitor{
		Self:  self,
		Peers: func(context.Context) ([]liveness.Peer, error) { return readPeers(peersFile) },
		Probe: func(ctx context.Context, p liveness.Peer) error {
			cfg := &tls.Config{
				MinVersion:   tls.VersionTLS13,
				Certificates: []tls.Certificate{cert},
				RootCAs:      roots,
				ServerName:   hostagent.ServerName(p.Name),
			}
			_, err := hostagent.Ping(ctx, p.Address, cfg, self)
			return err
		},
		Fencer: hostagent.DockerFencer{Docker: docker},
		Logger: logger.Named("liveness"),
	}, nil
}

func readPeers(path string) ([]liveness.Peer, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the agent's own configuration.
	if err != nil {
		return nil, err
	}
	var entries []peerEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("%s is not a list of hosts: %w", path, err)
	}
	out := make([]liveness.Peer, len(entries))
	for i, e := range entries {
		out[i] = liveness.Peer(e)
	}
	return out, nil
}
