package hosts

import (
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
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
		out = append(out, Peer{ID: h.ID, Name: h.Name, Address: cmp.Or(h.PingAddress, h.Address)})
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
}

// ControlPeers is a control host's peer list (R-421), read from the
// database each round. Reading it also keeps the host's own row saying where
// it answers pings, its cluster listener at Port, and the fingerprint of the
// certificate it answers with, Pin, in the row's ping columns. The row's
// address and pin stay the agent's, which the runtime reaches the host
// through (R-416): a host with both roles runs apps through its agent and
// answers pings on its listener. Written whenever it differs, so a write that
// failed is made the next round.
type ControlPeers struct {
	Store  HostPublisher
	Self   string
	Port   int
	Pin    string
	Logger *zap.Logger
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
		addr := PingAddress(h.Address, c.Port)
		if addr == h.PingAddress && h.PingCertSHA256 == c.Pin {
			break
		}
		if err := c.Store.SetHostPing(ctx, h.ID, addr, c.Pin); err != nil {
			if c.Logger != nil {
				c.Logger.Warn("could not record where this host answers the cluster's liveness checks; trying again next round",
					zap.String("host_id", h.ID), zap.String("address", addr), zap.Error(err))
			}
			break
		}
		list[i].PingAddress, list[i].PingCertSHA256 = addr, c.Pin
	}
	return LivenessPeers(list), nil
}

// errNotAHost refuses a certificate no host of the cluster joined with.
var errNotAHost = errs.New(errs.AuthInvalid, "This certificate is not one a host of the cluster joined with, so the cluster's hosts are not listed to it.").
	WithRemedy("Join the host to the cluster with a new join token.")

// Peers is the peer list, for the host whose verified certificate is
// certDER: one a host of the cluster joined with (cluster_hosts.cert_sha256),
// or the one a control host answers pings with (ping_cert_sha256), so a
// removed host no longer hears where the others are.
func (s *Service) Peers(ctx context.Context, certDER []byte) ([]Peer, error) {
	if len(certDER) == 0 {
		return nil, errNotAHost
	}
	list, err := s.Store.Hosts(ctx)
	if err != nil {
		return nil, err
	}
	pin := hostagent.Fingerprint256(certDER)
	for _, h := range list {
		if h.CertSHA256 == pin || h.PingCertSHA256 == pin {
			return LivenessPeers(list), nil
		}
	}
	return nil, errNotAHost
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
	block, _ := pem.Decode(authorityPEM)
	if block == nil {
		return nil, errors.New("the cluster authority's certificate is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("the cluster authority's certificate cannot be read: %w", err)
	}
	sum := sha256.Sum256(ca.RawSubjectPublicKeyInfo)
	cfg := PinnedTLS(hex.EncodeToString(sum[:]))
	cfg.Certificates = []tls.Certificate{cert}
	return cfg, nil
}

// PeerSync keeps a joined host's peer list current: every call fetches it
// from the cluster listener at Address and writes it to File, and when the
// listener cannot be reached returns what File last held (R-421: the control
// plane being away never stops an app).
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

// Peers is the peer list, fresh when the cluster answers.
func (p *PeerSync) Peers(ctx context.Context) ([]Peer, error) {
	list, err := p.fetch(ctx)
	if err != nil {
		last, rerr := ReadPeers(p.File)
		if rerr != nil {
			return nil, fmt.Errorf("the cluster's hosts could not be fetched (%w) and none are kept from before", err)
		}
		return last, nil
	}
	data, err := json.Marshal(list)
	if err == nil {
		err = keyfile.Replace(p.File, data)
	}
	if err != nil {
		return list, fmt.Errorf("the cluster's hosts could not be kept at %s: %w", p.File, err)
	}
	return list, nil
}

func (p *PeerSync) fetch(ctx context.Context) ([]Peer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(p.Address, "/")+PeersPath, nil)
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
		return nil, refusal(p.Address, resp.StatusCode, data)
	}
	var out struct {
		Hosts []Peer `json:"hosts"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("the cluster at %s answered with something Pando cannot read: %w", p.Address, err)
	}
	return out.Hosts, nil
}
