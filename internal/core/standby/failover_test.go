package standby

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/liveness"
	"github.com/trypando/pando/internal/core/notice"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// homesOf reads the fake store's rows as state.Standbys does.
type homesOf struct {
	*fakeStore
	pinned map[string]*spec.AppSpec
}

// HomedOn reads each app's priority from its pinned spec, as the query
// does, never through Plans.
func (h homesOf) HomedOn(_ context.Context, host string) ([]state.Standby, error) {
	out, err := h.where(func(sb state.Standby) bool {
		return (sb.Home == host && sb.PromotingAt == nil) || (sb.PromotingAt != nil && sb.StaleHost == host)
	})
	for i := range out {
		if s := h.pinned[out[i].AppID]; s != nil {
			out[i].Priority = s.Availability.Priority
		}
	}
	return out, err
}

func (h homesOf) LeftOn(_ context.Context, host string) ([]state.Standby, error) {
	return h.where(func(sb state.Standby) bool { return sb.StaleHost == host })
}

func (h homesOf) where(keep func(state.Standby) bool) ([]state.Standby, error) {
	if h.listErr != nil {
		return nil, h.listErr
	}
	var out []state.Standby
	for _, sb := range h.rows {
		if keep(sb) {
			out = append(out, sb)
		}
	}
	return out, nil
}

// cluster is the cluster's hosts as state.Cluster keeps them: the runtime
// names each host as cluster_hosts does.
type cluster struct {
	hosts map[string]state.Host
	lost  map[string]bool
}

func newCluster(names ...string) *cluster {
	c := &cluster{hosts: map[string]state.Host{}, lost: map[string]bool{}}
	for _, n := range names {
		c.hosts["host_"+n] = state.Host{ID: "host_" + n, Name: n, Address: n + ":7443"}
	}
	return c
}

func (c *cluster) Host(_ context.Context, id string) (state.Host, error) {
	h, ok := c.hosts[id]
	if !ok {
		return h, errs.Newf(errs.NotFound, "There is no host %s in the cluster.", id)
	}
	return h, nil
}

func (c *cluster) MarkHostLost(_ context.Context, id, _ string) (bool, error) {
	was := c.lost[id]
	c.lost[id] = true
	return !was, nil
}

func (c *cluster) ClearHostLost(_ context.Context, id string) error {
	delete(c.lost, id)
	return nil
}

func (c *cluster) LostHosts(context.Context) (map[string]bool, error) {
	out := map[string]bool{}
	for id := range c.lost {
		out[id] = true
	}
	return out, nil
}

func (c *cluster) RecordHostFence(context.Context, string, string) (bool, error) { return false, nil }

// appsOf answers liveness.Apps from the fake store's states.
type appsOf struct{ *fakeStore }

func (a appsOf) ByID(_ context.Context, id string) (state.App, bool, error) {
	st, ok := a.states[id]
	return state.App{ID: id, State: st}, ok, nil
}

// failoverRig is the liveness pass over the standby service, as main wires
// them: liveness.Service with standby.Failover for Homes, Failover and
// Returns.
type failoverRig struct {
	*harness
	cluster *cluster
	loss    *liveness.Service
}

func newFailoverRig(t *testing.T) *failoverRig {
	t.Helper()
	h := newHarness(appSpec("", 1))
	c := newCluster("app-1", "app-2", "app-3")
	f := &Failover{Standbys: h.svc, Homes: homesOf{h.store, h.plans.specs}, Hosts: c}
	return &failoverRig{harness: h, cluster: c, loss: &liveness.Service{
		Store: c, Apps: appsOf{h.store}, Audit: h.auditor, Homes: f, Failover: f, Returns: f,
	}}
}

// addApp is a live app on app-1 with the given priority and state, its
// standby placed on app-2 by a standby pass.
func (r *failoverRig) addApp(id string, priority int, st string) {
	s := appSpec("", 1)
	s.Availability.Priority = priority
	r.plans.specs[id] = s
	r.store.apps = append(r.store.apps, state.StandbyApp{AppID: id, State: st, DesiredState: "running", PinnedSpecID: "spec_1"})
	r.store.states[id] = st
}

// watch is the leader's liveness round over the rig's three hosts, each
// answering while answering says so.
func (r *failoverRig) watch(answering map[string]bool) *liveness.Watch {
	return &liveness.Watch{
		Hosts: func(context.Context) ([]liveness.Peer, error) {
			var out []liveness.Peer
			for _, n := range []string{"app-1", "app-2", "app-3"} {
				h := r.cluster.hosts["host_"+n]
				out = append(out, liveness.Peer{ID: h.ID, Name: h.Name, Address: h.Address})
			}
			return out, nil
		},
		Probe: func(_ context.Context, p liveness.Peer) ([]byte, error) {
			if !answering[p.ID] {
				return nil, errors.New("no route to host")
			}
			return json.Marshal(liveness.View{Host: p.ID})
		},
		Service: r.loss, Clock: clock.NewFake(r.clock.Now()),
	}
}

