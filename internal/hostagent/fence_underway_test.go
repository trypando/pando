package hostagent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/liveness"
)

// heldFencer is a fence whose stop takes until release is closed.
type heldFencer struct {
	began   chan struct{}
	release chan struct{}
}

func (f *heldFencer) Fence(ctx context.Context) (int, error) {
	select {
	case f.began <- struct{}{}:
	default:
	}
	select {
	case <-f.release:
		return 1, nil
	case <-ctx.Done():
		return 0, ctx.Err()
	}
}

// TestR421_AnAgentStartsNothingWhileItsFenceIsUnderWay asserts R-421 on a
// joined host: from the moment its Monitor decides to fence, through the
// stopping of its containers, its agent refuses every runtime call that
// would create or start one. A container started while the fence stops the
// others would run on beside the standby that replaces it.
func TestR421_AnAgentStartsNothingWhileItsFenceIsUnderWay(t *testing.T) {
	as := testAuthorities(t)
	ctx := context.Background()
	sock, started := dockerish(t)

	clk := clock.NewFake(time.Time{})
	fencer := &heldFencer{began: make(chan struct{}, 1), release: make(chan struct{})}
	mon := &liveness.Monitor{
		Self: "self",
		Peers: func(context.Context) ([]liveness.Peer, error) {
			return []liveness.Peer{{ID: "a"}, {ID: "b"}, {ID: "c"}, {ID: "d"}}, nil
		},
		Probe:  func(context.Context, liveness.Peer) ([]byte, error) { return nil, errors.New("no route to host") },
		Fencer: fencer,
		Clock:  clk,
	}
	addr := agentOn(ctx, t, as, &Agent{RuntimeSocket: sock, Fence: mon})
	tlsConfig := clientFor(t, as)
	hc := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return DialRuntime(ctx, addr, tlsConfig) },
	}}
	t.Cleanup(hc.CloseIdleConnections)
	post := func(path string) int {
		resp, err := hc.Post("http://docker"+path, "application/json", strings.NewReader("{}"))
		require.NoError(t, err)
		defer resp.Body.Close()
		_, _ = io.ReadAll(resp.Body)
		return resp.StatusCode
	}

	// Cut off for a whole Period: the fence is decided, and its stop begins.
	period := liveness.Config{}.Normalize()
	for start := clk.Now(); clk.Now().Sub(start) <= period.Period; {
		mon.Round(ctx)
		clk.Advance(period.Interval)
	}
	<-fencer.began
	require.False(t, mon.Fenced(), "the stop is still under way")

	require.Equal(t, http.StatusConflict, post("/v1.47/containers/create"), "refused from the decision on")
	require.Equal(t, http.StatusConflict, post("/containers/abc/start"))
	require.Zero(t, started.Load())

	close(fencer.release)
	mon.Settle()
	require.True(t, mon.Fenced())
	require.Equal(t, http.StatusConflict, post("/containers/abc/start"), "and once it has stopped them")
	require.Zero(t, started.Load())
}
