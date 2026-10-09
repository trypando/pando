package main

import (
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/appdelete"
	"github.com/trypando/pando/internal/core/applimit"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/backup"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/idle"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
)

// Idle apps and app limits (issue #131): what run wires, built here so a test
// can see it built.

// limitsWiring is the stores and services both features share.
type limitsWiring struct {
	Activity  *state.Activity
	AppLimits *state.AppLimits
	// Recorder is what the proxy tells of every request it lets through, and
	// what each replica runs to write it (R-394).
	Recorder *idle.Recorder
	// Limits and Settings answer the API (R-244, R-397).
	Limits   *applimit.Service
	Settings *idle.Settings
}

func newLimitsWiring(db *state.DB, policy corepolicy.Store, logger *zap.Logger) limitsWiring {
	activity := state.NewActivity(db)
	limits := state.NewAppLimits(db)
	return limitsWiring{
		Activity:  activity,
		AppLimits: limits,
		Recorder:  &idle.Recorder{Store: activity, Logger: logger},
		Limits:    &applimit.Service{Store: limits, Policy: policy},
		Settings:  &idle.Settings{Store: activity, Policy: policy, Clock: clock.System{}},
	}
}

// idlePassDeps is what the idle pass reaches beyond its own stores.
type idlePassDeps struct {
	Apps         *state.Apps
	Volumes      *state.Volumes
	Backups      *state.Backups
	Backup       *backup.Service
	BundleSource *state.BundleSource
	Policy       corepolicy.Store
	Notifier     idle.Notifier
	Auditor      *audit.Writer
	TeardownNow  func()
	Logger       *zap.Logger
}

// idlePass is the leader's idle job (R-393 – R-398). A deletion goes through
// the same service as a person's, so host policy's backup rule holds for it
// (R-284).
func (w limitsWiring) idlePass(d idlePassDeps) *idle.Pass {
	return &idle.Pass{
		Store: w.Activity,
		Apps:  d.Apps,
		Deleter: &appdelete.Service{
			Apps: d.Apps, Volumes: d.Volumes,
			Backups: d.Backups, Backup: d.Backup, BundleSource: d.BundleSource,
			Policy: d.Policy, Auditor: d.Auditor, TeardownNow: d.TeardownNow,
		},
		Policy:   d.Policy,
		Notifier: d.Notifier,
		Auditor:  d.Auditor,
		Clock:    clock.System{},
		Logger:   d.Logger,
	}
}

// signalOnce sends on ch without waiting: one pending signal is enough, and a
// caller asking again while one waits has nothing more to say.
func signalOnce(ch chan struct{}) func() {
	return func() {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
