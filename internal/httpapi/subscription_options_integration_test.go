//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// TestR375_AWebhookSendsTheRequestItsReceiverExpects asserts R-375: a webhook
// may choose its method and content type, carry headers of its own whose
// values are kept sealed, and render its body from a template — and Pando's
// own headers and signature still apply, over the body actually sent.
func TestR375_AWebhookSendsTheRequestItsReceiverExpects(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)

	sub := i.subscribe(i.admin(), map[string]any{
		"events": []string{"app.created"}, "destination": "webhook", "url": hook.URL,
		"method": "put", "content_type": "application/json",
		"headers":          map[string]string{"authorization": "Bearer receiver-credential", "X-Team": "ops"},
		"payload_template": `{"text": {{json .Subject}}, "app": {{json .App.Name}}, "link": {{json .Link}}}`,
	})

	// The header names are shown; their values never are.
	got := i.do(i.admin(), http.MethodGet, "/subscriptions/"+sub.ID, nil)
	require.Contains(t, got.String(), `"header_names":["Authorization","X-Team"]`)
	require.NotContains(t, got.String(), "receiver-credential")
	var row string
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT row_to_json(s)::text FROM subscriptions s WHERE id = $1`, sub.ID).Scan(&row))
	require.NotContains(t, row, "receiver-credential")

	appID := i.createApp(i.admin(), "billing")
	i.Dispatcher.Pass(context.Background())

	require.Equal(t, 1, hook.count())
	req, body := hook.hits[0], hook.bodies[0]
	require.Equal(t, http.MethodPut, req.Method)
	require.Equal(t, "Bearer receiver-credential", req.Header.Get("Authorization"))
	require.Equal(t, "ops", req.Header.Get("X-Team"))
	require.NotEmpty(t, req.Header.Get("Pando-Signature"), "Pando's headers are still sent")

	var sent map[string]string
	require.NoError(t, json.Unmarshal(body, &sent), string(body))
	require.Equal(t, "billing", sent["app"])
	require.Equal(t, "https://pando.test/admin/apps/"+appID+"/events", sent["link"], "R-369: the link reaches a webhook")
	require.NotEmpty(t, sent["text"])

	// Replacing the headers replaces them all.
	got = i.do(i.admin(), http.MethodPatch, "/subscriptions/"+sub.ID, map[string]any{"headers": map[string]string{}})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	require.NotContains(t, got.String(), "Authorization")
}

func TestR375_ARequestOptionThatCannotWorkIsRefusedWhenSaved(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	base := map[string]any{"events": []string{"*"}, "destination": "webhook", "url": hook.URL}
	with := func(k string, v any) map[string]any {
		m := map[string]any{}
		for key, val := range base {
			m[key] = val
		}
		m[k] = v
		return m
	}
	for name, body := range map[string]map[string]any{
		"a forged signature":          with("headers", map[string]string{"Pando-Signature": "v1=x"}),
		"a reserved header":           with("headers", map[string]string{"Host": "elsewhere"}),
		"a method a hook can't use":   with("method", "DELETE"),
		"a template that won't parse": with("payload_template", "{{.Subject"),
		"a template that isn't JSON":  with("payload_template", "subject: {{.Subject}}"),
	} {
		got := i.do(i.admin(), http.MethodPost, "/subscriptions", body)
		require.Equal(t, http.StatusBadRequest, got.Code, name+": "+got.String())
	}

	// Not JSON is fine when the content type says so.
	form := with("payload_template", "text={{.Subject}}")
	form["content_type"] = "application/x-www-form-urlencoded"
	i.subscribe(i.admin(), form)
}

// TestR368_AnAccountTokenOwnsWhatItsGrantsAllow asserts R-368 for account
// tokens: one may subscribe to what its grants let it see, it may not send to
// a destination that reaches people, and its deliveries stop when it is
// revoked.
func TestR368_AnAccountTokenOwnsWhatItsGrantsAllow(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	appID := i.createApp(i.admin(), "billing")
	ci := i.serviceToken(i.admin())
	tokenID, _, _ := strings.Cut(ci.token, ".")

	got := i.do(i.admin(), http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
		"plane": "control", "principal_kind": "token", "principal_id": tokenID, "role_id": "role_viewer",
	})
	require.Equal(t, http.StatusCreated, got.Code, got.String())

	got = i.do(ci, http.MethodPost, "/subscriptions", map[string]any{
		"app_id": appID, "events": []string{"*"}, "destination": "notify", "adapter_id": "ntf_people",
	})
	require.Equal(t, http.StatusBadRequest, got.Code, "a token has no inbox: "+got.String())

	sub := i.subscribe(ci, map[string]any{
		"app_id": appID, "events": []string{"app.state_changed"}, "destination": "webhook", "url": hook.URL,
	})
	var listed struct {
		Subscriptions []struct {
			ID           string `json:"id"`
			OwnerTokenID string `json:"owner_token_id"`
		} `json:"subscriptions"`
	}
	i.do(ci, http.MethodGet, "/subscriptions", nil).JSON(t, &listed)
	require.Len(t, listed.Subscriptions, 1)
	require.Equal(t, tokenID, listed.Subscriptions[0].OwnerTokenID)

	_, err := i.db.Exec(context.Background(), `UPDATE apps SET state = 'degraded' WHERE id = $1`, appID)
	require.NoError(t, err)
	i.Dispatcher.Pass(context.Background())
	require.Equal(t, 1, hook.count())

	require.NoError(t, i.Tokens.Revoke(context.Background(), tokenID))
	_, err = i.db.Exec(context.Background(), `UPDATE apps SET state = 'running' WHERE id = $1`, appID)
	require.NoError(t, err)
	i.Dispatcher.Pass(context.Background())
	require.Equal(t, 1, hook.count(), "a revoked token's subscription sends nothing")
	var reason string
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT last_error FROM event_deliveries WHERE subscription_id = $1 AND status = 'failed'`, sub.ID).Scan(&reason))
	require.Contains(t, reason, "revoked")
}

