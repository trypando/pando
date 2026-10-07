// Package deploy runs the deployment pipeline: everything after the plan
// boundary (design 05 §3, steps 8-16).
//
// Steps 1-7 belong to the planner and create nothing. Everything here has side
// effects, which is why the split exists: a plan-time failure means nothing was
// touched, and a failure in this package means something was.
package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/imageregistry"
	"github.com/trypando/pando/internal/core/oci"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

// Secrets resolves an app's stored secrets into values.
type Secrets interface {
	// Resolve returns an app's secret values by key. Called once per deploy, at
	// step 11 — after the plan boundary, because reading a secret is a side
	// effect and an audited one.
	Resolve(ctx context.Context, appID string) (map[string]secret.Value, error)

	// Versions returns each secret's version without decrypting anything. It
	// feeds the environment fingerprint (R-193), which is compared on every
	// reconciliation and must never be a reason to read a value.
	Versions(ctx context.Context, appID string) (map[string]int, error)
}

// Reconciles records what an app was last applied with.
type Reconciles interface {
	SetAppliedEnvFingerprint(ctx context.Context, appID, fingerprint string) error
}

// Runner executes deployments.
type Runner struct {
	registry *api.Registry
	planner  *planner.Planner
	apps     *state.Apps
	deploys  *state.Deployments
	secrets  Secrets
	logs     *LogStore

	// reconciles records the environment fingerprint a deploy applied, which is
	// the only thing that makes a rotated secret visible later (R-193).
	reconciles Reconciles

	// volumes records the storage a deploy created, so Pando knows about the
	// data it is responsible for.
	volumes *state.Volumes

	// services records which provisioned instance fills which slot (R-131).
	services *state.Services

	// secretStore stores the connection strings provisioning generates.
	//
	// Separate from the secrets field above, which is the narrow read-side
	// interface a deploy needs. Provisioning writes, and writing a secret is
	// not something every Runner caller should be able to hand in a stub for.
	secretStore *state.Secrets

	// security scores what was built, before it is applied (R-312, R-314).
	// Nil on an installation with no scanner, where nothing is scored and
	// nothing is enforced.
	security Security

	// sources fetches what a deploy builds, including an uploaded source
	// (R-262), which is kept in the directory it names.
	sources source.Sources

	// images resolves an image app's digest and registry credential.
	images *oci.Images

	// buildRegistry is the install's image registry, where a build goes when
	// the runtime pulls rather than imports (issue #72, PR 5). Nil when the
	// install has none, which single-host Docker does not need (O-34).
	buildRegistry imageregistry.Provider

	// ProxyUpstream is where routing adapters must send traffic (R-023). It is
	// Pando's proxy, always, and it is passed to every Ensure so that no adapter
	// has to work it out.
	ProxyUpstream string
}

// PrepareRevision pins the commit a deploy will build.
//
// Resolving a ref is an explicit act that produces a revision, never something a
// deploy does on its own (design 01 §2.1, R-120). A revision that already names
// a commit is returned unchanged, so redeploying it builds the same code even
// after the branch has moved — which is the whole point of pinning.
func (r *Runner) PrepareRevision(ctx context.Context, rev state.Revision, by string) (state.Revision, error) {
	s := rev.Body
	if s.Source.Type == spec.SourceImage {
		return r.pinImage(ctx, rev, by)
	}
	if s.Source.Type != spec.SourceGit || s.Source.Commit != "" {
		return rev, nil
	}

	checkout, err := r.sources.Fetch(ctx, s.Source)
	if err != nil {
		return state.Revision{}, err
	}
	defer checkout.Close()

	pinned := *s
	pinned.Source.Commit = checkout.Commit

	// A new revision, because spec_revisions is append-only and because the
	// pinned commit is a real change to how this app runs — one worth being
	// visible in the app's history rather than applied silently.
	return r.apps.CreateRevision(ctx, rev.AppID, &pinned, spec.OriginEdited, by)
}

// pinImage is PrepareRevision for an image app: a revision that names a tag
// and no digest gets a new revision pinning the digest the tag names now
// (issue #41), for the reason a branch gets a commit. Without it, a tag pushed
// again — :latest, or a release tag rebuilt — changes what runs with no new
// revision to show it, and a rollback goes back to whatever the tag means
// today rather than what ran then.
func (r *Runner) pinImage(ctx context.Context, rev state.Revision, by string) (state.Revision, error) {
	pinned, ok, err := pinnedSpec(ctx, r.images, rev.AppID, rev.Body)
	if err != nil {
		return state.Revision{}, err
	}
	if !ok {
		return rev, nil
	}
	return r.apps.CreateRevision(ctx, rev.AppID, pinned, spec.OriginEdited, by)
}

func NewRunner(registry *api.Registry, p *planner.Planner, apps *state.Apps, deploys *state.Deployments, secrets Secrets, reconciles Reconciles, logs *LogStore, volumes *state.Volumes, proxyUpstream string) *Runner {
	return &Runner{
		registry: registry, planner: p, apps: apps, deploys: deploys,
		secrets: secrets, reconciles: reconciles, logs: logs, volumes: volumes,
		ProxyUpstream: proxyUpstream,
	}
}

// WithSecurity enables the security score (R-310 – R-314).
//
// Optional in the same way WithServices is: a Runner without it deploys exactly
// as before. An installation with no scanner configured has no scores and no
// threshold to enforce, which is the shipped posture (R-317).
func (r *Runner) WithSecurity(s Security) *Runner {
	r.security = s
	return r
}

// Security is what a deploy needs from the security service, narrowed so the
// deploy path cannot reach for anything else.
type Security interface {
	Scan(ctx context.Context, req api.ScanRequest, principal audit.Event) (state.Scan, error)
	// Reuse returns a successful scan of the app's source, attached to specID,
	// when there is one: a deploy of an unchanged source uses that scan.
	Reuse(ctx context.Context, appID, specID, source string) (state.Scan, bool, error)
	Allows(ctx context.Context, appID, specID string) (security.Standing, error)
	Configured() (string, bool)
}

// WithServices enables provisioned slots (R-131).
//
// Optional rather than a constructor argument because a Runner without it is
// still correct: an app with no provisioned slot never notices, and one with a
// provisioned slot is refused at plan time rather than deployed half-wired.
func (r *Runner) WithServices(services *state.Services, secrets *state.Secrets) *Runner {
	r.services = services
	r.secretStore = secrets
	return r
}

// WithSources sets where a deploy fetches source from, and where uploaded
// source is kept (R-262). Without it, git and image sources deploy and an
// uploaded one is refused.
func (r *Runner) WithSources(sources source.Sources) *Runner {
	r.sources = sources
	return r
}

