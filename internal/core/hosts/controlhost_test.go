package hosts_test

import (
	"context"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

// publishStore is a cluster's hosts as a control host reads and updates
// them.
type publishStore struct {
	hosts  []state.Host
	err    error
	setErr error
	sets   int
}

func (p *publishStore) Hosts(context.Context) ([]state.Host, error) {
	out := append([]state.Host(nil), p.hosts...)
	return out, p.err
}

func (p *publishStore) SetHostAgent(_ context.Context, id, address, pin string) error {
	p.sets++
	if p.setErr != nil {
		return p.setErr
	}
	for i := range p.hosts {
		if p.hosts[i].ID == id {
			p.hosts[i].Address, p.hosts[i].CertSHA256 = address, pin
		}
	}
	return nil
}

func TestPingAddressKeepsTheMachineAndTakesTheListenersPort(t *testing.T) {
	require.Equal(t, "10.0.0.5:7444", hosts.PingAddress("10.0.0.5:7443", 7444))
	require.Equal(t, "core.example.com:7444", hosts.PingAddress("core.example.com", 7444))
	require.Equal(t, "[fd00::5]:7444", hosts.PingAddress("[fd00::5]:7443", 7444))
	require.Equal(t, "[fd00::5]:7444", hosts.PingAddress("[fd00::5]", 7444))
}

// TestR421_AControlHostRecordsWhereItAnswersPings asserts R-421: a control
// host's peer list is every host, and reading it keeps the host's own row
// pointing at its cluster listener with the fingerprint it answers with, the
// first host's placeholder replaced, written only when it differs.
func TestR421_AControlHostRecordsWhereItAnswersPings(t *testing.T) {
	ctx := context.Background()
	zeros := strings.Repeat("0", 64)
	pin := strings.Repeat("a", 64)
	store := &publishStore{hosts: []state.Host{
		{ID: "host_first", Name: "core-1", Address: "10.0.0.1:7443", CertSHA256: zeros},
		{ID: "host_core2", Name: "core-2", Address: "10.0.0.2:7443", CertSHA256: strings.Repeat("b", 64), JoinedWith: "jt_1"},
		{ID: "host_app1", Name: "app-1", Address: "10.0.0.3:7443", CertSHA256: strings.Repeat("c", 64), JoinedWith: "jt_2"},
	}}
	core, logs := observer.New(zap.WarnLevel)
	peers := &hosts.ControlPeers{Store: store, Self: "host_first", Port: 7444, Pin: pin, Logger: zap.New(core)}

	got, err := peers.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, []hosts.Peer{
		{ID: "host_first", Name: "core-1", Address: "10.0.0.1:7444"},
		{ID: "host_core2", Name: "core-2", Address: "10.0.0.2:7443"},
		{ID: "host_app1", Name: "app-1", Address: "10.0.0.3:7443"},
	}, got, "every host, whatever its roles")
	require.Equal(t, "10.0.0.1:7444", store.hosts[0].Address)
	require.Equal(t, pin, store.hosts[0].CertSHA256, "the placeholder pin is replaced")
	require.Equal(t, 1, store.sets)

	_, err = peers.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, store.sets, "nothing is written once the row is current")

	// A write that fails is logged, the list still returned, and made again
	// the next round.
	store.hosts[0].Address = "10.0.0.1:7443"
	store.setErr = errors.New("database away")
	got, err = peers.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1:7443", got[0].Address)
	require.Equal(t, 1, logs.FilterMessageSnippet("trying again next round").Len())
	store.setErr = nil
	_, err = peers.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.1:7444", store.hosts[0].Address)

	// Without a logger a failed write is only tried again.
	store.hosts[0].Address, store.setErr = "10.0.0.1:7443", errors.New("database away")
	quiet := &hosts.ControlPeers{Store: store, Self: "host_first", Port: 7444, Pin: pin}
	_, err = quiet.Peers(ctx)
	require.NoError(t, err)

	store.err = errors.New("database away")
	_, err = peers.Peers(ctx)
	require.Error(t, err, "the Monitor keeps the last list")
}

// TestR421_AControlHostKeepsOneCertificateForItsPings asserts R-421: the
// certificate a control host answers pings with is issued from the
// authority once and kept, so its pin holds across restarts, and issued
// again for another name or near its end.
func TestR421_AControlHostKeepsOneCertificateForItsPings(t *testing.T) {
	a, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	dir := filepath.Join(t.TempDir(), "cluster")
	now := time.Now()
	roles := []string{state.HostRoleControl, state.HostRoleApp}

	cert, pin, err := hosts.ControlHostCert(dir, a, "core-1", roles, now)
	require.NoError(t, err)
	require.Equal(t, hostagent.Fingerprint256(cert.Certificate[0]), pin)
	require.Contains(t, cert.Leaf.DNSNames, hostagent.ServerName("core-1"))
	pool := x509.NewCertPool()
	pool.AddCert(a.Cert)
	_, err = cert.Leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: hostagent.ServerName("core-1")})
	require.NoError(t, err, "issued from the cluster authority")
	info, err := os.Stat(filepath.Join(dir, hosts.HostKeyFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	_, again, err := hosts.ControlHostCert(dir, a, "core-1", roles, now)
	require.NoError(t, err)
	require.Equal(t, pin, again, "kept across restarts")

	_, renamed, err := hosts.ControlHostCert(dir, a, "core-2", roles, now)
	require.NoError(t, err)
	require.NotEqual(t, pin, renamed, "issued again for another name")

	_, renewed, err := hosts.ControlHostCert(dir, a, "core-2", roles, now.Add(80*24*time.Hour))
	require.NoError(t, err)
	require.NotEqual(t, renamed, renewed, "issued again near its end")

	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	_, _, err = hosts.ControlHostCert(filepath.Join(file, "cluster"), a, "core-1", roles, now)
	require.ErrorContains(t, err, "server.cluster_dir")

	locked := filepath.Join(t.TempDir(), "locked")
	require.NoError(t, os.Mkdir(locked, 0o500))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	_, _, err = hosts.ControlHostCert(locked, a, "core-1", roles, now)
	require.ErrorContains(t, err, "could not write this host's certificate")
}
