package main

import (
	"context"
	"encoding/json"
	"slices"
	"sync"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
)

// What keeps the control plane from undoing a fence (R-421, R-148): this
// host's own Monitor, asked by the reconciler, the standby pass and a joined
// host's agent; and every host's answer, recorded by the leader so every
// replica's reconciler leaves a fenced host's apps stopped.

// fenced reports whether this control host's Monitor has fenced. False until
// the Monitor runs: a host that has not started watching has not fenced.
func (c *controlPings) fenced() bool {
	if c == nil {
		return false
	}
	p := c.current.Load()
	return p != nil && p.monitor.Fenced()
}

// Fenced is fenced, for hostagent.Fence.
func (c *controlPings) Fenced() bool { return c.fenced() }

// hostFences records whether a host says it is fenced: state.Cluster.
type hostFences interface {
	SetHostFenced(ctx context.Context, hostID string, fenced bool) (bool, error)
}

// fenceRecorder records each host's fence as the leader's probes hear it,
// writing only when it changed. A host the leader does not reach keeps what
// it last said.
type fenceRecorder struct {
	store  hostFences
	logger *zap.Logger
	mu     sync.Mutex
	last   map[string]bool
}

// wrap is probe, recording the fence each answer reports.
func (f *fenceRecorder) wrap(probe func(context.Context, liveness.Peer) ([]byte, error)) func(context.Context, liveness.Peer) ([]byte, error) {
	return func(ctx context.Context, p liveness.Peer) ([]byte, error) {
		raw, err := probe(ctx, p)
		if err == nil {
			f.record(ctx, p.ID, raw)
		}
		return raw, err
	}
}

func (f *fenceRecorder) record(ctx context.Context, hostID string, raw []byte) {
	var v liveness.View
	if json.Unmarshal(raw, &v) != nil || v.Host != hostID {
		return
	}
	f.mu.Lock()
	was, known := f.last[hostID]
	f.mu.Unlock()
	if known && was == v.Fenced {
		return
	}
	if _, err := f.store.SetHostFenced(ctx, hostID, v.Fenced); err != nil {
		f.logger.Warn("could not record whether a host is fenced; trying again next round",
			zap.String("host_id", hostID), zap.Error(err))
		return
	}
	f.mu.Lock()
	if f.last == nil {
		f.last = map[string]bool{}
	}
	f.last[hostID] = v.Fenced
	f.mu.Unlock()
}

// startJoinedAgent runs the agent of a host that joined with both the
// control and app roles, beside its server (R-416): the runtime reaches the
// host's apps only through it, at the address the host joined with. Its pings
// are answered on the cluster listener (controlhost.go), so the agent answers
// none, and it refuses to start containers while the control host's Monitor
// has fenced (R-421). A host with the control role alone runs no agent.
func startJoinedAgent(ctx context.Context, dir string, joined hosts.Joined, pings *controlPings, logger *zap.Logger) {
	if !slices.Contains(joined.Roles, state.HostRoleApp) || joined.AppOnly() {
		return
	}
	agent, _, err := appHostAgent(dir, joined, logger)
	if err != nil {
		logger.Error("this host joined with the app role and cannot run its agent, so it runs no apps", zap.Error(err))
		return
	}
	agent.Fence = pings
	go func() {
		if err := serveAgent(ctx, agent, nil, agentListen()); err != nil {
			logger.Error("this host's agent stopped, so it runs no apps", zap.Error(err))
		}
	}()
}
