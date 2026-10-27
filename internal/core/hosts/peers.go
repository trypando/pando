package hosts

import (
	"cmp"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/keyfile"
)

// The cluster's hosts as each host's liveness checks reach them (R-421): the
// peer list a joined host fetches from the cluster listener every round and
// keeps on disk, so that a control plane it cannot reach leaves it the last
// list it heard.

// Peer is a host as another host's agent probes it.
type Peer struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address"`
	// Control is that the host holds the control role, so its Address is
	// a cluster listener: where an app host asks for this list again when
	// the one in its join token is gone (PeerSync).
	Control bool `json:"control,omitempty"`
	// Lost is that the host is marked lost (R-421): it is left out of the
	// cluster's size on every host, as it is on the leader.
	Lost bool `json:"lost,omitempty"`
}

// InstallHost reports whether h is the install's own first host: the machine
// the install started on, recorded at its first start (RecordInstallHost)
// rather than brought in by a join token.
func InstallHost(h state.Host) bool { return h.JoinedWith == "" }

// LivenessPeers is the hosts that take part in the cluster's liveness checks:
// every host, whatever its roles, the install's own first host included
// (R-421). An app host answers on its agent; a control host on its cluster
// listener (ControlPeers, its row's ping address), so every host counted in
// the cluster's size is also watched, and none is reported unreached for
// answering nowhere.
func LivenessPeers(list []state.Host) []Peer {
	out := make([]Peer, 0, len(list))
	for _, h := range list {
		out = append(out, Peer{ID: h.ID, Name: h.Name, Address: cmp.Or(h.PingAddress, h.Address),
			Control: slices.Contains(h.Roles, state.HostRoleControl), Lost: h.Lost})
	}
	return out
}

// PingAddress is address, host:port, with its port replaced by port:
// where a control host answers pings, on its cluster listener, at the
// machine it recorded. An address without a port is taken as a host.
func PingAddress(address string, port int) string {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = strings.Trim(address, "[]")
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// HostPublisher is the slice of state.Cluster a control host keeps its own
// row current with.
type HostPublisher interface {
	HostLister
	SetHostPing(ctx context.Context, hostID, address, certSHA256 string) error
	RotateHostCert(ctx context.Context, hostID, next string) error
}

// ControlPeers is a control host's peer list (R-421), read from the
// database each round. Reading it also keeps the host's own row saying where
// it answers pings, its cluster listener at Port, and the fingerprint of the
// certificate it answers with, Pin, in the row's ping columns. The row's
// address and pin stay the agent's, which the runtime reaches the host
// through (R-416): a host with both roles runs apps through its agent and
// answers pings on its listener. Written whenever it differs, so a write that
// failed is made the next round.
//
// A joined control host answers pings with the certificate its join wrote,
// which its agent also serves when it holds the app role, and renews it
// while it runs (ControlHostCert). So when that pin is not yet the row's
// agent pin, it becomes it and the old one is kept as the one before
// (RotateHostCert): the runtime accepts both (state.Host.Pins) until the
// agent serves the new one. The install's own host has no agent of its own.
type ControlPeers struct {
	Store HostPublisher
	Self  string
	Port  int
	Pin   string
	// PinOf, when set, is the fingerprint of the certificate the host
	// answers with now, in place of Pin: the certificate is renewed while
	// the host runs (R-421).
	PinOf  func() string
	Logger *zap.Logger
}

func (c *ControlPeers) pin() string {
	if c.PinOf != nil {
		return c.PinOf()
	}
	return c.Pin
}

// Peers lists every host, this one with where it answers now.
func (c *ControlPeers) Peers(ctx context.Context) ([]Peer, error) {
	list, err := c.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	for i, h := range list {
		if h.ID != c.Self {
			continue
		}
		addr, pin := PingAddress(h.Address, c.Port), c.pin()
		if addr == h.PingAddress && h.PingCertSHA256 == pin && !c.agentCertBehind(h, pin) {
			break
		}
		if err := c.publish(ctx, h, addr, pin); err != nil {
			if c.Logger != nil {
				c.Logger.Warn("could not record where this host answers the cluster's liveness checks; trying again next round",
					zap.String("host_id", h.ID), zap.String("address", addr), zap.Error(err))
			}
			break
		}
		list[i].PingAddress, list[i].PingCertSHA256 = addr, pin
	}
	return LivenessPeers(list), nil
}

// agentCertBehind reports whether pin, the certificate h answers pings
// with, is its agent's too and not yet recorded as such.
func (c *ControlPeers) agentCertBehind(h state.Host, pin string) bool {
	return !InstallHost(h) && pin != "" && pin != h.CertSHA256
}

// publish records where h answers pings and with what, and the renewed agent
// certificate when it is behind.
func (c *ControlPeers) publish(ctx context.Context, h state.Host, addr, pin string) error {
	if c.agentCertBehind(h, pin) {
		if err := c.Store.RotateHostCert(ctx, h.ID, pin); err != nil {
			return err
		}
	}
	if addr == h.PingAddress && pin == h.PingCertSHA256 {
		return nil
	}
	return c.Store.SetHostPing(ctx, h.ID, addr, pin)
}

// errNotAHost refuses a certificate no host of the cluster joined with.
var errNotAHost = errs.New(errs.AuthInvalid, "This certificate is not one a host of the cluster joined with, so the cluster's hosts are not listed to it.").
	WithRemedy("Join the host to the cluster with a new join token.")

// Peers is the peer list, for the host whose verified certificate is
// certDER: one a host of the cluster holds (cluster_hosts.cert_sha256, or
// the one before its latest renewal, or the one a control host answers pings
// with), never a removed host's, so a removed host no longer hears where the
// others are (R-415). A host presenting the certificate it renewed to is
// using it, so the one before is no longer its own (R-421).
func (s *Service) Peers(ctx context.Context, certDER []byte) ([]Peer, error) {
	h, pin, err := s.hostByCert(ctx, certDER)
	if err != nil {
		return nil, err
	}
	if pin == h.CertSHA256 && h.PrevCertSHA256 != "" {
		if _, err := s.Store.ConfirmHostCert(ctx, h.ID, pin); err != nil {
			return nil, err
		}
	}
	list, err := s.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	return LivenessPeers(list), nil
}

// ReadPeers reads a peer list written by PeerSync.
func ReadPeers(path string) ([]Peer, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the host's own cluster directory
	if err != nil {
		return nil, err
	}
	var out []Peer
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("%s is not a list of hosts: %w", path, err)
	}
	return out, nil
}

