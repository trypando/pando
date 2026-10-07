package policy_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/policy"
)

// TestR227_AnonymousDenialAuditIsAPolicySetting asserts the
// disable_anonymous_denial_audit setting: off by default, so anonymous
// denials are audited; settable at startup as PANDO_POLICY_<FIELD> like any
// other policy field (R-271); and audited when the policy cannot be read.
func TestR227_AnonymousDenialAuditIsAPolicySetting(t *testing.T) {
	ctx := context.Background()

	require.True(t, policy.Static(policy.Default()).AuditsAnonymousDenials(ctx), "audited by default")
	require.False(t, policy.Static(policy.Document{DisableAnonymousDenialAudit: true}).AuditsAnonymousDenials(ctx))

	o, err := policy.NewOverlay([]policy.Setting{{
		Key: "disable_anonymous_denial_audit", Value: "true",
		Source: policy.Source{Kind: "env", Name: "PANDO_POLICY_DISABLE_ANONYMOUS_DENIAL_AUDIT"},
	}})
	require.NoError(t, err)
	require.True(t, o.Apply(policy.Document{}).DisableAnonymousDenialAudit)

	unreadable := policy.New(func(context.Context) (policy.Document, error) {
		return policy.Document{}, errors.New("the database went away")
	})
	require.True(t, unreadable.AuditsAnonymousDenials(ctx), "when unsure, keep the record")
	require.NotEmpty(t, policy.Describe("disable_anonymous_denial_audit"))
}

// TestR274_ASnapshotIsTheDocumentAsItIsNow asserts that a snapshot answers
// as Allows does over the document it read, and that the evaluator itself is
// not changed by taking one: the next question reads policy again (R-274).
func TestR274_ASnapshotIsTheDocumentAsItIsNow(t *testing.T) {
	ctx := context.Background()
	doc := policy.Document{DisabledVerbs: []string{string(authz.AppExec)}}
	reads := 0
	e := policy.New(func(context.Context) (policy.Document, error) {
		reads++
		return doc, nil
	})

	snap, err := e.Snapshot(ctx)
	require.NoError(t, err)
	user := authz.Principal{Kind: authz.KindUser, ID: "usr_a", UserID: "usr_a", Status: "active"}
	for _, v := range authz.AppVerbs() {
		require.Equal(t, e.Allows(ctx, user, v, "app_x") == nil, snap.Allows(ctx, user, v, "app_x") == nil, v)
	}
	before := reads

	// Policy changes: the snapshot keeps what it read, the evaluator does not.
	doc = policy.Document{}
	require.Error(t, snap.Allows(ctx, user, authz.AppExec, "app_x"))
	require.NoError(t, e.Allows(ctx, user, authz.AppExec, "app_x"))
	require.Equal(t, before+1, reads)
}
