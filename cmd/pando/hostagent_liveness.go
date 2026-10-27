package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
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
	return newMonitor(self, func(context.Context) ([]hosts.Peer, error) { return hosts.ReadPeers(peersFile) },
		staticCert(cert), roots, docker, logger), nil
}

// staticCert presents cert at every handshake: for an agent given its
// certificate by whoever started it, which re-creates it to renew it.
func staticCert(cert tls.Certificate) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
}

// newMonitor is a joined host's Monitor (R-421): it probes the hosts peers
// lists with the certificate clientCert gives at each probe, which is renewed
// while the host runs, checking each agent against roots and the name it
// joined under, and fences through docker.
func newMonitor(self string, peers func(context.Context) ([]hosts.Peer, error),
	clientCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error),
	roots *x509.CertPool, docker hostagent.ContainerAPI, logger *zap.Logger) *liveness.Monitor {
	return &liveness.Monitor{
		Self: self,
		Peers: func(ctx context.Context) ([]liveness.Peer, error) {
			list, err := peers(ctx)
			return livenessPeers(list), err
		},
		Probe: func(ctx context.Context, p liveness.Peer) ([]byte, error) {
			cfg := &tls.Config{
				MinVersion:           tls.VersionTLS13,
				GetClientCertificate: clientCert,
				RootCAs:              roots,
				ServerName:           hostagent.ServerName(p.Name),
			}
			return probeResult(hostagent.Ping(ctx, p.Address, cfg, self))
		},
		Fencer: hostagent.DockerFencer{Docker: docker},
		Logger: logger.Named("liveness"),
	}
}