// JoinedTLS is how a joined host connects to the cluster listener: its own
// certificate, and the listener's checked against the cluster authority the
// host was given (ca.crt), whatever name the join token's address uses.
func JoinedTLS(authorityPEM []byte, cert tls.Certificate) (*tls.Config, error) {
	hash, err := authorityHash(authorityPEM)
	if err != nil {
		return nil, err
	}
	cfg := PinnedTLS(hash)
	cfg.Certificates = []tls.Certificate{cert}
	return cfg, nil
}

// PeerSync keeps a joined host's peer list current: every call fetches it
// from the cluster listener at Address and writes it to File, and when the
// listener cannot be reached returns what File last held (R-421: the control
// plane being away never stops an app). When Address does not answer, every
// control host in the list File holds is asked in turn, so a host whose join
// token named a control host that has since gone still hears of the cluster's
// changes.
type PeerSync struct {
	// Address is the cluster listener, https://host:port: the join token's.
	Address string
	TLS     *tls.Config
	File    string
	// HTTP is the client; nil is one with a short timeout, since a round
	// waits for it.
	HTTP *http.Client
}

func (p *PeerSync) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return &http.Client{Timeout: 2 * time.Second, Transport: &http.Transport{TLSClientConfig: p.TLS}}
}

// Peers is the peer list, fresh when a control host answers.
func (p *PeerSync) Peers(ctx context.Context) ([]Peer, error) {
	last, lerr := ReadPeers(p.File)
	list, err := p.fetchAny(ctx, last)
	if err != nil {
		if lerr != nil {
			return nil, fmt.Errorf("the cluster's hosts could not be fetched (%w) and none are kept from before", err)
		}
		return last, nil
	}
	return list, WritePeers(p.File, list)
}

// WritePeers keeps list at path, for ReadPeers, replacing what it held: the
// peer list a host probes when it cannot ask for a fresh one (R-421).
func WritePeers(path string, list []Peer) error {
	data, err := json.Marshal(list)
	if err == nil {
		err = keyfile.Replace(path, data)
	}
	if err != nil {
		return fmt.Errorf("the cluster's hosts could not be kept at %s: %w", path, err)
	}
	return nil
}

// fetchAny asks Address, then each control host in last, and returns the
// first list one answers with, or the first error.
func (p *PeerSync) fetchAny(ctx context.Context, last []Peer) ([]Peer, error) {
	list, first := p.fetch(ctx, p.Address)
	if first == nil {
		return list, nil
	}
	tried := map[string]bool{strings.TrimSuffix(p.Address, "/"): true}
	for _, peer := range last {
		addr := "https://" + peer.Address
		if !peer.Control || tried[addr] || ctx.Err() != nil {
			continue
		}
		tried[addr] = true
		if list, err := p.fetch(ctx, addr); err == nil {
			return list, nil
		}
	}
	return nil, first
}

func (p *PeerSync) fetch(ctx context.Context, address string) ([]Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(address, "/")+PeersPath, nil)
	if err != nil {
		return nil, err
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, refusal(address, resp.StatusCode, data)
	}
	var out struct {
		Hosts []Peer `json:"hosts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("the cluster at %s answered with something Pando cannot read: %w", address, err)
	}
	return out.Hosts, nil
}
