package multidocker

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// dockerTLSBundle is a CA and a client certificate it signed, with the
// client's key, as `docker context` would have them: ca.pem, cert.pem,
// key.pem.
func dockerTLSBundle(t *testing.T) string {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	caTmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "docker ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pando"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)

	var b strings.Builder
	_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	_ = pem.Encode(&b, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	_ = pem.Encode(&b, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return b.String()
}

// sshKeys returns a client private key in OpenSSH form and a host key pair.
func sshKeys(t *testing.T) (clientKey string, hostSigner ssh.Signer) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "pando")
	require.NoError(t, err)
	_, hostPriv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	hostSigner, err = ssh.NewSignerFromKey(hostPriv)
	require.NoError(t, err)
	return string(pem.EncodeToMemory(block)), hostSigner
}

// TestR190_AConfiguredHostListMakesOneAdapterPerHostWithoutContactingAny
// asserts Configure on a valid list: a Docker adapter per host, over TLS or
// SSH with the credentials held in memory only (R-190), the agent addresses
// derived, and nothing contacted.
func TestR190_AConfiguredHostListMakesOneAdapterPerHostWithoutContactingAny(t *testing.T) {
	ca, err := hostagent.NewAuthority()
	require.NoError(t, err)
	clientKey, hostSigner := sshKeys(t)
	hostKey := string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	cfg := map[string]any{
		"hosts": []map[string]any{
			{"name": "control", "control": true, "agent_address": "10.0.0.5:7443", "endpoint": "unix:///var/run/docker.sock"},
			{"name": "app-1", "endpoint": "tcp://10.0.0.6:2376", "no_placement": true},
			{"name": "app-2", "endpoint": "ssh://deploy@10.0.0.7:2222", "ssh_host_key": hostKey},
			{"name": "app-3"},
		},
		"agent_port":  8443,
		"agent_image": "registry.example/pando:1.2.3",
		"credentials": map[string]string{"agent_authority": string(ca), "docker_tls": dockerTLSBundle(t), "ssh_key": clientKey},
	}
	// app-3 reaches Docker from the environment and has no agent address.
	_, err = configureWith(cfg)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app-3 has no agent_address")

	cfg["hosts"].([]map[string]any)[3]["agent_address"] = "10.0.0.8:8443"
	a, err := configureWith(cfg)
	require.NoError(t, err)
	require.Len(t, a.hosts, 4)
	require.Equal(t, "control", a.control.cfg.Name)
	require.NotNil(t, a.local)
	require.Equal(t, "10.0.0.5:7443", a.hosts[0].agentAddr, "the address given")
	require.Equal(t, "10.0.0.6:8443", a.hosts[1].agentAddr, "the endpoint's host and the agent port")
	require.Equal(t, "10.0.0.7:8443", a.hosts[2].agentAddr)
	require.True(t, a.hosts[1].cfg.NoPlacement)
	require.Equal(t, "registry.example/pando:1.2.3", a.agentImage(context.Background()))
	require.Equal(t, 8443, a.hosts[1].agent.port)
	require.Equal(t, defaultNetworkPool, a.hosts[1].agent.pool.String())
	require.NotNil(t, a.clientCert.cert.Leaf)
	require.Equal(t, hostagent.ClientCommonName, a.clientCert.cert.Leaf.Subject.CommonName)
}

func configureWith(cfg map[string]any) (*Adapter, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	a := New()
	return a, a.Configure(context.Background(), raw)
}

