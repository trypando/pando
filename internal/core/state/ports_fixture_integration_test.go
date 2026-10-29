//go:build integration

package state_test

import "sync/atomic"

// lastPort hands out fixture ports.
var lastPort atomic.Int32

func init() { lastPort.Store(20000) }

// testPort is a port no other fixture app has. Pinning claims a port-mode
// app's port, and refuses one another app holds (issue #348).
func testPort() int { return int(lastPort.Add(1)) }
