package standby

import (
	"cmp"
	"context"
	"slices"

	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/notice"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Standby is where the app's standby stands, for its notice (R-422): when
// the last try to place it found no host with room, the runtime's message,
// which names the largest room any host has, and its remedy. Otherwise
// empty. The app deployed either way: no room is never a blocker. An app
// whose standby could not be refreshed still has the one it had, so it is
// not told it runs without one.
func (s *Service) Standby(ctx context.Context, appID string) (notice.StandbyStanding, error) {
	sb, found, err := s.Store.Get(ctx, appID)
	if err != nil || !found {
		return notice.StandbyStanding{}, err
	}
	if sb.Host != "" || sb.LastErrorCode != string(errs.CapacityWouldOversubscribe) {
		return notice.StandbyStanding{}, nil
	}
	return notice.StandbyStanding{Unplaced: sb.LastError, Remedy: sb.LastErrorRemedy}, nil
}

var _ notice.StandbyRooms = (*Service)(nil)

// Homes is the standby state the liveness pass reads. *state.Standbys is
// one.
type Homes interface {
	HomedOn(ctx context.Context, host string) ([]state.Standby, error)
	LeftOn(ctx context.Context, host string) ([]state.Standby, error)
}

// Hosts names a cluster host. *state.Cluster is one.
type Hosts interface {
	Host(ctx context.Context, hostID string) (state.Host, error)
}

// Failover is the standby side of the liveness pass (R-419, R-421): which
// apps a lost host held, starting their standbys, and, once the host is
// back, removing the copies it still holds. The liveness pass decides a host
// is lost or back; this never does. Hosts are the cluster's by ID there and
// the runtime's by name here: the runtime's host list names each joined host
// as cluster_hosts does (hosts.Runtime.Sync).
type Failover struct {
	Standbys *Service
	Homes    Homes
	Hosts    Hosts
}

var (
	_ liveness.Failover = (*Failover)(nil)
	_ liveness.Homes    = (*Failover)(nil)
	_ liveness.Returns  = (*Failover)(nil)
)

// PromoteStandby starts the app's standby, by the liveness pass. A failed app
// is refused there too (R-151).
func (f *Failover) PromoteStandby(ctx context.Context, appID string) error {
	_, err := f.Standbys.Promote(ctx, appID, "")
	return err
}

// AppsHomedOn is every app whose home is the host, highest failover priority
// first, then by app: the order a lost host's apps are started in, so that
// past the reservation (allow_failover_oversubscription) the ones that
// matter most take the room, and one that no longer fits is skipped for the
// next (R-422, multidocker failoverOrder). An app has a standby when one is
// placed on another host.
func (f *Failover) AppsHomedOn(ctx context.Context, hostID string) ([]liveness.HomedApp, error) {
	h, err := f.Hosts.Host(ctx, hostID)
	if err != nil {
		return nil, err
	}
	rows, err := f.Homes.HomedOn(ctx, h.Name)
	if err != nil {
		return nil, err
	}
	type ranked struct {
		app      liveness.HomedApp
		priority int
	}
	list := make([]ranked, 0, len(rows))
	for _, sb := range rows {
		list = append(list, ranked{
			app:      liveness.HomedApp{AppID: sb.AppID, HasStandby: sb.Host != "" && sb.Host != h.Name},
			priority: f.priority(ctx, sb.AppID),
		})
	}
	slices.SortStableFunc(list, func(a, b ranked) int {
		return cmp.Or(cmp.Compare(b.priority, a.priority), cmp.Compare(a.app.AppID, b.app.AppID))
	})
	out := make([]liveness.HomedApp, len(list))
	for i, r := range list {
		out[i] = r.app
	}
	return out, nil
}

// priority is the app's availability.priority, 0 when its spec cannot be
// read: the app is still failed over, after those that say more.
func (f *Failover) priority(ctx context.Context, appID string) int {
	_, sp, err := f.Standbys.Plans.Shape(ctx, appID)
	if err != nil || sp == nil {
		return 0
	}
	return sp.Availability.Priority
}

// AppsLeftOn is every app failed over away from the host, whose stale copy
// the host still holds.
func (f *Failover) AppsLeftOn(ctx context.Context, hostID string) ([]string, error) {
	h, err := f.Hosts.Host(ctx, hostID)
	if err != nil {
		return nil, err
	}
	rows, err := f.Homes.LeftOn(ctx, h.Name)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(rows))
	for i, sb := range rows {
		out[i] = sb.AppID
	}
	return out, nil
}

// RemoveStale removes the app's stale copy from the host it was failed over
// from, now back: removed, never started (R-419).
func (f *Failover) RemoveStale(ctx context.Context, appID string) error {
	return f.Standbys.RemoveStale(ctx, appID, "")
}
