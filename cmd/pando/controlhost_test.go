package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/id"
)

// The cluster of R-421 as the hosts run it: the install's first host and a
// joined control host each answering pings on a real cluster listener with
// the Monitor startControlLiveness makes, and an app host answering on its
// agent, all over TLS on loopback. Only the network between them (cut) and
// the clock are simulated, and each host's Docker, which counts the app
// containers its fence stops.

// rows is cluster_hosts, as every control host reads it from the shared
// database. down is Postgres being away.
type rows struct {
	mu     sync.Mutex
	hosts  []state.Host
	lost   map[string]bool
	fences map[string]string
	down   bool
}

var errDatabaseAway = errors.New("the database is away")

func (r *rows) Hosts(context.Context) ([]state.Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return nil, errDatabaseAway
	}
	return slices.Clone(r.hosts), nil
}

func (r *rows) row(hostID string) (int, error) {
	if r.down {
		return 0, errDatabaseAway
	}
	i := slices.IndexFunc(r.hosts, func(h state.Host) bool { return h.ID == hostID })
	if i < 0 {
		return 0, errors.New("no such host")
	}
	return i, nil
}

func (r *rows) SetHostAgent(_ context.Context, hostID, address, pin string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.row(hostID)
	if err == nil {
		r.hosts[i].Address, r.hosts[i].CertSHA256 = address, pin
	}
	return err
}

func (r *rows) Host(_ context.Context, hostID string) (state.Host, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	i, err := r.row(hostID)
	if err != nil {
		return state.Host{}, err
	}
	return r.hosts[i], nil
}

func (r *rows) MarkHostLost(_ context.Context, hostID, _ string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.row(hostID); err != nil {
		return false, err
	}
	newly := !r.lost[hostID]
	r.lost[hostID] = true
	return newly, nil
}

func (r *rows) ClearHostLost(_ context.Context, hostID string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.lost, hostID)
	return nil
}

func (r *rows) LostHosts(context.Context) (map[string]bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.down {
		return nil, errDatabaseAway
	}
	out := map[string]bool{}
	for k := range r.lost {
		out[k] = true
	}
	return out, nil
}

func (r *rows) RecordHostFence(_ context.Context, hostID, fenceID string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	newly := r.fences[hostID] != fenceID
	r.fences[hostID] = fenceID
	return newly, nil
}

func (r *rows) lostIDs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for k := range r.lost {
		out = append(out, k)
	}
	return out
}

// appDocker is a host's Docker with one running app container, and a
// count of the stops a fence made.
type appDocker struct {
	mu      sync.Mutex
	stopped int
}

func (d *appDocker) ContainerList(context.Context, client.ContainerListOptions) (client.ContainerListResult, error) {
	labels := map[string]string{"io.pando.managed": "true", "io.pando.app": "app_1"}
	return client.ContainerListResult{Items: []container.Summary{{ID: "c1", Labels: labels}}}, nil
}

func (d *appDocker) ContainerStop(context.Context, string, client.ContainerStopOptions) (client.ContainerStopResult, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.stopped++
	return client.ContainerStopResult{}, nil
}

func (d *appDocker) stops() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopped
}

// simHost is one host of the cluster: its Monitor and its Docker.
type simHost struct {
	id       string
	name     string
	control  bool
	listener string
	monitor  *liveness.Monitor
	docker   *appDocker
}

type simCluster struct {
	t         *testing.T
	clock     *clock.Fake
	rows      *rows
	authority *hosts.Authority
	hosts     []*simHost
	watches   map[string]*liveness.Watch

	mu  sync.Mutex
	cut map[string]bool
}

func (c *simCluster) reachable(from, to string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.cut[from] && !c.cut[to]
}

func (c *simCluster) setCut(hostID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cut[hostID] = true
}

var errCut = errors.New("the network between these hosts is cut")

// throughNetwork makes a Monitor's probes cross the simulated network.
func (c *simCluster) throughNetwork(h *simHost) {
	probe := h.monitor.Probe
	h.monitor.Probe = func(ctx context.Context, p liveness.Peer) error {
		if !c.reachable(h.id, p.ID) {
			return errCut
		}
		return probe(ctx, p)
	}
	h.monitor.Clock = c.clock
}

func portOf(t *testing.T, addr string) int {
	t.Helper()
	_, port, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	n, err := net.LookupPort("tcp", port)
	require.NoError(t, err)
	return n
}

