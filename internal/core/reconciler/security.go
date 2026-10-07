package reconciler

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/state"
)

// The security pass (R-315, R-316, design 09 §4.2).
//
// Not the deploy path, because the trigger is usually neither a deploy nor a
// scan: it is a policy change, or a rescan that found something new in an app
// nobody has touched for a month. This runs where the other slow, careful jobs
// run, and it is the one place that may stop somebody's working service —
// which is why every branch of it is audited and none of it is quiet.

// SecurityScores is what this pass needs from the security service.
//
// Place, the batched path, rather than a Report per app: a report is a policy
// load, a scan read and a state read for each app, which over twenty thousand
// apps is sixty thousand queries an hour for an answer one query and one
// policy load give (issue #72). The scores come from LiveSecurityState.
type SecurityScores interface {
	Configured() (string, bool)
	Place(ctx context.Context, scores map[string]security.Scores) (map[string]security.Placed, error)
}

// SecurityState is the store half.
type SecurityState interface {
	LiveSecurityState(ctx context.Context) ([]state.SecurityState, error)
	MarkInsecure(ctx context.Context, appID string, at time.Time) error
	ClearInsecure(ctx context.Context, appID string) error
	SetStoppedForSecurity(ctx context.Context, appID string, stopped bool) error
}

// Desired sets an app's desired state, which is how an app is stopped and
// started (design 09 §4.2: never a delete, never `failed`).
type Desired interface {
	SetDesiredState(ctx context.Context, appID, desired string) error
}

// PolicyStore reads host policy.
type PolicyStore interface {
	Document(ctx context.Context) (policy.Document, error)
}

// OwnerNotifier tells an app's owner what is about to happen to it (R-315).
type OwnerNotifier interface {
	Notify(ctx context.Context, userID, appID, subject, body string)
}

// enforceSecurity walks every live app and places it against the threshold.
func (g *GC) enforceSecurity(ctx context.Context) {
	if g.Security == nil || g.SecurityState == nil || g.PolicyStore == nil {
		return
	}
	if _, configured := g.Security.Configured(); !configured {
		// A threshold with nothing to enforce it is inert, and says so in the
		// console rather than here (R-317).
		return
	}

	doc, err := g.PolicyStore.Document(ctx)
	if err != nil {
		g.Logger.Warn("could not read host policy for the security pass", zap.Error(err))
		return
	}
	if doc.MinSecurityScore <= 0 {
		return
	}

	apps, err := g.SecurityState.LiveSecurityState(ctx)
	if err != nil {
		g.Logger.Warn("could not list apps for the security pass", zap.Error(err))
		return
	}

	scores := make(map[string]security.Scores, len(apps))
	for _, app := range apps {
		scores[app.AppID] = security.Scores{All: app.Score, Fixable: app.ScoreFixable}
	}
	placed, err := g.Security.Place(ctx, scores)
	if err != nil {
		g.Logger.Warn("could not place apps against the security threshold", zap.Error(err))
		return
	}

	for _, app := range apps {
		p, ok := placed[app.AppID]
		if !ok {
			continue
		}
		g.placeApp(ctx, doc, app, security.Standing{
			Score: p.Score, Threshold: doc.MinSecurityScore, Verdict: p.Verdict,
		})
	}
}

func (g *GC) placeApp(ctx context.Context, doc policy.Document, app state.SecurityState, standing security.Standing) {
	switch standing.Verdict {
	case security.VerdictOK:
		g.recovered(ctx, app, standing)
	case security.VerdictInsecure, security.VerdictUnscanned:
		g.insecure(ctx, doc, app, standing)
	}
}

