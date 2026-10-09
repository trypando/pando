//go:build integration

package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/detection"
	"github.com/trypando/pando/internal/core/subscription"
)

// TestR399_TheGaugesReadTheRunningServer asserts what serve hands the gauges:
// the real pool's connections and limit, the detection queue's running count,
// and adapter health only once the leader has checked.
func TestR399_TheGaugesReadTheRunningServer(t *testing.T) {
	db := connected(t)
	sources := metricsSources(db, &detection.Queue{}, &subscription.Dispatcher{}, 5*time.Minute)

	used, idle, limit := sources.Pool()
	require.GreaterOrEqual(t, used, int64(0))
	require.GreaterOrEqual(t, idle, int64(0))
	require.Equal(t, int64(db.Stat().MaxConns()), limit)
	require.Zero(t, sources.Detections(), "nothing is being detected")
	require.Nil(t, sources.Adapters(), "this replica has not checked, so it reports nothing")
}
