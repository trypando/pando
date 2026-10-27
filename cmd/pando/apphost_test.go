package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/hostagent"
)

const appHostID = "host_01HQ8ZZZZZZZZZZZZZZZZZZZZZ"

// writeJoinedFiles writes what a join gives an app host named name into dir,
// issued from authority.
func writeJoinedFiles(t *testing.T, dir string, authority *hosts.Authority, name string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	require.NoError(t, err)
	certPEM, _, err := authority.IssueHost(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), name, []string{"app"}, time.Now())
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	clientCert, clientKey, err := authority.IssueHostClient(appHostID, time.Now())
	require.NoError(t, err)
	for file, data := range map[string][]byte{
		hosts.HostCertFile:       certPEM,
		hosts.HostKeyFile:        pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}),
		hosts.AuthorityCertFile:  authority.CertPEM(),
		hosts.HostClientCertFile: clientCert,
		hosts.HostClientKeyFile:  []byte(clientKey.Reveal()),
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), data, 0o600))
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// TestR416_AJoinedAppHostRunsItsAgent asserts R-416 and R-421 on an app host:
// it runs its agent in this process with the certificate it joined with,
// named for the host, answers other hosts' pings with its own view, and keeps
// its peer list from the cluster listener, presenting its certificate.
func TestR416_AJoinedAppHostRunsItsAgent(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	agentAddr := freeAddr(t)

	asked := make(chan bool, 16)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked <- len(r.TLS.VerifiedChains) > 0
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": []hosts.Peer{{ID: appHostID, Name: "edge-1", Address: agentAddr}}})
	}))
	srv.TLS, err = authority.ListenerTLS([]string{"127.0.0.1"}, time.Now())
	require.NoError(t, err)
	srv.StartTLS()
	srv.TLS.Certificates = nil // httptest adds its own; the listener serves from GetCertificate
	t.Cleanup(srv.Close)

	joined := hosts.Joined{HostID: appHostID, Name: "edge-1", Roles: []string{"app"}, Cluster: srv.URL}
	_, _, _, err = appHostAgent(dir, joined, zap.NewNop())
	require.ErrorContains(t, err, "server.cluster_dir", "nothing written yet")

	writeJoinedFiles(t, dir, authority, "edge-1")
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "/tmp/pando-agent-test.sock")
	t.Setenv(multidocker.EnvAgentListen, agentAddr)
	t.Setenv(multidocker.EnvAgentPool, "")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runAppHost(ctx, dir, joined, zap.NewNop()) }()

	as := hostagent.Authorities{{Cert: authority.Cert, Key: authority.Key}}
	probe, err := as.IssueCollector()
	require.NoError(t, err)
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{probe}, RootCAs: as.Pool(), ServerName: hostagent.ServerName("edge-1")}
	var view []byte
	require.Eventually(t, func() bool {
		pctx, pcancel := context.WithTimeout(ctx, time.Second)
		defer pcancel()
		view, err = hostagent.Ping(pctx, agentAddr, cfg, "")
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the agent answers a ping, as edge-1")
	require.Contains(t, string(view), appHostID)

	require.True(t, <-asked, "the host presents its certificate for the peer list")
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(dir, hosts.PeersFile))
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the peer list is kept")

	cancel()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the app host did not stop")
	}
}

func TestAnAppHostRefusesWhatItCannotUse(t *testing.T) {
	dir := t.TempDir()
	authority, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	writeJoinedFiles(t, dir, authority, "edge-1")
	joined := hosts.Joined{HostID: appHostID, Name: "edge-1", Cluster: "https://127.0.0.1:1"}
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "/tmp/pando-agent-test.sock")

	t.Setenv(multidocker.EnvAgentPool, "everywhere")
	_, _, _, err = appHostAgent(dir, joined, zap.NewNop())
	require.ErrorContains(t, err, "is not an address range")
	t.Setenv(multidocker.EnvAgentPool, "")

	t.Setenv(multidocker.EnvAgentListen, "nowhere:at:all")
	require.ErrorContains(t, runAppHost(context.Background(), dir, joined, zap.NewNop()), "could not listen")

	for _, file := range []string{hosts.HostClientKeyFile, hosts.HostKeyFile, hosts.AuthorityCertFile} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, file), []byte("junk"), 0o600))
		_, _, _, err = appHostAgent(dir, joined, zap.NewNop())
		require.ErrorContains(t, err, "server.cluster_dir", file)
	}
}

func TestAnAppHostsNetworkPoolAndRuntimeSocket(t *testing.T) {
	t.Setenv(multidocker.EnvAgentPool, "")
	pool, err := appHostPool(hosts.Joined{})
	require.NoError(t, err)
	require.Equal(t, "10.213.0.0/16", pool.String(), "the runtime's default")
	pool, err = appHostPool(hosts.Joined{NetworkPool: "10.208.1.0/12"})
	require.NoError(t, err)
	require.Equal(t, "10.208.0.0/12", pool.String(), "the runtime's when the host joined")
	t.Setenv(multidocker.EnvAgentPool, "10.9.0.0/16")
	pool, err = appHostPool(hosts.Joined{NetworkPool: "10.208.0.0/12"})
	require.NoError(t, err)
	require.Equal(t, "10.9.0.0/16", pool.String(), "the environment's first")

	t.Setenv(multidocker.EnvAgentRuntimeSocket, "")
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	_, err = appHostRuntimeSocket()
	require.ErrorContains(t, err, multidocker.EnvAgentRuntimeSocket)
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	socket, err := appHostRuntimeSocket()
	require.NoError(t, err)
	require.Equal(t, "/run/user/1000/docker.sock", socket, "rootless Docker's")
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.1:2376")
	socket, _ = appHostRuntimeSocket()
	require.Equal(t, "/run/user/1000/docker.sock", socket, "a TCP DOCKER_HOST is not a socket")
	t.Setenv("DOCKER_HOST", "unix:///run/user/1001/docker.sock")
	socket, _ = appHostRuntimeSocket()
	require.Equal(t, "/run/user/1001/docker.sock", socket)
	t.Setenv(multidocker.EnvAgentRuntimeSocket, "/run/podman.sock")
	socket, _ = appHostRuntimeSocket()
	require.Equal(t, "/run/podman.sock", socket)
}

// recordStore records the first host as state.Cluster does.
type recordStore struct {
	got []state.Host
	err error
}

func (s *recordStore) RecordFirstHost(_ context.Context, h state.Host) (state.Host, bool, error) {
	if s.err != nil {
		return h, false, s.err
	}
	s.got = append(s.got, h)
	return h, len(s.got) == 1, nil
}

// TestR414_ThisMachineIsRecordedAsTheFirstHost asserts R-414's wiring: the
// first host is reached at the advertise address's host on the agent port,
// else at the cluster's hostname, and a store that fails stops nothing.
func TestR414_ThisMachineIsRecordedAsTheFirstHost(t *testing.T) {
	ctx := context.Background()
	store := &recordStore{}
	recordInstallHost(ctx, &config.Config{Server: config.Server{AdvertiseURL: "http://10.0.0.5:8080"}}, store, zap.NewNop())
	recordInstallHost(ctx, &config.Config{Server: config.Server{ExternalURL: "https://pando.example.com"}}, store, zap.NewNop())
	require.Len(t, store.got, 2)
	require.Equal(t, "10.0.0.5:7443", store.got[0].Address)
	require.Equal(t, "pando.example.com:7443", store.got[1].Address)
	require.Equal(t, []string{"control", "app"}, store.got[0].Roles)

	store.err = errors.New("database away")
	recordInstallHost(ctx, &config.Config{}, store, zap.NewNop())
}
