package hosts_test

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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
	"github.com/trypando/pando/internal/secret"
)

// TestR415_JoiningAtSetupNeedsTheSetupToken asserts R-046 on R-415's setup
// join: the setup token is checked before anything else, and a Pando that
// cannot check it joins nobody from the setup screen.
func TestR415_JoiningAtSetupNeedsTheSetupToken(t *testing.T) {
	c := newCluster(t)
	ctx := context.Background()

	j := joiningHost(t)
	_, err := j.SetupWithToken(ctx, secret.New("s3t"), c.token("app"), "edge-2")
	require.Equal(t, errs.Internal, errs.CodeOf(err), "no checker: refused")

	wrong := errs.New(errs.AuthInvalid, "That setup token is not the one this installation is waiting for.")
	var presented string
	j.SetupToken = func(_ context.Context, token secret.Value) error {
		presented = token.Reveal()
		if presented != "s3t" {
			return wrong
		}
		return nil
	}
	_, err = j.SetupWithToken(ctx, secret.New("guess"), c.token("app"), "edge-2")
	require.ErrorIs(t, err, wrong)
	require.Empty(t, c.store.hosts, "nothing reached the cluster")
	_, done, _ := j.Files.Read()
	require.False(t, done)

	restarted := false
	j.Restart = func() { restarted = true }
	joined, err := j.SetupWithToken(ctx, secret.New("s3t"), c.token("app"), "edge-2")
	require.NoError(t, err)
	require.Equal(t, "s3t", presented)
	require.Equal(t, "edge-2", joined.Name)
	require.True(t, restarted)
}

