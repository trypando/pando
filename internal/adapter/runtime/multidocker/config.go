package multidocker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/client"
	"golang.org/x/crypto/ssh"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// Config is the adapter's configuration.
type Config struct {
	// Hosts are the Docker hosts, in order. Exactly one is the control host:
	// the one Pando's replicas, the edge, BuildKit and the registry run on
	// (O-47). A JSON list, or a string holding one, which is how a form's
	// text area sends it.
	Hosts json.RawMessage `json:"hosts"`

	// NetworkPool, NetworkBlockBits and OCIRuntime are the single-host Docker
	// adapter's settings of the same names, applied on every host. Each host
	// carves its own app networks from the same range: a bridge network
	// exists on one host only.
	NetworkPool      string `json:"network_pool,omitempty"`
	NetworkBlockBits int    `json:"network_block_bits,omitempty"`
	OCIRuntime       string `json:"oci_runtime,omitempty"`

	// AgentImage is the image each host's forwarding agent and each
	// restricted app's egress gateway run: an image of Pando that every host
	// can pull. Empty is the image Pando's own container on the control host
	// was started from.
	AgentImage string `json:"agent_image,omitempty"`

	// AgentPort is the host port each agent publishes. Zero is 7443.
	AgentPort int `json:"agent_port,omitempty"`

	// Credentials arrive from the encrypted adapter_credentials store and
	// are held in memory only (R-190).
	Credentials Credentials `json:"credentials"`
}

// Credentials are the adapter's secrets.
type Credentials struct {
	// AgentAuthority is the agents' certificate authority as PEM, each
	// certificate followed by its key, the one that issues first. Generated
	// by `pando host-agent new-authority`. Pando issues its own client
	// certificate and each agent's server certificate from it.
	AgentAuthority string `json:"agent_authority"`

	// DockerTLS is the client certificate, its key and the CA that signed the
	// daemons' certificates, as PEM, for hosts reached over tcp://.
	DockerTLS string `json:"docker_tls,omitempty"`

	// SSHKey is the private key for hosts reached over ssh://.
	SSHKey string `json:"ssh_key,omitempty"`
}

// HostConfig is one Docker host.
type HostConfig struct {
	// Name is how the host is known in placement, the console, errors and
	// the agent's certificate: lower-case letters, digits and dashes.
	Name string `json:"name"`

	// Endpoint is the Docker API: unix:///var/run/docker.sock (the control
	// host, usually), tcp://host:2376 with docker_tls, or ssh://user@host
	// with ssh_key. Empty is the environment, as the single-host adapter.
	Endpoint string `json:"endpoint,omitempty"`

	// SSHHostKey is the host's SSH public key, as in known_hosts or
	// authorized_keys, for an ssh:// endpoint. Required: an unverified SSH
	// host key would hand the root-equivalent Docker credential to whoever
	// answers.
	SSHHostKey string `json:"ssh_host_key,omitempty"`

	// AgentAddress is where Pando's proxy reaches this host's agent, as
	// host:port. Empty is the endpoint's host and the agent port.
	AgentAddress string `json:"agent_address,omitempty"`

	// Control marks the control host.
	Control bool `json:"control,omitempty"`

	// NoPlacement keeps new apps off the host; apps already on it stay
	// (R-256). It is how a host is emptied before it is removed.
	NoPlacement bool `json:"no_placement,omitempty"`

	// TotalCPUMillis and TotalMemoryBytes tell Pando how much of the host it
	// may use, as on one host. Empty asks the daemon.
	TotalCPUMillis   int   `json:"total_cpu_millis,omitempty"`
	TotalMemoryBytes int64 `json:"total_memory_bytes,omitempty"`
	TotalDiskBytes   int64 `json:"total_disk_bytes,omitempty"`
}

var hostName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// parseHosts reads Hosts as a list, or as a string holding one.
func parseHosts(raw json.RawMessage) ([]HostConfig, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, errors.New("no hosts are listed")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		raw = json.RawMessage(text)
	}
	var hosts []HostConfig
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&hosts); err != nil {
		return nil, fmt.Errorf("the host list is not a JSON list of hosts: %w", err)
	}
	return hosts, nil
}

