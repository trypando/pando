package hosts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/keyfile"
)

// DefaultClusterDir is where a joined host keeps what it was given (R-415),
// unless server.cluster_dir says otherwise.
const DefaultClusterDir = "/var/lib/pando/cluster"

// Files in the cluster directory.
const (
	JoinedFile        = "joined.json"
	HostCertFile      = "host.crt"
	HostKeyFile       = "host.key"
	AuthorityCertFile = "ca.crt"
	// HostClientCertFile and HostClientKeyFile are what the host checks the
	// others' agents with (R-421); PeersFile is the cluster's hosts as the
	// host last heard them (PeerSync).
	HostClientCertFile = "host-client.crt"
	HostClientKeyFile  = "host-client.key"
	PeersFile          = "peers.json"
	// HostNextKeyFile is the key a renewal not yet answered and written asks
	// a certificate for (HostRenewal.nextKey, R-421).
	HostNextKeyFile = "host-next.key"
)

// JoinFiles is where a joining host writes what it was given.
type JoinFiles struct {
	// Dir holds the host's certificate and key, the authority's
	// certificate, the database address for a control host, and joined.json,
	// written last, which says the join is done.
	Dir string
	// DatabaseURLPath is where config.Load reads a joined control host's
	// database address from, ahead of database.url.
	DatabaseURLPath string
	// TokenKeyPath and AuthorityPath are server.token_key_path and
	// server.cluster_authority_path: a control host's keys replace what its
	// own first start made there.
	TokenKeyPath  string
	AuthorityPath string
}

// Joined is what joined.json records: that this Pando is a host of a cluster,
// and as what.
type Joined struct {
	HostID   string    `json:"host_id"`
	Name     string    `json:"name"`
	Roles    []string  `json:"roles"`
	Cluster  string    `json:"cluster"`
	JoinedAt time.Time `json:"joined_at"`
	// NetworkPool is the runtime's app network range when the host joined,
	// which its agent forwards into (R-416). Empty is the runtime's default.
	NetworkPool string `json:"network_pool,omitempty"`
}

// AppOnly reports whether the host holds the app role and not the control
// role: it runs as a host agent and serves no API (R-414).
func (j Joined) AppOnly() bool {
	return !slices.Contains(j.Roles, state.HostRoleControl)
}

// Read returns the join this Pando made, or ok false when it has not joined.
func (f JoinFiles) Read() (Joined, bool, error) {
	data, err := os.ReadFile(filepath.Join(f.Dir, JoinedFile)) // #nosec G304 -- the configured cluster directory
	if errors.Is(err, os.ErrNotExist) {
		return Joined{}, false, nil
	}
	if err != nil {
		return Joined{}, false, errs.Wrap(errs.Internal, "Pando could not read whether this host has joined a cluster.", err)
	}
	var j Joined
	if err := json.Unmarshal(data, &j); err != nil {
		return Joined{}, false, errs.Wrap(errs.Internal, fmt.Sprintf("%s is not readable, so Pando cannot tell which cluster this host joined.", filepath.Join(f.Dir, JoinedFile)), err).
			WithRemedy("Restore the file from a backup of this host, or remove the host from the cluster and join it again.")
	}
	return j, true, nil
}

// unmark removes joined.json, so this Pando is not joined and can join
// again: what a join that could not be confirmed leaves (Joiner.confirm).
func (f JoinFiles) unmark() {
	_ = os.Remove(filepath.Join(f.Dir, JoinedFile))
}

type fileWrite struct {
	path string
	data []byte
}

// write puts what a join gave this host on disk, every file 0600 and each
// replaced whole (keyfile.Replace), joined.json last.
func (f JoinFiles) write(j Joined, res JoinResult, keyPEM []byte) error {
	if err := os.MkdirAll(f.Dir, 0o700); err != nil {
		return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not create %s for what the cluster gave this host.", f.Dir), err)
	}
	writes := []fileWrite{
		{filepath.Join(f.Dir, HostKeyFile), keyPEM},
		{filepath.Join(f.Dir, HostCertFile), res.Certificate},
		{filepath.Join(f.Dir, AuthorityCertFile), res.Authority},
	}
	if len(res.HostClientCert) > 0 && !res.HostClientKey.IsZero() {
		writes = append(writes,
			fileWrite{filepath.Join(f.Dir, HostClientCertFile), res.HostClientCert},
			fileWrite{filepath.Join(f.Dir, HostClientKeyFile), []byte(res.HostClientKey.Reveal())})
	}
	if k := res.Keys; k != nil {
		writes = append(writes,
			fileWrite{f.TokenKeyPath, []byte(k.TokenKey.Reveal())},
			fileWrite{f.AuthorityPath, []byte(k.Authority.Reveal())},
			fileWrite{f.DatabaseURLPath, []byte(k.DatabaseURL.Reveal())})
		if k.SecretsKeyPath != "" && !k.SecretsKey.IsZero() {
			writes = append(writes, fileWrite{k.SecretsKeyPath, []byte(k.SecretsKey.Reveal())})
		}
	}
	marker, err := json.Marshal(j)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not record the join.", err)
	}
	writes = append(writes, fileWrite{filepath.Join(f.Dir, JoinedFile), marker})

	for _, w := range writes {
		if err := os.MkdirAll(filepath.Dir(w.path), 0o700); err != nil {
			return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not create the directory for %s.", w.path), err)
		}
		if err := keyfile.Replace(w.path, w.data); err != nil {
			return errs.Wrap(errs.Internal, fmt.Sprintf("Pando could not write %s, which this host needs to run in the cluster.", w.path), err).
				WithRemedy("Make the directory writable by Pando, remove this host from the cluster, and join it again with a new token.")
		}
	}
	return nil
}
