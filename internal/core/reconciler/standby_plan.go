package reconciler

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Shape is what the reconciler compares one app against now — its pinned
// spec's shape and its provisioned services, without environment — for
// something other than the loop that must start it the same way: its warm
// standby, made and promoted on another host (R-419). Complete adds the rest.
// Building it here keeps one definition of what an app is started with.
func (r *Reconciler) Shape(ctx context.Context, appID string) (api.BundlePlan, *spec.AppSpec, error) {
	app, found, err := r.Reconciles.ForApp(ctx, appID)
	if err != nil {
		return api.BundlePlan{}, nil, err
	}
	if !found {
		return api.BundlePlan{}, nil, errs.New(errs.NotFound, "This app has no deployed configuration to start.")
	}
	rev, found, err := r.Apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil {
		return api.BundlePlan{}, nil, err
	}
	if !found {
		return api.BundlePlan{}, nil, errs.New(errs.NotFound, "This app's deployed configuration could not be found.")
	}
	s := rev.Body
	want, _, err := r.desired(ctx, app, s, api.ObservedBundle{})
	if err != nil {
		return api.BundlePlan{}, nil, err
	}
	return want, s, nil
}
