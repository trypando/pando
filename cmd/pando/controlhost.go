package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"time"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/keyfile"
)

// controlSetupRetry is how long a control host waits before trying again to
// take part in the liveness checks, when it could not.
const controlSetupRetry = 5 * time.Second

// controlPings answers pings on the cluster listener for this host's
// Monitor, once it has one, and holds the database it reads its peers from,
// once it is reached.
type controlPings struct {
	current atomic.Pointer[controlPing]
	store   atomic.Pointer[storeRef]
	// reached wakes a setup waiting to try again: the database it may have
	// been missing has just been given (useStore).
	reached chan struct{}
	// done is closed once start's Monitor has stopped, after its context
	// ends: nothing of it writes to Dir after that.
	done chan struct{}
}

type storeRef struct{ hosts.HostPublisher }

// useStore gives the Monitor the database, once it is reached.
func (c *controlPings) useStore(s hosts.HostPublisher) {
	c.store.Store(&storeRef{s})
	select {
	case c.reached <- struct{}{}:
	default: // no setup is waiting, or one is already woken
	}
}

func (c *controlPings) db() hosts.HostPublisher {
	if r := c.store.Load(); r != nil {
		return r.HostPublisher
	}
	return nil
}

// unreachedFor is this host's Monitor's word on a peer, for a person marking
// it lost (liveness.Witness): how long it has watched it without contact.
func (c *controlPings) unreachedFor(hostID string) (time.Duration, bool) {
	p := c.current.Load()
	if p == nil {
		return 0, false
	}
	return p.monitor.UnreachedFor(hostID)
}

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
		hostagent.AnswerPing(conn, p.monitor, p.members.Admits)
	}
}

// attach makes srv, the cluster listener, answer pings.
func (c *controlPings) attach(srv *http.Server) {
	srv.TLSConfig.GetConfigForClient = c.configFor
	srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){hostagent.PingProtocol: c.serve}
}

// controlLiveness is what a control host's Monitor is made from: a control
// host's half of R-421. Every host in cluster_hosts, whatever its roles, is
// watched and watches: a control host runs the same liveness.Monitor an app
// host's agent runs, in the Pando process, and answers the other hosts' pings
// on its cluster listener (server.cluster_addr), on connections that ask for
// hostagent.PingProtocol. Its row records that address and the certificate
// it answers with (hosts.ControlPeers), so the other hosts probe it there.
// When it is the one cut off it stops its own app containers, and never
// Pando or the edge (hostagent.DockerFencer).
type controlLiveness struct {
	// Store is the database's view of the cluster's hosts; nil until the
	// database is reached, when start's pings are given it (useStore).
	Store     hosts.HostPublisher
	Authority *hosts.Authority
	// Dir is server.cluster_dir, where the host's certificate is kept, and
	// what it needs to answer pings before it reaches the database: who it
	// is (controlSelfFile) and the peers it last read (hosts.PeersFile).
	Dir string
	// Port is the cluster listener's: where the host answers pings.
	Port int
	// IsSelf picks this host's row: the one it joined as, or the install's
	// first host (hosts.InstallHost).
	IsSelf func(state.Host) bool
	// Joined is the join this host made, when it made one.
	Joined *hosts.Joined
	Docker hostagent.ContainerAPI
	Logger *zap.Logger

	// stores is where the database is found once it is reached; nil is
	// Store.
	stores func() hosts.HostPublisher
}

func (c controlLiveness) store() hosts.HostPublisher {
	if c.stores != nil {
		return c.stores()
	}
	return c.Store
}