// WithImages sets how an image app's image is read and pulled: its digest is
// resolved and pinned before a deploy, and its registry credential resolved
// for the pull (issue #41). Without it an image is pulled anonymously by tag.
func (r *Runner) WithImages(images *oci.Images) *Runner {
	r.images = images
	return r
}

// WithBuildRegistry sets the install's image registry (issue #72, PR 5).
// Without it, a build reaches only a runtime that imports it.
//
// Asked on every build and pull rather than held, because the registry and
// its credential can be changed from the console while Pando runs.
func (r *Runner) WithBuildRegistry(reg imageregistry.Provider) *Runner {
	r.buildRegistry = reg
	return r
}

// builtImageAuth is the install registry's credential for an image Pando
// pushed there, and nil for any other image. Minted fresh for ECR. A failure
// leaves the pull anonymous, which fails with the registry's own reason.
func (r *Runner) builtImageAuth(ctx context.Context, image string) *api.RegistryAuth {
	reg, err := r.currentRegistry(ctx)
	if err != nil {
		log.From(ctx).Warn("could not read the install registry", zap.Error(err))
		return nil
	}
	if !reg.Owns(image) {
		return nil
	}
	auth, err := reg.Auth(ctx)
	if err != nil {
		log.From(ctx).Warn("could not resolve the install registry's credential", zap.Error(err))
		return nil
	}
	return auth
}

// currentRegistry is the install registry now, nil when there is none.
func (r *Runner) currentRegistry(ctx context.Context) (*imageregistry.Registry, error) {
	if r.buildRegistry == nil {
		return nil, nil
	}
	return r.buildRegistry.Current(ctx)
}

// withBuiltImageAuth gives every workload that runs a build from the install's
// registry the registry's credential, for the one pull (design 03 §2.1). An
// image app's own credential, set by withPullAuth, is left alone.
func (r *Runner) withBuiltImageAuth(ctx context.Context, bundle *api.BundlePlan) {
	for i := range bundle.Workloads {
		w := &bundle.Workloads[i]
		if w.PullAuth == nil {
			w.PullAuth = r.builtImageAuth(ctx, w.Image)
		}
	}
}

