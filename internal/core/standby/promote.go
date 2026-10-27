package standby

import (
	"context"

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
	sb, sr, plan, err := s.standbyToStart(ctx, appID)
	if err != nil {
		return api.PromoteResult{}, err
	}
	got, err := sr.PromoteStandby(ctx, api.PromoteRequest{Bundle: plan, Host: sb.Host, HealthTimeout: s.HealthTimeout})
	if err != nil {
		s.audit(ctx, "app.standby.promote.failed", appID, by, map[string]any{"host": sb.Host, "error": message(err)})
		return got, err
	}
	now := s.now()
	sb.StaleHost, sb.PromotedAt = got.FormerHost, &now
	sb.Host, sb.SpecID, sb.Copies, sb.ShippedAt, sb.LastError = "", "", 0, nil, ""
	s.save(ctx, sb)
	s.audit(ctx, "app.standby.promote", appID, by, map[string]any{
		"host": got.Host, "former_host": got.FormerHost, "copy": got.Copy, "fell_back": got.FellBack, "healthy": got.Healthy,
	})
	return got, nil
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
func (s *Service) RemoveStale(ctx context.Context, appID, by string) error {
	sb, found, err := s.Store.Get(ctx, appID)
	if err != nil || !found || sb.StaleHost == "" {
		return err
	}
	sr, ok := s.standbyOf(ctx, sb.RuntimeRef)
	if !ok {
		return errs.Newf(errs.AdapterUnavailable,
			"The runtime %s, which ran this app before its failover, cannot remove the copy it left.", sb.RuntimeRef)
	}
	if err := sr.RemoveStaleCopy(ctx, api.BundleRef{BundleID: appID}, sb.StaleHost); err != nil {
		return err
	}
	stale := sb.StaleHost
	if sb.Host == "" {
		sb.Host, sb.SpecID = stale, ""
	}
	sb.StaleHost = ""
	s.save(ctx, sb)
	s.audit(ctx, "app.standby.stale_removed", appID, by, map[string]any{"host": stale})
	return nil
}
