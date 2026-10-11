package incus

import (
	"crypto/tls"
	"encoding/pem"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"regexp"
	"strings"

	incusclient "github.com/lxc/incus/v6/client"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

// Config is the adapter's configuration (notes-incus-runtime.md, Settings).
type Config struct {
	// Address is the Incus API: unix:///var/lib/incus/unix.socket, or
	// https://host:8443 with the tls_client credential.
	Address string `json:"address,omitempty"`

	// ServerCertificate is the Incus server's certificate as PEM, for an
	// https:// address. Not a secret; required, because an unverified server
	// would be handed the client certificate, which is root on the cluster.
	ServerCertificate string `json:"server_certificate,omitempty"`

	// Project is the Incus project everything the adapter makes is in.
	Project string `json:"project,omitempty"`

	// Network is the Incus network each VM's one NIC is on (R-026). It must
	// be routable from Pando's host: the proxy reaches each VM's agent on it.
	Network string `json:"network,omitempty"`

	// ProxySources are the addresses Pando's proxy connects from, as CIDRs
	// separated by commas or spaces. The only sources each VM admits.
	ProxySources string `json:"proxy_sources,omitempty"`

	// StoragePool is the pool for root disks and volumes. Empty is the
	// project's default profile's root pool.
	StoragePool string `json:"storage_pool,omitempty"`

	// Target pins every VM to one cluster member, or to a cluster group as
	// "@group". Empty leaves placement to Incus (R-256).
	Target string `json:"target,omitempty"`

	// Profiles are applied to each VM, comma-separated. Empty is none: a
	// default profile usually carries a NIC onto the LAN.
	Profiles string `json:"profiles,omitempty"`

	// AgentImage is the image of Pando each VM's forwarding agent and each
	// restricted app's egress gateway run. Required: a VM has no other way to
	// get Pando's binary.
	AgentImage string `json:"agent_image,omitempty"`

	// AgentPort is the port each VM's agent listens on. Zero is 7443.
	AgentPort int `json:"agent_port,omitempty"`

	// GuestImage is an Incus image alias or fingerprint to start VMs from,
	// instead of the one the adapter prepares. It must have Podman serving
	// its Docker-compatible API on /run/podman/podman.sock, and socat.
	GuestImage string `json:"guest_image,omitempty"`

	// RootSizeBytes is each VM's root disk. Zero is 20 GiB.
	RootSizeBytes int64 `json:"root_size_bytes,omitempty"`

	// GuestOverheadBytes is the memory each VM has beyond its workloads'
	// limits, for the guest kernel, Podman and the agent. Zero is 512 MiB.
	GuestOverheadBytes int64 `json:"guest_overhead_bytes,omitempty"`

	// DefaultCPUs and DefaultMemoryBytes size a VM whose workloads set no
	// limits. Zero is 1 CPU and 1 GiB.
	DefaultCPUs        int   `json:"default_cpus,omitempty"`
	DefaultMemoryBytes int64 `json:"default_memory_bytes,omitempty"`

	// NetworkPool and NetworkBlockBits are the Docker adapter's settings of
	// the same names, applied inside every VM.
	NetworkPool      string `json:"network_pool,omitempty"`
	NetworkBlockBits int    `json:"network_block_bits,omitempty"`

	// TotalCPUMillis and TotalMemoryBytes cap the capacity reported, for an
	// operator giving Pando part of a cluster that runs other things.
	TotalCPUMillis   int   `json:"total_cpu_millis,omitempty"`
	TotalMemoryBytes int64 `json:"total_memory_bytes,omitempty"`

	// Credentials arrive from the encrypted adapter_credentials store and are
	// held in memory only (R-190).
	Credentials Credentials `json:"credentials"`
}

// Credentials are the adapter's secrets.
type Credentials struct {
	// TLSClient is the client certificate and its key, as PEM, for an
	// https:// address.
	TLSClient string `json:"tls_client,omitempty"`

	// AgentAuthority is the agents' certificate authority, as from
	// `pando host-agent new-authority`.
	AgentAuthority string `json:"agent_authority"`
}

const (
	defaultAddress            = "unix:///var/lib/incus/unix.socket"
	defaultProject            = "default"
	defaultRootSizeBytes      = 20 << 30
	defaultGuestOverheadBytes = 512 << 20
	defaultCPUs               = 1
	defaultMemoryBytes        = 1 << 30
	defaultNetworkPool        = "10.213.0.0/16"
)

var (
	nameRE    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	targetRE  = regexp.MustCompile(`^@?[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	settingIn = "the Incus runtime's settings"
)

// validate fills defaults and refuses a configuration that cannot work,
// before anything is contacted (R-254).
func (c *Config) validate() error {
	refuse := func(format string, args ...any) *errs.Error {
		return errs.Newf(errs.ValidInvalid, format, args...)
	}
	if c.Address == "" {
		c.Address = defaultAddress
	}
	u, err := url.Parse(c.Address)
	if err != nil || (u.Scheme != "unix" && u.Scheme != "https") {
		return refuse("The Incus address %q is not usable. Valid answers: unix:///var/lib/incus/unix.socket, or https://host:8443.", c.Address).
			WithRemedy("Set address in " + settingIn + ".")
	}
	if u.Scheme == "https" {
		if strings.TrimSpace(c.ServerCertificate) == "" {
			return refuse("The Incus server at %s is reached over HTTPS, and its certificate is not set, so Pando cannot tell it is talking to that server.", c.Address).
				WithRemedy("Paste the server's certificate (the output of `incus info | sed -n '/BEGIN CERT/,/END CERT/p'` on the server) into server_certificate in " + settingIn + ".")
		}
		if _, err := clientCertificate(c.Credentials.TLSClient); err != nil {
			return refuse("The Incus server at %s is reached over HTTPS, and the client certificate is not usable: %v.", c.Address, err).
				WithRemedy("Paste a client certificate and its key, trusted on the server with `incus config trust add-certificate`, into the TLS client certificate credential.")
		}
	}
	if c.Project == "" {
		c.Project = defaultProject
	}
	if !nameRE.MatchString(c.Project) {
		return refuse("%q is not an Incus project name.", c.Project)
	}
	if c.Network == "" {
		return refuse("The Incus runtime needs the network its VMs attach to, and none is set. Pando's proxy reaches each app's VM on it, so it must be routable from Pando's host.").
			WithRemedy("Set network in " + settingIn + " to an Incus network, such as incusbr0 on one server or an OVN network on a cluster.")
	}
	if !nameRE.MatchString(c.Network) {
		return refuse("%q is not an Incus network name.", c.Network)
	}
	if _, err := c.proxySources(); err != nil {
		return refuse("The proxy sources are not usable: %v. Valid answer: the addresses Pando's proxy connects to app VMs from, such as 10.1.0.10/32.", err).
			WithRemedy("Set proxy_sources in " + settingIn + ".")
	}
	if c.StoragePool != "" && !nameRE.MatchString(c.StoragePool) {
		return refuse("%q is not an Incus storage pool name.", c.StoragePool)
	}
	if c.Target != "" && !targetRE.MatchString(c.Target) {
		return refuse("%q is not a cluster member or group. Valid answers: a member name, or @ and a group name.", c.Target)
	}
	for _, p := range c.profiles() {
		if !nameRE.MatchString(p) {
			return refuse("%q is not an Incus profile name.", p)
		}
	}
	if strings.TrimSpace(c.AgentImage) == "" {
		return refuse("The Incus runtime needs the image of Pando that each app's VM runs its forwarding agent from, and none is set.").
			WithRemedy("Set agent_image in " + settingIn + " to this version of Pando's image, such as trypando/pando:<version>, or a copy in the install's registry.")
	}
	if c.AgentPort == 0 {
		c.AgentPort = hostagent.DefaultPort
	}
	if c.AgentPort < 1 || c.AgentPort > 65535 {
		return refuse("The agent port %d is not a port number.", c.AgentPort)
	}
	if c.RootSizeBytes == 0 {
		c.RootSizeBytes = defaultRootSizeBytes
	}
	if c.GuestOverheadBytes == 0 {
		c.GuestOverheadBytes = defaultGuestOverheadBytes
	}
	if c.DefaultCPUs == 0 {
		c.DefaultCPUs = defaultCPUs
	}
	if c.DefaultMemoryBytes == 0 {
		c.DefaultMemoryBytes = defaultMemoryBytes
	}
	if c.RootSizeBytes < 4<<30 || c.GuestOverheadBytes < 256<<20 || c.DefaultCPUs < 1 || c.DefaultMemoryBytes < 256<<20 {
		return refuse("A VM needs at least a 4 GiB root disk, 256 MiB of guest overhead, one CPU and 256 MiB of memory.")
	}
	if c.NetworkPool == "" {
		c.NetworkPool = defaultNetworkPool
	}
	if _, err := netip.ParsePrefix(c.NetworkPool); err != nil {
		return refuse("The app network range %q is not an address range such as 10.213.0.0/16.", c.NetworkPool)
	}
	if strings.TrimSpace(c.Credentials.AgentAuthority) == "" {
		return refuse("The Incus runtime needs the agents' certificate authority, and none is set.").
			WithRemedy("Run `pando host-agent new-authority` and paste its output into the agent certificate authority credential.")
	}
	return nil
}

// proxySources parses ProxySources.
func (c *Config) proxySources() ([]netip.Prefix, error) {
	fields := strings.FieldsFunc(c.ProxySources, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	if len(fields) == 0 {
		return nil, errors.New("none are set")
	}
	out := make([]netip.Prefix, 0, len(fields))
	for _, f := range fields {
		p, err := netip.ParsePrefix(f)
		if err != nil {
			a, aerr := netip.ParseAddr(f)
			if aerr != nil {
				return nil, fmt.Errorf("%q is not an address or range", f)
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		if p.Bits() == 0 {
			// Every address: the ACL would admit the LAN and every other VM.
			return nil, fmt.Errorf("%q admits every address", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

func (c *Config) profiles() []string {
	var out []string
	for _, p := range strings.FieldsFunc(c.Profiles, func(r rune) bool { return r == ',' || r == ' ' }) {
		out = append(out, p)
	}
	return out
}

// clientCertificate reads a certificate and its key from one PEM bundle.
func clientCertificate(bundle string) (tls.Certificate, error) {
	var certPEM, keyPEM []byte
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		switch {
		case block.Type == "CERTIFICATE" && certPEM == nil:
			certPEM = pem.EncodeToMemory(block)
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			keyPEM = pem.EncodeToMemory(block)
		}
	}
	if certPEM == nil || keyPEM == nil {
		return tls.Certificate{}, errors.New("it needs a certificate and its private key, as PEM")
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

// connect opens the Incus API. Connecting does not contact the server over
// a unix socket; over HTTPS it fetches the server's description once.
func connect(c Config) (incusclient.InstanceServer, error) {
	u, err := url.Parse(c.Address)
	if err != nil {
		return nil, err
	}
	if u.Scheme == "unix" {
		return incusclient.ConnectIncusUnix(u.Path, nil)
	}
	certPEM, keyPEM := splitPEM(c.Credentials.TLSClient)
	return incusclient.ConnectIncus(c.Address, &incusclient.ConnectionArgs{
		TLSServerCert: c.ServerCertificate,
		TLSClientCert: certPEM,
		TLSClientKey:  keyPEM,
		SkipGetServer: true,
	})
}

// splitPEM separates a bundle into its certificate and its key.
func splitPEM(bundle string) (string, string) {
	var cert, key string
	rest := []byte(bundle)
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return cert, key
		}
		switch {
		case block.Type == "CERTIFICATE" && cert == "":
			cert = string(pem.EncodeToMemory(block))
		case strings.HasSuffix(block.Type, "PRIVATE KEY"):
			key = string(pem.EncodeToMemory(block))
		}
	}
}
