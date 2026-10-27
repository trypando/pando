package main

import (
	"context"
	"crypto/tls"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

// clusterLiveness builds R-421's control-plane half: the service that takes a
// host for lost and starts its standbys, and the leader job that watches the
// hosts. Homes and Failover are left for the standby slice to set (R-419);
// until they are, a lost host is marked and nothing is started.
//
// Probes are made with a host certificate issued from the cluster authority,
// which an agent answers with a ping and nothing else, and verified against
// the name the host joined under.
func clusterLiveness(store clusterStore, authority *hosts.Authority, apps *state.Apps, auditor liveness.Auditor, logger *zap.Logger) (*liveness.Service, *liveness.Watch) {
	svc := &liveness.Service{Store: store, Apps: apps, Audit: auditor, Logger: logger.Named("liveness")}

	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	cert, err := as.IssueHostClient()
	if err != nil {
		logger.Error("could not issue the certificate hosts are checked with; no host is watched", zap.Error(err))
		return svc, nil
	}
	tlsFor := func(p liveness.Peer) *tls.Config { return hostagent.ClientTLS(as, cert, p.Name) }
	probe := func(ctx context.Context, p liveness.Peer) ([]byte, error) {
		return hostagent.Ping(ctx, p.Address, tlsFor(p), "")
	}
	peers := func(ctx context.Context) ([]liveness.Peer, error) {
		list, err := store.Hosts(ctx)
		if err != nil {
			return nil, err
		}
		out := make([]liveness.Peer, len(list))
		for i, h := range list {
			out[i] = liveness.Peer{ID: h.ID, Name: h.Name, Address: h.Address}
		}
		return out, nil
	}

	// A host that answers is not gone, and is not marked lost by hand.
	svc.Answering = func(ctx context.Context, hostID string) bool {
		h, err := store.Host(ctx, hostID)
		if err != nil {
			return false
		}
		ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		_, err = probe(ctx, liveness.Peer{ID: h.ID, Name: h.Name, Address: h.Address})
		return err == nil
	}

	return svc, &liveness.Watch{Hosts: peers, Probe: probe, Service: svc, Logger: logger.Named("liveness")}
}

// clusterStore is the slice of state.Cluster the liveness wiring reads.
type clusterStore interface {
	liveness.Store
	Hosts(ctx context.Context) ([]state.Host, error)
}

var (
	_ liveness.Auditor = (*audit.Writer)(nil)
	_ clusterStore     = (*state.Cluster)(nil)
)
