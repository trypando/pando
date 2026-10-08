package docker

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// watchedActions are the container events that can change what Observe would
// report about a bundle (O-52). "health_status" matches every health_status
// event: the daemon matches a filter without a colon against the part of the
// action before it.
var watchedActions = []string{
	string(events.ActionStart),
	string(events.ActionDie),
	string(events.ActionOOM),
	string(events.ActionDestroy),
	string(events.ActionHealthStatus),
}

// WatchBundles follows the daemon's event stream for Pando's containers
// (O-52). It reports what happened and does nothing about it (R-148).
//
// One stream per daemon, filtered by the daemon to containers carrying
// Pando's label, so a host running other work sends nothing for it. It
// returns when the stream ends: the daemon restarting, the connection
// dropping. Reconnecting is the caller's, which then looks at every app
// again, since nothing between the two streams was seen.
func (a *Adapter) WatchBundles(ctx context.Context, sink func(api.BundleEvent)) error {
	if a.cli == nil {
		return errs.New(errs.AdapterUnavailable, "The Docker runtime has not been set up.")
	}
	// Events returns before the daemon answers, so a daemon that is down
	// would read as a stream that opened and closed at once. Asking first is
	// what makes BundleEventWatching mean the stream is open.
	if _, err := a.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not reach the Docker daemon to follow its events.", err)
	}

	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	res := a.cli.Events(streamCtx, client.EventsListOptions{
		Filters: make(client.Filters).
			Add("type", string(events.ContainerEventType)).
			Add("label", labelManaged+"=true").
			Add("event", watchedActions...),
	})
	sink(api.BundleEvent{Kind: api.BundleEventWatching})

	for {
		select {
		case m := <-res.Messages:
			if ev, ok := bundleEvent(m); ok {
				sink(ev)
			}
		case err := <-res.Err:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				err = errors.New("the event stream ended")
			}
			return errs.Wrap(errs.AdapterUnavailable, "Lost the Docker daemon's event stream.", err)
		}
	}
}

// bundleEvent translates one daemon event, or reports false for one that is
// not about an app's workload: a trial run's container, the edge, anything
// without a bundle label.
func bundleEvent(m events.Message) (api.BundleEvent, bool) {
	if m.Type != events.ContainerEventType {
		return api.BundleEvent{}, false
	}
	attrs := m.Actor.Attributes
	bundle := attrs[labelBundle]
	if bundle == "" || attrs[labelTrial] != "" {
		return api.BundleEvent{}, false
	}

	var kind api.BundleEventKind
	switch {
	case m.Action == events.ActionDie:
		kind = api.BundleEventExited
	case m.Action == events.ActionOOM:
		kind = api.BundleEventOOMKilled
	case m.Action == events.ActionStart:
		kind = api.BundleEventStarted
	case m.Action == events.ActionDestroy:
		kind = api.BundleEventRemoved
	case strings.HasPrefix(string(m.Action), string(events.ActionHealthStatus)):
		kind = api.BundleEventHealth
	default:
		return api.BundleEvent{}, false
	}

	ev := api.BundleEvent{Kind: kind, BundleID: bundle, Workload: attrs[labelWorkload]}
	switch {
	case m.TimeNano != 0:
		ev.At = time.Unix(0, m.TimeNano).UTC()
	case m.Time != 0:
		ev.At = time.Unix(m.Time, 0).UTC()
	}
	return ev, true
}