// TestR376_AFailedDeployTellsTheOwnerAndWhoeverStartedIt asserts R-376.
func TestR376_AFailedDeployTellsTheOwnerAndWhoeverStartedIt(t *testing.T) {
	ctx := context.Background()
	i := newInstall(t)
	appID := i.createApp(i.admin(), "billing")
	ann := i.user("ann")
	i.grantControl(appID, ann, "role_owner")

	rev, err := i.Apps.CreateRevision(ctx, appID, &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion, AppID: appID,
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/app"},
	}, spec.OriginManual, i.AdminID)
	require.NoError(t, err)
	deployments := state.NewDeployments(i.db)
	dep, err := deployments.Create(ctx, appID, rev.ID, "manual", i.userID(ann))
	require.NoError(t, err)
	require.NoError(t, deployments.Finish(ctx, dep.ID, state.DeployFailed, "BUILD_FAILED", "The build exited with status 1."))

	i.Dispatcher.Pass(ctx)

	var told []string
	for _, n := range i.People.sent() {
		if n.Kind == adapterapi.NotifyDeployFailed {
			for _, r := range n.Recipients {
				told = append(told, r.UserID)
			}
			require.Contains(t, n.Body, "The build exited with status 1.")
			require.Equal(t, "https://pando.test/admin/apps/"+appID+"/events", n.Link)
		}
	}
	require.ElementsMatch(t, []string{i.AdminID, i.userID(ann)}, told, "the owner and the person who deployed")
	require.Empty(t, i.Channel.sent(), "never to a channel")

	// A failed scheduled backup tells the owner and whoever manages backups —
	// here one person, told once.
	require.NoError(t, state.NewBackups(i.db).RecordAttempt(ctx, state.BackupAttempt{
		AppID: appID, Outcome: state.AttemptFailed, AttemptedAt: dep.StartedAt, Message: "The destination is full.",
	}))
	i.Dispatcher.Pass(ctx)
	var backup []adapterapi.Notification
	for _, n := range i.People.sent() {
		if n.Kind == adapterapi.NotifyBackupFailed {
			backup = append(backup, n)
		}
	}
	require.Len(t, backup, 1)
	require.Equal(t, []adapterapi.Recipient{{UserID: i.AdminID}}, backup[0].Recipients)
}
