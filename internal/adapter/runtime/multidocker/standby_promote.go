package multidocker

import (
	"context"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// PromoteStandby starts the standby as the app (R-419), from the newest copy
// of its volumes, falling back to older ones while it does not come up
// healthy (R-420).
//
// Until the app's plan is first applied on the standby's host nothing has
// started, and an error is a refusal: the result names no host, and what was
// restored there is removed again. From then on the app is on that host —
// its home mark is written first, so a restarted process and a lost host
// that returns both find it there — and an error keeps the result's host
// (R-153). A host that already holds the bundle's home mark had the app
// started on it by a promotion cut short: no copy is restored over the
// volume it runs on, and the promotion is reported done. Once the app is
// healthy its staging volumes on the host are removed: their disk is
// counted against the host (R-242, R-422), and the app's next standby is
// elsewhere.
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
	if !a.standby.begin(bundle) {
		return api.PromoteResult{}, errs.New(errs.StateInvalid, "This app's standby is already being started.")
	}
	defer a.standby.end(bundle)

	result := api.PromoteResult{}
	if former := a.cached(bundle); former != nil && former != target {
		result.FormerHost = former.cfg.Name
	}
	if a.markedHome(ctx, side, bundle) {
		a.homeOn(bundle, target)
		result.Host = target.cfg.Name
		observed, err := target.rt.Observe(ctx, api.BundleRef{BundleID: bundle})
		result.Healthy = err == nil && healthyBundle(req.Bundle, observed)
		return result, nil
	}
	if err := a.roomToStart(ctx, target, req.Bundle); err != nil {
		return api.PromoteResult{}, err
	}
	copies, attempts, err := a.promotionCopies(ctx, side, req.Bundle)
	if err != nil {
		return api.PromoteResult{}, err
	}
	if err := a.prepare(ctx, target, side, req.Bundle, copies); err != nil {
		return api.PromoteResult{}, err
	}

	// From here the app is on target, whatever the lost host says when it
	// answers again: nothing below or after may start the copy there.
	a.homeOn(bundle, target)
	result.Host = target.cfg.Name
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
	if result.Healthy {
		if err := side.RemoveStandbyStorage(ctx, bundle); err != nil {
			return result, err
		}
	}
	return result, nil
}

// markedHome reports whether the host holds the bundle's home mark: a
// promotion to it reached the start. A mark that cannot be read is taken as
// none, so the promotion goes on as from nothing.
func (a *Adapter) markedHome(ctx context.Context, side standbyHost, bundle string) bool {
	marks, err := side.HomeMarks(ctx)
	if err != nil {
		return false
	}
	_, ok := marks[bundle]
	return ok
}

// homeOn records, in this process, that the bundle runs on target.
func (a *Adapter) homeOn(bundle string, target *host) {
	a.standby.set(bundle, target.cfg.Name)
	a.placement.mu.Lock()
	a.placement.byBundle[bundle] = target.cfg.Name
	a.placement.mu.Unlock()
}

// prepare readies target to start the app: the standby's own workloads
// removed, the agent running, the newest copy restored into the app's
// volumes and the home mark written. Nothing is started. A failure removes
// what was restored, so the host holds nothing of the app's but the standby's
// copies, and the promotion is a refusal that can be tried again.
func (a *Adapter) prepare(ctx context.Context, target *host, side standbyHost, p api.BundlePlan, copies map[string][]int) error {
	if err := target.rt.Destroy(ctx, api.BundleRef{BundleID: standbyBundle(p.BundleID)}, api.DestroyOptions{}); err != nil {
		return err
	}
	if err := a.ensureAgent(ctx, target); err != nil {
		return err
	}
	err := a.restore(ctx, side, p, copies, 0)
	if err == nil {
		err = side.MarkHome(ctx, p.BundleID, time.Now())
	}
	if err != nil {
		_ = target.rt.Destroy(ctx, api.BundleRef{BundleID: p.BundleID}, api.DestroyOptions{KeepVolumes: false})
	}
	return err
}

// restore fills the app's volumes on the standby's host from copy i (0 the
// newest; a volume with fewer copies uses its oldest).
func (a *Adapter) restore(ctx context.Context, side standbyHost, p api.BundlePlan, copies map[string][]int, i int) error {
	for _, v := range p.Volumes {
		seqs := copies[v.VolumeID]
		if err := side.RestoreStandbyCopy(ctx, p.BundleID, v.VolumeID, seqs[max(len(seqs)-1-i, 0)]); err != nil {
			return err
		}
	}
	return nil
}

// startFromCopy applies the app's plan and waits for it to come up healthy:
// from the copy prepare restored, for i 0, and otherwise stopped and filled
// from copy i first.
func (a *Adapter) startFromCopy(ctx context.Context, target *host, side standbyHost, req api.PromoteRequest, copies map[string][]int, i int) (bool, error) {
	if i > 0 {
		if err := target.rt.Stop(ctx, api.BundleRef{BundleID: req.Bundle.BundleID}); err != nil {
			return false, err
		}
		if err := a.restore(ctx, side, req.Bundle, copies, i); err != nil {
			return false, err
		}
	}
	if _, err := target.rt.Apply(ctx, req.Bundle); err != nil {
		return false, err
	}
	return a.waitHealthy(ctx, target, req)
}