// recovered clears the mark, and starts an app Pando stopped.
//
// Only one Pando stopped: an app its owner stopped stays stopped, which is the
// whole reason `stopped_for_security` is recorded separately from the desired
// state (design 09 §4.2).
func (g *GC) recovered(ctx context.Context, app state.SecurityState, report security.Standing) {
	if app.InsecureSince == nil && !app.StoppedForSecurity {
		return
	}

	if app.InsecureSince != nil {
		if err := g.SecurityState.ClearInsecure(ctx, app.AppID); err != nil {
			g.Logger.Warn("could not clear an app's insecure mark",
				zap.String("app_id", app.AppID), zap.Error(err))
			return
		}
	}

	if app.StoppedForSecurity {
		if g.Desired != nil {
			if err := g.Desired.SetDesiredState(ctx, app.AppID, state.StateRunning); err != nil {
				g.Logger.Warn("could not start an app whose score recovered",
					zap.String("app_id", app.AppID), zap.Error(err))
				return
			}
		}
		if err := g.SecurityState.SetStoppedForSecurity(ctx, app.AppID, false); err != nil {
			g.Logger.Warn("could not clear an app's security stop",
				zap.String("app_id", app.AppID), zap.Error(err))
		}
	}

	g.auditSecurity(ctx, "app.security.recovered", app, report, nil)
}

// insecure marks, warns, notifies, and stops when the grace has run out.
func (g *GC) insecure(ctx context.Context, doc policy.Document, app state.SecurityState, report security.Standing) {
	now := g.now()

	if app.InsecureSince == nil {
		if err := g.SecurityState.MarkInsecure(ctx, app.AppID, now); err != nil {
			g.Logger.Warn("could not record an app as insecure",
				zap.String("app_id", app.AppID), zap.Error(err))
			return
		}
		app.InsecureSince = &now

		// The owner is told at the moment the clock starts, with the deadline
		// in the message. A deadline nobody was told about is an outage with
		// extra steps (R-315, R-316).
		g.notifyOwner(ctx, doc, app, report)
		g.auditSecurity(ctx, "app.security.insecure", app, report, map[string]any{
			"grace_hours": doc.GraceHours(),
			"action":      actionOf(doc),
		})
		return
	}

	if !security.Due(g.Clock, doc, app.InsecureSince) {
		return
	}
	if app.StoppedForSecurity || app.DesiredState == state.StateStopped {
		// Already stopped. Nothing to do, and nothing to say again — a pass
		// that re-audited every hour would bury the event that mattered.
		return
	}

	if g.Desired != nil {
		if err := g.Desired.SetDesiredState(ctx, app.AppID, state.StateStopped); err != nil {
			g.Logger.Warn("could not stop an insecure app",
				zap.String("app_id", app.AppID), zap.Error(err))
			return
		}
	}
	if err := g.SecurityState.SetStoppedForSecurity(ctx, app.AppID, true); err != nil {
		g.Logger.Warn("could not record why an app was stopped",
			zap.String("app_id", app.AppID), zap.Error(err))
	}

	g.auditSecurity(ctx, "app.security.stopped", app, report, map[string]any{
		"grace_hours":    doc.GraceHours(),
		"insecure_since": app.InsecureSince,
	})
	g.Logger.Info("stopped an app below the installation's security threshold",
		zap.String("app_id", app.AppID), zap.Int("threshold", doc.MinSecurityScore))
}

func (g *GC) notifyOwner(ctx context.Context, doc policy.Document, app state.SecurityState, report security.Standing) {
	if g.Notifier == nil || app.OwnerUserID == "" {
		return
	}

	score := "no score"
	if report.Score != nil {
		score = fmt.Sprintf("a score of %d", *report.Score)
	}

	body := fmt.Sprintf(
		"%s has %s, and this installation requires at least %d. Fix the findings on the app's security page and deploy again.",
		app.Name, score, doc.MinSecurityScore)
	if doc.StopsInsecureApps() {
		body += fmt.Sprintf(" If it is still below the requirement in %d hours, Pando will stop it.",
			doc.GraceHours())
	}

	g.Notifier.Notify(ctx, app.OwnerUserID, app.AppID, app.Name+" is below the security requirement", body)
}

func actionOf(doc policy.Document) string {
	if doc.StopsInsecureApps() {
		return policy.InsecureStop
	}
	return policy.InsecureWarn
}

func (g *GC) auditSecurity(ctx context.Context, action string, app state.SecurityState, report security.Standing, extra map[string]any) {
	if g.Auditor == nil {
		return
	}

	detail := map[string]any{
		"threshold": report.Threshold,
		"verdict":   string(report.Verdict),
	}
	if report.Score != nil {
		detail["score"] = *report.Score
	}
	for k, v := range extra {
		detail[k] = v
	}

	_ = g.Auditor.Write(ctx, AuditEvent{Action: action, AppID: app.AppID, Detail: detail})
}
