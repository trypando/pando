package observe

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
)

// selfRuntime counts what a runtime is asked about itself.
type selfRuntime struct {
	api.RuntimeAdapter
	caps, capacity atomic.Int32
}

func (s *selfRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	s.caps.Add(1)
	return api.RuntimeCapabilities{ReportsUsage: true}, nil
}

func (s *selfRuntime) Capacity(context.Context) (api.Capacity, error) {
	n := s.capacity.Add(1)
	return api.Capacity{TotalCPUMillis: int(n) * 1000}, nil
}

// What a runtime says about itself is asked once per runtime TTL however many
// usage panels poll, per runtime rather than per app, and asked again once the
// TTL has passed (issue #72).
func TestARuntimesCapabilitiesAndCapacityAreAskedOncePerTTL(t *testing.T) {
	c, clk := newCache()
	ctx := context.Background()
	rt, other := &selfRuntime{}, &selfRuntime{}

	for range 20 {
		caps, err := c.Capabilities(ctx, "rt_docker", rt)
		if err != nil || !caps.ReportsUsage {
			t.Fatalf("capabilities: %+v, %v", caps, err)
		}
		if _, err := c.Capacity(ctx, "rt_docker", rt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.Capabilities(ctx, "rt_k8s", other); err != nil {
		t.Fatal(err)
	}
	if got := rt.caps.Load(); got != 1 {
		t.Fatalf("capabilities asked %d times, want 1", got)
	}
	if got := rt.capacity.Load(); got != 1 {
		t.Fatalf("capacity asked %d times, want 1", got)
	}
	if got := other.caps.Load(); got != 1 {
		t.Fatalf("another runtime is its own answer, asked %d times", got)
	}

	// An app's Forget is about the app, not the runtime.
	c.Forget("app_1")
	if _, err := c.Capabilities(ctx, "rt_docker", rt); err != nil {
		t.Fatal(err)
	}
	if got := rt.caps.Load(); got != 1 {
		t.Fatalf("forgetting an app dropped the runtime's capabilities")
	}

	clk.Advance(DefaultRuntimeTTL + time.Second)
	capacity, err := c.Capacity(ctx, "rt_docker", rt)
	if err != nil {
		t.Fatal(err)
	}
	if capacity.TotalCPUMillis != 2000 {
		t.Fatalf("capacity after the TTL = %d, want a new reading", capacity.TotalCPUMillis)
	}
}

// A nil cache asks the runtime every time, as Observe and Usage do.
func TestANilCacheAsksTheRuntimeItself(t *testing.T) {
	var c *Cache
	rt := &selfRuntime{}
	for range 2 {
		if _, err := c.Capabilities(context.Background(), "rt", rt); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Capacity(context.Background(), "rt", rt); err != nil {
			t.Fatal(err)
		}
	}
	if rt.caps.Load() != 2 || rt.capacity.Load() != 2 {
		t.Fatalf("asked %d and %d times, want 2 each", rt.caps.Load(), rt.capacity.Load())
	}
	if New(WithRuntimeTTL(time.Minute)).runtimeTTL != time.Minute {
		t.Fatal("WithRuntimeTTL")
	}
}