// Run executes a deployment to completion.
//
// The app's state moves to deploying when the deploy is started and to running
// or degraded at the end. A deploy that fails before the runtime is touched — a
// build failure, a refused scan — puts back the state it found, because nothing
// about the running app changed (R-146). The deployment is failed; the app is
// not. One that fails later leaves the app failed or degraded (design 05 §1.2).
func (r *Runner) Run(ctx context.Context, dep state.Deployment, rev state.Revision) error {
	l := log.From(ctx).With(zap.String("deployment_id", dep.ID), zap.String("app_id", dep.AppID))
	sink := r.logs.Writer(dep.ID)
	defer sink.Close()

	// Where the app is left if the deploy stops here (design 05 §1.2). The
	// app was moved to deploying when this started, and the reconciler does
	// not look at a deploying app — so a deploy that ended without saying
	// otherwise left it deploying forever, never reconciled and never able
	// to reach failed (R-150, R-151).
	//
	// Before the runtime is touched: as it was, because nothing about the
	// running app changed (R-146). Once Apply has been called: failed if
	// starting it did not work, and degraded if it started and never became
	// healthy, which is where the reconciler's backoff and give-up threshold
	// take over (R-149, R-150).
	//
	// "" stands for the state the deploy found, which the starter stored on
	// the deployment (approval.Service.launch) and is read when it is needed:
	// this may be another replica, and the starter may have written it after
	// this one claimed the deploy.
	leaveAs := ""
	leave := func() {
		to := leaveAs
		if to == "" {
			prior, err := r.deploys.PriorState(ctx, dep.ID)
			if err != nil || prior == "" {
				return
			}
			to = prior
		}
		_ = r.apps.SetStateIf(ctx, dep.AppID, state.StateDeploying, to)
	}

	fail := func(step string, err error) error {
		code := string(errs.CodeOf(err))
		message := "The deploy failed."
		if e := errs.As(err); e != nil {
			message = e.Message
		}
		writeFailure(sink, step+" failed: "+message, err)
		l.Warn("deployment failed", zap.String("step", step), zap.Error(err))
		_ = r.deploys.Finish(ctx, dep.ID, state.DeployFailed, code, message)
		leave()
		return err
	}

	appSpec := rev.Body

	// A deploy never resolves a ref implicitly at runtime (design 01 §2.1).
	// The commit is pinned by PrepareRevision before the deployment exists, so
	// reaching here without one means the caller skipped that step — and a
	// deploy that silently built whatever the branch points at now would be
	// unreproducible in exactly the way R-120 exists to prevent.
	//
	// An upload is the exception, and it does not weaken the rule. There is no
	// revision to resolve: the archive on the server IS the pinned thing, and
	// it cannot change under the deploy because the next upload writes a new
	// one. Demanding a commit here would mean inventing one, which is the
	// failure R-120 actually names.
	if needsBuild(appSpec) && appSpec.Source.Commit == "" && appSpec.Source.Type != spec.SourceUpload {
		return fail("fetch", errs.New(errs.StateInvalid,
			"This app's spec does not say which commit to build.").
			WithRemedy("Pin a commit for this revision before deploying it."))
	}

	// Step 8: fetch the pinned commit.
	if appSpec.Source.Type == spec.SourceUpload {
		fmt.Fprintln(sink, "=> Using the uploaded source")
	} else {
		fmt.Fprintf(sink, "=> Fetching source at %s\n", short(appSpec.Source.Commit))
	}
	checkout, err := r.sources.Fetch(ctx, appSpec.Source)
	if err != nil {
		return fail("fetch", err)
	}
	defer checkout.Close()

	// Step 9: build. A failure here leaves the running app alone (R-146).
	image := appSpec.Source.Image
	var perWorkload map[string]string
	if needsBuild(appSpec) {
		if err := r.deploys.SetStatus(ctx, dep.ID, state.DeployBuilding); err != nil {
			return fail("build", err)
		}
		built, images, err := r.buildAll(ctx, appSpec, checkout, sink, dep.ID)
		perWorkload = images
		if err != nil {
			// The reason goes into the log the user is watching, not only into
			// the server's own log. A build that fails without saying why is
			// the failure mode R-105 exists to prevent.
			writeFailure(sink, messageOf(err), err)
			fmt.Fprintf(sink, "   The running version of this app was not touched.\n")
			_ = r.deploys.Finish(ctx, dep.ID, state.DeployFailed, string(errs.CodeOf(err)), messageOf(err))
			l.Warn("build failed; app state unchanged (R-146)", zap.Error(err))
			leave()
			return err
		}
		image = built
	}

	// An image app runs the digest its revision pinned, never whatever its tag
	// names today (issue #41, R-120's rule for commits), and pulls with the
	// app's own registry credential when it has one.
	image, perWorkload, pull, err := imagePull(ctx, r.images, dep.AppID, appSpec, image, perWorkload, sink)
	if err != nil {
		return fail("fetch", err)
	}

	// Step 10: scan what was built, and refuse to apply it if this
	// installation's threshold says so (R-312, R-314).
	//
	// Here rather than before the build because the image is what there is to
	// look at, and before `applying` because a refusal must leave the running
	// app untouched — the same contract a failed build has (R-146).
	commit := checkout.Identity()
	if commit == "" {
		commit = appSpec.Source.Commit
	}
	if err := r.scan(ctx, dep, appSpec, image, checkout.Dir, commit, sink); err != nil {
		writeFailure(sink, messageOf(err), err)
		fmt.Fprintf(sink, "   The running version of this app was not touched.\n")
		_ = r.deploys.Finish(ctx, dep.ID, state.DeployFailed, string(errs.CodeOf(err)), messageOf(err))
		leave()
		return err
	}

	// Step 10a: a port detection could only guess is checked against the image
	// that was just built (R-097). Before `applying`, for the same reason as the
	// scan: a refusal must leave the running app untouched (R-146).
	if needsPortCheck(appSpec) && image != "" {
		if runtime, ok := r.registry.Runtime(appSpec.Runtime.AdapterRef); ok {
			check := checkPortWith(ctx, runtime, appSpec, image, r.builtImageAuth(ctx, image), "port-"+strings.ToLower(dep.ID), sink)
			if check.Refusal != nil {
				writeFailure(sink, messageOf(check.Refusal), check.Refusal)
				fmt.Fprintf(sink, "   The running version of this app was not touched.\n")
				_ = r.deploys.Finish(ctx, dep.ID, state.DeployFailed,
					string(errs.CodeOf(check.Refusal)), messageOf(check.Refusal))
				leave()
				return check.Refusal
			}
			if check.Port != 0 {
				// A new revision rather than a quiet substitution: the port
				// traffic goes to is part of how the app runs, and the spec is
				// the record of that (R-020). The app's history shows the
				// assumed port was replaced by the observed one.
				observed, err := r.apps.CreateRevision(ctx, dep.AppID,
					withObservedPort(appSpec, check.Port), spec.OriginDetected, dep.CreatedBy)
				if err != nil {
					return fail("build", err)
				}
				rev = observed
				appSpec = observed.Body
			}
		}
	}

	if err := r.deploys.SetStatus(ctx, dep.ID, state.DeployApplying); err != nil {
		return fail("apply", err)
	}

	// Step 11: resolve secrets and materialize the environment.
	//
	// This is where slot and secret references become values, and it is the
	// last moment before the plan leaves core. Adapters receive the result
	// fully resolved and never learn which entries were sensitive.
	fmt.Fprintf(sink, "=> Preparing configuration\n")
	secrets, err := r.secrets.Resolve(ctx, dep.AppID)
	if err != nil {
		return fail("secrets", err)
	}

	// Provisioned slots become real workloads here (R-131). After secrets, so a
	// redeploy can reuse the credentials the database was created with, and
	// before the bundle plan, because the services are part of it.
	svcs, err := r.provision(ctx, appSpec, sink)
	if err != nil {
		return fail("apply", err)
	}

	// Egress resolved against policy as it is now, the same answer the plan
	// gave (R-188), and recorded so the reconciler restores exactly these.
	var rules api.EgressRules
	if r.planner != nil {
		eff, err := r.planner.Egress(ctx, appSpec)
		if err != nil {
			return fail("apply", err)
		}
		rules = eff.Rules
	}
	if err := r.deploys.SetEgressRules(ctx, dep.ID, rules); err != nil {
		return fail("apply", err)
	}

	bundle, err := r.bundlePlan(appSpec, image, perWorkload, secrets, svcs, rules)
	if err != nil {
		return fail("apply", err)
	}
	withPullAuth(&bundle, image, pull)
	r.withBuiltImageAuth(ctx, &bundle)
	bundle.FirstDeploy = r.firstDeploy(ctx, dep.AppID)

	runtime, ok := r.registry.Runtime(appSpec.Runtime.AdapterRef)
	if !ok {
		return fail("apply", errs.Newf(errs.PlanAdapterNotConfigured,
			"The runtime %q is not configured.", appSpec.Runtime.AdapterRef))
	}

	// Steps 12-13: volumes, then apply. Recreate is the default (R-144) and the
	// app is down during the swap, which is the accepted cost.
	fmt.Fprintf(sink, "=> Starting the app\n")
	leaveAs = state.StateFailed // design 05 §1.2: apply failed

	// Deploying is asking for the app to run (R-140), and from here it may.
	// Recorded before Apply rather than only on success: an app that starts
	// and never becomes healthy is left degraded for the reconciler to retry
	// toward R-150's threshold, and one whose desired state still said
	// stopped would instead be held stopped — never retried, never failed.
	// A failed app is untouched either way: the reconciler does not look at
	// failed, whatever its desired state (R-151).
	if err := r.apps.SetDesiredState(ctx, dep.AppID, "running"); err != nil {
		return fail("apply", err)
	}
	if _, err := runtime.Apply(ctx, bundle); err != nil {
		return fail("apply", err)
	}

	// Record the storage Pando now owns.
	//
	// Not bookkeeping. Until this existed the `volumes` table stayed empty
	// however many volumes an app declared, and three things quietly did
	// nothing: R-204's ON DELETE RESTRICT guarded no rows, deleting an app
	// never offered to keep its data because it counted none, and a DR bundle
	// contained the database and no app data whatsoever.
	//
	// The handles come from Observe rather than being assembled here, because
	// how a volume is named is the provider vocabulary core must never learn
	// (R-251).
	if r.volumes != nil && len(bundle.Volumes) > 0 {
		if err := r.recordVolumes(ctx, runtime, appSpec, bundle); err != nil {
			// Not fatal to the deploy: the app is running and refusing to say
			// so would be worse. But it is logged loudly, because an app whose
			// storage Pando does not know about is an app whose storage will
			// not be backed up.
			fmt.Fprintf(sink, "!! Could not record this app's storage: %v\n", err)
		}
	}

	// Step 14: route. Traffic goes to PANDO'S PROXY, never to the workload.
	fmt.Fprintf(sink, "=> Routing traffic\n")
	routing, ok := r.registry.Routing(appSpec.Routing.AdapterRef)
	if !ok {
		return fail("route", errs.Newf(errs.PlanAdapterNotConfigured,
			"The routing option %q is not configured.", appSpec.Routing.AdapterRef))
	}
	if _, err := routing.Ensure(ctx, api.RouteRequest{
		AppID:      dep.AppID,
		Mode:       appSpec.Routing.Mode,
		Hostname:   appSpec.Routing.Hostname,
		PathPrefix: appSpec.Routing.PathPrefix,
		Port:       appSpec.Routing.Port,

		// R-023 has no exceptions. Every route points here.
		ProxyUpstream: r.ProxyUpstream,

		TLS: api.TLSRequest{Enabled: appSpec.Routing.Mode != spec.RoutingPort, Hostname: appSpec.Routing.Hostname},
	}); err != nil {
		return fail("route", err)
	}

	// Step 15: wait for health.
	fmt.Fprintf(sink, "=> Waiting for the app to be ready\n")
	// From here the app is running what was applied, healthy or not: one
	// that never comes up is degraded, and the reconciler retries it with
	// backoff until it recovers or reaches the give-up threshold (R-150).
	leaveAs = state.StateDegraded
	healthy, err := r.waitForHealth(ctx, runtime, dep.AppID, bundle, sink)
	if err != nil {
		return fail("health", err)
	}

	// Step 16: commit.
	newState := state.StateRunning
	if !healthy {
		// Degraded, not failed: the workloads are up and the health signal has
		// not passed yet. Auto-rollback happens only if opted in (R-147).
		newState = state.StateDegraded
		fmt.Fprintf(sink, "\n!! The app started but has not reported healthy yet.\n")
	}

	if err := r.apps.Pin(ctx, dep.AppID, rev.ID, newState, dep.CreatedBy); err != nil {
		return fail("commit", err)
	}
	if err := r.apps.SetDesiredState(ctx, dep.AppID, "running"); err != nil {
		return fail("commit", err)
	}

	// What ran, and what it ran with. Both exist for the reconciler: it
	// restores a missing workload from the recorded image rather than
	// rebuilding, and it detects a rotated secret (R-193) by comparing this
	// fingerprint, because Observe returns no environment and never will.
	ran := workloadImages(ctx, runtime, appSpec, perWorkload, image, dep.AppID)
	if err := r.deploys.SetImageRef(ctx, dep.ID,
		firstNonEmpty(image, primaryImage(appSpec, perWorkload)),
		primaryDigest(ctx, runtime, appSpec, dep.AppID), ran); err != nil {
		return fail("commit", err)
	}
	if r.reconciles != nil {
		versions, err := r.secrets.Versions(ctx, dep.AppID)
		if err != nil {
			return fail("commit", err)
		}
		if err := r.reconciles.SetAppliedEnvFingerprint(ctx, dep.AppID,
			EnvFingerprint(rev.Body, versions)); err != nil {
			return fail("commit", err)
		}
	}
	// What the app was doing when this finished, which is a different question
	// from whether the deploy worked — and the one somebody reading a list of
	// deploys is asking.
	if err := r.deploys.SetResultState(ctx, dep.ID, newState); err != nil {
		return err
	}
	if err := r.deploys.Finish(ctx, dep.ID, state.DeploySucceeded, "", ""); err != nil {
		return err
	}

	fmt.Fprintf(sink, "\n== Deployed. The app is %s.\n", newState)
	l.Info("deployment succeeded", zap.String("state", newState))
	return nil
}

