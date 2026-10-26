package multidocker

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
)

// Warm standby (R-419, R-420, notes-cluster.md "Warm standby").
//
// An app's standby is its plan applied, created and never started, on a
// second host, under its own bundle name (standbyBundle) so nothing that
// reads where an app runs — Bundles, and through it placement — takes the
// standby's host for the app's. Its volumes are copied to staging volumes on
// that host on an interval (standby_ship.go). Promoting it removes the
// standby's own workloads, fills the app's volumes on that host from a copy,
// and applies the app's plan there: from then on that host holds the app's
// bundle and is its home, and the host it left holds a stale copy that is
// removed when it comes back, never started (RemoveStaleCopy).
//
// Where an app is stays recorded on the hosts (placement.go). A promotion
// adds one fact the hosts alone could not settle once the lost host returns,
// both holding the bundle: which one is newer. The promoted host keeps a home
// mark (docker.MarkHome), and the newest mark wins (preferHomes).

// standbyHost is what the standby needs of one host's Docker adapter, beside
// hostRuntime. The real one is *docker.Adapter.
type standbyHost interface {
	ApplyStopped(ctx context.Context, p api.BundlePlan) error
	ListVolume(ctx context.Context, bundleID, volumeID string) (docker.VolumeListing, error)
	ReadVolumeFiles(ctx context.Context, bundleID, volumeID string, paths []string, dst io.Writer) error
	StandbyCopies(ctx context.Context, bundleID, volumeID string) (docker.StandbyCopies, error)
	StageStandbyCopy(ctx context.Context, bundleID, volumeID string, remove []string, files io.Reader) error
	CommitStandbyCopy(ctx context.Context, bundleID, volumeID string, seq int, listing docker.VolumeListing, drop []int) error
	RestoreStandbyCopy(ctx context.Context, bundleID, volumeID string, seq int) error
	RemoveStandbyStorage(ctx context.Context, bundleID string) error
	MarkHome(ctx context.Context, bundleID string, at time.Time) error
	ClearHome(ctx context.Context, bundleID string) error
	HomeMarks(ctx context.Context) (map[string]time.Time, error)
}

var (
	_ standbyHost        = (*docker.Adapter)(nil)
	_ api.StandbyRuntime = (*Adapter)(nil)
)

// standbyState is the adapter's memory of promotions this process made,
// which outrank what the hosts say until the lost host's copy is removed.
type standbyState struct {
	mu    sync.Mutex
	homes map[string]string // bundle -> host promoted to
}

func (s *standbyState) set(bundle, host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.homes == nil {
		s.homes = map[string]string{}
	}
	s.homes[bundle] = host
}

func (s *standbyState) get(bundle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.homes[bundle]
	return h, ok
}

func (s *standbyState) snapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]string, len(s.homes))
	for b, h := range s.homes {
		out[b] = h
	}
	return out
}

// standbyBundle is the bundle name a standby is applied under.
func standbyBundle(bundleID string) string { return bundleID + "-standby" }

// standbyOf is a host's standby side.
func standbyOf(h *host) (standbyHost, error) {
	s, ok := h.rt.(standbyHost)
	if !ok {
		return nil, errs.Newf(errs.AdapterFailed, "Docker on %s cannot keep a standby.", h.cfg.Name)
	}
	return s, nil
}

// standbyPossible is SupportsStandby: two hosts open to new apps, so an app
// on one has somewhere else to keep its standby.
func (a *Adapter) standbyPossible() bool {
	open := 0
	for _, h := range a.hosts {
		if !h.cfg.NoPlacement {
			open++
		}
	}
	return open >= 2
}

func (a *Adapter) hostNamed(name string) (*host, error) {
	h := a.byName(name)
	if h == nil {
		return nil, errs.Newf(errs.AdapterFailed,
			"This app's standby is on %s, which is no longer in the Docker hosts runtime's host list.", name).
			WithRemedy("Wait for the next standby pass, which places the standby on a host that is listed.")
	}
	return h, nil
}

// ApplyStandby creates or refreshes the bundle's stopped standby on a host
// other than its own (R-419).
func (a *Adapter) ApplyStandby(ctx context.Context, req api.StandbyRequest) (api.StandbyHandle, error) {
	home, err := a.mustLocate(ctx, req.Bundle.BundleID)
	if err != nil {
		return api.StandbyHandle{}, err
	}
	target, err := a.chooseStandbyHost(ctx, home, req.Host, need(req.Bundle))
	if err != nil {
		return api.StandbyHandle{}, err
	}
	if req.Host != "" && req.Host != target.cfg.Name {
		// The standby moves: the old one is removed where it can be. A host
		// that does not answer keeps it until RemoveStandby reaches it.
		if old := a.byName(req.Host); old != nil {
			_ = a.removeStandbyOn(ctx, old, req.Bundle.BundleID)
		}
	}
	side, err := standbyOf(target)
	if err != nil {
		return api.StandbyHandle{}, err
	}
	if err := a.ensureAgent(ctx, target); err != nil {
		return api.StandbyHandle{}, err
	}
	p := standbyPlan(req.Bundle)
	if err := side.ApplyStopped(ctx, p); err != nil {
		return api.StandbyHandle{}, err
	}
	return api.StandbyHandle{Host: target.cfg.Name}, nil
}