// validate checks the configuration that can be checked without a network.
func validate(cfg Config, hosts []HostConfig) error {
	if len(hosts) == 0 {
		return errors.New("no hosts are listed")
	}
	seen := map[string]bool{}
	controls := 0
	for _, h := range hosts {
		if !hostName.MatchString(h.Name) {
			return fmt.Errorf("%q is not a usable host name: use lower-case letters, digits and dashes, such as app-host-1", h.Name)
		}
		if seen[h.Name] {
			return fmt.Errorf("two hosts are named %q", h.Name)
		}
		seen[h.Name] = true
		if h.Control {
			controls++
		}
		u, err := endpointURL(h.Endpoint)
		if err != nil {
			return fmt.Errorf("host %s: %w", h.Name, err)
		}
		switch u.Scheme {
		case "tcp":
			if cfg.Credentials.DockerTLS == "" {
				return fmt.Errorf("host %s is reached over tcp://, and the docker_tls credential is empty. Pando does not use the Docker API without TLS: it is root on that host", h.Name)
			}
		case "ssh":
			if cfg.Credentials.SSHKey == "" {
				return fmt.Errorf("host %s is reached over ssh://, and the ssh_key credential is empty", h.Name)
			}
			if h.SSHHostKey == "" {
				return fmt.Errorf("host %s is reached over ssh:// and has no ssh_host_key. Copy the line for this host from ~/.ssh/known_hosts, or the host's /etc/ssh/ssh_host_ed25519_key.pub", h.Name)
			}
		}
		if _, err := agentAddress(h, agentPort(cfg)); err != nil {
			return err
		}
	}
	if controls != 1 {
		return fmt.Errorf("exactly one host must be the control host, the one Pando runs on; %d are", controls)
	}
	if pool, ok := networkPool(cfg); !ok || !pool.IsValid() {
		return errors.New(`network_pool cannot be "off" on several hosts: each host's agent forwards only into the app network range, so it must be one`)
	}
	return nil
}

func endpointURL(endpoint string) (*url.URL, error) {
	if endpoint == "" {
		return &url.URL{Scheme: "env"}, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("%q is not a Docker endpoint", endpoint)
	}
	switch u.Scheme {
	case "unix", "tcp", "ssh", "npipe":
		return u, nil
	}
	return nil, fmt.Errorf("%q is not a Docker endpoint: use unix://, tcp:// or ssh://", endpoint)
}

func agentPort(cfg Config) int {
	if cfg.AgentPort > 0 && cfg.AgentPort < 65536 {
		return cfg.AgentPort
	}
	return hostagent.DefaultPort
}

// agentAddress is where the proxy dials a host's agent.
func agentAddress(h HostConfig, port int) (string, error) {
	if h.AgentAddress != "" {
		if _, _, err := net.SplitHostPort(h.AgentAddress); err != nil {
			return "", fmt.Errorf("host %s: agent_address %q is not host:port", h.Name, h.AgentAddress)
		}
		return h.AgentAddress, nil
	}
	u, err := endpointURL(h.Endpoint)
	if err != nil {
		return "", err
	}
	if (u.Scheme == "tcp" || u.Scheme == "ssh") && u.Hostname() != "" {
		return net.JoinHostPort(u.Hostname(), strconv.Itoa(port)), nil
	}
	return "", fmt.Errorf("host %s has no agent_address. Set it to the address Pando's container reaches this host at, and the agent port, such as 10.0.0.5:%d", h.Name, port)
}

const defaultNetworkPool = "10.213.0.0/16"

// networkPool is the app network range, read as the Docker adapter reads it.
func networkPool(cfg Config) (netip.Prefix, bool) {
	raw := strings.TrimSpace(cfg.NetworkPool)
	switch raw {
	case "off", "docker":
		return netip.Prefix{}, false
	case "":
		raw = defaultNetworkPool
	}
	pool, err := netip.ParsePrefix(raw)
	if err != nil || !pool.Addr().Is4() || pool.Bits() > 24 {
		return netip.Prefix{}, false
	}
	return pool.Masked(), true
}