// build runs the builder and hands the image to the runtime.
//
// The image streams from one adapter to the other through a pipe: it is never
// held in memory or staged on disk, and neither adapter learns anything about
// the other.
// buildAll produces every image this app needs.
//
// One for most apps. A compose app can need several: each service either names
// an image that already exists or says how to build one, so a file with a
// frontend and a worker is two builds and two images. That is what importing a
// compose file means, and the reason the importer used to throw the build
// instructions away is that there was nowhere to put more than one.
//
// Returns the app-wide image, if there is one, and the per-workload images
// keyed by workload name.
//
// Each build is named by the deployment that ran it (R-146): the image a
// deployment records is that build's and no other, whichever way it reached
// the runtime.
func (r *Runner) buildAll(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout, sink io.Writer, deploymentID string) (string, map[string]string, error) {
	if s.Build.Strategy != spec.BuildCompose {
		image, err := r.build(ctx, s, checkout, sink, nil, s.AppID, deploymentID)
		return image, nil, err
	}

	images := map[string]string{}
	for _, w := range s.Workloads {
		if w.Build == nil {
			// Names an image already. Nothing to build, and building something
			// to replace it would ignore what the compose file said.
			continue
		}
		fmt.Fprintf(sink, "=> Building %s\n", w.Name)

		// The cache is namespaced per workload within the app. Per app is
		// R-117's requirement — one app must not read another's layers — and
		// two services in one app sharing a namespace would thrash each other's
		// cache and collide on the built image's name.
		image, err := r.build(ctx, s, checkout, sink, w.Build, s.AppID+"/"+w.Name, deploymentID)
		if err != nil {
			return "", nil, err
		}
		images[w.Name] = image
	}
	return "", images, nil
}

