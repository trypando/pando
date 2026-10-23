package deploy

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/log"
)

// An app is built once (R-311, design 09 §4.1).
//
// Detection builds what the app would deploy, so the plan can show the score
// of the image that will run. The first deploy of that plan runs that image
// and reuses its scan rather than building and scanning a second time: the
// score somebody accepted is the score of what runs, and a build that is not
// reproducible bit for bit cannot make the two differ. A deploy whose spec
// changed what is built — a build argument, a Dockerfile, a target — or whose
// source is another commit builds as before.

// DetectionBuildFinder finds what detection built for an app.
type DetectionBuildFinder interface {
	// Built is the app's detection build, nil when there is none.
	Built(ctx context.Context, appID string) (*detect.Built, error)
}

// WithDetectionBuilds lets a deploy run the image detection built when its
// spec builds the same thing from the same source. Without it every deploy
// builds.
func (r *Runner) WithDetectionBuilds(b DetectionBuildFinder) *Runner {
	r.detectionBuilds = b
	return r
}

// BuildConcurrency bounds how many of an app's images are built at once.
type BuildConcurrency func(ctx context.Context) (int, error)

// PolicyReader is host policy, narrowed to the one call DeployConcurrency needs.
type PolicyReader interface {
	Document(ctx context.Context) (policy.Document, error)
}

// DeployConcurrency is host policy's max_concurrent_deploys, as a build
// limit: one app's builds take no more of the machine than that many deploys.
func DeployConcurrency(p PolicyReader) BuildConcurrency {
	return func(ctx context.Context) (int, error) {
		doc, err := p.Document(ctx)
		return doc.MaxConcurrentDeploys, err
	}
}

// WithBuildConcurrency sets how many of a compose app's services build at
// once: the install's deploy concurrency, so one app's builds take no more of
// the machine than that many deploys would. Without it, they build one at a
// time.
func (r *Runner) WithBuildConcurrency(limit BuildConcurrency) *Runner {
	r.buildConcurrency = limit
	return r
}

// BuildImages builds every image s needs from checkout, exactly as a deploy
// builds them — the same builder, the same Dockerfile or plan, the same
// egress rules (R-118) and isolation floor, and no runtime socket in the build
// (R-112) — and delivers them to the runtime the same way. tag names the
// build, as a deployment's ID names a deploy's (R-146).
//
// Detection's build (R-311). Returns the app's image, or a compose app's
// images by workload.
//
// One difference from a deploy, when detection asks for it (detect.WithSteer):
// the builder may steer a plan it made from convention when a build of it
// fails in a way it recognizes, and build again. The plan it built from in the
// end replaces s.Build.GeneratedFiles, so what detection proposes is what was
// built, and what a deploy replays (R-020).
func (r *Runner) BuildImages(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout, sink io.Writer, tag string) (string, map[string]string, error) {
	return r.buildAll(ctx, s, checkout, sink, tag)
}

// BuiltImageAuth is the credential an image Pando built is pulled with: the
// install registry's, for an image pushed there, and nil for any other.
func (r *Runner) BuiltImageAuth(ctx context.Context, image string) *api.RegistryAuth {
	// Detection asks only of images its own build produced.
	return r.builtImageAuth(ctx, nil, image)
}

// detectionBuild is detection's build of this app when it is what this deploy
// would build: the same build inputs (spec.BuildInputs) from the same source.
func (r *Runner) detectionBuild(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout) (*detect.Built, bool) {
	if r.detectionBuilds == nil {
		return nil, false
	}
	built, err := r.detectionBuilds.Built(ctx, s.AppID)
	if err != nil {
		log.From(ctx).Warn("could not read the image detection built; building", zap.Error(err))
		return nil, false
	}
	if !built.Matches(s, checkout.Identity()) {
		return nil, false
	}
	if s.Build.Strategy == spec.BuildCompose {
		// Every service that builds, or the deploy builds them all: a compose
		// app is not half reused.
		for _, w := range s.Workloads {
			if w.Build != nil && built.Images[w.Name] == "" {
				return nil, false
			}
		}
		return built, r.stillImported(ctx, s, built)
	}
	return built, built.Image != "" && r.stillImported(ctx, s, built)
}

// stillImported reports whether every image detection imported into the
// runtime is still there.
//
// An imported image lives only in the runtime's own store, and an image prune
// there removes it. Reused anyway, the deploy asked the runtime to run an ID no
// registry has, and Docker tried to pull it as docker.io/library/sha256:… —
// "not found", and no deploy (issue #90). Gone, it is built again: what is
// built is the same inputs from the same source, which is what reuse stood for
// (R-311). An image in a registry stays there and is not asked about.
func (r *Runner) stillImported(ctx context.Context, s *spec.AppSpec, built *detect.Built) bool {
	images := []string{built.Image}
	for _, image := range built.Images {
		images = append(images, image)
	}
	if r.registry == nil {
		return true // no runtime to ask; the apply reports a missing image
	}
	var runtime api.RuntimeAdapter
	for _, image := range images {
		if !strings.HasPrefix(image, "sha256:") {
			continue // empty, or a registry reference
		}
		if runtime == nil {
			rt, ok := r.registry.Runtime(s.Runtime.AdapterRef)
			if !ok {
				return false
			}
			runtime = rt
		}
		present, err := runtime.HasImage(ctx, image)
		if err != nil {
			log.From(ctx).Warn("could not check the image detection built; building", zap.Error(err))
			return false
		}
		if !present {
			log.From(ctx).Info("the image detection built is gone from the runtime; building", zap.String("image", image))
			return false
		}
	}
	return true
}

// buildLimit is how many images of one app build at once.
func (r *Runner) buildLimit(ctx context.Context) int {
	if r.buildConcurrency == nil {
		return 1
	}
	n, err := r.buildConcurrency(ctx)
	if err != nil || n <= 0 {
		return DefaultConcurrency()
	}
	return n
}

// lockedWriter serializes writes from builds running at once into one log.
type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// prefixedLines writes each whole line to out with prefix before it, so the
// logs of services building at once can be told apart. Close flushes a last
// line with no newline.
type prefixedLines struct {
	prefix string
	out    io.Writer
	buf    bytes.Buffer
}

func (p *prefixedLines) Write(b []byte) (int, error) {
	p.buf.Write(b)
	for {
		end := bytes.IndexByte(p.buf.Bytes(), '\n')
		if end < 0 {
			// No newline yet: kept for the next write.
			return len(b), nil
		}
		line := append([]byte(p.prefix), p.buf.Next(end+1)...)
		if _, err := p.out.Write(line); err != nil {
			return len(b), err
		}
	}
}

func (p *prefixedLines) Close() error {
	if p.buf.Len() == 0 {
		return nil
	}
	line := append(append([]byte(p.prefix), p.buf.Bytes()...), '\n')
	p.buf.Reset()
	_, err := p.out.Write(line)
	return err
}
