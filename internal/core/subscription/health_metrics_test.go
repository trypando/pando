package subscription

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/telemetry"
)

// TestR399_AdapterHealthIsTheLeadersLastRound asserts the adapter health
// gauge's source: every configured adapter from the last round of checks,
// healthy unless its check failed, and nothing once that round is older than
// the replica would still be checking — a replica that stopped leading must
// not keep reporting what it last saw.
func TestR399_AdapterHealthIsTheLeadersLastRound(t *testing.T) {
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("ntf_console", &recorder{kind: "console", audience: api.AudiencePeople}))
	require.NoError(t, reg.Register("ntf_slack", &recorder{kind: "slack", audience: api.AudienceChannel}))
	now := clock.NewFake(time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC))
	d := &Dispatcher{Registry: reg, Clock: now}

	require.Nil(t, d.AdapterHealth(time.Minute), "no round yet, nothing to report")

	// A round where every adapter passes says nothing as an event, so needs no
	// event store, and is still recorded for the gauge.
	d.CheckAdapters(context.Background())
	require.ElementsMatch(t, []telemetry.AdapterHealth{
		{Category: string(api.CategoryNotify), ID: "ntf_console", Healthy: true},
		{Category: string(api.CategoryNotify), ID: "ntf_slack", Healthy: true},
	}, d.AdapterHealth(time.Minute))

	d.recordHealth(map[string]error{"ntf_slack": errors.New("the webhook answered 500")})
	require.ElementsMatch(t, []telemetry.AdapterHealth{
		{Category: string(api.CategoryNotify), ID: "ntf_console", Healthy: true},
		{Category: string(api.CategoryNotify), ID: "ntf_slack", Healthy: false},
	}, d.AdapterHealth(time.Minute))

	now.Advance(2 * time.Minute)
	require.Nil(t, d.AdapterHealth(time.Minute), "a stale round is not reported")
}