func (r *Runner) build(ctx context.Context, s *spec.AppSpec, checkout *source.Checkout, sink io.Writer, wb *spec.WorkloadBuild, cacheNamespace, deploymentID string) (string, error) {
	builder, ok := r.registry.Builder(s.Build.AdapterRef)
	if !ok {
		return "", errs.Newf(errs.PlanAdapterNotConfigured,
			"The builder %q is not configured.", s.Build.AdapterRef)
	}
	runtime, ok := r.registry.Runtime(s.Runtime.AdapterRef)
	if !ok {
		return "", errs.Newf(errs.PlanAdapterNotConfigured,
			"The runtime %q is not configured.", s.Runtime.AdapterRef)
	}

	caps, err := runtime.Capabilities(ctx)
	if err != nil {
		return "", err
	}
	builderCaps, err := builder.Capabilities(ctx)
	if err != nil {
		return "", err
	}
	// The planner asked the same question; asked again because a runtime or
	// the registry may have changed since, and a build nothing can run is the
	// failure this exists to prevent (R-254).
	reg, err := r.currentRegistry(ctx)
	if err != nil {
		return "", err
	}
	delivery, err := planner.ChooseDelivery(s.Runtime.AdapterRef, caps, s.Build.AdapterRef, builderCaps, reg)
	if err != nil {
		return "", err
	}

	args := map[string]string{}
	for _, kv := range s.Build.Args {
		args[kv.Key] = kv.Value
	}

	// One service of a compose app is an ordinary Dockerfile build: its own
	// context and its own file, from the same checkout.
	strategy, dockerfile, context, target := s.Build.Strategy, s.Build.Dockerfile, s.Build.Context, s.Build.Target
	if wb != nil {
		strategy, dockerfile, context, target = spec.BuildDockerfile, wb.Dockerfile, wb.Context, wb.Target
		// The service's own build arguments, over the app's.
		for _, kv := range wb.Args {
			args[kv.Key] = kv.Value
		}
	} else {
		fmt.Fprintf(sink, "=> Building\n")
	}

	req := api.BuildRequest{
		Source:     checkout.View(s.Source.Subdir),
		Strategy:   strategy,
		Dockerfile: dockerfile,
		Context:    context,
		StaticDir:  s.Build.StaticDir,

		// The command a person gave when detection asked for one. Only for the
		// app-wide build: a compose service's Dockerfile says its own.
		StartCommand: startCommand(s, wb),

		// The reviewed plan, replayed. Without this the builder plans again at
		// build time and an edit made in the console never reaches the build.
		GeneratedFiles: s.Build.GeneratedFiles,
		Args:           args,
		Target:         target,

		IsolationFloor: s.Build.IsolationFloor,
		Timeout:        time.Duration(s.Build.TimeoutSeconds) * time.Second,
		EgressMode:     s.Build.EgressMode,
		EgressAllow:    s.Build.EgressAllow,

		// R-117: per-app cache namespace, so one app's build cannot read
		// layers produced by another's.
		CacheNamespace: cacheNamespace,

		LogSink: sink,

		// Named by the deployment, never a tag the next build moves (R-146).
		Tag: deploymentID,
	}

	if delivery == api.ImageDeliveryRegistry {
		workload := ""
		if wb != nil {
			_, workload, _ = strings.Cut(cacheNamespace, "/")
		}
		target, err := reg.Target(ctx, s.AppID, workload, deploymentID)
		if err != nil {
			return "", err
		}
		req.Push = target
		result, err := builder.Build(ctx, req)
		if err != nil {
			return "", err
		}
		// What runs is repository@digest (R-120). A push the builder could not
		// pin is not run by tag instead.
		if result.Digest == "" || !strings.HasSuffix(result.ImageRef, "@"+result.Digest) {
			return "", errs.Newf(errs.BuildFailed,
				"The build was pushed to %s, and the builder did not report the digest it was stored under.", reg.Host()).
				WithRemedy("Deploy again. If it happens again, check the build service's logs for the push.")
		}
		fmt.Fprintf(sink, "=> Pushed to %s at %s\n", reg.Host(), shortDigest(result.Digest))
		return result.ImageRef, nil
	}

	pr, pw := io.Pipe()

	var (
		wg         sync.WaitGroup
		importedID string
		importErr  error
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		importedID, importErr = runtime.ImportImage(ctx, pr)
		// Draining matters: if the import fails early, the builder would block
		// writing into a pipe nobody reads.
		_, _ = io.Copy(io.Discard, pr)
	}()

	req.ImageSink = pw
	result, buildErr := builder.Build(ctx, req)
	_ = pw.CloseWithError(buildErr)
	wg.Wait()

	if buildErr != nil {
		return "", buildErr
	}
	if importErr != nil {
		return "", importErr
	}
	if importedID != "" {
		return importedID, nil
	}
	return result.ImageRef, nil
}

// startCommand is the primary workload's command as a shell command line, for a
// builder that plans the build itself.
//
// Detection stores an answered start command as `sh -c <line>`, so that shape
// is unwrapped rather than quoted twice; any other argv is joined, which is what
// a person reading it would type.
func startCommand(s *spec.AppSpec, wb *spec.WorkloadBuild) string {
	if wb != nil || s.Build.Strategy != spec.BuildBuildpack {
		return ""
	}
	primary, ok := s.PrimaryWorkload()
	if !ok || len(primary.Command) == 0 {
		return ""
	}
	if len(primary.Command) == 3 && primary.Command[0] == "sh" && primary.Command[1] == "-c" {
		return primary.Command[2]
	}
	if len(primary.Command) == 1 {
		return primary.Command[0]
	}
	return strings.Join(primary.Command, " ")
}

// bundlePlan resolves the spec into what the runtime is asked to apply.
// BundlePlanShape builds the bundle plan without resolving any environment.
//
// The reconciler compares what should be running against what is, on every tick
// for every app. That comparison needs the shape — workloads, images, ports,
// mounts, volumes — and must never be a reason to decrypt a secret, so this
// stops short of environment and R-193's fingerprint covers the rest.
//
// Egress is the rules the app's last successful deploy ran with, which the
// deployment records: rules take effect at a deploy and a running app is not
// changed underneath it (R-183, O-10), so correcting drift restores what was
// deployed rather than what policy says now.
func BundlePlanShape(s *spec.AppSpec, image string, perWorkload map[string]string, rules api.EgressRules) (api.BundlePlan, error) {
	return bundlePlanFor(s, image, perWorkload, nil, provisioned{}, false, rules)
}

func (r *Runner) bundlePlan(s *spec.AppSpec, image string, perWorkload map[string]string, secrets map[string]secret.Value, svcs provisioned, rules api.EgressRules) (api.BundlePlan, error) {
	return bundlePlanFor(s, image, perWorkload, secrets, svcs, true, rules)
}

func bundlePlanFor(s *spec.AppSpec, image string, perWorkload map[string]string, secrets map[string]secret.Value, svcs provisioned, withEnv bool, rules api.EgressRules) (api.BundlePlan, error) {
	plan := api.BundlePlan{
		BundleID: s.AppID,
		Network: api.NetworkPlan{
			Private: true, // R-026, always.
			Egress:  rules,
		},
		Labels: map[string]string{"pando.app": s.AppID},
	}

	for _, v := range s.Volumes {
		plan.Volumes = append(plan.Volumes, api.VolumePlan{VolumeID: v.ID, Name: v.Name})
	}
	// A provisioned service's storage is an ordinary app volume, which is what
	// makes R-135 true: it is recorded, backed up, offered at delete and
	// reclaimed afterwards by code that knows nothing about services.
	plan.Volumes = append(plan.Volumes, svcs.volumes...)

	for _, w := range s.Workloads {
		var env map[string]secret.Value
		if withEnv {
			resolved, err := resolveEnv(s, w, secrets, svcs)
			if err != nil {
				return api.BundlePlan{}, err
			}
			env = resolved
		}

		wp := api.WorkloadPlan{
			Name: w.Name,
			// R-222: every workload is capped, so a chatty app cannot fill a
			// disk shared with twenty others.
			LogBytes: s.Retention.LogBytes,
			// This workload's own build first, then the image the compose file
			// named, then the app's single built image — and that last one
			// only when this workload is not built separately.
			//
			// Without the condition, a workload whose own image is not known
			// here takes the app's. For a compose app that is the primary
			// workload's image, so a reconciler plan built without the
			// per-workload record replaced the application container with a
			// second copy of the proxy. Empty is the honest answer: nothing
			// starts a workload from an image Pando cannot name.
			Image:      workloadImage(w, perWorkload, image),
			Command:    w.Command,
			Entrypoint: w.Entrypoint,
			WorkingDir: w.WorkingDir,
			Env:        env,
			DependsOn:  append(append([]string(nil), w.DependsOn...), svcs.dependsOn(s, w)...),
			Exposed:    w.Exposed,
			Resources: api.ResourcePlan{
				CPUMillis:   s.Resources.CPUMillis,
				MemoryBytes: s.Resources.MemoryBytes,
			},
		}
		if w.Resources != nil {
			wp.Resources = api.ResourcePlan{CPUMillis: w.Resources.CPUMillis, MemoryBytes: w.Resources.MemoryBytes}
		}
		for _, m := range w.Mounts {
			wp.Mounts = append(wp.Mounts, api.MountPlan{VolumeID: m.VolumeID, Path: m.Path, ReadOnly: m.ReadOnly})
		}
		// Configuration carried in the spec (R-020): read once at detection,
		// pinned to this revision, replayed on every start.
		for _, f := range w.Files {
			wp.Files = append(wp.Files, api.FilePlan{Path: f.Path, Content: f.Content, Mode: f.Mode})
		}
		for _, port := range w.Ports {
			wp.Ports = append(wp.Ports, api.PortPlan{Number: port.Number, Protocol: port.Protocol})
		}
		if h := healthPlan(s, w); h != nil {
			wp.Health = h
		}
		plan.Workloads = append(plan.Workloads, wp)
	}

	// Appended, not interleaved: the runtime orders by DependsOn, and the app's
	// workloads name the services they need.
	//
	// The caps come from the app, not the adapter. R-222 says every workload is
	// capped, and a provisioned Postgres is a workload — one that will happily
	// log every connection and every checkpoint onto a disk shared with twenty
	// other apps. An adapter cannot know the app's limits and should not be
	// asked to; the app's own allocation is the honest answer, and it is the
	// same one a second workload declared in the spec already gets.
	for _, w := range svcs.workloads {
		w.LogBytes = s.Retention.LogBytes
		if w.Resources == (api.ResourcePlan{}) {
			w.Resources = api.ResourcePlan{
				CPUMillis:   s.Resources.CPUMillis,
				MemoryBytes: s.Resources.MemoryBytes,
			}
		}
		plan.Workloads = append(plan.Workloads, w)
	}
	return plan, nil
}

