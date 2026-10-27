package standby

import (
	"cmp"
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// AppStates reads a live app's state. *state.Standbys is one.
type AppStates interface {
	AppState(ctx context.Context, appID string) (string, bool, error)
}

// Promote starts an app's standby as the app, on its standby's host, once
// the app's host is lost (R-419). Whether it is lost is the caller's to say:
// the liveness pass when a majority of hosts agree (R-421), or a person with
// install.cluster.manage who says the host is gone. by is that person's ID,
// empty for the liveness pass.
//
// A failed app is refused (R-151). From here the app's home is the standby's
// host, the host it left holds a stale copy (RemoveStale), and the next pass
// gives the app a new standby.
//
// The promotion is written ahead (R-153): before the runtime starts anything
// the row says the app's home is the standby's host and a promotion is under
// way, so a promotion cut short — an error, a lost lease, a restart — never
// leaves the app running on the standby's host while Pando records it on the
// lost one. Only a refusal before anything was started (the runtime names no
// host) puts the row back.
func (s *Service) Promote(ctx context.Context, appID, by string) (api.PromoteResult, error) {
	st, found, err := s.Apps.AppState(ctx, appID)
	if err != nil {
		return api.PromoteResult{}, err
	}
	if !found {
		return api.PromoteResult{}, errs.New(errs.NotFound, "There is no such app.")
	}
	if st == state.StateFailed {
		return api.PromoteResult{}, errs.New(errs.StateInvalid,
			"This app has failed, and a failed app is not started on another host: it would fail there too.").
			WithRemedy("Fix what made the app fail and deploy it again. Its standby is kept.")
	}
	unlock := s.lock(appID)
	defer unlock()
	return s.promoteLocked(ctx, appID, by)
}

func (s *Service) promoteLocked(ctx context.Context, appID, by string) (api.PromoteResult, error) {
	sb, sr, plan, err := s.standbyToStart(ctx, appID)
	if err != nil {
		return api.PromoteResult{}, err
	}
	before := sb
	if sb.PromotingAt == nil {
		if sb, err = s.writeAhead(ctx, before); err != nil {
			return api.PromoteResult{}, err
		}
	}
	got, err := sr.PromoteStandby(ctx, api.PromoteRequest{Bundle: plan, Host: sb.Host, HealthTimeout: s.HealthTimeout})
	if err != nil && got.Host == "" {
		// Refused before anything was started: the app is where it was.
		s.putBack(ctx, sb, before)
		_ = s.audit(ctx, "app.standby.promote.failed", appID, by, map[string]any{"host": sb.Host, "error": message(err)})
		return got, err
	}
	// From here the app is on the standby's host, even when the runtime
	// reported an error after starting it there: an app that is not healthy
	// is the reconciler's to report on that host, never restarted on the
	// lost one (R-148, R-153).
	done := sb
	done.PromotingAt = nil
	done.Home = cmp.Or(got.Host, sb.Host)
	// Where the app ran before holds a stale copy. The runtime says when it
	// knows; the home recorded at the last ApplyStandby says otherwise, so a
	// leader that restarted since still removes it when the host returns.
	done.StaleHost = cmp.Or(got.FormerHost, sb.StaleHost)
	done.Host, done.SpecID, done.Copies, done.ShippedAt = "", "", 0, nil
	cleared(&done)
	if err != nil {
		failed(&done, err)
	}
	s.save(ctx, done, &sb)
	detail := map[string]any{
		"host": got.Host, "former_host": got.FormerHost, "copy": got.Copy, "fell_back": got.FellBack, "healthy": got.Healthy,
	}
	if err != nil {
		detail["error"] = message(err)
	}
	_ = s.audit(ctx, "app.standby.promote", appID, by, detail)
	return got, err
}

// writeAhead records the promotion before the runtime starts anything: the
// app's home is the standby's host, the host it was on holds the stale copy,
// and a promotion is under way.
func (s *Service) writeAhead(ctx context.Context, sb state.Standby) (state.Standby, error) {
	now := s.now()
	ahead := sb
	ahead.StaleHost = cmp.Or(sb.Home, sb.StaleHost)
	ahead.Home, ahead.PromotedAt, ahead.PromotingAt = sb.Host, &now, &now
	ok, err := s.Store.SaveIf(ctx, ahead, &sb)
	if err != nil {
		return sb, err
	}
	if !ok {
		return sb, errs.New(errs.StateInvalid,
			"This app's standby changed while Pando was starting it, so it was not started.").
			WithRemedy("Mark the host lost again to start the standby as it now is.")
	}
	return ahead, nil
}

// putBack undoes a promotion written ahead that the runtime refused before
// starting anything. A promotion written ahead by an earlier try, whose row
// as it was before is not known, goes back to the host it left.
func (s *Service) putBack(ctx context.Context, ahead, before state.Standby) {
	back := before
	if before.PromotingAt != nil {
		back.Home, back.StaleHost = ahead.StaleHost, ""
		back.PromotedAt = nil
	}
	back.PromotingAt = nil
	s.save(ctx, back, &ahead)
}

// promoteGrace is how long a promotion may be under way before the pass
// takes it for cut short and finishes it: every copy given its health
// timeout, and one more.
func (s *Service) promoteGrace() time.Duration {
	timeout := s.HealthTimeout
	if timeout <= 0 {
		timeout = api.DefaultPromoteHealthTimeout
	}
	return time.Duration(api.DefaultStandbyCopies+1) * timeout
}

// resume finishes a promotion that was cut short — by a restart, or a leader
// that lost its lease — once it has been under way longer than a promotion
// takes. A promotion this process is making holds the app's lock and is left
// to finish. A failed app is not started (R-151).
func (s *Service) resume(ctx context.Context, app state.StandbyApp) {
	sb := app.Standby
	if sb.PromotingAt == nil || app.Deleted || app.State == state.StateFailed || s.now().Sub(*sb.PromotingAt) < s.promoteGrace() {
		return
	}
	unlock, ok := s.tryLock(app.AppID)
	if !ok {
		return
	}
	defer unlock()
	// The liveness pass audits each failover it starts; finishing one cut
	// short is a start of its own, written first, and not made without its
	// event (R-421, R-228). The next pass tries again.
	if err := s.audit(ctx, "cluster.failover", app.AppID, "", map[string]any{
		"from_host_name": sb.StaleHost, "to_host_name": sb.Home, "trigger": "resume",
	}); err != nil {
		return
	}
	if _, err := s.promoteLocked(ctx, app.AppID, ""); err != nil {
		s.logger().Warn("could not finish an app's promotion that was cut short; trying again next pass",
			zap.String("app_id", app.AppID), zap.Error(err))
	}
}

// standbyToStart is the app's standby, its runtime and its plan.
func (s *Service) standbyToStart(ctx context.Context, appID string) (state.Standby, api.StandbyRuntime, api.BundlePlan, error) {
	sb, found, err := s.Store.Get(ctx, appID)
	if err != nil {
		return sb, nil, api.BundlePlan{}, err
	}
	if !found || sb.Host == "" {
		return sb, nil, api.BundlePlan{}, errs.New(errs.NotFound, "This app has no standby to start.").
			WithRemedy("Restore the app from a backup instead.")
	}
	sr, ok := s.standbyOf(ctx, sb.RuntimeRef)
	if !ok {
		return sb, nil, api.BundlePlan{}, errs.Newf(errs.AdapterUnavailable,
			"The runtime %s, which keeps this app's standby, cannot start it now.", sb.RuntimeRef)
	}
	plan, sp, err := s.Plans.Shape(ctx, appID)
	if err != nil {
		return sb, nil, plan, err
	}
	err = s.Plans.Complete(ctx, &plan, sp)
	return sb, sr, plan, err
}

// RemoveStale removes the copy an app left on the host it was failed over
// from, once that host is back (R-419): it is removed, never started. That
// host then becomes where the app's next standby goes, unless the app has
// one elsewhere already. Whether the host is back is the caller's to say.
// While the app's promotion is under way nothing is removed and the caller
// is told to try again, so the host stays marked lost until it is done.
func (s *Service) RemoveStale(ctx context.Context, appID, by string) error {
	unlock := s.lock(appID)
	defer unlock()
	sb, found, err := s.Store.Get(ctx, appID)
	if err != nil || !found || sb.StaleHost == "" {
		return err
	}
	if sb.PromotingAt != nil {
		return errs.Newf(errs.StateInvalid,
			"This app is being started on %s, so the copy it left on %s is not removed yet.", sb.Host, sb.StaleHost).
			WithRemedy("Wait for the start to finish; the copy is removed on the next round.")
	}
	sr, ok := s.standbyOf(ctx, sb.RuntimeRef)
	if !ok {
		return errs.Newf(errs.AdapterUnavailable,
			"The runtime %s, which ran this app before its failover, cannot remove the copy it left.", sb.RuntimeRef)
	}
	if err := sr.RemoveStaleCopy(ctx, api.BundleRef{BundleID: appID}, sb.StaleHost); err != nil {
		return err
	}
	stale, prev := sb.StaleHost, sb
	if sb.Host == "" {
		sb.Host, sb.SpecID = stale, ""
	}
	sb.StaleHost = ""
	s.save(ctx, sb, &prev)
	_ = s.audit(ctx, "app.standby.stale_removed", appID, by, map[string]any{"host": stale})
	return nil
}
