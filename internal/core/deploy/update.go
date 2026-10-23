package deploy

import (
	"context"
	"fmt"

	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// ImageCheck is what a check of an image app's tag found (R-143a).
type ImageCheck struct {
	Image string `json:"image"`
	// Current is the digest the pinned revision runs; empty when it names none.
	Current string `json:"current_digest"`
	// Latest is the digest the tag names now.
	Latest    string `json:"latest_digest"`
	Available bool   `json:"update_available"`
	// Revision is the pinned revision that was checked.
	Revision int `json:"revision"`
}

// CheckImage reports whether the tag the app's pinned revision names now
// points at a different build. It writes nothing (R-143a), so a newer saved
// revision does not stop it; only the upgrade would bury one.
func (r *Runner) CheckImage(ctx context.Context, app state.App) (ImageCheck, error) {
	rev, err := r.pinned(ctx, app)
	if err != nil {
		return ImageCheck{}, err
	}
	check, err := checkImage(ctx, r.images, app.ID, rev.Body)
	check.Revision = rev.Revision
	return check, err
}

// UpgradeImage writes a revision that runs digest, the build a check found
// (R-143a). Deploying it is the caller's next step, through the ordinary path.
func (r *Runner) UpgradeImage(ctx context.Context, app state.App, digest, by string) (state.Revision, error) {
	rev, err := r.pinnedOnly(ctx, app)
	if err != nil {
		return state.Revision{}, err
	}
	upgraded, err := upgradedSpec(ctx, r.images, app.ID, rev.Body, digest)
	if err != nil {
		return state.Revision{}, err
	}
	return r.apps.CreateRevision(ctx, app.ID, upgraded, spec.OriginEdited, by)
}

// pinnedOnly is the app's pinned revision, refused when a newer one is saved
// and not deployed: an upgrade on top of the pinned one would bury that edit.
func (r *Runner) pinnedOnly(ctx context.Context, app state.App) (state.Revision, error) {
	pinned, err := r.pinned(ctx, app)
	if err != nil {
		return state.Revision{}, err
	}
	// Newest first, and without bodies: only the numbers are compared.
	revs, err := r.apps.ListRevisions(ctx, app.ID)
	if err != nil {
		return state.Revision{}, err
	}
	if len(revs) > 0 && revs[0].Revision > pinned.Revision {
		return state.Revision{}, errs.Newf(errs.StateInvalid,
			"This app has changes that are not deployed yet (revision %d).", revs[0].Revision).
			WithRemedy(fmt.Sprintf("Deploy revision %d, or wait for its approval if it is waiting for one, "+
				"then check for an update again.", revs[0].Revision))
	}
	return pinned, nil
}

// pinned is the app's pinned revision, with its body.
func (r *Runner) pinned(ctx context.Context, app state.App) (state.Revision, error) {
	if app.PinnedSpecID == "" {
		return state.Revision{}, errs.New(errs.StateInvalid, "This app has not been deployed yet, so there is no image to check.")
	}
	rev, found, err := r.apps.RevisionByID(ctx, app.PinnedSpecID)
	if err != nil {
		return state.Revision{}, err
	}
	if !found {
		return state.Revision{}, errs.New(errs.NotFound, "This app's pinned spec is missing.")
	}
	return rev, nil
}

func checkImage(ctx context.Context, images *oci.Images, appID string, s *spec.AppSpec) (ImageCheck, error) {
	if s.Source.Type != spec.SourceImage || s.Source.Image == "" {
		return ImageCheck{}, errs.New(errs.StateInvalid,
			"Only an app deployed from a published image can be checked for a newer build.")
	}
	if images == nil {
		return ImageCheck{}, errs.New(errs.AdapterUnavailable,
			"Pando cannot read image registries on this server, so it cannot check for a newer build.")
	}
	latest, err := tagDigest(ctx, images, appID, s)
	if err != nil {
		return ImageCheck{}, err
	}
	return ImageCheck{
		Image:     s.Source.Image,
		Current:   s.Source.Digest,
		Latest:    latest,
		Available: latest != s.Source.Digest,
	}, nil
}

// upgradedSpec is s pinned to digest, refused unless digest is still what the
// tag names: the person upgrades to the build they were shown, not a later one.
func upgradedSpec(ctx context.Context, images *oci.Images, appID string, s *spec.AppSpec, digest string) (*spec.AppSpec, error) {
	if digest == "" {
		return nil, errs.New(errs.ValidInvalid, "An upgrade needs the digest the update check returned, as latest_digest.")
	}
	check, err := checkImage(ctx, images, appID, s)
	if err != nil {
		return nil, err
	}
	if !check.Available {
		return nil, errs.Newf(errs.StateInvalid, "This app already runs the newest build of %s.", check.Image)
	}
	if check.Latest != digest {
		return nil, errs.Newf(errs.StateInvalid, "The image %s has changed since it was checked.", check.Image).
			WithRemedy("Check for an update again, then upgrade to the build it finds.")
	}
	return withDigest(s, digest), nil
}
