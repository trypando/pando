package planner_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
)

// scopedRuntime answers Capacity and LargestFitFor from one reading per read
// scope, as the Kubernetes runtime does, and counts its readings.
type scopedRuntime struct {
	*fakeRuntime
	readings *int
}

type readingKey struct{}

func (s scopedRuntime) reading(ctx context.Context) (int, error) {
	return api.ScopedRead(ctx, readingKey{}, func() (int, error) { *s.readings++; return *s.readings, nil })
}

func (s scopedRuntime) Capacity(ctx context.Context) (api.Capacity, error) {
	if _, err := s.reading(ctx); err != nil {
		return api.Capacity{}, err
	}
	return s.fakeRuntime.Capacity(ctx)
}

func (s scopedRuntime) LargestFitFor(ctx context.Context, appID string) (*api.Fit, error) {
	if _, err := s.reading(ctx); err != nil {
		return nil, err
	}
	return s.fakeRuntime.LargestFitFor(ctx, appID)
}

// TestR242_APlanReadsTheRuntimesRoomOnceAndAfreshEachTime asserts the
// capacity check asks Capacity and LargestFitFor within one read scope, so a
// runtime reads its room once per plan, and that the next plan reads again
// rather than reuse the last plan's reading.
func TestR242_APlanReadsTheRuntimesRoomOnceAndAfreshEachTime(t *testing.T) {
	readings := 0
	rt := scopedRuntime{capableRuntime(), &readings}
	p := planner.New(registry(t, rt, capableRouting(), capableBuilder()), policy.Static(policy.Default()), fixedAllocations{})

	_, err := p.Check(context.Background(), plannableSpec())
	require.NoError(t, err)
	require.Equal(t, 1, readings)
	_, err = p.Check(context.Background(), plannableSpec())
	require.NoError(t, err)
	require.Equal(t, 2, readings)
}
