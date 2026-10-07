//go:build integration

package httpapi_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestR365_TheRunningDispatcherDeliversWhatIsAuditedWithoutBeingAsked asserts
// the dispatcher as the server runs it (issue #72): routing and sending are
// separate loops, and an audited action reaches its webhook with nobody
// calling Pass. The subscription list is cached, as it is in production.
func TestR365_TheRunningDispatcherDeliversWhatIsAuditedWithoutBeingAsked(t *testing.T) {
	i := newInstall(t)
	i.allowPrivateWebhooks()
	hook := newReceiver(t)
	sub := i.subscribe(i.admin(), map[string]any{
		"events": []string{"app.created"}, "destination": "webhook", "url": hook.URL,
	})

	// Cached as in production, briefly: an event newer than the cached list
	// waits for the next list (R-367), so this is how long it may wait.
	i.Dispatcher.SubscriptionCache = 200 * time.Millisecond
	i.Dispatcher.Interval = 20 * time.Millisecond
	i.Dispatcher.Senders = 2
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		i.Dispatcher.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	appID := i.createApp(i.admin(), "billing")
	require.NotEmpty(t, appID)
	require.Eventually(t, func() bool { return hook.count() == 1 }, 30*time.Second, 20*time.Millisecond,
		"the webhook is posted to")
	require.Eventually(t, func() bool {
		list := i.deliveries(i.admin(), sub.ID)
		return len(list) == 1 && list[0].Status == "succeeded"
	}, 10*time.Second, 20*time.Millisecond, "and the delivery is recorded as sent")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the dispatcher did not stop")
	}
	require.Equal(t, 1, hook.count(), "sent once")
}