// resolveEnv turns spec references into values.
//
// A reference that cannot be resolved is an error rather than an empty string:
// starting an app with a blank database password because a secret was missing is
// the kind of failure that looks like it worked.
func resolveEnv(s *spec.AppSpec, w spec.Workload, secrets map[string]secret.Value, svcs provisioned) (map[string]secret.Value, error) {
	env := make(map[string]secret.Value, len(w.Env))

	for _, e := range w.Env {
		switch {
		case e.Value != nil:
			// A variable detection read out of `.env.example` and nobody
			// filled in is a name, not a value. Setting it empty is a
			// different thing from leaving it unset, and the difference
			// decides what many apps do: `process.env.PORT || 3000` takes the
			// default either way, `if "APP_BASE_URL" in os.environ` does not.
			// An empty value somebody typed is kept — that is an answer.
			if *e.Value == "" && e.Source == spec.EnvFromDetection {
				continue
			}
			env[e.Key] = secret.New(*e.Value)

		case e.SecretRef != nil:
			v, ok := secrets[*e.SecretRef]
			if !ok {
				return nil, errs.Newf(errs.StateInvalid,
					"%s needs a stored secret that this app does not have.", e.Key).
					WithDetail("secret_key", *e.SecretRef).
					WithRemedy("Set the value through the app's secrets, then deploy again.")
			}
			env[e.Key] = v

		case e.SlotRef != nil:
			slot, ok := s.Slot(*e.SlotRef)
			// An optional slot nobody filled leaves its variable unset. It was
			// refused here like a required one, so a slot marked optional —
			// by detection, or by a person who judged the app runs without it
			// — still failed the deploy, and "optional" meant nothing past the
			// planner (R-132 blocks only a required one).
			if ok && slot.Resolution == nil && !slot.Required {
				continue
			}
			if !ok || slot.Resolution == nil {
				return nil, errs.Newf(errs.PlanSlotUnfilled,
					"%s comes from a slot that has not been filled.", e.Key).
					WithDetail("slot_key", *e.SlotRef)
			}
			switch slot.Resolution.Mode {
			case spec.ResolutionBound:
				env[e.Key] = secret.New(slot.Resolution.Target)
			case spec.ResolutionLiteral:
				v, ok := secrets[slot.Resolution.SecretRef]
				if !ok {
					return nil, errs.Newf(errs.StateInvalid,
						"%s comes from a stored value this app does not have.", e.Key).
						WithDetail("slot_key", slot.Key)
				}
				env[e.Key] = v
			case spec.ResolutionProvisioned:
				v, ok := svcs.connections[slot.Key]
				if !ok {
					// Reachable only if provisioning was skipped, which means
					// the workload is about to start with no database and no
					// message saying why.
					return nil, errs.Newf(errs.PlanSlotUnfilled,
						"%s comes from a service Pando has not provisioned yet.", e.Key).
						WithDetail("slot_key", slot.Key).
						WithRemedy("Deploy again, or connect this slot to an instance you already run.")
				}
				env[e.Key] = v
			default:
				return nil, errs.Newf(errs.PlanSlotUnfilled,
					"%s comes from a slot Pando cannot fill.", e.Key).
					WithDetail("slot_key", slot.Key)
			}
		}
	}
	if port, ok := defaultPort(w); ok {
		env["PORT"] = secret.New(port)
	}
	return env, nil
}

// defaultPort is the PORT the primary workload is started with, when the spec
// does not set one itself.
//
// PORT is the convention nearly every platform uses to tell an app where to
// listen, and most frameworks honor it: Express and Koa apps written for a
// platform read it, and gunicorn binds 0.0.0.0:$PORT when it is set rather than
// its own 127.0.0.1:8000. Pando never set it, so a Procfile saying
// `--bind 0.0.0.0:$PORT` started with an empty port, and apps that would have
// listened wherever they were told listened on their own default instead of the
// port Pando routes to (issue #55).
//
// The value is the port Pando already sends traffic to, so this changes where
// an app listens only toward where it is reached. A PORT the spec sets, even an
// empty one somebody typed, is left alone.
func defaultPort(w spec.Workload) (string, bool) {
	if !w.Primary || len(w.Ports) == 0 {
		return "", false
	}
	for _, e := range w.Env {
		if e.Key != "PORT" {
			continue
		}
		// A name read out of .env.example with no value is not a choice.
		unfilled := e.Source == spec.EnvFromDetection && e.Value != nil && *e.Value == ""
		if !unfilled {
			return "", false
		}
	}
	return strconv.Itoa(w.Ports[0].Number), true
}

