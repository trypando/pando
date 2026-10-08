package syslog

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
)

// TestR382_ASendCanceledBeforeItStartsSendsNothing asserts a canceled send is
// not delivered, so core counts nothing and sends the batch again.
func TestR382_ASendCanceledBeforeItStartsSendsNothing(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer func() { _ = ln.Close() }()
	accepted := make(chan struct{}, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- struct{}{}
			_ = c.Close()
		}
	}()

	a := configured(t, map[string]any{"address": ln.Addr().String(), "tls": "off"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = a.Send(ctx, api.AuditBatch{Events: []json.RawMessage{json.RawMessage(`{"id":1}`)}, IDs: []int64{1}, Actions: []string{"test.one"}})
	require.ErrorIs(t, err, context.Canceled)
	select {
	case <-accepted:
		t.Fatal("a canceled send connected to the collector")
	case <-time.After(200 * time.Millisecond):
	}
}
