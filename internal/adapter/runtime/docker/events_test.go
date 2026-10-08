package docker

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

func containerEvent(action events.Action, attrs map[string]string) events.Message {
	return events.Message{
		Type: events.ContainerEventType, Action: action,
		Actor:    events.Actor{ID: "c1", Attributes: attrs},
		TimeNano: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC).UnixNano(),
	}
}

var appAttrs = map[string]string{labelBundle: "app_01ABC", labelWorkload: "web", labelManaged: "true", "name": "web-1"}

// TestDaemonEventsAreTranslatedToBundleEvents pins the translation of the
// daemon's container events into what the reconciler acts on (O-52).
func TestDaemonEventsAreTranslatedToBundleEvents(t *testing.T) {
	cases := map[events.Action]api.BundleEventKind{
		events.ActionDie:                   api.BundleEventExited,
		events.ActionOOM:                   api.BundleEventOOMKilled,
		events.ActionStart:                 api.BundleEventStarted,
		events.ActionDestroy:               api.BundleEventRemoved,
		events.ActionHealthStatusUnhealthy: api.BundleEventHealth,
		events.ActionHealthStatusHealthy:   api.BundleEventHealth,
	}
	for action, want := range cases {
		got, ok := bundleEvent(containerEvent(action, appAttrs))
		require.True(t, ok, action)
		require.Equal(t, api.BundleEvent{
			Kind: want, BundleID: "app_01ABC", Workload: "web",
			At: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC),
		}, got, action)
	}

	seconds := containerEvent(events.ActionDie, appAttrs)
	seconds.TimeNano, seconds.Time = 0, 1700000000
	got, ok := bundleEvent(seconds)
	require.True(t, ok)
	require.Equal(t, time.Unix(1700000000, 0).UTC(), got.At)

	for name, m := range map[string]events.Message{
		"another action":    containerEvent(events.ActionPause, appAttrs),
		"no bundle label":   containerEvent(events.ActionDie, map[string]string{labelManaged: "true"}),
		"a trial container": containerEvent(events.ActionDie, map[string]string{labelBundle: "app_01ABC", labelTrial: "trial_1"}),
		"not a container":   {Type: events.NetworkEventType, Action: events.ActionDestroy, Actor: events.Actor{Attributes: appAttrs}},
	} {
		_, ok := bundleEvent(m)
		require.False(t, ok, name)
	}
}

// TestWatchBundlesFollowsTheDaemonsEventStream asserts that the adapter asks
// the daemon for Pando's container events only, says Watching once the stream
// is open, passes on each app event, and returns when the stream ends.
func TestWatchBundlesFollowsTheDaemonsEventStream(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	var query string
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("filters")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		enc := json.NewEncoder(w)
		_ = enc.Encode(containerEvent(events.ActionDie, appAttrs))
		_ = enc.Encode(containerEvent(events.ActionDie, map[string]string{labelManaged: "true"}))
		_ = enc.Encode(containerEvent(events.ActionStart, appAttrs))
		w.(http.Flusher).Flush()
	})

	var mu sync.Mutex
	var got []api.BundleEvent
	err := a.WatchBundles(context.Background(), func(e api.BundleEvent) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
	})
	require.Error(t, err, "the stream ending is reported")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, got, 3)
	require.Equal(t, api.BundleEventWatching, got[0].Kind)
	require.Equal(t, api.BundleEventExited, got[1].Kind)
	require.Equal(t, "app_01ABC", got[1].BundleID)
	require.Equal(t, api.BundleEventStarted, got[2].Kind)

	var filters map[string]map[string]bool
	require.NoError(t, json.Unmarshal([]byte(query), &filters))
	require.True(t, filters["label"][labelManaged+"=true"], "only Pando's containers")
	require.True(t, filters["type"]["container"])
	require.True(t, filters["event"]["die"])
	require.True(t, filters["event"]["oom"])
}

// TestWatchBundlesEndsWithItsContext asserts that canceling the watch is not
// reported as a lost stream.
func TestWatchBundlesEndsWithItsContext(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	err := a.WatchBundles(ctx, func(e api.BundleEvent) {
		if e.Kind == api.BundleEventWatching {
			cancel()
		}
	})
	require.ErrorIs(t, err, context.Canceled)
}

// TestWatchBundlesNeedsTheDaemon asserts that a daemon that does not answer
// is an error before anything is said to be watched.
func TestWatchBundlesNeedsTheDaemon(t *testing.T) {
	a := New()
	require.Error(t, a.WatchBundles(context.Background(), func(api.BundleEvent) {}), "not configured")

	require.NoError(t, a.Configure(context.Background(), []byte(`{"host":"tcp://127.0.0.1:1"}`)))
	called := false
	err := a.WatchBundles(context.Background(), func(api.BundleEvent) { called = true })
	require.Error(t, err)
	require.False(t, called)
}
