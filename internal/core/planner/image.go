package planner

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// ImageReader reads an app's image from its registry with the app's own
// credential (oci.Images).
type ImageReader interface {
	Inspect(ctx context.Context, appID, reference string, want oci.Platform) (oci.Inspection, error)
}

// WithImages enables the plan-time checks on an image app's image: that it
// has a build for the runtime's platform, and which of the paths it keeps
// data in the spec leaves without storage (issue #41).
func (p *Planner) WithImages(images ImageReader) *Planner {
	p.images = images
	return p
}

// checkImage refuses an image with no build for the runtime's platform before
// anything is pulled, where otherwise it would start and exit with "exec
// format error" — or, on a host with emulation, run at a fraction of its speed
// with nothing saying why. It also notes any path the image declares with
// VOLUME that the spec gives no storage, which is R-203's silent loss: a note
// and never a blocker, since the app's author may know the path holds nothing.
//
// A registry that cannot be read is not a refusal here: the pull will say so
// with its own reason, and an unreachable registry at plan time is often a
// network blip by deploy time.
func (p *Planner) checkImage(ctx context.Context, s *spec.AppSpec, caps api.RuntimeCapabilities) ([]string, error) {
	if p.images == nil || s.Source.Type != spec.SourceImage || s.Source.Image == "" {
		return nil, nil
	}
	host, known := oci.ParsePlatform(caps.Platform)
	insp, err := p.images.Inspect(ctx, s.AppID, oci.Reference(s.Source), host)
	if err != nil {
		return []string{"Pando could not read the image from its registry to check it before deploying: " + errs.As(err).Message}, nil
	}
	if known && !insp.Supports(host) {
		return nil, oci.PlatformMismatch(s.Source.Image, host, insp.Platforms)
	}
	if insp.Config == nil {
		return nil, nil
	}
	return unkeptVolumes(s, insp.Config.Volumes), nil
}

// unkeptVolumes names image VOLUME paths the primary workload mounts nothing
// at or above.
func unkeptVolumes(s *spec.AppSpec, declared []string) []string {
	primary, ok := s.PrimaryWorkload()
	if !ok {
		return nil
	}
	var missing []string
	for _, v := range declared {
		clean := path.Clean(v)
		kept := false
		for _, m := range primary.Mounts {
			mp := path.Clean(m.Path)
			if clean == mp || strings.HasPrefix(clean, mp+"/") {
				kept = true
				break
			}
		}
		if !kept {
			missing = append(missing, clean)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return []string{fmt.Sprintf(
		"The image keeps data in %s, and this app has no storage there. Anything written there is "+
			"discarded the next time the app is deployed, while the app keeps reporting itself healthy. "+
			"Add a volume for it if it holds data you need.", strings.Join(missing, ", "))}
}
