package hosts

import (
	"context"
	"sync/atomic"
)

// Members is the cluster's hosts as a host last heard them, by ID: a ping
// from a host not among them, one removed from the cluster, is refused
// (R-415, hostagent.Agent.Member). Its probe certificate may still be
// within its validity; its ID never comes back, since a host that joins
// again is a new host.
type Members struct {
	ids atomic.Pointer[map[string]bool]
}

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

// Has reports whether hostID is in the cluster as last heard. Before any
// list is heard every host is taken, so a host that has just started answers
// the others rather than being reported unreached by all of them.
func (m *Members) Has(hostID string) bool {
	ids := m.ids.Load()
	return ids == nil || (*ids)[hostID]
}