// TestR422_AnAppWithNoRoomGetsTheNoticeAndStillDeploys asserts that an app
// whose standby fits on no host runs without one, with a notice naming the
// largest room any host has, and that nothing about it is refused: the
// standby pass records the runtime's no-room answer, the app stays as it
// deployed, and the notice goes once a host has room.
func TestR422_AnAppWithNoRoomGetsTheNoticeAndStillDeploys(t *testing.T) {
	ctx := context.Background()
	h := newHarness(appSpec("", 1))
	h.policy.AllowFailoverOversubscription = true
	h.svc.Policy = staticPolicy{h.policy}
	noRoom := errs.New(errs.CapacityWouldOversubscribe,
		"This app runs without a standby: it needs 2.00 cores of CPU, 4.0 GB of memory and 0 MB of disk on a host other than app-1, kept free for when app-1 is lost, and the most any host has is 1.00 cores of CPU, 2.0 GB of memory and 0 MB of disk, on app-2.").
		WithRemedy("Lower what the app asks for, stop or delete an app on another host, or add a host.")
	h.rt.applyErr = noRoom

	h.svc.Once(ctx)
	require.Len(t, h.rt.applied, 1)
	require.True(t, h.rt.applied[0].Bundle.Failover.AllowOversubscription, "host policy reaches the runtime")
	require.Equal(t, state.StateRunning, h.store.states["app_a"], "the app is left as it deployed")
	require.Empty(t, h.rt.shipped, "nothing to ship to")

	standing, err := h.svc.Standby(ctx, "app_a")
	require.NoError(t, err)
	require.Equal(t, noRoom.Message, standing.Unplaced)
	require.Equal(t, noRoom.Remedy, standing.Remedy)
	notices := notice.Compute(notice.Inputs{Room: &standing})
	require.Len(t, notices, 1)
	require.Equal(t, notice.KeyNoStandby, notices[0].Key)
	require.Contains(t, notices[0].Message, "the most any host has is 1.00 cores")

	// Another failure is not the notice; neither is an app with no row.
	h.rt.applyErr = errs.New(errs.AdapterUnavailable, "Docker on app-2 did not answer.")
	h.svc.Once(ctx)
	standing, err = h.svc.Standby(ctx, "app_a")
	require.NoError(t, err)
	require.Empty(t, standing.Unplaced)
	standing, err = h.svc.Standby(ctx, "app_none")
	require.NoError(t, err)
	require.Empty(t, standing)

	// A host makes room: the standby is placed and the notice is gone.
	h.rt.applyErr = nil
	h.svc.Once(ctx)
	sb := h.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Host)
	require.Equal(t, "app-1", sb.Home)
	require.Empty(t, sb.LastErrorCode)
	standing, err = h.svc.Standby(ctx, "app_a")
	require.NoError(t, err)
	require.Empty(t, standing)

	// A refresh with no room keeps the standby it has: not told otherwise.
	h.rt.applyErr = noRoom
	h.store.apps[0].PinnedSpecID = "spec_2"
	h.svc.Once(ctx)
	standing, err = h.svc.Standby(ctx, "app_a")
	require.NoError(t, err)
	require.Empty(t, standing)
	require.Equal(t, string(errs.CapacityWouldOversubscribe), h.store.rows["app_a"].LastErrorCode)

	h.store.getErr = errors.New("database away")
	_, err = h.svc.Standby(ctx, "app_a")
	require.Error(t, err)
}

