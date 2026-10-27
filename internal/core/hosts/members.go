package hosts

import (
	"context"
	"encoding/json"
	"os"
	"sync"
	"sync/atomic"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/keyfile"
)

// Members is the cluster's hosts as a host last heard them, by ID, and every
// host removed from it: a ping from a host not among them, or from one
// removed, is refused (R-415, hostagent.Agent.Member).
//
// What is revoked is kept for good, here and in RevokedFile, which a host
// reads before it hears any list (LoadRevoked): a removed host's probe
// certificate may still be within its validity, its ID never comes back
// (a host that joins again is a new host), and a host that restarts while
// the control plane is away, or that reads an older peer list, still refuses
// it. Revocations come with the peer list, every round (PeerSync on an app
// host, the database on a control host), so a removal reaches every host's
// answers within a probe interval of being made.
type Members struct {
	ids atomic.Pointer[map[string]bool]

	mu      sync.Mutex
	revoked atomic.Pointer[revokedSet]
	// unsaved is that the last write of RevokedFile failed.
	unsaved atomic.Bool
}

type revokedSet struct {
	hosts map[string]bool
	certs map[string]bool
}

// RevokedFile, in the cluster directory, is every host removed from the
// cluster as this host last heard it (Members).
const RevokedFile = "revoked.json"

// Track is peers, remembering each list it returns.
func (m *Members) Track(peers func(context.Context) ([]Peer, error)) func(context.Context) ([]Peer, error) {
	return func(ctx context.Context) ([]Peer, error) {
		list, err := peers(ctx)
		if len(list) > 0 {
			ids := make(map[string]bool, len(list))
			for _, p := range list {
				ids[p.ID] = true
			}
			m.ids.Store(&ids)
		}
		return list, err
	}
}

// Revoke adds r to what is refused, and reports whether that added
// anything. Nothing is ever taken back.
func (m *Members) Revoke(r state.Revoked) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	next := revokedSet{hosts: map[string]bool{}, certs: map[string]bool{}}
	if cur := m.revoked.Load(); cur != nil {
		for k := range cur.hosts {
			next.hosts[k] = true
		}
		for k := range cur.certs {
			next.certs[k] = true
		}
	}
	before := len(next.hosts) + len(next.certs)
	for _, h := range r.Hosts {
		next.hosts[h] = true
	}
	for _, c := range r.Certs {
		next.certs[c] = true
	}
	if len(next.hosts)+len(next.certs) == before {
		return false
	}
	m.revoked.Store(&next)
	return true
}

// Revoked is everything refused, for RevokedFile.
func (m *Members) Revoked() state.Revoked {
	out := state.Revoked{Hosts: []string{}, Certs: []string{}}
	if cur := m.revoked.Load(); cur != nil {
		for k := range cur.hosts {
			out.Hosts = append(out.Hosts, k)
		}
		for k := range cur.certs {
			out.Certs = append(out.Certs, k)
		}
	}
	return out
}

// Has reports whether hostID is in the cluster as last heard and was never
// removed. Before any list is heard every host not removed is taken, so a
// host that has just started answers the others rather than being reported
// unreached by all of them.
func (m *Members) Has(hostID string) bool {
	if r := m.revoked.Load(); r != nil && r.hosts[hostID] {
		return false
	}
	ids := m.ids.Load()
	return ids == nil || (*ids)[hostID]
}

// Admits reports whether a ping from hostID, sent with the certificate whose
// fingerprint is certSHA256, is answered: the host is a member and neither it
// nor the certificate was revoked (R-415).
func (m *Members) Admits(hostID, certSHA256 string) bool {
	if r := m.revoked.Load(); r != nil && r.certs[certSHA256] {
		return false
	}
	return m.Has(hostID)
}

// LoadRevoked adds what path, a RevokedFile, holds. A file that is missing
// or unreadable adds nothing: the next peer list brings it again.
func (m *Members) LoadRevoked(path string) {
	data, err := os.ReadFile(path) // #nosec G304 -- the host's own cluster directory
	if err != nil {
		return
	}
	var r state.Revoked
	if json.Unmarshal(data, &r) == nil {
		m.Revoke(r)
	}
}

// KeepRevoked adds r to what is refused and, when that added anything or the
// last write failed, writes everything refused to path, a RevokedFile, for
// the next start.
func (m *Members) KeepRevoked(path string, r state.Revoked) error {
	if !m.Revoke(r) && !m.unsaved.Load() {
		return nil
	}
	data, err := json.Marshal(m.Revoked())
	if err == nil {
		err = keyfile.Replace(path, data)
	}
	m.unsaved.Store(err != nil)
	return err
}
