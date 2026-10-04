package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
)

// The upgrade is wired from what serve already holds: the port the helper
// checks is the one Pando listens on, the outcome lives under its data
// directory, and an install with no runtime says so rather than panicking.
func TestTheUpgradeIsWiredFromTheServersOwnConfiguration(t *testing.T) {
	cfg := &config.Config{}
	cfg.Server.Addr = ":9090"
	cfg.Server.WorkDir = t.TempDir()
	cfg.Database.URL = "postgres://pando:pw@postgres:5432/pando"

	svc := newUpgradeService(upgradeDeps{
		cfg:      cfg,
		updates:  &update.Checker{},
		registry: adapterapi.NewRegistry(),
		policy:   func(context.Context) (policy.Document, error) { return policy.Default(), nil },
		logger:   zap.NewNop(),
	})
	require.Equal(t, "9090", svc.Port)
	require.Equal(t, cfg.Server.WorkDir, svc.WorkDir)
	require.Equal(t, cfg.Database.URL, svc.DatabaseURL.Reveal())
	require.NotNil(t, svc.Verify)

	_, err := svc.Runtime(context.Background())
	require.ErrorContains(t, err, "no runtime adapter")
}

// The helper refuses to start without the database URL, which Pando passes in
// its environment rather than on the command line (R-194).
func TestTheUpgradeHelperNeedsTheDatabaseURL(t *testing.T) {
	t.Setenv(upgrade.DatabaseURLEnv, "")
	cmd := upgradeHelperCmd()
	require.True(t, cmd.Hidden)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--container", "c0ffee", "--outcome", t.TempDir() + "/outcome.json"})
	require.ErrorContains(t, cmd.Execute(), upgrade.DatabaseURLEnv)

	timeout, err := cmd.Flags().GetDuration("ready-timeout")
	require.NoError(t, err)
	require.Equal(t, 5*time.Minute, timeout)
}

// dockerSwap and snapshotDB are what RunHelper sees; they hold no logic of
// their own, so a swap that was never created has no logs to read.
func TestTheHelpersSwapHasNoLogsBeforeANewContainerExists(t *testing.T) {
	s := &dockerSwap{}
	require.Empty(t, s.Logs(context.Background()))
	var _ upgrade.Swap = s
	var _ upgrade.Database = snapshotDB{}
}
