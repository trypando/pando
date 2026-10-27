//go:build integration

package main

import (
	"context"

	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/state"
)

// registerAdapters is registerAdaptersOn for the install's own machine, as
// the tests that load adapters run.
func registerAdapters(ctx context.Context, db *state.DB, store *state.Adapters, notifications *state.Notifications,
	declared []config.AdapterDecl, logger *zap.Logger) (*adapterapi.Registry, *state.AdapterCredentials, error) {
	return registerAdaptersOn(ctx, db, store, notifications, declared, multidocker.Machine{}, logger)
}