// newClient makes the Docker client for one host.
func newClient(h HostConfig, creds Credentials) (*client.Client, error) {
	u, err := endpointURL(h.Endpoint)
	if err != nil {
		return nil, err
	}
	switch u.Scheme {
	case "env":
		return client.New(client.FromEnv)
	case "unix", "npipe":
		return client.New(client.WithHost(h.Endpoint))
	case "tcp":
		tlsConfig, err := dockerTLS(creds.DockerTLS)
		if err != nil {
			return nil, err
		}
		hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tlsConfig}}
		return client.New(client.WithHTTPClient(hc), client.WithHost(h.Endpoint))
	case "ssh":
		d, err := newSSHDialer(u, creds.SSHKey, h.SSHHostKey)
		if err != nil {
			return nil, err
		}
		return client.New(client.WithHost("unix:///var/run/docker.sock"), client.WithDialContext(d.dial))
	}
	return nil, fmt.Errorf("%q is not a Docker endpoint", h.Endpoint)
}

// dockerTLS reads a client certificate, its key and the daemons' CA.
func dockerTLS(bundle string) (*tls.Config, error) {
	var certPEM, keyPEM []byte
	roots := x509.NewCertPool()
	rest := []byte(bundle)
	var certs [][]byte
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE":
			certs = append(certs, pem.EncodeToMemory(block))
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			keyPEM = pem.EncodeToMemory(block)
		}
	}
	// The client certificate is the one that matches the key; every other
	// certificate is trusted as an authority for the daemons.
	for _, c := range certs {
		if _, err := tls.X509KeyPair(c, keyPEM); err == nil && certPEM == nil {
			certPEM = c
			continue
		}
		roots.AppendCertsFromPEM(c)
	}
	if certPEM == nil {
		return nil, errors.New("docker_tls has no client certificate matching its private key. Paste the CA (ca.pem), the client certificate (cert.pem) and its key (key.pem)")
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, RootCAs: roots}, nil
}

// sshDialer reaches a remote Docker socket over one SSH connection, opened
// when first needed and again after it drops.
type sshDialer struct {
	addr   string
	config *ssh.ClientConfig

	mu   sync.Mutex
	conn *ssh.Client
}

func newSSHDialer(u *url.URL, key, hostKey string) (*sshDialer, error) {
	signer, err := ssh.ParsePrivateKey([]byte(key))
	if err != nil {
		return nil, errors.New("ssh_key is not a private key Pando can read; use an unencrypted OpenSSH or PEM key")
	}
	pub, err := parseHostKey(hostKey)
	if err != nil {
		return nil, err
	}
	user := u.User.Username()
	if user == "" {
		user = "root"
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	return &sshDialer{
		addr: net.JoinHostPort(u.Hostname(), port),
		config: &ssh.ClientConfig{
			User:            user,
			Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
			HostKeyCallback: ssh.FixedHostKey(pub),
			Timeout:         10 * time.Second,
		},
	}, nil
}

// parseHostKey reads a key from a known_hosts line or an authorized_keys
// one.
func parseHostKey(line string) (ssh.PublicKey, error) {
	if _, _, pub, _, _, err := ssh.ParseKnownHosts([]byte(line)); err == nil {
		return pub, nil
	}
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil {
		return nil, errors.New("ssh_host_key is not an SSH public key; copy the host's line from ~/.ssh/known_hosts")
	}
	return pub, nil
}

func (d *sshDialer) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for attempt := 0; attempt < 2; attempt++ {
		if d.conn == nil {
			var nd net.Dialer
			raw, err := nd.DialContext(ctx, "tcp", d.addr)
			if err != nil {
				return nil, err
			}
			c, chans, reqs, err := ssh.NewClientConn(raw, d.addr, d.config)
			if err != nil {
				_ = raw.Close()
				return nil, err
			}
			d.conn = ssh.NewClient(c, chans, reqs)
		}
		conn, err := d.conn.Dial("unix", "/var/run/docker.sock")
		if err == nil {
			return conn, nil
		}
		_ = d.conn.Close()
		d.conn = nil
	}
	return nil, errs.New(errs.AdapterUnavailable, "Could not reach Docker over SSH.")
}
