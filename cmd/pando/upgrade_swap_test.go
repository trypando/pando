package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/core/upgrade"
)

// Every step the helper takes fails cleanly, with an error, when the
// container it was told to replace does not exist — whether or not a daemon
// is there to say so. RunHelper decides what each failure means; these only
// have to report it.
func TestTheHelpersStepsReportAMissingContainerAsAnError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := docker.NewReplacer()
	require.NoError(t, err)

	s := &dockerSwap{r: r, old: docker.Old{ID: "pando-no-such-container", Name: "pando-no-such-container"}, port: "8080"}
	require.Error(t, s.Stop(ctx))
	require.Error(t, s.Recreate(ctx, "busybox:1.37"))
	s.newID = "pando-no-such-container-new"
	require.Error(t, s.Ready(ctx, time.Second))
	require.Empty(t, s.Logs(ctx))
	s.Discard(ctx)
	require.Error(t, s.Restore(ctx))
	require.Error(t, s.Finish(ctx, "busybox:1.37", ""))

	db := snapshotDB{url: "postgres://pando:pw@127.0.0.1:1/pando?connect_timeout=1"}
	require.Error(t, db.Snapshot(ctx))
	require.Error(t, db.Restore(ctx))
}

// With the database URL present, the helper goes as far as reading the
// container it was told to replace, and stops there when it cannot.
func TestTheUpgradeHelperStopsWhenItCannotReadTheContainer(t *testing.T) {
	t.Setenv(upgrade.DatabaseURLEnv, "postgres://pando:pw@127.0.0.1:1/pando")
	cmd := upgradeHelperCmd()
	cmd.SetArgs([]string{"--container", "pando-no-such-container", "--outcome", filepath.Join(t.TempDir(), "outcome.json")})
	require.Error(t, cmd.Execute())
}
