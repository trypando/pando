package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/dockernet"
	"github.com/trypando/pando/internal/hostagent"
)

// runAppHost is what a host that joined with the app role alone runs in
// place of the server: its agent, in this process, with what the join gave it
// (R-416, R-421). It needs no database: the control plane being away changes
// nothing here.
//
// An agent that cannot start in full — its runtime socket unknown, a file
// missing — runs appHostFallback rather than exit, when its own certificate
// can be read: an app host whose Pando cannot start is not cut off, and its
// apps may be running (R-421). Only a host without its own certificate exits.
func runAppHost(ctx context.Context, dir string, joined hosts.Joined, logger *zap.Logger) error {
	agent, monitor, certs, err := appHostAgent(dir, joined, logger)
	if err != nil {
		fallback, fmonitor, ferr := appHostFallback(dir, joined, logger)
		if ferr != nil {
			return err
		}
		logger.Error("this app host cannot run its agent; until this is fixed and Pando restarted it carries no runtime calls, "+
			"and it still checks the other hosts and stops its own apps if it is cut off",
			zap.String("host_id", joined.HostID), zap.Error(err))
		if serr := serveAgent(ctx, fallback, fmonitor, agentListen()); serr != nil {
			return serr
		}
		return err
	}
	// R-421: renewed from the cluster listener before they expire.
	go certs.Host.Run(ctx, certRenewCheck)
	logger.Info("this host joined a cluster as an app host and runs its agent",
		zap.String("host_id", joined.HostID), zap.String("name", joined.Name), zap.String("cluster", joined.Cluster))
	return serveAgent(ctx, agent, monitor, agentListen())
}

// appHostAgent is a joined app host's agent and its Monitor: the agent serves
// with the host's own certificate, named hostagent.ServerName(<name>), checks
// every connection against the cluster authority, and carries the control
// plane's runtime calls to the host's rootless runtime (R-416). The Monitor
// checks the other hosts with the probe certificate the join issued it,
// keeping its peer list current from the cluster listener (R-421). Both
// certificates are renewed while it runs (the returned HostCerts), and each
// handshake takes the ones held then.
func appHostAgent(dir string, joined hosts.Joined, logger *zap.Logger) (*hostagent.Agent, *liveness.Monitor, *hosts.HostCerts, error) {
	files, err := readClusterFiles(dir, hosts.HostCertFile, hosts.HostKeyFile, hosts.AuthorityCertFile,
		hosts.HostClientCertFile, hosts.HostClientKeyFile)
	if err != nil {
		return nil, nil, nil, err
	}
	caPEM := files[2]
	host509, err := tls.X509KeyPair(files[0], files[1])
	if err != nil {
		return nil, nil, nil, appHostError(dir, err)
	}
	client509, err := tls.X509KeyPair(files[3], files[4])
	if err != nil {
		return nil, nil, nil, appHostError(dir, err)
	}
	renewal := &hosts.HostRenewal{Address: joined.Cluster, Dir: dir, Name: joined.Name, AuthorityPEM: caPEM}
	certs := renewal.Certs(host509, client509)
	certs.Host.OnError = func(err error) {
		logger.Warn("this host's certificates could not be renewed; trying again", zap.Error(err))
	}
	serverTLS, err := hostagent.RenewingServerTLS(certs.Host, caPEM)
	if err != nil {
		return nil, nil, nil, appHostError(dir, err)
	}
	pool, err := appHostPool(joined)
	if err != nil {
		return nil, nil, nil, err
	}
	socket, err := appHostRuntimeSocket()
	if err != nil {
		return nil, nil, nil, err
	}
	members := appHostMembers(dir)
	agent := &hostagent.Agent{TLS: serverTLS, Pool: pool, Logger: logger, RuntimeSocket: socket, Member: members.Admits}

	clusterTLS, err := hosts.JoinedTLS(caPEM, host509)
	if err != nil {
		return nil, nil, nil, appHostError(dir, err)
	}
	clusterTLS.Certificates, clusterTLS.GetClientCertificate = nil, certs.Host.GetClientCertificate
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	docker, err := client.New(client.WithHost("unix://" + socket))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("this app host could not reach its container runtime at %s, which it stops apps through when it is cut off: %w", socket, err)
	}
	sync := &hosts.PeerSync{Address: joined.Cluster, TLS: clusterTLS, File: filepath.Join(dir, hosts.PeersFile), Members: members}
	return agent, newMonitor(joined.HostID, members.Track(sync.Peers), certs.Probe.GetClientCertificate, roots, docker, logger), certs, nil
}

// certRenewCheck is how often a joined host looks whether its certificates
// are due for renewal (hostagent.RenewBefore).
const certRenewCheck = time.Hour

// appHostMembers is the cluster's hosts as this one hears them, refusing
// from the start every host it last heard was removed (R-415).
func appHostMembers(dir string) *hosts.Members {
	m := &hosts.Members{}
	m.LoadRevoked(filepath.Join(dir, hosts.RevokedFile))
	return m
}

