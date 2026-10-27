package hosts

import (
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
	"net/http"
	"os"
	"strings"
	"time"

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
// every host that joined. The install's own first host is left out: its agent
// is the one the runtime starts, which answers no ping, and a host that never
// answers would be taken for lost by the others (R-421).
func LivenessPeers(list []state.Host) []Peer {
	out := make([]Peer, 0, len(list))
	for _, h := range list {
		if !InstallHost(h) {
			out = append(out, Peer{ID: h.ID, Name: h.Name, Address: h.Address})
		}
	}
	return out
}

// errNotAHost refuses a certificate no host of the cluster joined with.
var errNotAHost = errs.New(errs.AuthInvalid, "This certificate is not one a host of the cluster joined with, so the cluster's hosts are not listed to it.").
	WithRemedy("Join the host to the cluster with a new join token.")

// Peers is the peer list, for the host whose verified certificate is
// certDER: one a host of the cluster joined with (cluster_hosts.cert_sha256),
// so a removed host no longer hears where the others are.
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
		if h.CertSHA256 == pin {
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
