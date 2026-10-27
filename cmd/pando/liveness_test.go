package main

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/tokenkey"
	"github.com/trypando/pando/internal/hostagent"
)

// livenessAgent is an agent for the host named name, its certificate issued
// from the cluster authority, answering pings with its own Monitor.
func livenessAgent(t *testing.T, as hostagent.Authorities, name string) string {
	t.Helper()
	certPEM, keyPEM, err := as.IssueServer(name)
	require.NoError(t, err)
	serverTLS, err := hostagent.ServerTLS(certPEM, keyPEM, as.PoolPEM())
	require.NoError(t, err)
	agent := &hostagent.Agent{
		TLS: serverTLS, Pool: netip.MustParsePrefix("10.213.0.0/16"),
		Liveness: &liveness.Monitor{Self: "host_01HQ8ZZZZZZZZZZZZZZZZZZZZZ"},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { _ = agent.Serve(ctx, ln); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return ln.Addr().String()
}

func TestClusterLivenessProbesHostsWithAHostCertificate(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	addr := livenessAgent(t, as, "app-1")

	svc, watch := clusterLiveness(state.NewCluster(nil, make([]byte, tokenkey.Size)), authority, nil, nil, zap.NewNop())
	require.NotNil(t, svc.Answering)
	require.NotNil(t, watch)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	view, err := watch.Probe(ctx, liveness.Peer{ID: "host_x", Name: "app-1", Address: addr})
	require.NoError(t, err)
	require.Contains(t, string(view), `"host":"host_01HQ8ZZZZZZZZZZZZZZZZZZZZZ"`)

	// The agent's certificate is checked against the host's name.
	_, err = watch.Probe(ctx, liveness.Peer{ID: "host_x", Name: "app-2", Address: addr})
	require.Error(t, err)
}

func TestAgentLivenessTakesPartOnlyOnAJoinedHost(t *testing.T) {
	logger := zap.NewNop()
	t.Setenv(envHostID, "")
	m, err := agentLiveness(nil, logger)
	require.NoError(t, err)
	require.Nil(t, m, "a multi-host Docker forwarding agent takes no part")

	t.Setenv(envHostID, "host_01HQ8ZZZZZZZZZZZZZZZZZZZZZ")
	t.Setenv(envPeersFile, "")
	_, err = agentLiveness(nil, logger)
	require.ErrorContains(t, err, envPeersFile)

	peers := filepath.Join(t.TempDir(), "peers.json")
	t.Setenv(envPeersFile, peers)
	t.Setenv(envHostCert, "nope")
	_, err = agentLiveness(nil, logger)
	require.ErrorContains(t, err, envHostCert)

	caPEM, err := hostagent.NewAuthority()
	require.NoError(t, err)
	as, err := hostagent.ParseAuthorities(caPEM)
	require.NoError(t, err)
	cert, err := as.IssueHostClient(appHostID)
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(cert.PrivateKey)
	require.NoError(t, err)
	t.Setenv(envHostCert, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})))
	t.Setenv(envHostKey, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	_, err = agentLiveness([]byte("not pem"), logger)
	require.ErrorContains(t, err, "no authority")

	m, err = agentLiveness(as.PoolPEM(), logger)
	require.NoError(t, err)
	require.NotNil(t, m)

	_, err = m.Peers(context.Background())
	require.Error(t, err, "no file yet")
	require.NoError(t, os.WriteFile(peers, []byte("{"), 0o600))
	_, err = m.Peers(context.Background())
	require.ErrorContains(t, err, "is not a list of hosts")

	addr := livenessAgent(t, as, "app-1")
	require.NoError(t, os.WriteFile(peers, []byte(`[{"id":"host_a","name":"app-1","address":"`+addr+`"}]`), 0o600))
	list, err := m.Peers(context.Background())
	require.NoError(t, err)
	require.Equal(t, []liveness.Peer{{ID: "host_a", Name: "app-1", Address: addr}}, list)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, m.Probe(ctx, list[0]))
}

type wiringStore struct {
	liveness.Store
	hosts []state.Host
	err   error
}

func (s wiringStore) Hosts(context.Context) ([]state.Host, error) { return s.hosts, s.err }

func (s wiringStore) Host(_ context.Context, id string) (state.Host, error) {
	for _, h := range s.hosts {
		if h.ID == id {
			return h, nil
		}
	}
	return state.Host{}, errors.New("no such host")
}

func TestClusterLivenessListsHostsAndAsksWhetherOneAnswers(t *testing.T) {
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	addr := livenessAgent(t, as, "app-1")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	silent := ln.Addr().String()
	require.NoError(t, ln.Close())

	store := wiringStore{hosts: []state.Host{
		{ID: "host_first", Name: "core-1", Address: silent},
		{ID: "host_up", Name: "app-1", Address: addr, JoinedWith: "jt_1"},
		{ID: "host_down", Name: "app-2", Address: silent, JoinedWith: "jt_2"},
	}}
	svc, watch := clusterLiveness(store, authority, nil, nil, zap.NewNop())
	ctx := context.Background()

	peers, err := watch.Hosts(ctx)
	require.NoError(t, err)
	require.Equal(t, []liveness.Peer{
		{ID: "host_first", Name: "core-1", Address: silent},
		{ID: "host_up", Name: "app-1", Address: addr},
		{ID: "host_down", Name: "app-2", Address: silent},
	}, peers, "every host is watched, the install's first included (R-421)")

	require.True(t, svc.Answering(ctx, "host_up"))
	require.False(t, svc.Answering(ctx, "host_down"))
	require.False(t, svc.Answering(ctx, "host_nobody"))

	_, watch = clusterLiveness(wiringStore{err: errors.New("database away")}, authority, nil, nil, zap.NewNop())
	_, err = watch.Hosts(ctx)
	require.Error(t, err)
}
