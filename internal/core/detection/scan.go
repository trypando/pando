package detection

import (
	"context"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

// scanRequest is what detection scans (R-311): what the app will deploy, as far
// as it exists before anything is built.
//
// An app that runs a published image (an image app, or a repository whose
// maintainer publishes one, R-094 tier 1) has its image already. That image is
// scanned by the digest its tag names now, with the checkout when there is
// one: the scan a deploy of the same digest reuses (design 09 §4.1), so the
// plan shows the score of what will run. An app Pando builds has no image until
// it is built, which detection deliberately does not do. Its source is scanned
// alone, and that number is the source's, not the app's, until the deploy
// scans the image (security.AwaitsImage).
//
// False when there is nothing to scan: an image app whose digest could not be
// read has no checkout, and scanning "" would scan whatever directory the
// server runs in. A tag is not scanned in place of a digest, since the name
// says nothing about what a deploy will pull.
func (r *Runner) scanRequest(ctx context.Context, appID string, src spec.Source,
	proposal *detect.Proposal, checkout *source.Checkout,
) (api.ScanRequest, bool) {
	req := api.ScanRequest{AppID: appID, SourceDir: checkout.Dir, Commit: checkout.Identity()}

	draft := &proposal.DraftSpec
	if src.Type == spec.SourceImage {
		// The digest detection pinned, which the revision runs: the same
		// reference a deploy pulls and scans.
		if draft.Source.Digest != "" {
			req.Commit = draft.Source.Digest
			req.Image = oci.Reference(draft.Source)
		}
	} else if published := oci.Published(draft); published != "" {
		req.Image = r.pinPublished(ctx, appID, published)
	}
	if req.Image != "" {
		req.PullAuth = r.pullAuth(ctx, appID, req.Image)
	}
	return req, req.SourceDir != "" || req.Image != ""
}

// pinPublished is a repository's published image pinned to the digest its
// reference names now, the way a deploy pins it, so a deploy that finds the
// same digest reuses this scan. Empty when the registry cannot be read: the
// source is still scanned, and the deploy scans the image.
func (r *Runner) pinPublished(ctx context.Context, appID, ref string) string {
	if strings.Contains(ref, "@sha256:") {
		return ref
	}
	if r.Job == nil || r.Job.Images == nil {
		return ""
	}
	insp, err := r.Job.Images.Inspect(ctx, appID, ref, oci.Platform{})
	if err != nil || insp.Digest == "" {
		return ""
	}
	return oci.Pin(ref, insp.Digest)
}

// pullAuth is the app's registry credential for an image, when it has one.
func (r *Runner) pullAuth(ctx context.Context, appID, ref string) *api.RegistryAuth {
	if r.Job == nil || r.Job.Images == nil {
		return nil
	}
	a, err := r.Job.Images.Auth(ctx, appID, ref)
	if err != nil || a == nil {
		return nil
	}
	return &api.RegistryAuth{Registry: a.Registry, Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
}
