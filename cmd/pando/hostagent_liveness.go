package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

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
// nothing here. members, when given, hears each peer list read, and the hosts
// removed from the cluster kept beside it (hosts.RevokedFile), so that the
// agent refuses a removed host's pings (R-415).
func agentLiveness(authoritiesPEM []byte, members *hosts.Members, timing liveness.Config, logger *zap.Logger) (*liveness.Monitor, error) {
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
	peers := func(context.Context) ([]hosts.Peer, error) { return hosts.ReadPeers(peersFile) }
	if members != nil {
		revoked := filepath.Join(filepath.Dir(peersFile), hosts.RevokedFile)
		members.LoadRevoked(revoked)
		read := peers
		peers = members.Track(func(ctx context.Context) ([]hosts.Peer, error) {
			members.LoadRevoked(revoked)
			return read(ctx)
		})
	}
	return newMonitor(self, peers, staticCert(cert), roots, docker, timing, logger), nil
}

// staticCert presents cert at every handshake: for an agent given its
// certificate by whoever started it, which re-creates it to renew it.
func staticCert(cert tls.Certificate) func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return &cert, nil }
}

// newMonitor is a joined host's Monitor (R-421): it probes the hosts peers
// lists with the certificate clientCert gives at each probe, which is renewed
// while the host runs, checking each agent against roots and the name it
// joined under, and fences through docker, on the cluster's timing.
func newMonitor(self string, peers func(context.Context) ([]hosts.Peer, error),
	clientCert func(*tls.CertificateRequestInfo) (*tls.Certificate, error),
	roots *x509.CertPool, docker hostagent.ContainerAPI, timing liveness.Config, logger *zap.Logger) *liveness.Monitor {
	return &liveness.Monitor{
		Self:   self,
		Config: timing,
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
