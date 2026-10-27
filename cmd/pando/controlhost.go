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

	dockerruntime "github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
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
	// runtimeKind is the kind of the install's default runtime, once known
	// (useRuntime), and dir where it is kept for the next start.
	runtimeKind atomic.Pointer[string]
	dir         string
}

// controlRuntimeFile, in server.cluster_dir, is the kind of the install's
// runtime as this control host last heard it, so that the next start knows,
// before the database is reached, whether apps run here (placesApps).
const controlRuntimeFile = "control-runtime.json"

// useRuntime records the kind of the install's default runtime, "" for
// none, and keeps it for the next start. A write that fails costs only that
// start refusing pings, as one that does not know yet does, until the
// database is reached.
func (c *controlPings) useRuntime(kind string) {
	c.runtimeKind.Store(&kind)
	if c.dir == "" {
		return
	}
	if data, err := json.Marshal(map[string]string{"kind": kind}); err == nil {
		_ = keyfile.Replace(filepath.Join(c.dir, controlRuntimeFile), data)
	}
}

// loadRuntime reads the kind kept by useRuntime. Nothing kept leaves it
// unknown.
func (c *controlPings) loadRuntime() {
	data, err := os.ReadFile(filepath.Join(c.dir, controlRuntimeFile)) // #nosec G304 -- the configured cluster directory
	if err != nil {
		return
	}
	var kept struct {
		Kind *string `json:"kind"`
	}
	if json.Unmarshal(data, &kept) == nil && kept.Kind != nil {
		c.runtimeKind.Store(kept.Kind)
	}
}

// kind is the install's runtime's kind, and whether it is known.
func (c *controlPings) kind() (string, bool) {
	if k := c.runtimeKind.Load(); k != nil {
		return *k, true
	}
	return "", false
}

// placesApps reports whether the install's runtime places apps on a control
// host with roles, so that its Monitor has something of its own to fence
// (R-153). Docker, on one host or many, runs apps on the install's own host
// and on a host joined with the app role; Kubernetes and Incus place them
// across their own machines and never on this one through its Docker, and
// take no app-role join (hosts.CheckAppRoleJoin); with no runtime nothing is
// placed anywhere. A host with the control role alone runs none. Not known
// yet — the first start, before the database — is taken as placing them:
// the host then refuses pings while its Docker is away, which is contact and
// never makes it lost, rather than say it is well with apps it cannot stop.
func placesApps(kind string, known bool, roles []string) bool {
	if !slices.Contains(roles, state.HostRoleApp) {
		return false
	}
	return !known || kind == dockerruntime.Kind || kind == multidocker.Kind
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
	// answer is monitor's view, given only while this host could stop its
	// own apps (fenceGate, R-153).
	answer hostagent.Liveness
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
		hostagent.AnswerPing(conn, p.answer, p.members.Admits)
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
	// Docker is the runtime this host's fence stops its apps through.
	Docker runtimeAPI
	// Timing is the cluster's (cluster.liveness_period); zero is the
	// default.
	Timing liveness.Config
	Logger *zap.Logger
	// kind is the install's runtime's kind, and whether it is known
	// (controlPings.kind); nil is not known.
	kind func() (string, bool)

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
	pings := &controlPings{reached: make(chan struct{}, 1), done: make(chan struct{}), dir: c.Dir}
	pings.loadRuntime()
	c.kind = pings.kind
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
	monitor := newMonitor(self.HostID, members.Track(peers.Peers), probe.GetClientCertificate, as.Pool(), c.Docker, c.Timing, c.Logger)
	return &controlPing{
		answer: &fenceGate{liveness: monitor, reachable: c.fenceable(self.Roles), every: fenceGateEvery(c.Timing)},
		tls: &tls.Config{
			MinVersion:     tls.VersionTLS13,
			GetCertificate: cert.GetCertificate,
			ClientAuth:     tls.RequireAndVerifyClientCert,
			ClientCAs:      as.Pool(),
			NextProtos:     []string{hostagent.PingProtocol},
		},
		monitor: monitor,
		members: members,
	}, nil
}

// fenceable is whether a control host with roles could stop its own apps:
// nil when the install's runtime places none here (placesApps), else
// whether its Docker answers.
func (c controlLiveness) fenceable(roles []string) func(context.Context) error {
	return func(ctx context.Context) error {
		kind, known := "", false
		if c.kind != nil {
			kind, known = c.kind()
		}
		if !placesApps(kind, known, roles) {
			return nil
		}
		return c.Docker.Reachable(ctx)
	}
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
	joined hosts.Joined, hasJoined bool, timing liveness.Config, logger *zap.Logger) *controlPings {
	c := controlLiveness{Authority: authority, Dir: dir, Port: port, IsSelf: selfHost(joined, hasJoined), Timing: timing, Logger: logger.Named("liveness")}
	if hasJoined {
		c.Joined = &joined
	}
	// The runtime's own Docker: what this host's app containers run on.
	// While it cannot be reached, a host it places apps on refuses pings
	// (fenceGate): contact all the same, so it is not taken for lost while
	// it can be reached, and it never says it is well with apps it could not
	// stop (R-153).
	cli, err := fenceClient(joined, hasJoined)
	c.Docker = dockerRuntime{cli}
	if err != nil {
		c.Docker = unreachableRuntime{err: err}
	}
	return c.start(ctx)
}

// fenceClient is the Docker a control host's fence stops apps through: the
// one its apps run on. A joined host with the app role runs them through its
// agent's rootless socket (appHostRuntimeSocket), which DOCKER_HOST need not
// name; checking or fencing any other runtime would stop nothing while its
// apps ran (R-153). The install's own host runs them on the runtime Pando
// reads from its environment.
func fenceClient(joined hosts.Joined, hasJoined bool) (*client.Client, error) {
	if hasJoined && slices.Contains(joined.Roles, state.HostRoleApp) {
		sock, err := appHostRuntimeSocket()
		if err != nil {
			return nil, err
		}
		return client.New(client.WithHost("unix://" + sock))
	}
	return client.New(client.FromEnv)
}

// selfHost picks this host's row: the one it joined as, or else the
// install's first host.
func selfHost(joined hosts.Joined, hasJoined bool) func(state.Host) bool {
	if hasJoined {
		return func(h state.Host) bool { return h.ID == joined.HostID }
	}
	return hosts.InstallHost
}

// routeSyncHost is what the replicas of this host share for route sync's
// one pass at a time (R-417): the ID it joined as, or one name for every
// replica of the install's first host.
func routeSyncHost(joined hosts.Joined, hasJoined bool) string {
	if hasJoined {
		return joined.HostID
	}
	return "install"
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