// start makes this host's Monitor and runs it until ctx ends, trying again
// every controlSetupRetry until it can: a control host that answers no ping
// would be reported unreached by every other host. It needs no database when
// it has started once before, or joined: who it is and the peers it last read
// are kept in Dir, so a control host whose Pando waits for Postgres still
// answers pings and still fences when it is the one cut off (R-421). The
// returned pings are what the cluster listener answers with (attach), and
// are given the database once it is reached (useStore).
func (c controlLiveness) start(ctx context.Context) *controlPings {
	pings := &controlPings{reached: make(chan struct{}, 1), done: make(chan struct{})}
	if c.Store != nil {
		pings.useStore(c.Store)
	}
	c.stores = pings.db
	go func() {
		defer close(pings.done)
		for {
			p, err := c.setup(ctx)
			if err == nil {
				pings.current.Store(p)
				p.monitor.Run(ctx)
				return
			}
			c.Logger.Warn("this control host cannot take part in the cluster's liveness checks yet; trying again", zap.Error(err))
			select {
			case <-ctx.Done():
				return
			case <-pings.reached:
			case <-time.After(controlSetupRetry):
			}
		}
	}()
	return pings
}

// setup finds who this host is, its certificate and the one it checks other
// hosts with, and makes its Monitor.
func (c controlLiveness) setup(ctx context.Context) (*controlPing, error) {
	self, err := c.self(ctx)
	if err != nil {
		return nil, err
	}
	// Both renewed while the host runs, before they expire (R-421).
	cert, err := hosts.RenewingControlHostCert(c.Dir, c.Authority, self.Name, self.Roles, time.Now)
	if err != nil {
		return nil, err
	}
	as := c.Authority.Authorities()
	probeCert, err := as.IssueHostClient(self.HostID)
	if err != nil {
		return nil, fmt.Errorf("could not issue the certificate this host checks the others with: %w", err)
	}
	probe := as.InMemory(probeCert)
	// R-415: the hosts removed from the cluster are refused from the start,
	// before any list is read, and from every list read after.
	members := &hosts.Members{}
	members.LoadRevoked(filepath.Join(c.Dir, hosts.RevokedFile))
	peers := &controlPeers{self: self.HostID, port: c.Port, file: filepath.Join(c.Dir, hosts.PeersFile),
		pinOf: func() string { return hostagent.Fingerprint256(cert.Current().Certificate[0]) },
		store: c.store, members: members, logger: c.Logger}
	return &controlPing{
		tls: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: cert.GetCertificate,
			ClientAuth:     tls.RequireAndVerifyClientCert,
			ClientCAs:      as.Pool(),
			NextProtos:     []string{hostagent.PingProtocol},
		},
		monitor: newMonitor(self.HostID, members.Track(peers.Peers), probe.GetClientCertificate, as.Pool(), c.Docker, c.Logger),
		members: members,
	}, nil
}

// controlSelfFile, in server.cluster_dir, is who this control host is, kept
// from the database's row so that the next start can answer pings before the
// database is reached.
const controlSelfFile = "control-host.json"

// controlSelf is who a control host is.
type controlSelf struct {
	HostID string   `json:"host_id"`
	Name   string   `json:"name"`
	Roles  []string `json:"roles"`
}

// self is this host's row, read from the database when it answers and kept;
// else what the join recorded; else what was kept.
func (c controlLiveness) self(ctx context.Context) (controlSelf, error) {
	var dbErr error
	if st := c.store(); st != nil {
		list, err := st.Hosts(ctx)
		if err == nil {
			i := slices.IndexFunc(list, c.IsSelf)
			if i < 0 {
				return controlSelf{}, errors.New("this host is not recorded in the cluster's hosts yet")
			}
			self := controlSelf{HostID: list[i].ID, Name: list[i].Name, Roles: list[i].Roles}
			c.keepSelf(self)
			return self, nil
		}
		dbErr = err
	}
	if c.Joined != nil && c.Joined.Name != "" {
		return controlSelf{HostID: c.Joined.HostID, Name: c.Joined.Name, Roles: c.Joined.Roles}, nil
	}
	var kept controlSelf
	data, err := os.ReadFile(filepath.Join(c.Dir, controlSelfFile)) // #nosec G304 -- the configured cluster directory
	if err == nil && json.Unmarshal(data, &kept) == nil && kept.HostID != "" && kept.Name != "" {
		return kept, nil
	}
	if dbErr != nil {
		return controlSelf{}, dbErr
	}
	return controlSelf{}, errors.New("this control host does not know which host of the cluster it is until it reaches the database")
}

