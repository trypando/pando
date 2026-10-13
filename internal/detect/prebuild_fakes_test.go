package detect_test

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
)

// The fake planners plan no build ahead of a Dockerfile.

func (failingPlanner) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}

func (plannerWithoutCMD) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}

func (plannerExposing80) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}

func (plannerReportingStart) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}

func (plannerWritingCMD) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}

func (plannerRunningMake) PlanPrebuild(context.Context, api.SourceView, string) (api.BuildPlan, error) {
	return api.BuildPlan{}, nil
}
