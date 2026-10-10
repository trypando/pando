// Package disklimit holds apps to their disk limits (R-403, issue #130).
//
// Every app has a disk limit (R-240), and until this existed it was only
// counted: the planner refused a deploy that would commit more disk than the
// host has (R-242), and then nothing stopped the app writing past it. One app
// could fill the disk every app and Pando share. A container runtime can only
// enforce a size on a container's own layer, on some storage drivers, and
// never on a volume — so Pando measures instead, and stops an app that stays
// over.
//
// The pass runs on the leader. For each running app it reads what its
// containers and volumes hold, from the runtime (R-245), and:
//
//   - at 90% of the limit, tells the owner once;
//   - over the limit, tells the owner and records the reading;
//   - over the limit at the next reading too, stops it.
//
// Two readings rather than one, so a moment over — a large file written and
// then removed — is a warning and not a stop. An app Pando stopped stays
// stopped until somebody starts it, and starting it clears every mark in the
// same write (migration 71), so this package never starts anything.
package disklimit

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/state"
)

// DefaultEvery is how often the pass runs, and so how long an app is over its
// limit before it is stopped: a disk fills in minutes, not days.
const DefaultEvery = 10 * time.Minute

// WarnAt is the share of its limit at which an app's owner is told it is near
// it, and ClearAt the share below which that warning is withdrawn, so an app
// hovering at 90% is not warned every pass.
const (
	WarnAt  = 0.9
	ClearAt = 0.8
)

// Store is the state the pass reads and writes. *state.Disk is one.
type Store interface {
	DiskCandidates(ctx context.Context) ([]state.DiskApp, error)
	SetDiskMark(ctx context.Context, appID string, which state.DiskMark, at *time.Time) error
	StopForDisk(ctx context.Context, appID string) (bool, error)
}

// UsageReader reads what an app is using from the runtime it runs on. supported is
// false when that runtime cannot say, and then the pass leaves the app alone.
type UsageReader interface {
	Usage(ctx context.Context, runtimeRef, appID string) (reading api.BundleUsage, supported bool, err error)
}

// Notifier tells people things. subscription.Router is one.
type Notifier interface {
	Notify(ctx context.Context, n api.Notification) error
}

// Auditor writes audit events.
type Auditor interface {
	Write(ctx context.Context, e audit.Event) error
}

// Pass is the disk job.
type Pass struct {
	Store Store
	Usage UsageReader

	// Notifier and Auditor are optional, and nil says nothing. Production
	// wires both.
	Notifier Notifier
	Auditor  Auditor

	Clock  clock.Clock
	Logger *zap.Logger

	// Every is how often Run runs a pass. Zero is DefaultEvery.
	Every time.Duration
}

