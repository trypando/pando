//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/subscription"
	"github.com/trypando/pando/internal/secret"
)

// fakeNotify is a notify adapter that records what it is given.
type fakeNotify struct {
	kind     string
	audience adapterapi.NotifyAudience
	mu       sync.Mutex
	got      []adapterapi.Notification
}

func (f *fakeNotify) Kind() string                                     { return f.kind }
func (f *fakeNotify) Category() adapterapi.Category                    { return adapterapi.CategoryNotify }
func (f *fakeNotify) Configure(context.Context, json.RawMessage) error { return nil }
func (f *fakeNotify) HealthCheck(context.Context) error                { return nil }
func (f *fakeNotify) Capabilities() adapterapi.NotifyCapabilities {
	return adapterapi.NotifyCapabilities{Audience: f.audience}
}
func (f *fakeNotify) Notify(_ context.Context, n adapterapi.Notification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, n)
	return nil
}

func (f *fakeNotify) sent() []adapterapi.Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]adapterapi.Notification(nil), f.got...)
}

// receiver is a webhook endpoint that records what it is sent and answers
// with whatever status the test sets.
type receiver struct {
	*httptest.Server
	status atomic.Int32
	mu     sync.Mutex
	hits   []*http.Request
	bodies [][]byte
}

func newReceiver(t *testing.T) *receiver {
	t.Helper()
	r := &receiver{}
	r.status.Store(http.StatusOK)
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		r.hits = append(r.hits, req)
		r.bodies = append(r.bodies, b)
		r.mu.Unlock()
		w.WriteHeader(int(r.status.Load()))
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hits)
}

type createdSub struct {
	ID         string   `json:"id"`
	Events     []string `json:"events"`
	Enabled    bool     `json:"enabled"`
	SigningKey string   `json:"signing_key"`
	Reason     string   `json:"disabled_reason"`
}

type deliveryRow struct {
	ID         string `json:"id"`
	Event      string `json:"event"`
	Status     string `json:"status"`
	Attempts   int    `json:"attempts"`
	LastError  string `json:"last_error"`
	AttemptLog []struct {
		Attempt    int  `json:"attempt"`
		StatusCode *int `json:"status_code"`
	} `json:"attempt_log"`
}

func (i *install) allowPrivateWebhooks() {
	i.setPolicy(func(d *corepolicy.Document) { d.AllowPrivateWebhooks = true })
}

func (i *install) subscribe(s *session, body map[string]any) createdSub {
	i.t.Helper()
	got := i.do(s, http.MethodPost, "/subscriptions", body)
	require.Equal(i.t, http.StatusCreated, got.Code, got.String())
	var out createdSub
	got.JSON(i.t, &out)
	return out
}

func (i *install) deliveries(s *session, subID string) []deliveryRow {
	i.t.Helper()
	got := i.do(s, http.MethodGet, "/subscriptions/"+subID+"/deliveries", nil)
	require.Equal(i.t, http.StatusOK, got.Code, got.String())
	var out struct {
		Deliveries []deliveryRow `json:"deliveries"`
	}
	got.JSON(i.t, &out)
	return out.Deliveries
}

// TestR372_AWebhookToAPrivateAddressIsRefusedUnlessPolicyAllowsIt asserts
// R-372: a webhook may not point into Pando's own network by default, and
// host policy can allow it.
func TestR372_AWebhookToAPrivateAddressIsRefusedUnlessPolicyAllowsIt(t *testing.T) {
	i := newInstall(t)
	hook := newReceiver(t) // 127.0.0.1

	for _, url := range []string{hook.URL, "http://169.254.169.254/latest/meta-data", "http://10.0.0.5/hook"} {
		got := i.do(i.admin(), http.MethodPost, "/subscriptions", map[string]any{
			"events": []string{"*"}, "destination": "webhook", "url": url,
		})
		require.Equal(t, http.StatusForbidden, got.Code, got.String())
		require.Equal(t, "POLICY_WEBHOOK_PRIVATE_ADDRESS", got.ErrorCode(), url)
	}

	i.allowPrivateWebhooks()
	i.subscribe(i.admin(), map[string]any{"events": []string{"*"}, "destination": "webhook", "url": hook.URL})
}