// TestR421_ALostHostsAppsArePromotedThroughStandby asserts that taking a
// host for lost starts, through the standby service, the standby of each app
// homed on it, highest priority first, then by app; that a failed app is
// skipped (R-151) and so is one without a standby; and that one whose
// standby no longer fits is skipped for the next (R-422).
func TestR421_ALostHostsAppsArePromotedThroughStandby(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	r.store.apps = nil
	r.addApp("app_a", 0, state.StateRunning)
	r.addApp("app_b", 5, state.StateRunning)
	r.addApp("app_c", 5, state.StateRunning)
	r.addApp("app_d", 9, state.StateRunning)
	r.addApp("app_failed", 9, state.StateRunning)
	r.svc.Once(ctx)
	for _, id := range []string{"app_a", "app_b", "app_c", "app_d", "app_failed"} {
		require.Equal(t, "app-1", r.store.rows[id].Home, id)
		require.Equal(t, "app-2", r.store.rows[id].Host, id)
	}
	r.store.states["app_failed"] = state.StateFailed
	r.store.rows["app_lone"] = state.Standby{AppID: "app_lone", Home: "app-1", RuntimeRef: "rt_hosts"}
	r.store.states["app_lone"] = state.StateRunning
	r.store.rows["app_elsewhere"] = state.Standby{AppID: "app_elsewhere", Home: "app-3", Host: "app-2", RuntimeRef: "rt_hosts"}
	r.rt.promoteErr = map[string]error{"app_d": errs.New(errs.CapacityWouldOversubscribe, "This app's standby was not started on app-2.")}
	r.rt.promoteOK = api.PromoteResult{Host: "app-2", Healthy: true}
	// Past the reservation, the standbys on app-2 start one after another.
	r.svc.Policy = staticPolicy{policy.Document{AllowFailoverOversubscription: true}}

	out, err := r.loss.MarkLost(ctx, "host_app-1", liveness.Actor{ID: "usr_admin"})
	require.NoError(t, err)

	var tried []string
	for _, p := range r.rt.promoted {
		tried = append(tried, p.Bundle.BundleID)
	}
	require.Equal(t, []string{"app_d", "app_b", "app_c", "app_a"}, tried, "priority, then app; app_d did not fit and the rest still start")
	require.Equal(t, []string{"app_b", "app_c", "app_a"}, out.Started)
	skipped := map[string]string{}
	for _, s := range out.Skipped {
		skipped[s.AppID] = s.Reason
	}
	require.Equal(t, map[string]string{
		"app_d": liveness.SkipPromoteErr, "app_failed": liveness.SkipFailed, "app_lone": liveness.SkipNoStandby,
	}, skipped)

	sb := r.store.rows["app_b"]
	require.Equal(t, "app-2", sb.Home, "the standby's host is the app's home")
	require.Equal(t, "app-1", sb.StaleHost, "the host it left, though the runtime did not say")
	require.Empty(t, sb.Host)
	require.Equal(t, "app-2", r.store.rows["app_failed"].Host, "a failed app's standby is kept, not started")
	require.Contains(t, r.auditor.actions, "cluster.failover")
	require.Contains(t, r.auditor.actions, "app.standby.promote")

	// The lost host's apps are listed through the cluster's own host IDs.
	_, err = (&Failover{Standbys: r.svc, Homes: homesOf{r.store, r.plans.specs}, Hosts: r.cluster}).AppsHomedOn(ctx, "host_none")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}

// TestR419_AReturningHostsCopyIsRemovedNeverRestartedAndItsLostMarkCleared
// asserts the leader's half of a lost host coming back: each app failed over
// away from it has its copy there removed through the runtime, nothing is
// started on it, and only then is it no longer marked lost.
func TestR419_AReturningHostsCopyIsRemovedNeverRestartedAndItsLostMarkCleared(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	r.svc.Once(ctx)
	r.rt.promoteOK = api.PromoteResult{Host: "app-2", FormerHost: "app-1", Healthy: true}
	_, err := r.loss.MarkLost(ctx, "host_app-1", liveness.Actor{ID: "usr_admin"})
	require.NoError(t, err)
	require.Len(t, r.rt.promoted, 1)
	require.True(t, r.cluster.lost["host_app-1"])

	answering := map[string]bool{"host_app-2": true, "host_app-3": true}
	w := r.watch(answering)

	// Still away: nothing is removed.
	w.Round(ctx)
	require.Empty(t, r.rt.stale)
	require.True(t, r.cluster.lost["host_app-1"])

	// A removal that fails keeps the mark, for the next round.
	answering["host_app-1"] = true
	r.rt.staleErr = errs.New(errs.AdapterUnavailable, "Docker on app-1 did not answer.")
	w.Round(ctx)
	require.True(t, r.cluster.lost["host_app-1"])

	r.rt.staleErr = nil
	w.Round(ctx)
	require.Contains(t, r.rt.stale, "app_a@app-1")
	require.False(t, r.cluster.lost["host_app-1"], "its lost mark is cleared")
	require.Len(t, r.rt.promoted, 1, "nothing more is started")
	for _, p := range r.rt.applied {
		require.NotEqual(t, "app-1", p.Host, "the copy there is never restarted")
	}
	sb := r.store.rows["app_a"]
	require.Empty(t, sb.StaleHost)
	require.Equal(t, "app-1", sb.Host, "the host that came back takes the next standby")
	require.Contains(t, r.auditor.actions, "cluster.host.returned")

	// Back and nothing left there: no more work.
	r.cluster.lost["host_app-1"] = true
	w.Round(ctx)
	require.Len(t, r.rt.stale, 1)
	require.False(t, r.cluster.lost["host_app-1"])
}

