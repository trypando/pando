package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
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
// the name the host joined under. local is this control host's own Monitor's
// word on a host (controlPings.unreachedFor), which a person marking a host
// lost is checked against with the rest (liveness.Witness). timing is the
// cluster's (cluster.liveness_period), the same on every host.
func clusterLiveness(store clusterStore, authority *hosts.Authority, apps *state.Apps, auditor liveness.Auditor,
	local func(string) (time.Duration, bool), timing liveness.Config, logger *zap.Logger) (*liveness.Service, *liveness.Watch) {
	svc := &liveness.Service{Store: store, Apps: apps, Audit: auditor, Logger: logger.Named("liveness")}

	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	// The collector's own certificate: it names no host, so its pings are
	// recorded as no host's (hostagent.ProbeSender).
	cert, err := as.IssueCollector()
	if err != nil {
		logger.Error("could not issue the certificate hosts are checked with; no host is watched", zap.Error(err))
		return svc, nil
	}
	tlsFor := func(p liveness.Peer) *tls.Config { return hostagent.ClientTLS(as, cert, p.Name) }
	probe := func(ctx context.Context, p liveness.Peer) ([]byte, error) {
		return probeResult(hostagent.Ping(ctx, p.Address, tlsFor(p), ""))
	}
	peers := func(ctx context.Context) ([]liveness.Peer, error) {
		list, err := store.Hosts(ctx)
		if err != nil {
			return nil, err
		}
		// Every host, the install's first included: control hosts answer
		// on their cluster listeners (controlhost.go).
		return livenessPeers(hosts.LivenessPeers(list)), nil
	}

	// A host heard from lately is not gone, and is not marked lost by hand.
	svc.Witness = &liveness.Witness{Hosts: peers, Probe: probe, Local: local, Config: timing}

	// What each host says of its fence is recorded, so every replica's
	// reconciler leaves a fenced host's apps stopped (R-421, clusterholds.go).
	watchProbe := probe
	if fences, ok := store.(hostFences); ok {
		watchProbe = (&fenceRecorder{store: fences, logger: logger.Named("liveness")}).wrap(probe)
	}
	return svc, &liveness.Watch{Hosts: peers, Probe: watchProbe, Service: svc, Config: timing, Logger: logger.Named("liveness")}
}

// livenessTiming is the cluster's timing as liveness reads it (R-421).
func livenessTiming(c config.Cluster) liveness.Config {
	return liveness.Config{Period: c.LivenessPeriod, Interval: c.LivenessInterval}.Normalize()
}

// probeResult is a ping's outcome as liveness reads it: a view, contact
// without one (the machine answered, though its Pando did not give a view),
// or silence (R-421).
func probeResult(view []byte, err error) ([]byte, error) {
	if err != nil && hostagent.Contacted(err) {
		return nil, fmt.Errorf("%w: %w", liveness.ErrContactOnly, err)
	}
	return view, err
}

// livenessPeers is list as liveness probes it.
func livenessPeers(list []hosts.Peer) []liveness.Peer {
	out := make([]liveness.Peer, len(list))
	for i, p := range list {
		out[i] = liveness.Peer{ID: p.ID, Name: p.Name, Address: p.Address, Lost: p.Lost}
	}
	return out
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
