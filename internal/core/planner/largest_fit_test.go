package planner_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
)

// TestR242_AnAppNoSinglePlaceHasCPUForIsRefused asserts R-242's per-place
// check for CPU: memory fits where the app may go, CPU does not, and the
// refusal names CPU. A policy that allows CPU oversubscription lifts it.
func TestR242_AnAppNoSinglePlaceHasCPUForIsRefused(t *testing.T) {
	rt := capableRuntime()
	rt.largestFit = &api.Fit{CPUMillis: 250, MemoryBytes: 8 << 30}
	s := plannableSpec() // 500 millicores

	p := planner.New(registry(t, rt, capableRouting(), capableBuilder()), policy.Static(policy.Default()), fixedAllocations{})
	_, err := p.Check(context.Background(), s)
	require.Equal(t, errs.CapacityWouldOversubscribe, errs.CodeOf(err))
	e := errs.As(err)
	require.Equal(t, "CPU", e.Details["resource"])
	require.Contains(t, e.Message, "enough CPU free")

	doc := policy.Default()
	doc.AllowCPUOversubscription = true
	p = planner.New(registry(t, rt, capableRouting(), capableBuilder()), policy.Static(doc), fixedAllocations{})
	_, err = p.Check(context.Background(), s)
	require.NoError(t, err)
}

// failingFit is a runtime that cannot say how much room is left.
type failingFit struct{ *fakeRuntime }

func (failingFit) LargestFitFor(context.Context, string) (*api.Fit, error) {
	return nil, errors.New("connection refused")
}

func TestR242_ARuntimeThatCannotSayWhereThereIsRoomIsUnavailable(t *testing.T) {
	p := planner.New(registry(t, failingFit{capableRuntime()}, capableRouting(), capableBuilder()),
		policy.Static(policy.Default()), fixedAllocations{})
	_, err := p.Check(context.Background(), plannableSpec())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "how much room is left")
}