// TestR421_AJoinedHostIsGivenWhatItChecksTheOthersWith asserts R-421's join
// half: the host gets a pando-host client certificate, from the authority,
// for a key only it and the join saw, kept 0600 beside its own, and the
// runtime's app network range.
func TestR421_AJoinedHostIsGivenWhatItChecksTheOthersWith(t *testing.T) {
	c := newCluster(t)
	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{"network_pool":"10.208.0.0/12"}`)}}
	c.svc.Runtime = newRuntime(t, f)
	j := joiningHost(t)
	joined, err := j.Join(context.Background(), c.token("app"), "edge-2")
	require.NoError(t, err)
	require.Equal(t, "10.208.0.0/12", joined.NetworkPool)
	again, _, err := j.Files.Read()
	require.NoError(t, err)
	require.Equal(t, "10.208.0.0/12", again.NetworkPool, "kept in joined.json")

	certPEM := read(t, filepath.Join(j.Files.Dir, hosts.HostClientCertFile))
	keyPEM := read(t, filepath.Join(j.Files.Dir, hosts.HostClientKeyFile))
	pair, err := tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
	require.NoError(t, err)
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	require.NoError(t, err)
	require.Equal(t, hostagent.HostCommonName, leaf.Subject.CommonName)
	require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, leaf.ExtKeyUsage)
	require.NoError(t, leaf.CheckSignatureFrom(c.svc.Authority.Cert))
	info, err := os.Stat(filepath.Join(j.Files.Dir, hosts.HostClientKeyFile))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	// The key redacts itself anywhere but the wire.
	res := hosts.JoinResult{HostClientKey: secret.New(keyPEM)}
	logged, err := json.Marshal(res)
	require.NoError(t, err)
	require.NotContains(t, string(logged), "PRIVATE KEY")
}

func TestTheRuntimesNetworkPoolIsReadWhereItCanBe(t *testing.T) {
	ctx := context.Background()
	f := &runtimeFakes{}
	require.Empty(t, newRuntime(t, f).NetworkPool(ctx), "no runtime")
	f.adapters = []state.AdapterConfig{singleDocker(`not json`)}
	require.Empty(t, newRuntime(t, f).NetworkPool(ctx))
	f.adapters = []state.AdapterConfig{singleDocker(` {"network_pool":" 10.1.0.0/16 "}`)}
	require.Equal(t, "10.1.0.0/16", newRuntime(t, f).NetworkPool(ctx))
}

// hostCert is a host certificate from the cluster's authority, as a join
// issues it, and its fingerprint.
func hostCert(t *testing.T, a *hosts.Authority, name string) (tls.Certificate, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	require.NoError(t, err)
	csr := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der})
	certPEM, sha, err := a.IssueHost(csr, name, []string{"app"}, time.Now())
	require.NoError(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	require.NoError(t, err)
	pair, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	require.NoError(t, err)
	return pair, sha
}

// TestR421_OnlyAHostOfTheClusterHearsWhereTheOthersAre asserts R-421's peer
// list: it is given to a certificate a host joined with and to nothing else,
// and leaves out the install's first host, whose agent answers no ping.
func TestR421_OnlyAHostOfTheClusterHearsWhereTheOthersAre(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	ctx := context.Background()
	cert, sha := hostCert(t, svc.Authority, "edge-1")
	store.hosts = []state.Host{
		{ID: "host_first", Name: "core-1", Address: "10.0.0.1:7443", CertSHA256: pin},
		{ID: "host_edge1", Name: "edge-1", Address: "10.0.0.2:7443", CertSHA256: sha, JoinedWith: "jt_1"},
		{ID: "host_edge2", Name: "edge-2", Address: "10.0.0.3:7443", CertSHA256: pin, JoinedWith: "jt_2"},
	}

	_, err := svc.Peers(ctx, nil)
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err))
	other, _ := hostCert(t, newService(t, &fakeStore{}).Authority, "edge-1")
	_, err = svc.Peers(ctx, other.Certificate[0])
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), "a certificate no host joined with")

	peers, err := svc.Peers(ctx, cert.Certificate[0])
	require.NoError(t, err)
	require.Equal(t, []hosts.Peer{
		{ID: "host_first", Name: "core-1", Address: "10.0.0.1:7443"},
		{ID: "host_edge1", Name: "edge-1", Address: "10.0.0.2:7443"},
		{ID: "host_edge2", Name: "edge-2", Address: "10.0.0.3:7443"},
	}, peers)

	store.hosts = store.hosts[:1]
	_, err = svc.Peers(ctx, cert.Certificate[0])
	require.Equal(t, errs.AuthInvalid, errs.CodeOf(err), "a removed host hears nothing")
}

// peerServer is the cluster listener's peer list, as httpapi serves it.
func peerServer(t *testing.T, svc *hosts.Service) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var der []byte
		if len(r.TLS.VerifiedChains) > 0 {
			der = r.TLS.VerifiedChains[0][0].Raw
		}
		peers, err := svc.Peers(r.Context(), der)
		if err != nil {
			e := errs.As(err)
			w.WriteHeader(e.Status())
			_ = json.NewEncoder(w).Encode(e)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": peers})
	}))
	tlsConfig, err := svc.Authority.ListenerTLS([]string{"127.0.0.1"}, time.Now())
	require.NoError(t, err)
	srv.TLS = tlsConfig
	srv.StartTLS()
	srv.TLS.Certificates = nil // httptest adds its own; the listener serves from GetCertificate
	t.Cleanup(srv.Close)
	return srv
}

// TestR421_AJoinedHostKeepsItsPeerListCurrent asserts R-421's peer file: each
// call fetches the list over mutual TLS and keeps it, and a cluster that
// cannot be reached leaves the last list.
func TestR421_AJoinedHostKeepsItsPeerListCurrent(t *testing.T) {
	store := &fakeStore{}
	svc := newService(t, store)
	cert, sha := hostCert(t, svc.Authority, "edge-1")
	store.hosts = []state.Host{{ID: "host_edge1", Name: "edge-1", Address: "10.0.0.2:7443", CertSHA256: sha, JoinedWith: "jt_1"}}
	srv := peerServer(t, svc)
	ctx := context.Background()

	_, err := hosts.JoinedTLS([]byte("not pem"), cert)
	require.Error(t, err)
	_, err = hosts.JoinedTLS(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("junk")}), cert)
	require.Error(t, err)
	cfg, err := hosts.JoinedTLS(svc.Authority.CertPEM(), cert)
	require.NoError(t, err)

	file := filepath.Join(t.TempDir(), hosts.PeersFile)
	sync := &hosts.PeerSync{Address: srv.URL + "/", TLS: cfg, File: file}
	_, err = sync.Peers(ctx)
	require.NoError(t, err)
	kept, err := hosts.ReadPeers(file)
	require.NoError(t, err)
	require.Equal(t, []hosts.Peer{{ID: "host_edge1", Name: "edge-1", Address: "10.0.0.2:7443"}}, kept)

	// Removed: refused, and the last list stands.
	store.hosts = nil
	list, err := sync.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, kept, list)

	// Away: the last list stands.
	srv.Close()
	list, err = sync.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, kept, list)

	// Away with nothing kept.
	require.NoError(t, os.Remove(file))
	_, err = sync.Peers(ctx)
	require.ErrorContains(t, err, "none are kept from before")

	require.NoError(t, os.WriteFile(file, []byte("{"), 0o600))
	_, err = hosts.ReadPeers(file)
	require.ErrorContains(t, err, "is not a list of hosts")
}

func TestAPeerListThatCannotBeKeptIsStillAnswered(t *testing.T) {
	list := []hosts.Peer{{ID: "host_a", Name: "a", Address: "a:7443"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": list})
	}))
	t.Cleanup(srv.Close)
	sync := &hosts.PeerSync{Address: srv.URL, File: filepath.Join(t.TempDir(), "missing", "peers.json"), HTTP: srv.Client()}
	got, err := sync.Peers(context.Background())
	require.ErrorContains(t, err, "could not be kept")
	require.Equal(t, list, got)

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	t.Cleanup(bad.Close)
	sync = &hosts.PeerSync{Address: bad.URL, File: filepath.Join(t.TempDir(), "peers.json"), HTTP: bad.Client()}
	_, err = sync.Peers(context.Background())
	require.ErrorContains(t, err, "cannot read")

	sync = &hosts.PeerSync{Address: "::not a url", File: filepath.Join(t.TempDir(), "peers.json")}
	_, err = sync.Peers(context.Background())
	require.Error(t, err)
}

// firstHostStore records the first host as state.Cluster does.
type firstHostStore struct {
	hosts []state.Host
	err   error
}

func (f *firstHostStore) RecordFirstHost(_ context.Context, h state.Host) (state.Host, bool, error) {
	if f.err != nil {
		return state.Host{}, false, f.err
	}
	for _, have := range f.hosts {
		if hosts.InstallHost(have) {
			return state.Host{}, false, nil
		}
	}
	h.ID = "host_first"
	f.hosts = append(f.hosts, h)
	return h, true, nil
}

// TestR414_TheFirstHostIsInTheClusterFromItsFirstStart asserts R-414: the
// install's own machine is recorded as a host with both roles at its first
// start, once, and the cluster's count is its rows — one for a single host,
// as before — while the runtime's host list and the liveness checks leave it
// out, so a single-host install behaves as it did.
func TestR414_TheFirstHostIsInTheClusterFromItsFirstStart(t *testing.T) {
	ctx := context.Background()
	store := &firstHostStore{}
	h, recorded, err := hosts.RecordInstallHost(ctx, store, hosts.InstallHostInfo{
		Hostname: "Build-01.example.com", Address: "10.0.0.5:7443", Platform: "linux/amd64", Version: "1.2.3",
	})
	require.NoError(t, err)
	require.True(t, recorded)
	require.Equal(t, "build-01", h.Name)
	require.Equal(t, []string{state.HostRoleControl, state.HostRoleApp}, h.Roles)
	require.Equal(t, "10.0.0.5:7443", h.Address)
	require.Regexp(t, `^[0-9a-f]{64}$`, h.CertSHA256)
	require.True(t, hosts.InstallHost(h))

	_, recorded, err = hosts.RecordInstallHost(ctx, store, hosts.InstallHostInfo{Hostname: "replica-2"})
	require.NoError(t, err)
	require.False(t, recorded, "a second replica, or a restart, records nothing")

	svc := newService(t, &fakeStore{hosts: store.hosts})
	n, err := svc.Count(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	app, err := hosts.RuntimeHosts(store.hosts, "control")
	require.NoError(t, err)
	require.Empty(t, app, "the first host is the runtime's control entry already")
	require.Equal(t, []hosts.Peer{{ID: h.ID, Name: h.Name, Address: h.Address, Control: true}}, hosts.LivenessPeers(store.hosts),
		"the first host is watched like any other (R-421)")

	store.err = errors.New("database away")
	_, _, err = hosts.RecordInstallHost(ctx, store, hosts.InstallHostInfo{})
	require.Error(t, err)
}

func TestTheFirstHostIsNamedByItsMachinesFirstLabel(t *testing.T) {
	for in, want := range map[string]string{
		"Build-01.example.com": "build-01",
		"pando":                "pando",
		"Ben's MacBook.local":  "ben-s-macbook",
		"":                     "control",
		"...":                  "control",
		"--":                   "control",
		"a234567890123456789012345678901234567890123456789012345678901234567890": "a23456789012345678901234567890123456789012345678901234567890123",
	} {
		require.Equal(t, want, hosts.InstallHostName(in), in)
	}
}

// TestR415_TheClusterListenerChecksAHostsCertificateWhenOneIsGiven asserts
// the listener's TLS: a joining host has no certificate and is let through
// to the token check; one that has joined is checked against the authority.
func TestR415_TheClusterListenerChecksAHostsCertificateWhenOneIsGiven(t *testing.T) {
	svc := newService(t, &fakeStore{})
	cfg, err := svc.Authority.ListenerTLS(nil, time.Now())
	require.NoError(t, err)
	require.Equal(t, tls.VerifyClientCertIfGiven, cfg.ClientAuth)
	require.NotNil(t, cfg.ClientCAs)
}

// TestR421_AJoinedHostAsksTheOtherControlHostsWhenItsOwnIsGone asserts R-421's
// peer list: when the control host in a host's join token goes away, the host
// asks every control host in the list it last heard, and keeps hearing of the
// cluster's changes. App hosts in the list are never asked.
func TestR421_AJoinedHostAsksTheOtherControlHostsWhenItsOwnIsGone(t *testing.T) {
	var appAsked atomic.Int32
	app := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { appAsked.Add(1) }))
	t.Cleanup(app.Close)
	var second []hosts.Peer
	core2 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": second})
	}))
	t.Cleanup(core2.Close)
	core2Addr := strings.TrimPrefix(core2.URL, "https://")
	first := []hosts.Peer{
		{ID: "host_core1", Name: "core-1", Address: "core-1:7444", Control: true},
		{ID: "host_app1", Name: "app-1", Address: strings.TrimPrefix(app.URL, "https://")},
		{ID: "host_core2", Name: "core-2", Address: core2Addr, Control: true},
	}
	second = append(slices.Clone(first), hosts.Peer{ID: "host_app2", Name: "app-2", Address: "10.0.0.9:7443"})
	core1 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"hosts": first})
	}))
	file := filepath.Join(t.TempDir(), hosts.PeersFile)
	sync := &hosts.PeerSync{Address: core1.URL, File: file, HTTP: core1.Client()}
	ctx := context.Background()
	got, err := sync.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, first, got)

	core1.Close()
	got, err = sync.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, second, got, "heard from core-2")
	kept, err := hosts.ReadPeers(file)
	require.NoError(t, err)
	require.Equal(t, second, kept)
	require.Zero(t, appAsked.Load(), "an app host is not a cluster listener")

	// Every control host away: the last list stands.
	core2.Close()
	got, err = sync.Peers(ctx)
	require.NoError(t, err)
	require.Equal(t, second, got)
}