func TestAConfigurationThatCannotBeReadSaysWhat(t *testing.T) {
	ca, err := hostagent.NewAuthority()
	require.NoError(t, err)
	creds := map[string]string{"agent_authority": string(ca)}
	one := func(h map[string]any) map[string]any {
		base := map[string]any{"name": "control", "control": true, "agent_address": "10.0.0.5:7443"}
		for k, v := range h {
			base[k] = v
		}
		return map[string]any{"hosts": []map[string]any{base}, "credentials": creds}
	}

	err = New().Configure(context.Background(), json.RawMessage(`{"hosts": 7`))
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))

	for name, c := range map[string]struct {
		cfg  map[string]any
		says string
	}{
		"no hosts":                      {map[string]any{"credentials": creds}, "host list"},
		"an empty list":                 {map[string]any{"hosts": []any{}, "credentials": creds}, "no hosts"},
		"an unknown field":              {map[string]any{"hosts": []map[string]any{{"name": "control", "colour": "red"}}}, "host list"},
		"the same name twice":           {map[string]any{"hosts": []map[string]any{{"name": "a", "control": true, "agent_address": "a:1"}, {"name": "a", "agent_address": "a:1"}}, "credentials": creds}, "two hosts"},
		"a bad endpoint":                {one(map[string]any{"endpoint": "http://10.0.0.5"}), "not a Docker endpoint"},
		"an endpoint that is not a URL": {one(map[string]any{"endpoint": "tcp://[::1"}), "not a Docker endpoint"},
		"a bad agent address":           {one(map[string]any{"agent_address": "10.0.0.5"}), "not host:port"},
		"an ssh host without a key":     {one(map[string]any{"endpoint": "ssh://pando@10.0.0.6"}), "ssh_key credential is empty"},
	} {
		raw, err := json.Marshal(c.cfg)
		require.NoError(t, err)
		err = New().Configure(context.Background(), raw)
		require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), name)
		require.Contains(t, errs.As(err).Message+errs.As(err).Error(), c.says, name)
	}

	// An authority PEM that is not one.
	bad := one(nil)
	bad["credentials"] = map[string]string{"agent_authority": "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n"}
	_, err = configureWith(bad)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "new-authority")
}

func TestTheAgentPortAndTheAppRangeAreReadAsTheDockerAdapterReadsThem(t *testing.T) {
	require.Equal(t, hostagent.DefaultPort, agentPort(Config{}))
	require.Equal(t, hostagent.DefaultPort, agentPort(Config{AgentPort: 70000}))
	require.Equal(t, 9000, agentPort(Config{AgentPort: 9000}))

	for raw, ok := range map[string]bool{
		"":               true,
		"10.50.0.0/16":   true,
		" 10.50.7.0/24 ": true,
		"off":            false,
		"docker":         false,
		"10.50.0.0/25":   false, // too small to carve app networks from
		"fd00::/48":      false,
		"nonsense":       false,
	} {
		_, got := networkPool(Config{NetworkPool: raw})
		require.Equal(t, ok, got, raw)
	}
	pool, _ := networkPool(Config{NetworkPool: "10.50.7.9/24"})
	require.Equal(t, "10.50.7.0/24", pool.String())
}

func TestDockerTLSFindsTheClientCertificateByItsKey(t *testing.T) {
	cfg, err := dockerTLS(dockerTLSBundle(t))
	require.NoError(t, err)
	require.Len(t, cfg.Certificates, 1)
	require.NotNil(t, cfg.RootCAs)

	_, err = dockerTLS("")
	require.ErrorContains(t, err, "no client certificate")

	// A CA alone, with no key to match a certificate to.
	bundle := dockerTLSBundle(t)
	_, err = dockerTLS(bundle[:strings.Index(bundle, "-----BEGIN EC PRIVATE KEY")])
	require.ErrorContains(t, err, "cert.pem")
}

func TestNewClientMakesOneForEachKindOfEndpoint(t *testing.T) {
	clientKey, hostSigner := sshKeys(t)
	hostKey := string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey()))
	creds := Credentials{DockerTLS: dockerTLSBundle(t), SSHKey: clientKey}
	for _, endpoint := range []string{"", "unix:///var/run/docker.sock", "tcp://10.0.0.6:2376", "ssh://10.0.0.7"} {
		cli, err := newClient(HostConfig{Name: "h", Endpoint: endpoint, SSHHostKey: hostKey}, creds)
		require.NoError(t, err, endpoint)
		require.NotNil(t, cli)
		_ = cli.Close()
	}
	_, err := newClient(HostConfig{Endpoint: "tcp://10.0.0.6:2376"}, Credentials{DockerTLS: "nothing"})
	require.Error(t, err)
	_, err = newClient(HostConfig{Endpoint: "ssh://10.0.0.7", SSHHostKey: hostKey}, Credentials{SSHKey: "not a key"})
	require.ErrorContains(t, err, "ssh_key is not a private key")
	_, err = newClient(HostConfig{Endpoint: "ssh://10.0.0.7", SSHHostKey: "not a key"}, creds)
	require.ErrorContains(t, err, "ssh_host_key")
	_, err = newClient(HostConfig{Endpoint: "ftp://10.0.0.7"}, creds)
	require.Error(t, err)
}