// newSimCluster is a first host (core-1), a joined control host (core-2) and
// an app host (app-1), recorded as a join and the first start record them:
// the control hosts at the agent port, the first host with no pin.
func newSimCluster(t *testing.T) *simCluster {
	t.Helper()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	c := &simCluster{
		t: t, clock: clock.NewFake(time.Time{}), authority: authority, cut: map[string]bool{},
		rows:    &rows{lost: map[string]bool{}, fences: map[string]string{}},
		watches: map[string]*liveness.Watch{},
	}
	first := &simHost{id: id.New(id.Host), name: "core-1", control: true, docker: &appDocker{}}
	core2 := &simHost{id: id.New(id.Host), name: "core-2", control: true, docker: &appDocker{}}
	app := &simHost{id: id.New(id.Host), name: "app-1", docker: &appDocker{}}
	c.hosts = []*simHost{first, core2, app}

	core2Dir := filepath.Join(t.TempDir(), "cluster")
	core2Roles := []string{state.HostRoleControl}
	_, core2Pin, err := hosts.ControlHostCert(core2Dir, authority, core2.name, core2Roles, time.Now())
	require.NoError(t, err, "what the join wrote")
	appAddr, appPin := c.startAppHost(app)
	c.rows.hosts = []state.Host{
		{ID: first.id, Name: first.name, Roles: []string{state.HostRoleControl, state.HostRoleApp}, Address: "127.0.0.1:7443", CertSHA256: strings.Repeat("0", 64)},
		{ID: core2.id, Name: core2.name, Roles: core2Roles, Address: "127.0.0.1:7443", CertSHA256: core2Pin, JoinedWith: "jt_1"},
		{ID: app.id, Name: app.name, Roles: []string{state.HostRoleApp}, Address: appAddr, CertSHA256: appPin, JoinedWith: "jt_2"},
	}
	c.startControlHost(first, filepath.Join(t.TempDir(), "cluster"), hosts.InstallHost)
	c.startControlHost(core2, core2Dir, func(h state.Host) bool { return h.ID == core2.id })
	return c
}

// startControlHost runs a control host's cluster listener with the Monitor
// controlLiveness makes, and its leader job, as the server does.
func (c *simCluster) startControlHost(h *simHost, dir string, isSelf func(state.Host) bool) {
	t := c.t
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p, err := controlLiveness{
		Store: c.rows, Authority: c.authority, Dir: dir, Port: portOf(t, addr), IsSelf: isSelf,
		Docker: h.docker, Logger: zap.NewNop(),
	}.setup(ctx)
	require.NoError(t, err)
	h.monitor, h.listener = p.monitor, addr
	c.throughNetwork(h)
	pings := &controlPings{}
	pings.current.Store(p)
	srv, err := startClusterListener(&config.Config{Server: config.Server{ClusterAddr: addr}}, c.authority, http.NotFoundHandler(), pings, zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = srv.Close() })

	_, watch := clusterLiveness(c.rows, c.authority, nil, nil, zap.NewNop())
	probe := watch.Probe
	watch.Probe = func(ctx context.Context, p liveness.Peer) ([]byte, error) {
		if !c.reachable(h.id, p.ID) {
			return nil, errCut
		}
		return probe(ctx, p)
	}
	watch.Clock = c.clock
	c.watches[h.id] = watch
}

