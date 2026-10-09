//go:build integration

package audit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/audit"
)

// TestR379_ANamedTargetIsKeptOverTheApp asserts R-379: an event that names
// its own target is recorded as about that target, with the app it happened
// in kept beside it rather than taking its place, and an outcome its action
// does not end in derived from the catalog's list.
func TestR379_ANamedTargetIsKeptOverTheApp(t *testing.T) {
	r := newRetention(t)
	ctx := context.Background()
	w := audit.New(r.app)
	require.NoError(t, w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: "usr_ops", Action: "grant.create",
		AppID: "app_1", TargetKind: "user", TargetID: "usr_grantee", Detail: map[string]any{"role": "app.viewer"},
	}))
	require.NoError(t, w.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "app.delete.backup_failed", AppID: "app_1",
	}))

	recs, err := audit.NewReader(r.app).List(ctx, audit.Query{})
	require.NoError(t, err)
	require.Len(t, recs, 2)
	backup, grant := recs[0], recs[1]

	assert.Equal(t, "user", grant.TargetKind)
	assert.Equal(t, "usr_grantee", grant.TargetID)
	assert.Equal(t, "app_1", grant.AppID, "the app is kept beside a narrower target")
	assert.Equal(t, "success", grant.Outcome)
	assert.Equal(t, map[string]any{"role": "app.viewer"}, grant.Detail)

	assert.Equal(t, "failed", backup.Outcome, "a failure the action's last segment does not say")
	assert.Equal(t, "app", backup.TargetKind)
	assert.Equal(t, "app_1", backup.TargetID)
}