// standbyPlan is the app's plan under the standby's bundle name, with its own
// copy of the labels so the app's are not changed.
func standbyPlan(p api.BundlePlan) api.BundlePlan {
	labels := make(map[string]string, len(p.Labels)+1)
	for k, v := range p.Labels {
		labels[k] = v
	}
	labels["pando.standby"] = p.BundleID
	p.Labels = labels
	p.BundleID = standbyBundle(p.BundleID)
	p.FirstDeploy = false
	return p
}

// removeStandbyOn removes the bundle's standby and its copies from one host.
func (a *Adapter) removeStandbyOn(ctx context.Context, h *host, bundleID string) error {
	side, err := standbyOf(h)
	if err != nil {
		return err
	}
	if err := h.rt.Destroy(ctx, api.BundleRef{BundleID: standbyBundle(bundleID)}, api.DestroyOptions{}); err != nil {
		return err
	}
	return side.RemoveStandbyStorage(ctx, bundleID)
}

// RemoveStandby removes the bundle's standby from every host. A host that does
// not answer is reported, so the caller tries again.
func (a *Adapter) RemoveStandby(ctx context.Context, ref api.BundleRef) error {
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		return a.removeStandbyOn(ctx, h, ref.BundleID)
	})
	return firstError(a.hosts, results)
}

// RemoveStaleCopy removes the bundle from host, which it was failed over from
// (R-419). The app's home is refused: that copy is the app.
func (a *Adapter) RemoveStaleCopy(ctx context.Context, ref api.BundleRef, hostName string) error {
	h, err := a.hostNamed(hostName)
	if err != nil {
		return err
	}
	if home, ok := a.standby.get(ref.BundleID); (ok && home == hostName) || (!ok && a.cached(ref.BundleID) == h) {
		return errs.Newf(errs.ValidInvalid,
			"%s is where this app runs now, so its copy there is not stale and is not removed.", hostName)
	}
	side, err := standbyOf(h)
	if err != nil {
		return err
	}
	if err := h.rt.Destroy(ctx, ref, api.DestroyOptions{KeepVolumes: false}); err != nil {
		return err
	}
	if !h.cfg.Control {
		if err := h.rt.DetachProxy(ctx, ref.BundleID); err != nil {
			return err
		}
	}
	if err := side.ClearHome(ctx, ref.BundleID); err != nil {
		return err
	}
	a.refresh(ctx)
	return nil
}

// PromoteStandby starts the standby as the app (R-419), from the newest copy
// of its volumes, falling back to older ones while it does not come up
// healthy (R-420).
func (a *Adapter) PromoteStandby(ctx context.Context, req api.PromoteRequest) (api.PromoteResult, error) {
	bundle := req.Bundle.BundleID
	target, err := a.hostNamed(req.Host)
	if err != nil {
		return api.PromoteResult{}, err
	}
	side, err := standbyOf(target)
	if err != nil {
		return api.PromoteResult{}, err
	}
	copies, attempts, err := a.promotionCopies(ctx, side, req.Bundle)
	if err != nil {
		return api.PromoteResult{}, err
	}

	result := api.PromoteResult{Host: target.cfg.Name}
	if former := a.cached(bundle); former != nil && former != target {
		result.FormerHost = former.cfg.Name
	}
	// From here the app is on target, whatever the lost host says when it
	// answers again: nothing below or after may start the copy there.
	a.standby.set(bundle, target.cfg.Name)
	a.placement.mu.Lock()
	a.placement.byBundle[bundle] = target.cfg.Name
	a.placement.mu.Unlock()

	if err := target.rt.Destroy(ctx, api.BundleRef{BundleID: standbyBundle(bundle)}, api.DestroyOptions{}); err != nil {
		return result, err
	}
	if err := a.ensureAgent(ctx, target); err != nil {
		return result, err
	}
	for i := 0; i < attempts; i++ {
		result.Copy, result.FellBack = i, i > 0
		healthy, err := a.startFromCopy(ctx, target, side, req, copies, i)
		if err != nil {
			return result, err
		}
		if healthy {
			result.Healthy = true
			break
		}
	}
	if err := side.MarkHome(ctx, bundle, time.Now()); err != nil {
		return result, err
	}
	return result, nil
}