// startAppHost runs an app host's agent with its Monitor, peers as the
// cluster listener lists them, and returns its address and pin.
func (c *simCluster) startAppHost(h *simHost) (string, string) {
	t := c.t
	as := hostagent.Authorities{{Cert: c.authority.Cert, Key: c.authority.Key}}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := as.IssueHost(h.name, []string{state.HostRoleApp}, &key.PublicKey)
	require.NoError(t, err)
	probeCert, err := as.IssueHostClient()
	require.NoError(t, err)
	peers := func(ctx context.Context) ([]hosts.Peer, error) {
		list, err := c.rows.Hosts(ctx)
		if err != nil {
			return nil, err
		}
		return hosts.LivenessPeers(list), nil
	}
	h.monitor = newMonitor(h.id, peers, probeCert, as.Pool(), h.docker, zap.NewNop())
	c.throughNetwork(h)
	agent := &hostagent.Agent{
		TLS: &tls.Config{
			MinVersion:   tls.VersionTLS13,
			Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
			ClientAuth:   tls.RequireAndVerifyClientCert, ClientCAs: as.Pool(),
		},
		Pool: netip.MustParsePrefix("10.213.0.0/16"), Liveness: h.monitor,
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String(), hostagent.Fingerprint256(der)
}

// leader is the control host leading: the first host, unless it is cut off,
// when its lease passes to the other control host.
func (c *simCluster) leader() string {
	for _, h := range c.hosts {
		if h.control && c.reachable(h.id, h.id) {
			return h.id
		}
	}
	return ""
}

// run steps the cluster one probe interval at a time for d.
func (c *simCluster) run(d time.Duration) {
	ctx := context.Background()
	interval := liveness.Config{}.Normalize().Interval
	for elapsed := time.Duration(0); elapsed < d; elapsed += interval {
		c.clock.Advance(interval)
		for _, h := range c.hosts {
			h.monitor.Round(ctx)
		}
		if l := c.leader(); l != "" {
			c.watches[l].Round(ctx)
		}
	}
}

func (c *simCluster) fenced() []string {
	var out []string
	for _, h := range c.hosts {
		if h.monitor.Fenced() || h.docker.stops() > 0 {
			out = append(out, h.name)
		}
	}
	return out
}

func (c *simCluster) lost() []string {
	var out []string
	for _, x := range c.rows.lostIDs() {
		for _, h := range c.hosts {
			if h.id == x {
				out = append(out, h.name)
			}
		}
	}
	return out
}

var period = liveness.Config{}.Normalize().Period

// TestR421_EveryHostOfAHealthyClusterReachesEveryOther asserts R-421 with
// the install's first host, a joined control host and an app host: every
// host is watched and watching, so nobody is unreached, nobody fences, and
// nobody is taken for lost. The control hosts' rows come to name their
// cluster listeners, the first host's placeholder pin replaced.
func TestR421_EveryHostOfAHealthyClusterReachesEveryOther(t *testing.T) {
	c := newSimCluster(t)
	c.run(10 * period)
	require.Empty(t, c.fenced())
	require.Empty(t, c.lost())

	for _, h := range c.hosts {
		var v liveness.View
		require.NoError(t, json.Unmarshal(h.monitor.Answer(""), &v))
		require.Len(t, v.UnreachedMS, 2, "%s watches both other hosts", h.name)
		for peer, ms := range v.UnreachedMS {
			require.Less(t, time.Duration(ms)*time.Millisecond, period/2, "%s reaches %s", h.name, peer)
		}
	}
	list, err := c.rows.Hosts(context.Background())
	require.NoError(t, err)
	for _, h := range list[:2] {
		require.NotEqual(t, "127.0.0.1:7443", h.Address, "%s answers on its cluster listener", h.Name)
		require.NotEqual(t, strings.Repeat("0", 64), h.CertSHA256)
	}

	// The listener still serves its HTTP to everything else.
	pool := x509.NewCertPool()
	pool.AddCert(c.authority.Cert)
	httpClient := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: "localhost"},
	}}
	resp, err := httpClient.Get("https://" + c.hosts[0].listener + "/cluster/nothing")
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// TestR421_ACutOffHostFencesItselfAndOnlyItIsTakenForLost asserts R-421:
// each host cut off in turn, control hosts included, stops its own app
// containers after T and is the only host taken for lost after 3T; the hosts
// still reaching each other stop nothing.
func TestR421_ACutOffHostFencesItselfAndOnlyItIsTakenForLost(t *testing.T) {
	for _, victim := range []string{"core-1", "core-2", "app-1"} {
		t.Run(victim, func(t *testing.T) {
			c := newSimCluster(t)
			c.run(2 * period)
			var cut *simHost
			for _, h := range c.hosts {
				if h.name == victim {
					cut = h
				}
			}
			c.setCut(cut.id)

			c.run(period - liveness.Config{}.Normalize().Interval)
			require.Empty(t, c.fenced(), "nothing stops before T")
			c.run(2 * liveness.Config{}.Normalize().Interval)
			require.Equal(t, []string{victim}, c.fenced(), "the cut-off host stops its apps after T")
			require.Positive(t, cut.docker.stops())

			c.run(period)
			require.Empty(t, c.lost(), "nobody is taken for lost before 3T")
			c.run(2*period + 2*liveness.Config{}.Normalize().Interval)
			require.Equal(t, []string{victim}, c.lost())
			require.Equal(t, []string{victim}, c.fenced(), "nobody else stopped anything")
		})
	}
}

// TestR421_PostgresDownStopsNothing asserts R-421's "Postgres or the
// control plane being unreachable never stops an app": with the database
// away every host keeps the hosts it last read, still reaches them, and
// nothing fences or is taken for lost.
func TestR421_PostgresDownStopsNothing(t *testing.T) {
	c := newSimCluster(t)
	c.run(2 * period)
	c.rows.mu.Lock()
	c.rows.down = true
	c.rows.mu.Unlock()
	c.run(10 * period)
	require.Empty(t, c.fenced())
	require.Empty(t, c.lost())
}