func TestTheSSHHostKeyIsReadFromAKnownHostsOrAuthorizedKeysLine(t *testing.T) {
	_, hostSigner := sshKeys(t)
	authorized := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))
	known := "10.0.0.7 " + authorized

	for _, line := range []string{authorized, known} {
		pub, err := parseHostKey(line)
		require.NoError(t, err, line)
		require.Equal(t, hostSigner.PublicKey().Marshal(), pub.Marshal())
	}
	_, err := parseHostKey("10.0.0.7 ssh-ed25519 nope")
	require.Error(t, err)

	u, _ := url.Parse("ssh://10.0.0.7")
	clientKey, _ := sshKeys(t)
	d, err := newSSHDialer(u, clientKey, authorized)
	require.NoError(t, err)
	require.Equal(t, "10.0.0.7:22", d.addr)
	require.Equal(t, "root", d.config.User)
}

// sshServer is an SSH server on loopback that accepts one client key and
// answers a request for Docker's socket with an echo.
func sshServer(t *testing.T, hostSigner ssh.Signer, clientKey string) string {
	t.Helper()
	signer, err := ssh.ParsePrivateKey([]byte(clientKey))
	require.NoError(t, err)
	want := signer.PublicKey().Marshal()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if string(key.Marshal()) == string(want) {
				return nil, nil
			}
			return nil, errors.New("unknown key")
		},
	}
	cfg.AddHostKey(hostSigner)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			raw, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, chans, reqs, err := ssh.NewServerConn(raw, cfg)
				if err != nil {
					_ = raw.Close()
					return
				}
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() != "direct-streamlocal@openssh.com" {
						_ = nc.Reject(ssh.UnknownChannelType, "no")
						continue
					}
					ch, chReqs, err := nc.Accept()
					if err != nil {
						continue
					}
					go ssh.DiscardRequests(chReqs)
					go func() { _, _ = io.Copy(ch, ch); _ = ch.Close() }()
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// TestR190_DockerOverSSHIsReachedOnlyOnThePinnedHostKey asserts that the
// SSH dialer reaches Docker's socket on the host whose key was configured,
// and refuses a host presenting any other: an unverified host would be
// handed the root-equivalent Docker credential.
func TestR190_DockerOverSSHIsReachedOnlyOnThePinnedHostKey(t *testing.T) {
	clientKey, hostSigner := sshKeys(t)
	addr := sshServer(t, hostSigner, clientKey)
	u, err := url.Parse("ssh://pando@" + addr)
	require.NoError(t, err)

	d, err := newSSHDialer(u, clientKey, string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for range 2 { // the second reuses the SSH connection
		conn, err := d.dial(ctx, "unix", "docker.sock")
		require.NoError(t, err)
		_, err = io.WriteString(conn, "ping")
		require.NoError(t, err)
		buf := make([]byte, 4)
		_, err = io.ReadFull(conn, buf)
		require.NoError(t, err)
		require.Equal(t, "ping", string(buf))
		_ = conn.Close()
	}

	// Another host key: the handshake is refused.
	_, other := sshKeys(t)
	d2, err := newSSHDialer(u, clientKey, string(ssh.MarshalAuthorizedKey(other.PublicKey())))
	require.NoError(t, err)
	_, err = d2.dial(ctx, "unix", "docker.sock")
	require.Error(t, err)
	require.Nil(t, d2.conn)

	// Nothing listening.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := ln.Addr().String()
	_ = ln.Close()
	u3, _ := url.Parse("ssh://pando@" + closed)
	d3, err := newSSHDialer(u3, clientKey, string(ssh.MarshalAuthorizedKey(hostSigner.PublicKey())))
	require.NoError(t, err)
	_, err = d3.dial(ctx, "unix", "docker.sock")
	require.Error(t, err)
}