// appHostFallback is what an app host runs when its full agent cannot
// start: an agent that carries no runtime calls, and the host's Monitor, so
// that a host that answers the cluster's pings is also one that stops its own
// apps when it is cut off (R-153, R-421). Without the Monitor, a cut-off host
// whose agent had failed was taken for lost and its standbys started while
// its own copies kept running.
//
// It needs only the host's own certificate and the authority's. It probes
// with its probe certificate or, when that cannot be read, with its own: the
// other hosts refuse such a ping, and a refusal is still contact, which is
// all fencing counts. It fences through the host's runtime at the socket
// appHostRuntimeSocket names, and answers pings only while that runtime
// answers (fenceGate): a host that could not stop its apps reports nothing.
func appHostFallback(dir string, joined hosts.Joined, logger *zap.Logger) (*hostagent.Agent, *liveness.Monitor, error) {
	files, err := readClusterFiles(dir, hosts.HostCertFile, hosts.HostKeyFile, hosts.AuthorityCertFile)
	if err != nil {
		return nil, nil, err
	}
	serverTLS, err := hostagent.ServerTLS(files[0], files[1], files[2])
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	host509, err := tls.X509KeyPair(files[0], files[1])
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	clusterTLS, err := hosts.JoinedTLS(files[2], host509)
	if err != nil {
		return nil, nil, appHostError(dir, err)
	}
	pool, err := appHostPool(joined)
	if err != nil {
		pool = netip.MustParsePrefix(dockernet.DefaultPool)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(files[2])
	members := appHostMembers(dir)
	sync := &hosts.PeerSync{Address: joined.Cluster, TLS: clusterTLS, File: filepath.Join(dir, hosts.PeersFile), Members: members}
	runtime := localRuntime()
	monitor := newMonitor(joined.HostID, members.Track(sync.Peers), staticCert(fallbackProbeCert(dir, host509)), roots, runtime, logger)
	gate := &fenceGate{liveness: monitor, reachable: runtime.Reachable, every: fenceGateEvery}
	return &hostagent.Agent{TLS: serverTLS, Pool: pool, Liveness: gate, Logger: logger, Member: members.Admits}, monitor, nil
}

// fallbackProbeCert is the probe certificate the join wrote, or host when it
// cannot be read.
func fallbackProbeCert(dir string, host tls.Certificate) tls.Certificate {
	files, err := readClusterFiles(dir, hosts.HostClientCertFile, hosts.HostClientKeyFile)
	if err != nil {
		return host
	}
	probe, err := tls.X509KeyPair(files[0], files[1])
	if err != nil {
		return host
	}
	return probe
}

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

// readClusterFiles reads names from the cluster directory, in order.
func readClusterFiles(dir string, names ...string) ([][]byte, error) {
	out := make([][]byte, len(names))
	for i, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n)) // #nosec G304 -- the configured cluster directory
		if err != nil {
			return nil, appHostError(dir, err)
		}
		out[i] = data
	}
	return out, nil
}

func appHostError(dir string, err error) error {
	return fmt.Errorf("this app host cannot run its agent: what it was given when it joined, in %s (server.cluster_dir), is missing or unreadable (%w). Remove the host from the cluster, then join it again with a new join token", dir, err)
}

// agentListen is where the agent listens: PANDO_AGENT_LISTEN, else every
// address on the agent port.
func agentListen() string {
	if listen := os.Getenv(multidocker.EnvAgentListen); listen != "" {
		return listen
	}
	return ":" + strconv.Itoa(hostagent.DefaultPort)
}

// appHostPool is the app network range the agent forwards into:
// PANDO_AGENT_NETWORK_POOL, else the runtime's when the host joined, else the
// runtime's default.
func appHostPool(joined hosts.Joined) (netip.Prefix, error) {
	raw := os.Getenv(multidocker.EnvAgentPool)
	if raw == "" {
		raw = joined.NetworkPool
	}
	if raw == "" {
		raw = dockernet.DefaultPool
	}
	pool, err := netip.ParsePrefix(raw)
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("this app host cannot run its agent: %q is not an address range. Valid answer: an IPv4 range such as %s, in %s", raw, dockernet.DefaultPool, multidocker.EnvAgentPool)
	}
	return pool.Masked(), nil
}

// appHostRuntimeSocket is the host's rootless container runtime socket
// (R-402, R-416): PANDO_AGENT_RUNTIME_SOCKET, else DOCKER_HOST when it names
// a socket, else $XDG_RUNTIME_DIR/docker.sock, where rootless Docker puts it.
func appHostRuntimeSocket() (string, error) {
	if s := os.Getenv(multidocker.EnvAgentRuntimeSocket); s != "" {
		return s, nil
	}
	if s, ok := strings.CutPrefix(os.Getenv("DOCKER_HOST"), "unix://"); ok && s != "" {
		return s, nil
	}
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return filepath.Join(d, "docker.sock"), nil
	}
	return "", errors.New("this app host cannot run its agent: it does not know where its container runtime's socket is. " +
		"Set " + multidocker.EnvAgentRuntimeSocket + " to the rootless runtime's socket, such as /run/user/1000/docker.sock")
}
