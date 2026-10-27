package multidocker

import (
	"context"
	"io"
	"strconv"
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
	StandbyLoads(ctx context.Context) ([]docker.StandbyLoad, error)
}

var (
	_ standbyHost        = (*docker.Adapter)(nil)
	_ api.StandbyRuntime = (*Adapter)(nil)
)

// standbyState is the adapter's memory of promotions this process made,
// which outrank what the hosts say until the lost host's copy is removed,
// and of the promotions it is making now.
type standbyState struct {
	mu    sync.Mutex
	homes map[string]string // bundle -> host promoted to
	// promoting are the bundles being promoted: nothing else touches their
	// standby meanwhile (R-153, busy).
	promoting map[string]bool
	// rooms holds, per host, a promotion's room check through its first
	// start (holdRoom).
	rooms map[string]*sync.Mutex
}

// holdRoom takes the host's room for one promotion: from its check of what
// the host has free through its first start, so two promotions to one host
// side by side never both pass the check before either has started (R-422).
// The release it returns may be called more than once.
func (s *standbyState) holdRoom(host string) func() {
	s.mu.Lock()
	if s.rooms == nil {
		s.rooms = map[string]*sync.Mutex{}
	}
	mu, ok := s.rooms[host]
	if !ok {
		mu = &sync.Mutex{}
		s.rooms[host] = mu
	}
	s.mu.Unlock()
	mu.Lock()
	return sync.OnceFunc(mu.Unlock)
}

// begin marks the bundle as being promoted, and reports false when it
// already is.
func (s *standbyState) begin(bundle string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.promoting[bundle] {
		return false
	}
	if s.promoting == nil {
		s.promoting = map[string]bool{}
	}
	s.promoting[bundle] = true
	return true
}

func (s *standbyState) end(bundle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.promoting, bundle)
}

// busy refuses a change to the bundle's standby while it is being promoted:
// moving it removes the copies the promotion restores from, and a ship's
// commit drops the oldest copy, which the promotion may fall back to (R-153,
// R-420). Core serializes these too (standby.Service); this is the runtime's
// own guard.
func (s *standbyState) busy(bundle string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.promoting[bundle] {
		return nil
	}
	return errs.New(errs.StateInvalid,
		"This app's standby is being started as the app, so it is not changed now.").
		WithRemedy("Wait for the start to finish; the next standby pass makes the app a new standby.")
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
	if err := a.standby.busy(req.Bundle.BundleID); err != nil {
		return api.StandbyHandle{}, err
	}
	home, err := a.mustLocate(ctx, req.Bundle.BundleID)
	if err != nil {
		return api.StandbyHandle{}, err
	}
	target, err := a.placeStandby(ctx, home, req.Host, req.Bundle)
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
	p := standbyPlan(req.Bundle, home.cfg.Name)
	if err := side.ApplyStopped(ctx, p); err != nil {
		return api.StandbyHandle{}, err
	}
	return api.StandbyHandle{Host: target.cfg.Name, Home: home.cfg.Name}, nil
}

// standbyPlan is the app's plan under the standby's bundle name, with its own
// copy of the labels so the app's are not changed. The pando.standby* labels
// are what the failover reservation reads back from the standby's containers
// (listStandbys, R-422): the app, the host it runs on, its priority and the
// disk its copies are counted at.
func standbyPlan(p api.BundlePlan, primary string) api.BundlePlan {
	labels := make(map[string]string, len(p.Labels)+4)
	for k, v := range p.Labels {
		labels[k] = v
	}
	labels["pando.standby"] = p.BundleID
	labels["pando.standby.primary"] = primary
	labels["pando.standby.priority"] = strconv.Itoa(p.Failover.Priority)
	labels["pando.standby.disk"] = strconv.FormatInt(standbyDisk(diskLimit(p), 0), 10)
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
	if err := a.standby.busy(ref.BundleID); err != nil {
		return err
	}
	results := a.eachHost(ctx, func(ctx context.Context, h *host) error {
		return a.removeStandbyOn(ctx, h, ref.BundleID)
	})
	return firstError(a.hosts, results)
}

// RemoveStaleCopy removes the bundle from host, which it was failed over from
// (R-419). The app's home is refused: that copy is the app. A deleted app has
// no home, and its copy keeps its volumes when the delete kept its storage
// (R-204).
func (a *Adapter) RemoveStaleCopy(ctx context.Context, ref api.BundleRef, hostName string, opts api.StaleCopyOptions) error {
	h, err := a.hostNamed(hostName)
	if err != nil {
		return err
	}
	if !opts.AppDeleted && a.isHome(ref.BundleID, h) {
		return errs.Newf(errs.ValidInvalid,
			"%s is where this app runs now, so its copy there is not stale and is not removed.", hostName)
	}
	side, err := standbyOf(h)
	if err != nil {
		return err
	}
	keep := opts.AppDeleted && opts.KeepVolumes
	if err := h.rt.Destroy(ctx, ref, api.DestroyOptions{KeepVolumes: keep}); err != nil {
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

// isHome reports whether h is where the bundle runs now: the host this
// process promoted it to, or else where placement has it.
func (a *Adapter) isHome(bundle string, h *host) bool {
	if home, ok := a.standby.get(bundle); ok {
		return home == h.cfg.Name
	}
	return a.cached(bundle) == h
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