// healthPlan applies the source precedence in R-221.
func healthPlan(s *spec.AppSpec, w spec.Workload) *api.HealthPlan {
	if w.Health != nil {
		return &api.HealthPlan{
			Command: w.Health.Command, Path: w.Health.Path, Port: w.Health.Port,
			IntervalSeconds: orDefault(w.Health.IntervalSeconds, 30),
			TimeoutSeconds:  orDefault(w.Health.TimeoutSeconds, 5),
			Retries:         orDefault(w.Health.Retries, 3),
		}
	}
	if s.Health.Source == "" || (s.Health.Path == "" && s.Health.Port == 0) {
		// No health signal configured. Not an error and not unhealthy: an app
		// with no health check is running, not perpetually degraded (R-221).
		return nil
	}
	if !w.Primary {
		return nil
	}
	return &api.HealthPlan{
		Path: s.Health.Path, Port: s.Health.Port,
		IntervalSeconds: orDefault(s.Health.IntervalSeconds, 30),
		TimeoutSeconds:  orDefault(s.Health.TimeoutSeconds, 5),
		Retries:         orDefault(s.Health.Retries, 3),
	}
}

// waitForHealth polls until every workload is running and, where a signal
// exists, healthy.
//
// Returns false rather than an error when health does not pass: the app is
// degraded, which is recoverable and still being worked, not failed.
func (r *Runner) waitForHealth(ctx context.Context, runtime api.RuntimeAdapter, appID string, bundle api.BundlePlan, sink io.Writer) (bool, error) {
	started := time.Now()
	deadline := started.Add(2 * time.Minute)
	settledSince := started
	var restarted time.Time

	for {
		observed, err := runtime.Observe(ctx, api.BundleRef{BundleID: appID})
		if err != nil {
			return false, err
		}

		allRunning := len(observed.Workloads) >= len(bundle.Workloads)
		healthy := true
		anyExited := false
		for _, w := range observed.Workloads {
			if !w.Running {
				allRunning = false
			}
			if !w.Running && !w.Restarting && w.ExitCode != nil {
				anyExited = true
			}
			// nil means no signal, which is not unhealthy (R-221).
			if w.Healthy != nil && !*w.Healthy {
				healthy = false
			}
		}

		// A part that stopped while the rest came up is started again, for as
		// long as the deploy is waiting. A backend that could not reach a
		// database still initializing, or a proxy whose upstream was that
		// backend, is how a compose app starts — under `docker compose up`
		// the services' restart policy starts them again, and Pando replaces
		// that policy with its own (issue #55). Applying the same plan starts
		// what is stopped and touches nothing that is running.
		if anyExited && time.Since(restarted) >= exitRestartEvery {
			_, _ = runtime.Apply(ctx, bundle)
			restarted = time.Now()
			settledSince = restarted
		}

		// Up for the settle period, not merely up at the first look. An app
		// that exits a second after it starts is running when it is first
		// observed, and was reported deployed on that one glimpse (issue #55).
		if allRunning && healthy && time.Since(settledSince) >= exitSettle {
			return true, nil
		}
		if time.Now().After(deadline) {
			// Still stopped after being started again for the whole window:
			// the app does not run, and the deploy says so with its output.
			if exited, ok := primaryExited(bundle, observed); ok {
				return false, exitedFailure(ctx, runtime, appID, exited, sink)
			}
			return false, nil
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func needsBuild(s *spec.AppSpec) bool {
	if s.Build.Strategy == spec.BuildPrebuilt {
		return false
	}
	return s.Source.Type != spec.SourceImage
}

// writeFailure puts a failure into the log the person is watching: the
// headline, then why, then what to do.
//
// The build step always printed the cause; the steps after it did not, and an
// adapter's message names only what it could not do. So a deploy that got all
// the way to starting the app ended:
//
//	!! apply failed: Could not create "proxy".
//
// while the reason — a mount the daemon refused — sat in the server's log where
// the person deploying cannot see it. An error nobody can act on is the failure
// R-105 exists to prevent, and the last line of a deploy is the worst place for
// one.
func writeFailure(sink io.Writer, headline string, err error) {
	fmt.Fprintf(sink, "\n!! %s\n", headline)

	if detail := detailOf(err); detail != "" && !strings.Contains(headline, detail) {
		fmt.Fprintf(sink, "   %s\n", detail)
	}
	if e := errs.As(err); e != nil && e.Remedy != "" {
		fmt.Fprintf(sink, "   %s\n", e.Remedy)
	}
}

// imagePull settles what an image app's workloads run and how the pull
// authenticates: the pinned reference in place of the tag, and the app's
// registry credential resolved for this one deploy. Anything that is not an
// image app passes through unchanged.
func imagePull(ctx context.Context, images *oci.Images, appID string, s *spec.AppSpec,
	image string, perWorkload map[string]string, sink io.Writer,
) (string, map[string]string, *api.RegistryAuth, error) {
	if s.Source.Type != spec.SourceImage || s.Source.Image == "" {
		return image, perWorkload, nil, nil
	}
	pinned := oci.Reference(s.Source)
	if s.Source.Digest != "" {
		fmt.Fprintf(sink, "=> Running %s at %s\n", s.Source.Image, shortDigest(s.Source.Digest))
	}
	auth, err := images.Auth(ctx, appID, s.Source.Image)
	if err != nil {
		return "", nil, nil, err
	}
	var pull *api.RegistryAuth
	if auth != nil {
		pull = &api.RegistryAuth{Registry: auth.Registry, Username: auth.Username, Password: auth.Password, IdentityToken: auth.IdentityToken}
	}
	return pinned, pinWorkloads(s, perWorkload, pinned), pull, nil
}

// pinnedSpec is the revision body an unpinned image app deploys from: the
// same spec with the digest its tag names now. False when there is nothing
// to pin — not an image, already pinned, or no way to read the registry.
func pinnedSpec(ctx context.Context, images *oci.Images, appID string, s *spec.AppSpec) (*spec.AppSpec, bool, error) {
	if s.Source.Type != spec.SourceImage || s.Source.Digest != "" || s.Source.Image == "" || images == nil {
		return nil, false, nil
	}
	insp, err := images.Inspect(ctx, appID, s.Source.Image, oci.Platform{})
	if err != nil {
		return nil, false, err
	}
	pinned := *s
	pinned.Source.Digest = insp.Digest
	return &pinned, true, nil
}

// pinWorkloads points every workload that runs the app's image at its pinned
// reference. A workload naming a different image — a sidecar from a compose
// file — is left as it is: the app's pin is a digest of the app's image.
func pinWorkloads(s *spec.AppSpec, perWorkload map[string]string, pinned string) map[string]string {
	out := map[string]string{}
	for k, v := range perWorkload {
		out[k] = v
	}
	for _, w := range s.Workloads {
		if out[w.Name] != "" || w.Build != nil {
			continue
		}
		if w.Image == "" || w.Image == s.Source.Image {
			out[w.Name] = pinned
		}
	}
	return out
}

// withPullAuth gives the app's registry credential to the workloads that run
// the app's image, and to no other: a sidecar's public image is pulled
// anonymously, and a credential for one registry is not sent to another.
func withPullAuth(bundle *api.BundlePlan, image string, auth *api.RegistryAuth) {
	if auth == nil {
		return
	}
	for i := range bundle.Workloads {
		if bundle.Workloads[i].Image == image {
			bundle.Workloads[i].PullAuth = auth
		}
	}
}

// workloadImage is the image one workload runs.
func workloadImage(w spec.Workload, perWorkload map[string]string, appImage string) string {
	if built := perWorkload[w.Name]; built != "" {
		return built
	}
	if w.Image != "" {
		return w.Image
	}
	if w.Build != nil {
		// Built separately, and this deployment does not know what came out of
		// that build. The app-level image is a different workload's.
		return ""
	}
	return appImage
}

// detailOf returns the underlying cause for the build log.
//
// Build failures are the one place an internal cause is worth showing: the
// caller is looking at their own build, and "the build failed" with nothing
// further is unactionable.
func detailOf(err error) string {
	e := errs.As(err)
	if e == nil {
		return err.Error()
	}
	if cause := errors.Unwrap(e); cause != nil {
		return cause.Error()
	}
	return ""
}

func messageOf(err error) string {
	if e := errs.As(err); e != nil {
		return e.Message
	}
	return "The deploy failed."
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func orDefault(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// shortDigest is a digest as a person reads one: the algorithm dropped and
// the first twelve hex characters, the way docker images shows it.
func shortDigest(digest string) string {
	_, hex, found := strings.Cut(digest, ":")
	if !found {
		hex = digest
	}
	if len(hex) > 12 {
		return hex[:12]
	}
	return hex
}

func short(commit string) string {
	if len(commit) > 8 {
		return commit[:8]
	}
	return commit
}

// primaryDigest asks the runtime what the primary workload actually resolved to.
//
// The runtime's own observation rather than anything Pando computed: a tag can
// move, a build can produce a digest nobody predicted, and the only thing worth
// comparing against a running container later is what was running now. Empty on
// any failure, which makes image drift undetectable rather than making every
// workload look wrong.
func primaryDigest(ctx context.Context, runtime api.RuntimeAdapter, s *spec.AppSpec, appID string) string {
	observed, err := runtime.Observe(ctx, api.BundleRef{BundleID: appID})
	if err != nil {
		return ""
	}

	// The primary workload's, where there is one. This used to take whichever
	// running workload Observe listed first, which for a two-service app is a
	// coin toss — and the digest is what "this workload is running the wrong
	// image" is decided by, so getting it from a different workload makes the
	// app permanently wrong in the reconciler's eyes.
	primary, ok := s.PrimaryWorkload()
	if ok {
		for _, w := range observed.Workloads {
			if w.Name == primary.Name && w.ImageDigest != "" {
				return w.ImageDigest
			}
		}
	}

	for _, w := range observed.Workloads {
		if w.Running && w.ImageDigest != "" {
			return w.ImageDigest
		}
	}
	return ""
}

// workloadImages records what each part of the app actually ran.
//
// The reference comes from the plan — what Pando asked for — and the digest
// from the runtime, which is what a running container can be compared against.
// One image is the whole story only for an app built from a single Dockerfile;
// a compose app builds per service, and the reconciler needs to restore each
// one with its own image rather than with the app's.
func workloadImages(ctx context.Context, runtime api.RuntimeAdapter, s *spec.AppSpec,
	perWorkload map[string]string, image, appID string,
) map[string]state.WorkloadImage {
	digests := map[string]string{}
	if observed, err := runtime.Observe(ctx, api.BundleRef{BundleID: appID}); err == nil {
		for _, w := range observed.Workloads {
			digests[w.Name] = w.ImageDigest
		}
	}

	out := map[string]state.WorkloadImage{}
	for _, w := range s.Workloads {
		ref := firstNonEmpty(perWorkload[w.Name], w.Image, image)
		if ref == "" && digests[w.Name] == "" {
			continue
		}
		out[w.Name] = state.WorkloadImage{Ref: ref, Digest: digests[w.Name]}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// recordVolumes writes the volume rows for a deploy, reading handles back from
// the runtime.
//
// Observe rather than Apply's return value, because Apply reports a bundle
// handle and not the volumes inside it — and asking the runtime what exists is
// the honest question anyway. An adapter that created a volume under a name of
// its own choosing is answered here rather than guessed at.
func (r *Runner) recordVolumes(ctx context.Context, runtime api.RuntimeAdapter, s *spec.AppSpec, bundle api.BundlePlan) error {
	observed, err := runtime.Observe(ctx, api.BundleRef{BundleID: bundle.BundleID})
	if err != nil {
		return err
	}

	handles := make(map[string]string, len(observed.Volumes))
	for _, v := range observed.Volumes {
		handles[v.VolumeID] = v.Handle
	}

	// The plan's volumes, not the spec's: a provisioned service's storage is in
	// the plan and nowhere in the spec, and a database volume Pando does not
	// record is a database that is never backed up (R-135).
	records := make([]state.VolumeRecord, 0, len(bundle.Volumes))
	for _, v := range bundle.Volumes {
		records = append(records, state.VolumeRecord{
			VolumeID: v.VolumeID, Name: v.Name, Handle: handles[v.VolumeID],
		})
	}
	return r.volumes.RecordFromRuntime(ctx, s.AppID, s.Runtime.AdapterRef, records)
}

// primaryImage is the image the app's primary workload runs.
//
// A compose app builds one image per service and leaves the spec's top-level
// image empty, so "what is this app running" appears to have no single answer —
// except that it does: the primary workload is the one the proxy sends traffic
// to (R-030), and it is the one a scanner, a rollback and a person all mean.
func primaryImage(s *spec.AppSpec, perWorkload map[string]string) string {
	if s == nil {
		return ""
	}
	if primary, ok := s.PrimaryWorkload(); ok {
		if built, have := perWorkload[primary.Name]; have && built != "" {
			return built
		}
		if primary.Image != "" {
			return primary.Image
		}
	}
	for _, w := range s.Workloads {
		if built, have := perWorkload[w.Name]; have && built != "" {
			return built
		}
	}
	return ""
}

// firstDeploy reports whether the app has never had a successful deploy
// (api.BundlePlan.FirstDeploy), from its own deployments: none succeeded, so
// nothing of it runs anywhere. A runtime spread over several machines may then
// place it while one of them does not answer. Any doubt answers false, which
// is the safe value: a deployed app is never placed afresh (O-46).
func (r *Runner) firstDeploy(ctx context.Context, appID string) bool {
	if r.deploys == nil {
		return false
	}
	running, err := r.deploys.RunningSpecID(ctx, appID)
	return err == nil && running == ""
}