// Run passes until ctx is canceled.
func (p *Pass) Run(ctx context.Context) {
	ticker := time.NewTicker(p.every())
	defer ticker.Stop()
	for {
		p.Once(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Once runs one pass.
func (p *Pass) Once(ctx context.Context) {
	apps, err := p.Store.DiskCandidates(ctx)
	if err != nil {
		p.logger().Warn("could not list apps for the disk pass", zap.Error(err))
		return
	}
	for _, app := range apps {
		if ctx.Err() != nil {
			return
		}
		p.visit(ctx, app)
	}
}

func (p *Pass) visit(ctx context.Context, app state.DiskApp) {
	if app.DiskBytes <= 0 || app.RuntimeRef == "" {
		return
	}
	reading, supported, err := p.Usage.Usage(ctx, app.RuntimeRef, app.AppID)
	if err != nil {
		p.logger().Warn("could not read an app's disk use", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	used, known := Used(reading)
	if !supported || !known {
		return
	}

	now := p.now()
	switch {
	case used > app.DiskBytes && app.OverAt != nil:
		p.stop(ctx, app, used)
	case used > app.DiskBytes:
		p.over(ctx, app, used, now)
	default:
		p.under(ctx, app, used, now)
	}
}

// Used is what an app's containers and volumes hold. A part the runtime cannot
// measure (-1) is left out, so the figure is never more than the truth, and
// known is false only when nothing could be measured.
func Used(r api.BundleUsage) (used int64, known bool) {
	for _, w := range r.Workloads {
		if w.DiskBytes >= 0 {
			used += w.DiskBytes
			known = true
		}
	}
	for _, v := range r.Volumes {
		if v.Bytes >= 0 {
			used += v.Bytes
			known = true
		}
	}
	return used, known
}

func (p *Pass) under(ctx context.Context, app state.DiskApp, used int64, now time.Time) {
	if app.OverAt != nil {
		p.mark(ctx, app.AppID, state.DiskOver, nil)
	}
	switch {
	case app.WarnedAt == nil && float64(used) >= WarnAt*float64(app.DiskBytes):
		p.mark(ctx, app.AppID, state.DiskWarned, &now)
		p.notify(ctx, app, app.Name+" is near its disk limit",
			fmt.Sprintf("%s is using %s of its %s disk limit, counting its containers and its volumes. "+
				"If it goes over and stays over, Pando stops it. %s", app.Name, size(used), size(app.DiskBytes), remedy))
		p.audit(ctx, "app.disk.warning", app, used, map[string]any{"over": false})
	case app.WarnedAt != nil && float64(used) < ClearAt*float64(app.DiskBytes):
		p.mark(ctx, app.AppID, state.DiskWarned, nil)
	}
}

func (p *Pass) over(ctx context.Context, app state.DiskApp, used int64, now time.Time) {
	p.mark(ctx, app.AppID, state.DiskOver, &now)
	if app.WarnedAt == nil {
		p.mark(ctx, app.AppID, state.DiskWarned, &now)
	}
	p.notify(ctx, app, app.Name+" is over its disk limit",
		fmt.Sprintf("%s is using %s, over its %s disk limit, counting its containers and its volumes. "+
			"If it still is at Pando's next check, in about %s, Pando will stop it. Its data is kept. %s",
			app.Name, size(used), size(app.DiskBytes), minutes(p.every()), remedy))
	p.audit(ctx, "app.disk.warning", app, used, map[string]any{"over": true})
}

func (p *Pass) stop(ctx context.Context, app state.DiskApp, used int64) {
	stopped, err := p.Store.StopForDisk(ctx, app.AppID)
	if err != nil {
		p.logger().Warn("could not stop an app over its disk limit", zap.String("app_id", app.AppID), zap.Error(err))
		return
	}
	if !stopped {
		// Stopped or deleted by somebody since the pass read it; theirs is
		// the newer decision.
		return
	}
	p.audit(ctx, "app.disk.stopped", app, used, nil)
	p.notify(ctx, app, app.Name+" was stopped",
		fmt.Sprintf("%s stayed over its %s disk limit, using %s, so Pando stopped it. Its data is kept. %s "+
			"Start it again from its page in Pando once it has room; if it is still over, Pando stops it again.",
			app.Name, size(app.DiskBytes), size(used), remedy))
	p.logger().Info("stopped an app over its disk limit", zap.String("app_id", app.AppID),
		zap.Int64("used_bytes", used), zap.Int64("limit_bytes", app.DiskBytes))
}

// remedy is the same in every message: the two ways out.
const remedy = "To give it more room, raise resources.disk_bytes in its configuration and deploy it, " +
	"which needs permission to override resource limits; or remove data it no longer needs."

func (p *Pass) mark(ctx context.Context, appID string, which state.DiskMark, at *time.Time) {
	if err := p.Store.SetDiskMark(ctx, appID, which, at); err != nil {
		p.logger().Warn("could not record an app's disk use", zap.String("app_id", appID), zap.Error(err))
	}
}

func (p *Pass) notify(ctx context.Context, app state.DiskApp, subject, body string) {
	if p.Notifier == nil || app.OwnerUserID == "" {
		return
	}
	if err := p.Notifier.Notify(ctx, api.Notification{
		Kind:       api.NotifyAppDisk,
		AppID:      app.AppID,
		Recipients: []api.Recipient{{UserID: app.OwnerUserID}},
		Subject:    subject,
		Body:       body,
	}); err != nil {
		p.logger().Warn("could not tell an app's owner about its disk use",
			zap.String("app_id", app.AppID), zap.Error(err))
	}
}

func (p *Pass) audit(ctx context.Context, action string, app state.DiskApp, used int64, extra map[string]any) {
	if p.Auditor == nil {
		return
	}
	detail := map[string]any{"used_bytes": used, "limit_bytes": app.DiskBytes}
	for k, v := range extra {
		detail[k] = v
	}
	if err := p.Auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: "disk",
		Action: action, AppID: app.AppID, TargetKind: "app", TargetID: app.AppID, Detail: detail,
	}); err != nil {
		p.logger().Error("audit write failed", zap.String("action", action), zap.Error(err))
	}
}

func (p *Pass) every() time.Duration {
	if p.Every <= 0 {
		return DefaultEvery
	}
	return p.Every
}

func (p *Pass) now() time.Time {
	if p.Clock == nil {
		return time.Now().UTC()
	}
	return p.Clock.Now()
}

func (p *Pass) logger() *zap.Logger {
	if p.Logger == nil {
		return zap.NewNop()
	}
	return p.Logger
}

// size is a number of bytes as a person reads it.
func size(b int64) string {
	const gib, mib = 1 << 30, 1 << 20
	if b >= gib {
		return fmt.Sprintf("%.1f GiB", float64(b)/gib)
	}
	return fmt.Sprintf("%d MiB", b/mib)
}

func minutes(d time.Duration) string {
	if d < 2*time.Minute {
		return "a minute"
	}
	return fmt.Sprintf("%d minutes", int(d/time.Minute))
}