// TestFailoverReadsFailClosed covers the reads behind AppsHomedOn and
// AppsLeftOn failing, and an app whose spec cannot be read, which is still
// failed over, last.
func TestFailoverReadsFailClosed(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	f := &Failover{Standbys: r.svc, Homes: homesOf{r.store, r.plans.specs}, Hosts: r.cluster}
	r.store.rows["app_unread"] = state.Standby{AppID: "app_unread", Home: "app-1", Host: "app-2"}
	r.store.rows["app_a"] = state.Standby{AppID: "app_a", Home: "app-1", Host: "app-2"}
	r.plans.specs["app_a"].Availability.Priority = 1
	homed, err := f.AppsHomedOn(ctx, "host_app-1")
	require.NoError(t, err)
	require.Equal(t, []liveness.HomedApp{{AppID: "app_a", HasStandby: true, Queue: "app_a"}, {AppID: "app_unread", HasStandby: true, Queue: "app_unread"}}, homed)

	_, err = f.AppsLeftOn(ctx, "host_none")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
	r.store.listErr = errors.New("database away")
	_, err = f.AppsHomedOn(ctx, "host_app-1")
	require.Error(t, err)
	_, err = f.AppsLeftOn(ctx, "host_app-1")
	require.Error(t, err)
}

// TestR421_TheInstallsOwnMachinesAppsAreFailedOverWhenItIsLost asserts R-421
// for the machine the install started on: its row is named as the runtime
// names it (hosts.Runtime, the conversion), so when it is lost the apps the
// runtime reported homed there are found by its row and started on their
// standbys' hosts.
func TestR421_TheInstallsOwnMachinesAppsAreFailedOverWhenItIsLost(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	r.rt.homeName = "3f9c2a1b7d4e" // what the runtime calls the install's own machine
	r.cluster.hosts["host_install"] = state.Host{ID: "host_install", Name: "3f9c2a1b7d4e", Address: "10.0.0.5:7443"}
	r.svc.Once(ctx)
	require.Equal(t, "3f9c2a1b7d4e", r.store.rows["app_a"].Home)
	r.rt.promoteOK = api.PromoteResult{Host: "app-2", Healthy: true}

	loss, err := r.loss.MarkLost(ctx, "host_install", liveness.Actor{ID: "usr_1"})
	require.NoError(t, err)
	require.Equal(t, []string{"app_a"}, loss.Started)
	sb := r.store.rows["app_a"]
	require.Equal(t, "app-2", sb.Home)
	require.Equal(t, "3f9c2a1b7d4e", sb.StaleHost)
}

// TestR421_FailoverOrdersAppsWithoutBuildingTheirPlans asserts that the
// apps a lost host held are ordered by the priority their pinned specs say,
// highest first, then by app, without building any app's plan: a big host's
// failover does not wait on one plan per app before its first standby
// starts (R-421, R-422).
func TestR421_FailoverOrdersAppsWithoutBuildingTheirPlans(t *testing.T) {
	ctx := context.Background()
	r := newFailoverRig(t)
	r.store.apps = nil
	r.addApp("app_a", 0, state.StateRunning)
	r.addApp("app_b", 5, state.StateRunning)
	r.addApp("app_c", 5, state.StateRunning)
	r.addApp("app_d", 9, state.StateRunning)
	r.svc.Once(ctx)
	shaped := r.plans.shaped

	f := &Failover{Standbys: r.svc, Homes: homesOf{r.store, r.plans.specs}, Hosts: r.cluster}
	homed, err := f.AppsHomedOn(ctx, "host_app-1")
	require.NoError(t, err)
	var order []string
	for _, a := range homed {
		order = append(order, a.AppID)
	}
	require.Equal(t, []string{"app_d", "app_b", "app_c", "app_a"}, order)
	require.Equal(t, shaped, r.plans.shaped, "no app's plan is built to order them")
	for _, a := range homed {
		require.Equal(t, a.AppID, a.Queue, "under the reservation every standby fits, so each starts on its own")
	}

	// Past it, the standbys on one host start in that order, each into what
	// the one before left (R-422); so too when host policy cannot be read.
	for _, p := range []PolicyLoader{staticPolicy{policy.Document{AllowFailoverOversubscription: true}}, brokenPolicy{}, nil} {
		r.svc.Policy = p
		homed, err = f.AppsHomedOn(ctx, "host_app-1")
		require.NoError(t, err)
		for _, a := range homed {
			require.Equal(t, "app-2", a.Queue)
		}
	}
}

type brokenPolicy struct{}

func (brokenPolicy) Load(context.Context) (policy.Document, error) {
	return policy.Document{}, errors.New("database away")
}