// TestR369_AWebhookDeliveryIsSignedAndRecorded asserts R-365, R-369 and
// R-371: an audited action reaches the outbox, is posted to the webhook with a
// signature its key verifies and an event ID to deduplicate on, and the
// delivery and its attempt are recorded.
func TestR369_AWebhookDeliveryIsSignedAndRecorded(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)

	sub := i.subscribe(i.admin(), map[string]any{
		"events": []string{"app.created"}, "destination": "webhook", "url": hook.URL,
	})
	require.NotEmpty(t, sub.SigningKey, "the key is shown when the subscription is made")

	appID := i.createApp(i.admin(), "billing")
	i.Dispatcher.Pass(context.Background())

	require.Equal(t, 1, hook.count())
	req, body := hook.hits[0], hook.bodies[0]
	require.Equal(t, "app.created", req.Header.Get(subscription.HeaderEvent))
	require.NotEmpty(t, req.Header.Get(subscription.HeaderEventID))
	ts, err := strconv.ParseInt(req.Header.Get(subscription.HeaderTimestamp), 10, 64)
	require.NoError(t, err)
	require.True(t, subscription.Verify(secret.New(sub.SigningKey), req.Header.Get(subscription.HeaderSignature),
		ts, body, time.Now(), 5*time.Minute), "the signature verifies with the key shown at creation")
	require.False(t, subscription.Verify(secret.New("whsec_wrong"), req.Header.Get(subscription.HeaderSignature),
		ts, body, time.Now(), 5*time.Minute))
	require.Contains(t, string(body), appID)
	require.Contains(t, string(body), `"name":"billing"`)

	list := i.deliveries(i.admin(), sub.ID)
	require.Len(t, list, 1)
	require.Equal(t, "succeeded", list[0].Status)

	// The key is never shown again.
	got := i.do(i.admin(), http.MethodGet, "/subscriptions/"+sub.ID, nil)
	require.NotContains(t, got.String(), "signing_key")
	require.NotContains(t, got.String(), sub.SigningKey)
}

// TestR371_TheSigningKeyIsStoredSealed asserts R-371 (and R-190): the key is
// held as the secrets adapter's ciphertext, nowhere in the clear, and
// rotating it returns a new one once.
func TestR371_TheSigningKeyIsStoredSealed(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	sub := i.subscribe(i.admin(), map[string]any{"events": []string{"*"}, "destination": "webhook", "url": hook.URL})

	var ciphertext []byte
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT ciphertext FROM subscription_secrets WHERE subscription_id = $1 AND field = 'signing_key'`, sub.ID).Scan(&ciphertext))
	require.NotEmpty(t, ciphertext)
	require.NotContains(t, string(ciphertext), sub.SigningKey)

	var row string
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT row_to_json(s)::text FROM subscriptions s WHERE id = $1`, sub.ID).Scan(&row))
	require.NotContains(t, row, sub.SigningKey)

	got := i.do(i.admin(), http.MethodPost, "/subscriptions/"+sub.ID+"/signing-key", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var rotated createdSub
	got.JSON(t, &rotated)
	require.NotEmpty(t, rotated.SigningKey)
	require.NotEqual(t, sub.SigningKey, rotated.SigningKey)
}

// TestR369_AFailedDeliveryIsRetriedAndCanBeSentAgain asserts R-369: a non-2xx
// answer is recorded as an attempt and retried later, and a delivery can be
// sent again by hand.
func TestR369_AFailedDeliveryIsRetriedAndCanBeSentAgain(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	hook.status.Store(http.StatusInternalServerError)

	sub := i.subscribe(i.admin(), map[string]any{"events": []string{"app.*"}, "destination": "webhook", "url": hook.URL})
	i.createApp(i.admin(), "billing")
	i.Dispatcher.Pass(context.Background())

	list := i.deliveries(i.admin(), sub.ID)
	require.Len(t, list, 1)
	require.Equal(t, "pending", list[0].Status, "a failure is retried, not given up on")
	require.Equal(t, 1, list[0].Attempts)
	require.Contains(t, list[0].LastError, "500")

	// Not due yet: the next pass leaves it alone.
	i.Dispatcher.Pass(context.Background())
	require.Equal(t, 1, hook.count())

	hook.status.Store(http.StatusNoContent)
	got := i.do(i.admin(), http.MethodPost, "/subscriptions/"+sub.ID+"/deliveries/"+list[0].ID+"/redeliver", nil)
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	i.Dispatcher.Pass(context.Background())

	got = i.do(i.admin(), http.MethodGet, "/subscriptions/"+sub.ID+"/deliveries/"+list[0].ID, nil)
	var d deliveryRow
	got.JSON(t, &d)
	require.Equal(t, "succeeded", d.Status)
	require.Len(t, d.AttemptLog, 2, "every attempt is kept")
	require.Equal(t, 500, *d.AttemptLog[0].StatusCode)
	require.Equal(t, 204, *d.AttemptLog[1].StatusCode)
}

