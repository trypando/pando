package hostagent

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"
)

// TestR421_AnyAnswerFromTheMachineIsContact asserts R-421's "a host that is
// not cut off is never taken for lost": a ping that gets no view back still
// says the machine answered when it did — the connection refused (its Pando
// process is down), the handshake failed, the ping refused, or anything else
// read back — and only silence is not contact.
func TestR421_AnyAnswerFromTheMachineIsContact(t *testing.T) {
	ctx := context.Background()
	addr, as := pingAgent(t, &fakeLiveness{})
	_, err := Ping(ctx, addr, hostTLS(t, as), peerID)
	require.True(t, Contacted(err), "a view")

	// Nobody listening: the machine resets the connection.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closed := ln.Addr().String()
	require.NoError(t, ln.Close())
	_, err = Ping(ctx, closed, hostTLS(t, as), peerID)
	require.Error(t, err)
	require.True(t, Contacted(err), "refused: %v", err)
	var pe *PingError
	require.ErrorAs(t, err, &pe)
	require.Equal(t, closed, pe.Addr)

	// The ping refused, a handshake that fails, an odd answer.
	refusing, as2 := pingAgent(t, nil)
	_, err = Ping(ctx, refusing, hostTLS(t, as2), peerID)
	require.ErrorContains(t, err, "takes no part")
	require.True(t, Contacted(err))
	_, err = Ping(ctx, refusing, hostTLS(t, as), peerID)
	require.Error(t, err, "another authority's certificate")
	require.True(t, Contacted(err))
	odd, cfg := fakeAgentAnswering(t, "HELLO\n")
	_, err = Ping(ctx, odd, cfg, peerID)
	require.True(t, Contacted(err))

	// A machine that took the connection and says nothing is still a machine
	// that is up.
	mute, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = mute.Close() })
	tctx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()
	_, err = Ping(tctx, mute.Addr().String(), hostTLS(t, as), peerID)
	require.True(t, Contacted(err))

	// Silence: a dial that times out, and a canceled one.
	gone, cancelGone := context.WithCancel(ctx)
	cancelGone()
	_, err = Ping(gone, "192.0.2.1:7443", hostTLS(t, as), peerID)
	require.Error(t, err)
	require.False(t, Contacted(err), "a dial that never got an answer: %v", err)
	require.False(t, Contacted(errors.New("plain")))
	require.False(t, Contacted(fmt.Errorf("wrapped: %w", &PingError{Err: errors.New("timeout")})))
	require.Equal(t, "timeout", (&PingError{Err: errors.New("timeout")}).Error())
}

// TestR421_ManySlowContainersFenceWithinTheBound asserts R-421's timing: a
// host running twenty app containers that each take their whole grace to
// stop has stopped them all in about one grace, not twenty, far inside the
// two periods before their standbys start.
func TestR421_ManySlowContainersFenceWithinTheBound(t *testing.T) {
	d := &fakeDocker{delay: 300 * time.Millisecond}
	for i := range 20 {
		d.items = append(d.items, container.Summary{
			ID: fmt.Sprintf("c%d", i), Labels: map[string]string{labelManaged: "true", labelApp: fmt.Sprintf("app_%d", i)},
		})
	}
	start := time.Now()
	n, err := DockerFencer{Docker: d}.Fence(context.Background())
	require.NoError(t, err)
	require.Equal(t, 20, n)
	require.Less(t, time.Since(start), 3*time.Second, "stopped at once, not one after another")
	require.Less(t, fenceStopSeconds, 10)
}
