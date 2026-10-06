package detect

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// ImageReader reads an app's image from its registry, with the app's own
// registry credential (oci.Images).
type ImageReader interface {
	Inspect(ctx context.Context, appID, reference string, want oci.Platform) (oci.Inspection, error)
	Auth(ctx context.Context, appID, reference string) (*oci.Auth, error)
}

// readImage asks the registry about the image before anything runs (issue
// #41). It pins the digest the tag names now, and returns what the image's
// configuration declares for the host's platform.
//
// A refusal the user can act on — the image does not exist, it needs a
// credential, it has no build for this host — blocks the proposal with the
// reason. A registry that does not answer does not: the trial run pulls the
// image anyway, and says so itself if it cannot.
func (j *Job) readImage(ctx context.Context, appID string, src spec.Source) (spec.Source, *oci.Inspection, error) {
	if j.Images == nil {
		return src, nil, nil
	}
	host := j.hostPlatform(ctx)
	insp, err := j.Images.Inspect(ctx, appID, src.Image, host)
	if err != nil {
		if errs.CodeOf(err) == errs.ValidInvalid {
			return src, nil, err
		}
		return src, nil, nil
	}
	src.Digest = insp.Digest
	if host.OS != "" && !insp.Supports(host) {
		return src, &insp, oci.PlatformMismatch(src.Image, host, insp.Platforms)
	}
	return src, &insp, nil
}

// hostPlatform is what the runtime runs images for, zero when it cannot say.
func (j *Job) hostPlatform(ctx context.Context) oci.Platform {
	if j.Runtime == nil {
		return oci.Platform{}
	}
	caps, err := j.Runtime.Capabilities(ctx)
	if err != nil {
		return oci.Platform{}
	}
	p, _ := oci.ParsePlatform(caps.Platform)
	return p
}

// applyImageConfig fills a prebuilt draft from what its image declares.
//
// EXPOSE is the image author naming the port, which the spec records as
// PortExpose so the review shows it was declared rather than watched; a trial
// run that observes the port supersedes it. VOLUME is the author naming where
// data lives, recorded as storage for the same reason a compose file's volumes
// are (R-200) — the case R-203 calls the worst failure in the system is that
// path left in the container's writable layer. HEALTHCHECK becomes the
// workload's check. CMD is not copied: the image runs its own.
func applyImageConfig(draft Draft, questions []Question, cfg *oci.Config) (Draft, []Question, []string) {
	if cfg == nil {
		return draft, questions, nil
	}
	var evidence []string

	for i, w := range draft.Workloads {
		if !w.Primary {
			continue
		}
		if len(w.Ports) == 0 && len(cfg.ExposedPorts) > 0 {
			ports := make([]spec.Port, 0, len(cfg.ExposedPorts))
			listed := make([]string, 0, len(cfg.ExposedPorts))
			for _, n := range cfg.ExposedPorts {
				ports = append(ports, spec.Port{Number: n, Protocol: "http", Source: spec.PortExpose})
				listed = append(listed, strconv.Itoa(n))
			}
			draft.Workloads[i].Ports = webPortsFirst(ports)
			evidence = append(evidence, "the image declares port "+strings.Join(listed, ", ")+" with EXPOSE")
			questions = dropQuestion(questions, KeyPrimaryPort)
		}
		if w.Health == nil && cfg.Healthcheck != nil {
			draft.Workloads[i].Health = &spec.Healthcheck{
				Command:         cfg.Healthcheck.Command,
				IntervalSeconds: int(cfg.Healthcheck.Interval.Seconds()),
				TimeoutSeconds:  int(cfg.Healthcheck.Timeout.Seconds()),
				Retries:         cfg.Healthcheck.Retries,
			}
			evidence = append(evidence, "the image declares a HEALTHCHECK")
		}
		break
	}

	if len(cfg.Volumes) > 0 {
		before := len(draft.Volumes)
		draft = applyImageVolumes(draft, Trial{ImageVolumes: cfg.Volumes})
		if len(draft.Volumes) > before {
			evidence = append(evidence, fmt.Sprintf("the image keeps data in %s (VOLUME)", strings.Join(cfg.Volumes, ", ")))
		}
	}
	return draft, questions, evidence
}

func dropQuestion(questions []Question, key string) []Question {
	out := questions[:0:0]
	for _, q := range questions {
		if q.Key != key {
			out = append(out, q)
		}
	}
	return out
}

// pullAuth converts resolved credentials for a runtime.
func pullAuth(a *oci.Auth) *api.RegistryAuth {
	if a == nil {
		return nil
	}
	return &api.RegistryAuth{Registry: a.Registry, Username: a.Username, Password: a.Password, IdentityToken: a.IdentityToken}
}
