package main

import (
	"context"
	"sync"
	"time"

	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/hostagent"
)

// The rule every host that answers the cluster's pings is held to (R-153,
// R-421): it answers only while it could stop its own apps if it were cut
// off. A host that answers is reached; a host that could not fence and is
// reached is never taken for lost, so no standby starts beside its apps; and
// a refusal is contact too. What a host that cannot fence gives up is its
// view of the others. An app host's fallback (appHostFallback) and every
// control host (controlLiveness) answer through a fenceGate.

// fenceGateEvery is how long a fenceGate takes the runtime's last answer
// for: one probe interval [P].
var fenceGateEvery = liveness.Config{}.Normalize().Interval

// fenceGate answers pings with liveness's view only while this host's
// runtime answers, so only while the host could stop its apps (R-153).
// Otherwise the answer is nil, which the agent sends as a refusal: contact
// all the same (R-421), so the host is never taken for lost while it can be
// reached, and it says nothing of the others it could not act on.
type fenceGate struct {
	liveness  hostagent.Liveness
	reachable func(context.Context) error
	every     time.Duration

	mu      sync.Mutex
	checked time.Time
	ok      bool
}

func (g *fenceGate) Answer(from string) []byte {
	if !g.canFence() {
		return nil
	}
	return g.liveness.Answer(from)
}

func (g *fenceGate) canFence() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.checked.IsZero() && time.Since(g.checked) < g.every {
		return g.ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	g.ok, g.checked = g.reachable(ctx) == nil, time.Now()
	return g.ok
}

// runtimeAPI is the host's container runtime as the fallback stops apps
// through it.
type runtimeAPI interface {
	hostagent.ContainerAPI
	Reachable(ctx context.Context) error
}

// localRuntime is the host's own runtime, at the socket appHostRuntimeSocket
// names; when it names none, a runtime every call to which fails with why.
func localRuntime() runtimeAPI {
	socket, err := appHostRuntimeSocket()
	if err != nil {
		return unreachableRuntime{err: err}
	}
	docker, err := client.New(client.WithHost("unix://" + socket))
	if err != nil {
		return unreachableRuntime{err: err}
	}
	return dockerRuntime{docker}
}

// dockerRuntime is the Docker API at a socket, which may not answer.
type dockerRuntime struct{ *client.Client }

func (d dockerRuntime) Reachable(ctx context.Context) error {
	_, err := d.Ping(ctx, client.PingOptions{})
	return err
}
