package subscription

import (
	"context"
	"strings"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/core/state"
)

// Pando's own notifications about failures, sent to the people they concern
// without anybody subscribing (R-376):
//
//   - deploy_failed: the app's owner, and whoever started the deploy.
//   - backup_failed: the app's owner, and everybody holding
//     install.backup.manage.
//
// Driven from the outbox rather than from the code that failed, so a deploy
// that fails anywhere — in the runner, in a trial, at startup when an
// interrupted one is recorded — tells the same people the same way. Sent
// once, as an event is routed. A Pando that stops between routing and
// sending loses the notification and not the event: the event is still
// delivered to every subscription, which is the delivery R-366 promises.

// Deployments reads who started a deploy.
type Deployments interface {
	ByID(ctx context.Context, deploymentID string) (state.Deployment, bool, error)
}

// VerbHolders lists the people holding an install verb.
type VerbHolders interface {
	InstallVerbHolders(ctx context.Context, verb authz.Verb) ([]string, error)
}

// TokenOwners resolves a delegated token to the person it acts for.
type TokenOwners interface {
	Owner(ctx context.Context, tokenID string) (string, bool, error)
}

func (d *Dispatcher) announce(ctx context.Context, routed []state.Event) {
	if d.Notifier == nil {
		return
	}
	for _, e := range routed {
		var kind api.NotificationKind
		var people []string
		switch e.Name {
		case events.DeployFailed:
			kind = api.NotifyDeployFailed
			people = append(d.appOwner(ctx, e.AppID), d.deployStarter(ctx, e)...)
		case events.BackupFailed:
			kind = api.NotifyBackupFailed
			people = d.appOwner(ctx, e.AppID)
			if d.Holders != nil {
				holders, err := d.Holders.InstallVerbHolders(ctx, authz.InstallBackupManage)
				if err != nil {
					d.logger().Warn("could not read who manages backups", zap.Error(err))
				}
				people = append(people, holders...)
			}
		default:
			continue
		}
		recipients := unique(people)
		if len(recipients) == 0 {
			continue
		}
		n := Describe(e, appOf(ctx, d.Apps, e.AppID))
		n.Kind = kind
		n.Link = LinkFor(d.ExternalURL, e.AppID)
		for _, r := range recipients {
			n.Recipients = append(n.Recipients, api.Recipient{UserID: r})
		}
		if err := d.Notifier.Notify(ctx, n); err != nil {
			d.logger().Warn("could not send a failure notification", zap.String("event_id", e.ID), zap.Error(err))
		}
	}
}

func (d *Dispatcher) appOwner(ctx context.Context, appID string) []string {
	if appID == "" || d.Apps == nil {
		return nil
	}
	app, ok, err := d.Apps.ByID(ctx, appID)
	if err != nil || !ok || app.OwnerUserID == "" {
		return nil
	}
	return []string{app.OwnerUserID}
}

// deployStarter is the person who started a deploy: its creator, or the
// person a delegated token that created it acts for. Pando itself and an
// account token are nobody to notify.
func (d *Dispatcher) deployStarter(ctx context.Context, e state.Event) []string {
	depID, _ := e.Data["deployment_id"].(string)
	if depID == "" || d.Deployments == nil {
		return nil
	}
	dep, ok, err := d.Deployments.ByID(ctx, depID)
	if err != nil || !ok {
		return nil
	}
	by := dep.CreatedBy
	switch {
	case strings.HasPrefix(by, "usr_"):
		return []string{by}
	case strings.HasPrefix(by, "tok_") && d.TokenOwners != nil:
		if owner, ok, err := d.TokenOwners.Owner(ctx, by); err == nil && ok && owner != "" {
			return []string{owner}
		}
	}
	return nil
}

func unique(ids []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}
