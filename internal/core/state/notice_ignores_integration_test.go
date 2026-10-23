//go:build integration

package state_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR412_IgnoresArePerPersonAndLeaveNothingBehind asserts the storage half
// of R-412: an ignore is one person's, ignoring again replaces what was
// ignored, restoring removes it, and an app's removal takes its ignores with
// it rather than leaving rows that block anything.
func TestR412_IgnoresArePerPersonAndLeaveNothingBehind(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db := connected(t)
	ignores := state.NewNoticeIgnores(db)

	ada := seedUser(t, db, "ada")
	bob := seedUser(t, db, "bob")
	appID := seedApp(t, db, ada.ID)

	require.NoError(t, ignores.Ignore(ctx, ada.ID, appID, "variables.unset", "fp1"))
	got, err := ignores.For(ctx, ada.ID, appID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"variables.unset": "fp1"}, got)

	theirs, err := ignores.For(ctx, bob.ID, appID)
	require.NoError(t, err)
	require.Empty(t, theirs, "one person's ignore is nobody else's")

	require.NoError(t, ignores.Ignore(ctx, ada.ID, appID, "variables.unset", "fp2"))
	got, err = ignores.For(ctx, ada.ID, appID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"variables.unset": "fp2"}, got, "ignoring again replaces the fingerprint")

	for range 2 {
		require.NoError(t, ignores.Restore(ctx, ada.ID, appID, "variables.unset"))
	}
	got, err = ignores.For(ctx, ada.ID, appID)
	require.NoError(t, err)
	require.Empty(t, got)

	require.Error(t, ignores.Ignore(ctx, ada.ID, appID, "", "fp"), "a key is never empty")

	require.NoError(t, ignores.Ignore(ctx, ada.ID, appID, "config.unshipped", "fp"))
	_, err = db.Exec(ctx, `DELETE FROM apps WHERE id = $1`, appID)
	require.NoError(t, err)
	var left int
	require.NoError(t, db.QueryRow(ctx, `SELECT count(*) FROM notice_ignores WHERE app_id = $1`, appID).Scan(&left))
	require.Zero(t, left)
}
