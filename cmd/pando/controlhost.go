package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

// A control host's half of R-421. Every host in cluster_hosts, whatever its
// roles, is watched and watches: a control host runs the same
// liveness.Monitor an app host's agent runs, in the Pando process, and
// answers the other hosts' pings on its cluster listener
// (server.cluster_addr), on connections that ask for
// hostagent.PingProtocol. Its row records that address and the certificate
// it answers with (hosts.ControlPeers), so the other hosts probe it there.
// When it is the one cut off it stops its own app containers, and never
// Pando or the edge (hostagent.DockerFencer).

// controlSetupRetry is how long a control host waits before trying again to
// take part in the liveness checks, when it could not.
const controlSetupRetry = 5 * time.Second

// controlPings answers pings on the cluster listener for this host's
// Monitor, once it has one.
type controlPings struct{ current atomic.Pointer[controlPing] }

// controlPing is the TLS a ping is answered with, and the Monitor that
// answers it.
type controlPing struct {
	tls     *tls.Config
	monitor *liveness.Monitor
	// members is the cluster's hosts as this one last read them: a removed
	// host's pings are refused (R-415).
	members *hosts.Members
}

var errNotWatching = errors.New("this control host is not answering the cluster's liveness checks yet")

// configFor is the cluster listener's GetConfigForClient: a connection asking
// for PingProtocol is served with the host's own certificate and must present
// one from the cluster authority; any other is the listener's as before.
func (c *controlPings) configFor(hello *tls.ClientHelloInfo) (*tls.Config, error) {
	if !slices.Contains(hello.SupportedProtos, hostagent.PingProtocol) {
		return nil, nil
	}
	p := c.current.Load()
	if p == nil {
		return nil, errNotWatching
	}
	return p.tls, nil
}

// serve answers a connection that negotiated PingProtocol.
func (c *controlPings) serve(_ *http.Server, conn *tls.Conn, _ http.Handler) {
	if p := c.current.Load(); p != nil {
		hostagent.AnswerPing(conn, p.monitor, p.members.Has)
	}
}

// attach makes srv, the cluster listener, answer pings.
func (c *controlPings) attach(srv *http.Server) {
	srv.TLSConfig.GetConfigForClient = c.configFor
	srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){hostagent.PingProtocol: c.serve}
}

// controlLiveness is what a control host's Monitor is made from.
type controlLiveness struct {
	Store     hosts.HostPublisher
	Authority *hosts.Authority
	// Dir is server.cluster_dir, where the host's certificate is kept.
	Dir string
	// Port is the cluster listener's: where the host answers pings.
	Port int
	// IsSelf picks this host's row: the one it joined as, or the install's
	// first host (hosts.InstallHost).
	IsSelf func(state.Host) bool
	Docker hostagent.ContainerAPI
	Logger *zap.Logger
}

// start makes this host's Monitor and runs it until ctx ends, trying again
// every controlSetupRetry until it can: a control host that answers no ping
// would be reported unreached by every other host. The returned pings are
// what the cluster listener answers with (attach).
func (c controlLiveness) start(ctx context.Context) *controlPings {
	pings := &controlPings{}
	go func() {
		for {
			p, err := c.setup(ctx)
			if err == nil {
				pings.current.Store(p)
				p.monitor.Run(ctx)
				return
			}
			c.Logger.Error("this control host cannot take part in the cluster's liveness checks yet; trying again", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(controlSetupRetry):
			}
		}
	}()
	return pings
}

// setup finds this host's row, its certificate and the one it checks other
// hosts with, and makes its Monitor, peers read from the database.
func (c controlLiveness) setup(ctx context.Context) (*controlPing, error) {
	list, err := c.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	i := slices.IndexFunc(list, c.IsSelf)
	if i < 0 {
		return nil, errors.New("this host is not recorded in the cluster's hosts yet")
	}
	self := list[i]
	// Both renewed while the host runs, before they expire (R-421).
	cert, err := hosts.RenewingControlHostCert(c.Dir, c.Authority, self.Name, self.Roles, time.Now)
	if err != nil {
		return nil, err
	}
	as := c.Authority.Authorities()
	probeCert, err := as.IssueHostClient(self.ID)
	if err != nil {
		return nil, fmt.Errorf("could not issue the certificate this host checks the others with: %w", err)
	}
	probe := as.InMemory(probeCert)
	peers := &hosts.ControlPeers{
		Store: c.Store, Self: self.ID, Port: c.Port, Logger: c.Logger,
		PinOf: func() string { return hostagent.Fingerprint256(cert.Current().Certificate[0]) },
	}
	members := &hosts.Members{}
	return &controlPing{
		tls: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: cert.GetCertificate,
			ClientAuth:     tls.RequireAndVerifyClientCert,
			ClientCAs:      as.Pool(),
			NextProtos:     []string{hostagent.PingProtocol},
		},
		monitor: newMonitor(self.ID, members.Track(peers.Peers), probe.GetClientCertificate, as.Pool(), c.Docker, c.Logger),
		members: members,
	}, nil
}

// startControlLiveness runs this control host's Monitor (R-421). joined is
// the join this host made, when it made one; otherwise it is the install's
// first host.
func startControlLiveness(ctx context.Context, dir string, port int, store hosts.HostPublisher, authority *hosts.Authority,
	joined hosts.Joined, hasJoined bool, logger *zap.Logger) *controlPings {
	isSelf := selfHost(joined, hasJoined)
	// The runtime's own Docker: what this host's app containers run on. One
	// that cannot be reached still leaves the host answering pings, since
	// a host that answers none would be taken for lost while it runs; its
	// fence then fails, and says so, every round it is cut off.
	cli, err := client.New(client.FromEnv)
	var docker hostagent.ContainerAPI = cli
	if err != nil {
		docker = unreachableRuntime{err: err}
	}
	return controlLiveness{
		Store: store, Authority: authority, Dir: dir, Port: port, IsSelf: isSelf, Docker: docker,
		Logger: logger.Named("liveness"),
	}.start(ctx)
}

// selfHost picks this host's row: the one it joined as, or else the
// install's first host.
func selfHost(joined hosts.Joined, hasJoined bool) func(state.Host) bool {
	if hasJoined {
		return func(h state.Host) bool { return h.ID == joined.HostID }
	}
	return hosts.InstallHost
}

// unreachableRuntime is a container runtime this host could not connect to:
// every call fails with why.
type unreachableRuntime struct{ err error }

func (u unreachableRuntime) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	return client.ContainerListResult{}, fmt.Errorf("this host's container runtime could not be reached: %w", u.err)
}

func (u unreachableRuntime) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, fmt.Errorf("this host's container runtime could not be reached: %w", u.err)
}
