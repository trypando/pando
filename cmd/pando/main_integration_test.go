//go:build integration

package main

import (
	"os"
	"testing"
)

// TestMain keeps these tests from starting a real BuildKit. With no address,
// the BuildKit builder starts Pando's own on the Docker daemon (issue #130),
// and a test that wires the server up would leave one running on the
// developer's machine, published on its loopback. An address nothing listens
// on keeps the builder configured and unhealthy, which is all these tests
// need of it; the managed path has its own tests in the builder's package.
func TestMain(m *testing.M) {
	if os.Getenv("PANDO_BUILDKIT_ADDRESS") == "" {
		_ = os.Setenv("PANDO_BUILDKIT_ADDRESS", "tcp://127.0.0.1:1")
	}
	os.Exit(m.Run())
}