// keepSelf writes who this host is for the next start. A write that fails
// costs only that start waiting for the database.
func (c controlLiveness) keepSelf(self controlSelf) {
	data, err := json.Marshal(self)
	if err == nil {
		err = os.MkdirAll(c.Dir, 0o700)
	}
	if err == nil {
		err = keyfile.Replace(filepath.Join(c.Dir, controlSelfFile), data)
	}
	if err != nil {
		c.Logger.Warn("could not keep which host this is for the next start", zap.Error(err))
	}
}

// controlPeers is a control host's peer list: read from the database once
// it is reached (hosts.ControlPeers), and kept in file, which is read until
// then.
type controlPeers struct {
	self string
	port int
	// pinOf is the fingerprint of the certificate the host answers with
	// now, which is renewed while it runs (R-421).
	pinOf func() string
	file  string
	store func() hosts.HostPublisher
	// members is given the hosts removed from the cluster with each list
	// read from the database (R-415).
	members *hosts.Members
	logger  *zap.Logger
}

func (c *controlPeers) Peers(ctx context.Context) ([]hosts.Peer, error) {
	st := c.store()
	if st == nil {
		return hosts.ReadPeers(c.file)
	}
	list, err := (&hosts.ControlPeers{Store: st, Self: c.self, Port: c.port, PinOf: c.pinOf, Logger: c.logger}).Peers(ctx)
	if err != nil {
		return nil, err
	}
	if err := hosts.WritePeers(c.file, list); err != nil {
		c.logger.Warn("could not keep the cluster's hosts for the next start", zap.Error(err))
	}
	c.keepRevoked(ctx, st)
	return list, nil
}

// keepRevoked gives members the hosts removed from the cluster, and keeps
// them for the next start (R-415). One that cannot be read now is read with
// the next list; what was revoked before stays revoked meanwhile.
func (c *controlPeers) keepRevoked(ctx context.Context, st hosts.HostPublisher) {
	if c.members == nil {
		return
	}
	revoked, err := st.Revoked(ctx)
	if err == nil {
		err = c.members.KeepRevoked(filepath.Join(filepath.Dir(c.file), hosts.RevokedFile), revoked)
	}
	if err != nil {
		c.logger.Warn("could not read or keep the hosts removed from the cluster; trying again next round", zap.Error(err))
	}
}

// startControlLiveness runs this control host's Monitor (R-421), before the
// database is reached when it can. joined is the join this host made, when it
// made one; otherwise it is the install's first host.
func startControlLiveness(ctx context.Context, dir string, port int, authority *hosts.Authority,
	joined hosts.Joined, hasJoined bool, logger *zap.Logger) *controlPings {
	c := controlLiveness{Authority: authority, Dir: dir, Port: port, IsSelf: selfHost(joined, hasJoined), Logger: logger.Named("liveness")}
	if hasJoined {
		c.Joined = &joined
	}
	// The runtime's own Docker: what this host's app containers run on. One
	// that cannot be reached still leaves the host answering pings, since
	// a host that answers none would be taken for lost while it runs; its
	// fence then fails, and says so, every round it is cut off.
	cli, err := client.New(client.FromEnv)
	c.Docker = cli
	if err != nil {
		c.Docker = unreachableRuntime{err: err}
	}
	return c.start(ctx)
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

func (u unreachableRuntime) Reachable(context.Context) error {
	return fmt.Errorf("this host's container runtime could not be reached: %w", u.err)
}

func (u unreachableRuntime) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	return client.ContainerStopResult{}, fmt.Errorf("this host's container runtime could not be reached: %w", u.err)
}
