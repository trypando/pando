package upgrade

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
)

// LoopInterval is how often the loop looks. A maintenance window is whole
// hours, so five minutes finds every one.
const LoopInterval = 5 * time.Minute

// Run does what an upgrade leaves to the Pando that comes after it, and what
// the schedule asks for, until ctx ends:
//
//   - records the last upgrade's outcome in the audit log, once (R-356);
//   - drops the rollback copy after the soak (R-359);
//   - tells install.upgrade holders about a new release, once per version
//     (R-362);
//   - starts an automatic patch upgrade inside the window (R-361).
func (s *Service) Run(ctx context.Context) {
	for {
		s.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-s.Clock.After(LoopInterval):
		}
	}
}

// Tick is one pass of Run.
func (s *Service) Tick(ctx context.Context) {
	if err := s.record(ctx); err != nil {
		s.Logger.Warn("could not record the last upgrade's outcome", zap.Error(err))
	}
	if err := s.notifyAvailable(ctx); err != nil {
		s.Logger.Warn("could not send the update-available notification", zap.Error(err))
	}
	if err := s.schedule(ctx); err != nil {
		s.Logger.Warn("the scheduled upgrade did not start", zap.Error(err))
	}
}

func (s *Service) record(ctx context.Context) error {
	path := OutcomePath(s.WorkDir)
	o, err := ReadOutcome(path)
	if err != nil || o == nil {
		return err
	}

	if o.State == StateRunning && s.Clock.Since(o.StartedAt) > staleRunning {
		// The helper died without saying how it went. This Pando is running,
		// so whatever happened, it is what is here now.
		o.State, o.FinishedAt = StateFailed, s.Clock.Now()
		o.Reason = "The upgrade helper stopped without recording how the upgrade went. Pando " + s.Version + " is running."
	}

	if o.State != StateRunning && !o.Recorded {
		action := "upgrade." + o.State
		if o.State == StateSucceeded {
			action = "upgrade.succeeded"
		}
		s.Audit(ctx, audit.Event{
			PrincipalKind: audit.PrincipalKind(authz.KindSystem), PrincipalID: authz.System().ID,
			Action: action, TargetKind: "upgrade", TargetID: o.ID,
			Detail: map[string]any{"from": o.From, "to": o.To, "started_by": o.StartedBy, "automatic": o.Automatic, "reason": o.Reason},
		})
		if o.State != StateSucceeded {
			s.send(ctx, api.Notification{
				Kind:    api.NotifyUpgradeFailed,
				Subject: "Pando did not upgrade to " + o.To,
				Body:    o.Reason,
			})
		}
		if rt, err := s.Runtime(ctx); err == nil {
			if up, ok := rt.(api.SelfUpgrader); ok {
				_ = up.RemoveHelpers(ctx)
			}
		}
		o.Recorded = true
	}

	// The copy is kept until the new version has been healthy for the soak,
	// and only while it is still that version that is running (R-359).
	if o.State == StateSucceeded && !o.SnapshotAt.IsZero() && !o.SnapshotGone &&
		strings.TrimPrefix(s.Version, "v") == o.To && s.Clock.Since(o.FinishedAt) >= s.soak() {
		if err := s.DropSnapshot(ctx); err != nil {
			_ = WriteOutcome(path, *o)
			return err
		}
		o.SnapshotGone = true
	}
	return WriteOutcome(path, *o)
}

// notifyAvailable sends update_available once per version (R-362). Which
// version was last announced is kept beside the outcome, so a restart does not
// announce it again.
func (s *Service) notifyAvailable(ctx context.Context) error {
	st, err := s.Updates.Status(ctx)
	if err != nil || !st.Available {
		return err
	}
	marker := filepath.Join(s.WorkDir, "upgrade", "announced")
	if b, err := os.ReadFile(marker); err == nil && strings.TrimSpace(string(b)) == st.Latest {
		return nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	body := "Pando " + st.Latest + " is released; this installation runs " + st.Current + ". The Updates screen has what changed and how to upgrade."
	if st.Security {
		body = "Pando " + st.Latest + " is released and fixes a security advisory; this installation runs " + st.Current + ". The Updates screen has what changed and how to upgrade."
	}
	s.send(ctx, api.Notification{Kind: api.NotifyUpdateAvailable, Subject: "Pando " + st.Latest + " is available", Body: body})
	if err := os.MkdirAll(filepath.Dir(marker), 0o700); err != nil {
		return err
	}
	return os.WriteFile(marker, []byte(st.Latest), 0o600)
}

func (s *Service) send(ctx context.Context, n api.Notification) {
	if s.Notify == nil || s.Recipients == nil {
		return
	}
	ids, err := s.Recipients(ctx)
	if err != nil {
		s.Logger.Warn("could not read who to notify about an upgrade", zap.Error(err))
		return
	}
	for _, id := range ids {
		n.Recipients = append(n.Recipients, api.Recipient{UserID: id})
	}
	if len(n.Recipients) > 0 {
		_ = s.Notify(ctx, n)
	}
}

// schedule starts an automatic patch upgrade when policy allows one now
// (R-361): the newest stable patch of the running minor line, never a version
// an earlier upgrade already failed to reach.
func (s *Service) schedule(ctx context.Context) error {
	doc, err := s.Policy(ctx)
	if err != nil || !doc.AutoUpgradePatches || !doc.UpgradeInPlace || doc.MaintenanceWindow == "" {
		return err
	}
	w, err := policy.ParseWindow(doc.MaintenanceWindow)
	if err != nil || !w.Open(s.Clock.Now()) {
		return err
	}
	st, err := s.Updates.Status(ctx)
	if err != nil || !st.Available || st.Development {
		return err
	}
	current := "v" + strings.TrimPrefix(s.Version, "v")
	target := ""
	for _, r := range st.Releases {
		v := "v" + r.Version
		if !r.Prerelease && semver.Prerelease(v) == "" && semver.MajorMinor(v) == semver.MajorMinor(current) &&
			semver.Compare(v, current) > 0 && (target == "" || semver.Compare(v, "v"+target) > 0) {
			target = r.Version
		}
	}
	if target == "" {
		return nil
	}
	if o, _ := ReadOutcome(OutcomePath(s.WorkDir)); o != nil && o.To == target && o.State != StateSucceeded {
		return nil
	}
	plan, err := s.PlanFor(ctx, target)
	if err != nil || !plan.Possible {
		return err
	}
	_, err = s.Start(ctx, authz.System(), Request{Version: target, Automatic: true})
	return err
}
