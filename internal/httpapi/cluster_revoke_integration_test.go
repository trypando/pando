//go:build integration

package httpapi_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	corehosts "github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

// fixedView answers every ping with the same view.
type fixedView struct{}

func (fixedView) Answer(string) []byte { return []byte(`{"host":"x"}`) }

// joinedFiles is what a joined host keeps in its cluster directory.
type joinedFiles struct {
	dir   string
	host  tls.Certificate
	probe tls.Certificate
	ca    []byte
}

func readJoined(t *testing.T, dir string) joinedFiles {
	t.Helper()
	host, err := tls.LoadX509KeyPair(filepath.Join(dir, corehosts.HostCertFile), filepath.Join(dir, corehosts.HostKeyFile))
	require.NoError(t, err)
	probe, err := tls.LoadX509KeyPair(filepath.Join(dir, corehosts.HostClientCertFile), filepath.Join(dir, corehosts.HostClientKeyFile))
	require.NoError(t, err)
	ca, err := os.ReadFile(filepath.Join(dir, corehosts.AuthorityCertFile))
	require.NoError(t, err)
	return joinedFiles{dir: dir, host: host, probe: probe, ca: ca}
}

// agentFor serves an agent with f's certificate that answers pings as
// members admits them, and returns its address.
func agentFor(t *testing.T, f joinedFiles, members *corehosts.Members) string {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(f.dir, corehosts.HostCertFile))
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(filepath.Join(f.dir, corehosts.HostKeyFile))
	require.NoError(t, err)
	serverTLS, err := hostagent.ServerTLS(certPEM, keyPEM, f.ca)
	require.NoError(t, err)
	agent := &hostagent.Agent{TLS: serverTLS, Pool: netip.MustParsePrefix("10.213.0.0/16"), Liveness: fixedView{}, Member: members.Admits}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

// TestR415_ARemovedHostsProbeCertificatesAreRefusedEverywhere asserts R-415
// end to end against the database: removing a host revokes every
// certificate it was issued, its probe certificates by its ID, and the hosts
// that answer pings hear so with the peer list they read every round, keep
// it, and refuse the removed host's pings from then on: at once after that
// read, after a restart before any list is read, and from a list read while
// the control plane is away.
func TestR415_ARemovedHostsProbeCertificatesAreRefusedEverywhere(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	srv := clusterListener(t, i)
	ctx := context.Background()
	gone, stays := freshHost(t), freshHost(t)
	goneJoin, err := gone.Join(ctx, mintJoin(t, i, srv, "app"), "edge-1")
	require.NoError(t, err)
	staysJoin, err := stays.Join(ctx, mintJoin(t, i, srv, "app"), "edge-2")
	require.NoError(t, err)
	goneFiles, staysFiles := readJoined(t, gone.Files.Dir), readJoined(t, stays.Files.Dir)

	members := &corehosts.Members{}
	cfg, err := corehosts.JoinedTLS(staysFiles.ca, staysFiles.host)
	require.NoError(t, err)
	sync := &corehosts.PeerSync{Address: staysJoin.Cluster, TLS: cfg, File: filepath.Join(staysFiles.dir, corehosts.PeersFile), Members: members}
	peers := members.Track(sync.Peers)
	_, err = peers(ctx)
	require.NoError(t, err)
	addr := agentFor(t, staysFiles, members)

	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(goneFiles.ca)
	ping := func(to string) error {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		_, err := hostagent.Ping(pctx, to, &tls.Config{
			MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{goneFiles.probe},
			RootCAs: roots, ServerName: hostagent.ServerName("edge-2"),
		}, goneJoin.HostID)
		return err
	}
	require.NoError(t, ping(addr), "a member's ping is answered")

	removed := i.do(i.admin(), http.MethodDelete, "/cluster/hosts/"+goneJoin.HostID, nil)
	require.Equal(t, http.StatusOK, removed.Code, removed.String())
	revoked, err := state.NewCluster(i.db, testTokenKey).Revoked(ctx)
	require.NoError(t, err)
	require.Contains(t, revoked.Hosts, goneJoin.HostID, "its probe certificates, by its ID")
	require.Contains(t, revoked.Certs, hostagent.Fingerprint256(goneFiles.host.Certificate[0]))

	// The next round's list says so, and the agent refuses it at once.
	_, err = peers(ctx)
	require.NoError(t, err)
	require.ErrorContains(t, ping(addr), "is not in this cluster")

	// Restarted, with the control plane away and an old list on disk that
	// still names the removed host: refused before any list is read, and
	// after the old one is.
	old := []corehosts.Peer{{ID: goneJoin.HostID, Name: "edge-1"}, {ID: staysJoin.HostID, Name: "edge-2"}}
	require.NoError(t, corehosts.WritePeers(sync.File, old))
	restarted := &corehosts.Members{}
	restarted.LoadRevoked(filepath.Join(staysFiles.dir, corehosts.RevokedFile))
	again := agentFor(t, staysFiles, restarted)
	require.ErrorContains(t, ping(again), "is not in this cluster", "before any list")
	srv.Close()
	away := &corehosts.PeerSync{Address: staysJoin.Cluster, TLS: cfg, File: sync.File, Members: restarted}
	list, err := restarted.Track(away.Peers)(ctx)
	require.NoError(t, err)
	require.Equal(t, old, list, "the control plane is away: the old list stands")
	require.ErrorContains(t, ping(again), "is not in this cluster", "from an old list")
}