// TestR370_AnEndpointFailingForADayIsTurnedOffAndItsOwnerTold asserts R-370.
func TestR370_AnEndpointFailingForADayIsTurnedOffAndItsOwnerTold(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	hook.status.Store(http.StatusBadGateway)

	sub := i.subscribe(i.admin(), map[string]any{"events": []string{"app.*"}, "destination": "webhook", "url": hook.URL})

	// A day of failures already behind it.
	_, err := i.db.Exec(context.Background(), `
		UPDATE subscriptions SET failing_since = now() - interval '25 hours', consecutive_failures = 4
		WHERE id = $1`, sub.ID)
	require.NoError(t, err)

	i.createApp(i.admin(), "billing")
	i.Dispatcher.Pass(context.Background())

	got := i.do(i.admin(), http.MethodGet, "/subscriptions/"+sub.ID, nil)
	var after createdSub
	got.JSON(t, &after)
	require.False(t, after.Enabled)
	require.Contains(t, after.Reason, "turned this subscription off")

	var told bool
	for _, n := range i.People.sent() {
		if n.Kind == adapterapi.NotifySubscriptionDisabled {
			told = true
			require.Equal(t, i.AdminID, n.Recipients[0].UserID)
		}
	}
	require.True(t, told, "the owner is told")

	// The turning-off is audited, and is itself an event.
	var events int
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT count(*) FROM events WHERE name = 'subscription.disabled'`).Scan(&events))
	require.Equal(t, 1, events)

	// Turning it on clears the record.
	got = i.do(i.admin(), http.MethodPatch, "/subscriptions/"+sub.ID, map[string]any{"enabled": true})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	got.JSON(t, &after)
	require.True(t, after.Enabled)
}

// TestR368_ASubscriptionIsWorthNoMoreThanItsOwnersAccess asserts R-368: an
// app subscription needs sight of the app, an install-wide one needs
// install.events.manage, and a delivery is checked against the owner's access
// when it is sent, not when the subscription was made.
func TestR368_ASubscriptionIsWorthNoMoreThanItsOwnersAccess(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	appID := i.createApp(i.admin(), "billing")
	ann := i.user("ann")

	// Install-wide needs the install verb.
	got := i.do(ann, http.MethodPost, "/subscriptions", map[string]any{
		"events": []string{"*"}, "destination": "webhook", "url": hook.URL,
	})
	require.Equal(t, http.StatusForbidden, got.Code, got.String())

	// An app she cannot see is not found.
	got = i.do(ann, http.MethodPost, "/subscriptions", map[string]any{
		"app_id": appID, "events": []string{"*"}, "destination": "webhook", "url": hook.URL,
	})
	require.Equal(t, http.StatusNotFound, got.Code, got.String())

	i.grantControl(appID, ann, "role_viewer")
	sub := i.subscribe(ann, map[string]any{
		"app_id": appID, "events": []string{"app.restarted"}, "destination": "webhook", "url": hook.URL,
	})

	// Nobody else sees it; an administrator with install.events.manage does.
	bob := i.user("bob")
	require.Equal(t, http.StatusNotFound, i.do(bob, http.MethodGet, "/subscriptions/"+sub.ID, nil).Code)
	require.Equal(t, http.StatusOK, i.do(i.admin(), http.MethodGet, "/subscriptions/"+sub.ID, nil).Code)

	// Her access goes, then the event happens.
	var grants struct {
		Grants []struct {
			ID          string `json:"id"`
			PrincipalID string `json:"principal_id"`
			Plane       string `json:"plane"`
		} `json:"grants"`
	}
	i.do(i.admin(), http.MethodGet, "/apps/"+appID+"/grants", nil).JSON(t, &grants)
	for _, g := range grants.Grants {
		if g.PrincipalID == i.userID(ann) {
			require.Equal(t, http.StatusNoContent, i.do(i.admin(), http.MethodDelete, "/apps/"+appID+"/grants/"+g.ID, nil).Code)
		}
	}
	require.NoError(t, audit.New(i.db.Pool).Write(context.Background(), audit.Event{
		PrincipalKind: audit.KindUser, PrincipalID: i.AdminID, Action: "app.restart", AppID: appID,
	}))
	i.Dispatcher.Pass(context.Background())

	require.Equal(t, 0, hook.count(), "nothing reaches someone who can no longer see the app")
	var status, reason string
	require.NoError(t, i.db.QueryRow(context.Background(),
		`SELECT status, last_error FROM event_deliveries WHERE subscription_id = $1`, sub.ID).Scan(&status, &reason))
	require.Equal(t, "failed", status)
	require.Contains(t, reason, "can no longer see this app")
}

// TestR364_ASubscriptionNamesOnlyCataloguedEvents asserts R-364: a filter
// that matches no event is refused rather than stored, and the catalog is
// served.
func TestR364_ASubscriptionNamesOnlyCataloguedEvents(t *testing.T) {
	i := newInstall(t)
	got := i.do(i.admin(), http.MethodPost, "/subscriptions", map[string]any{
		"events": []string{"deploy.fialed"}, "destination": "notify", "adapter_id": "ntf_channel",
	})
	require.Equal(t, http.StatusBadRequest, got.Code, got.String())
	require.Equal(t, "VALID_UNKNOWN_EVENT", got.ErrorCode())

	got = i.do(i.admin(), http.MethodGet, "/events", nil)
	require.Equal(t, http.StatusOK, got.Code)
	require.Contains(t, got.String(), `"deploy.failed"`)
	require.Contains(t, got.String(), `"ntf_channel"`)
}

// TestR374_AChannelSubscriptionSendsThroughItsAdapter asserts R-374 and R-365:
// a state change caught by a trigger reaches a channel adapter, laid out as a
// notification with the event's ID.
func TestR374_AChannelSubscriptionSendsThroughItsAdapter(t *testing.T) {
	i := newInstall(t)
	appID := i.createApp(i.admin(), "billing")
	i.subscribe(i.admin(), map[string]any{
		"app_id": appID, "events": []string{"app.state_changed"}, "destination": "notify", "adapter_id": "ntf_channel",
	})

	// An app's state changes without an audited action; the trigger tells it.
	_, err := i.db.Exec(context.Background(), `UPDATE apps SET state = 'degraded' WHERE id = $1`, appID)
	require.NoError(t, err)
	i.Dispatcher.Pass(context.Background())

	sent := i.Channel.sent()
	require.Len(t, sent, 1)
	require.Equal(t, "app.state_changed", string(sent[0].Kind))
	require.Contains(t, sent[0].Subject, "billing is degraded")
	require.NotEmpty(t, sent[0].EventID)
	require.Empty(t, sent[0].Recipients, "a channel gets no recipients")
}

// TestR266_ShareNotificationIsOffUnlessAPersonTurnsItOn asserts R-266 as
// amended and R-373: sharing an app sends nothing by default, and a person who
// turns it on for a channel gets it there; Pando's own notifications never go
// to a channel adapter.
func TestR266_ShareNotificationIsOffUnlessAPersonTurnsItOn(t *testing.T) {
	i := newInstall(t)
	appID := i.createApp(i.admin(), "billing")
	carol := i.user("carol")
	dave := i.user("dave")

	share := func(who *session) {
		got := i.do(i.admin(), http.MethodPost, "/apps/"+appID+"/grants", map[string]any{
			"plane": "data", "principal_kind": "user", "principal_id": i.userID(who),
		})
		require.Equal(t, http.StatusCreated, got.Code, got.String())
	}

	var view struct {
		Choices []struct {
			Kind    string `json:"kind"`
			Channel string `json:"channel"`
			Enabled bool   `json:"enabled"`
		} `json:"choices"`
	}
	i.do(carol, http.MethodGet, "/notification-preferences", nil).JSON(t, &view)
	for _, c := range view.Choices {
		require.NotEqual(t, "ntf_channel", c.Channel, "a channel is not a preference: it never gets personal notifications")
		if c.Kind == "app_shared" {
			require.False(t, c.Enabled, "off by default (R-266)")
		}
	}

	share(carol)
	require.Empty(t, i.People.sent())

	got := i.do(dave, http.MethodPut, "/notification-preferences", map[string]any{
		"choices": []map[string]any{{"kind": "app_shared", "channel": "ntf_people", "enabled": true}},
	})
	require.Equal(t, http.StatusOK, got.Code, got.String())
	share(dave)

	sent := i.People.sent()
	require.Len(t, sent, 1)
	require.Equal(t, adapterapi.NotifyAppShared, sent[0].Kind)
	require.Equal(t, i.userID(dave), sent[0].Recipients[0].UserID)
	require.Empty(t, i.Channel.sent())

	got = i.do(dave, http.MethodPut, "/notification-preferences", map[string]any{
		"choices": []map[string]any{{"kind": "no_such_kind", "channel": "ntf_people", "enabled": true}},
	})
	require.Equal(t, http.StatusBadRequest, got.Code)
}

// TestR367_ATestDeliveryGoesToOneSubscriptionOnly asserts R-367: a test is
// sent to the subscription it tests and to no other, whatever filters say.
func TestR367_ATestDeliveryGoesToOneSubscriptionOnly(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	a, b := newReceiver(t), newReceiver(t)
	// Neither filter names the test event; a test goes regardless.
	subA := i.subscribe(i.admin(), map[string]any{"events": []string{"deploy.failed"}, "destination": "webhook", "url": a.URL})
	i.subscribe(i.admin(), map[string]any{"events": []string{"deploy.failed"}, "destination": "webhook", "url": b.URL})

	got := i.do(i.admin(), http.MethodPost, "/subscriptions/"+subA.ID+"/test", nil)
	require.Equal(t, http.StatusAccepted, got.Code, got.String())
	i.Dispatcher.Pass(context.Background())

	require.Equal(t, 1, a.count())
	require.Equal(t, 0, b.count())
	require.Equal(t, "subscription.test", a.hits[0].Header.Get(subscription.HeaderEvent))
}

// TestR366_ADeliveryOutlivesAPandoThatStoppedMidSend asserts R-366: an event
// waits in the outbox until routed, and a delivery claimed by a Pando that
// stopped before recording an attempt is sent once its lease runs out — and
// not before, so two Pandos do not both send it.
func TestR366_ADeliveryOutlivesAPandoThatStoppedMidSend(t *testing.T) {
	ctx := context.Background()
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	sub := i.subscribe(i.admin(), map[string]any{"events": []string{"app.created"}, "destination": "webhook", "url": hook.URL})
	i.createApp(i.admin(), "billing")

	// The first Pando routes the event and claims the delivery, then stops.
	svc := i.Server.Subscriptions
	subs, err := svc.Subscriptions.List(ctx, state.SubscriptionFilter{EnabledOnly: true})
	require.NoError(t, err)
	_, err = svc.Events.Route(ctx, 100, time.Time{}, func(_ context.Context, e state.Event) ([]string, error) {
		return subscription.Matching(subs, e), nil
	})
	require.NoError(t, err)
	claimed, err := svc.Deliveries.Claim(ctx, time.Now().UTC(), 2*time.Minute, 10)
	require.NoError(t, err)
	require.Len(t, claimed, 1)

	// Another pass while the lease holds sends nothing.
	i.Dispatcher.Pass(ctx)
	require.Equal(t, 0, hook.count())

	// The lease runs out; the delivery goes, once.
	_, err = i.db.Exec(ctx, `UPDATE event_deliveries SET next_attempt_at = now() - interval '1 second' WHERE subscription_id = $1`, sub.ID)
	require.NoError(t, err)
	i.Dispatcher.Pass(ctx)
	i.Dispatcher.Pass(ctx)
	require.Equal(t, 1, hook.count())
	require.Equal(t, "succeeded", i.deliveries(i.admin(), sub.ID)[0].Status)
}