// promotionCopies is each volume's copies, oldest first, and how many starts
// they allow: one per copy the volume with the most has. A volume with no
// copy at all is refused, rather than started empty beside data it never got.
func (a *Adapter) promotionCopies(ctx context.Context, side standbyHost, p api.BundlePlan) (map[string][]int, int, error) {
	copies := map[string][]int{}
	attempts := 1
	for _, v := range p.Volumes {
		have, err := side.StandbyCopies(ctx, p.BundleID, v.VolumeID)
		if err != nil {
			return nil, 0, err
		}
		if len(have.Seqs) == 0 {
			return nil, 0, errs.Newf(errs.AdapterFailed,
				"This app's standby has no copy of its storage %q yet, so starting it would start the app without its data.", v.Name).
				WithRemedy("Bring the app's host back, or restore the app from a backup.")
		}
		copies[v.VolumeID] = have.Seqs
		attempts = max(attempts, min(len(have.Seqs), api.DefaultStandbyCopies))
	}
	return copies, attempts, nil
}

// startFromCopy fills the app's volumes from copy i (0 the newest; a volume
// with fewer copies uses its oldest), applies the app's plan and waits for
// it to come up healthy.
func (a *Adapter) startFromCopy(ctx context.Context, target *host, side standbyHost, req api.PromoteRequest, copies map[string][]int, i int) (bool, error) {
	bundle := req.Bundle.BundleID
	if i > 0 {
		if err := target.rt.Stop(ctx, api.BundleRef{BundleID: bundle}); err != nil {
			return false, err
		}
	}
	for _, v := range req.Bundle.Volumes {
		seqs := copies[v.VolumeID]
		seq := seqs[max(len(seqs)-1-i, 0)]
		if err := side.RestoreStandbyCopy(ctx, bundle, v.VolumeID, seq); err != nil {
			return false, err
		}
	}
	if _, err := target.rt.Apply(ctx, req.Bundle); err != nil {
		return false, err
	}
	return a.waitHealthy(ctx, target, req)
}

// waitHealthy watches the promoted bundle until it has been healthy on two
// looks in a row, or the timeout passes.
func (a *Adapter) waitHealthy(ctx context.Context, target *host, req api.PromoteRequest) (bool, error) {
	timeout, poll := req.HealthTimeout, req.PollEvery
	if timeout <= 0 {
		timeout = api.DefaultPromoteHealthTimeout
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	streak := 0
	for {
		observed, err := target.rt.Observe(ctx, api.BundleRef{BundleID: req.Bundle.BundleID})
		if err == nil && healthyBundle(req.Bundle, observed) {
			if streak++; streak >= 2 {
				return true, nil
			}
		} else {
			streak = 0
		}
		if !time.Now().Before(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// healthyBundle: every planned workload running, not restarting, and healthy
// where it has a health check; one without a check only has to be running
// (R-221).
func healthyBundle(p api.BundlePlan, observed api.ObservedBundle) bool {
	seen := map[string]api.ObservedWorkload{}
	for _, w := range observed.Workloads {
		seen[w.Name] = w
	}
	for _, w := range p.Workloads {
		o, ok := seen[w.Name]
		if !ok || !o.Running || o.Restarting {
			return false
		}
		if o.Healthy != nil && !*o.Healthy {
			return false
		}
		if w.Health != nil && o.Healthy == nil {
			return false
		}
	}
	return true
}

// preferHomes settles where a bundle is when placement cannot (refresh): a
// promotion this process made wins outright, and a bundle found on several
// hosts goes to the one with the newest home mark. Called with the refresh
// lock held.
func (a *Adapter) preferHomes(ctx context.Context, next map[string]string, found map[string]map[string]bool) {
	for b, h := range a.standby.snapshot() {
		next[b] = h
	}
	holders := map[string][]string{}
	for _, h := range a.hosts {
		for b := range found[h.cfg.Name] {
			holders[b] = append(holders[b], h.cfg.Name)
		}
	}
	marks := map[string]map[string]time.Time{}
	for b, on := range holders {
		if len(on) < 2 {
			continue
		}
		if _, promoted := a.standby.get(b); promoted {
			continue
		}
		if best := a.newestMark(ctx, b, on, marks); best != "" {
			next[b] = best
		}
	}
}

func (a *Adapter) newestMark(ctx context.Context, bundle string, on []string, marks map[string]map[string]time.Time) string {
	best, bestAt := "", time.Time{}
	for _, name := range on {
		m, ok := marks[name]
		if !ok {
			m = a.homeMarks(ctx, name)
			marks[name] = m
		}
		if at, ok := m[bundle]; ok && at.After(bestAt) {
			best, bestAt = name, at
		}
	}
	return best
}

func (a *Adapter) homeMarks(ctx context.Context, name string) map[string]time.Time {
	h := a.byName(name)
	if h == nil {
		return nil
	}
	side, err := standbyOf(h)
	if err != nil {
		return nil
	}
	m, err := side.HomeMarks(ctx)
	if err != nil {
		return nil
	}
	return m
}
