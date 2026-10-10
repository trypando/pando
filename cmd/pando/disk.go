package main

import (
	"context"

	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/disklimit"
	"github.com/trypando/pando/internal/core/state"
)

// runtimeUsage reads an app's usage from the runtime it runs on, for the disk
// pass (R-403). Asked directly rather than through the console's observation
// cache: the pass runs on the leader every few minutes, and each answer it
// acts on should be the runtime's own.
type runtimeUsage struct{ registry *adapterapi.Registry }

func (u runtimeUsage) Usage(ctx context.Context, ref, appID string) (adapterapi.BundleUsage, bool, error) {
	rt, ok := u.registry.Runtime(ref)
	if !ok {
		return adapterapi.BundleUsage{}, false, nil
	}
	caps, err := rt.Capabilities(ctx)
	if err != nil {
		return adapterapi.BundleUsage{}, false, err
	}
	if !caps.ReportsUsage {
		return adapterapi.BundleUsage{}, false, nil
	}
	reading, err := rt.Usage(ctx, adapterapi.BundleRef{BundleID: appID})
	return reading, true, err
}

// diskPass is the leader's disk job (R-403).
func diskPass(db *state.DB, registry *adapterapi.Registry, notifier disklimit.Notifier, auditor *audit.Writer, logger *zap.Logger) *disklimit.Pass {
	return &disklimit.Pass{
		Store:    state.NewDisk(db),
		Usage:    runtimeUsage{registry: registry},
		Notifier: notifier,
		Auditor:  auditor,
		Clock:    clock.System{},
		Logger:   logger,
	}
}
