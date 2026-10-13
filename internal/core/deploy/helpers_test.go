package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// In-package: these are the small decisions a deploy makes on the way past —
// whether to build at all, what to tell someone when it failed, and what the
// runtime says is actually running. Each is unexported and each is wrong in a
// way that is quiet.

// R-221: an app with no health check is running, not perpetually degraded. "No
// signal" and "unhealthy" must not collapse.
func TestR221_NoHealthConfiguredMeansNoHealthPlan(t *testing.T) {
	s := &spec.AppSpec{}
	require.Nil(t, healthPlan(s, spec.Workload{Name: "web", Primary: true}))

	// A source with nothing to probe is still no signal.
	require.Nil(t, healthPlan(&spec.AppSpec{
		Health: spec.Health{Source: spec.HealthSource("http")},
	}, spec.Workload{Name: "web", Primary: true}))
}

// The app-level check describes the app's own endpoint, so it belongs to the
// primary workload and to nothing else.
func TestTheAppLevelHealthCheckAppliesToThePrimaryWorkloadOnly(t *testing.T) {
	s := &spec.AppSpec{Health: spec.Health{
		Source: spec.HealthSource("http"), Path: "/healthz", Port: 3000,
	}}

	plan := healthPlan(s, spec.Workload{Name: "web", Primary: true})
	require.NotNil(t, plan)
	require.Equal(t, "/healthz", plan.Path)
	require.Equal(t, 3000, plan.Port)

	require.Nil(t, healthPlan(s, spec.Workload{Name: "worker"}),
		"a worker is not the app's endpoint")
}

// A workload's own check wins, and applies whether or not it is primary.
func TestAWorkloadsOwnHealthCheckWinsOverTheApps(t *testing.T) {
	s := &spec.AppSpec{Health: spec.Health{
		Source: spec.HealthSource("http"), Path: "/healthz", Port: 3000,
	}}
	w := spec.Workload{Name: "worker", Health: &spec.Healthcheck{
		Command: []string{"pg_isready"}, IntervalSeconds: 5, TimeoutSeconds: 2, Retries: 10,
	}}

	plan := healthPlan(s, w)
	require.NotNil(t, plan)
	require.Equal(t, []string{"pg_isready"}, plan.Command)
	require.Empty(t, plan.Path, "the app's path does not leak into a workload's own check")
	require.Equal(t, 5, plan.IntervalSeconds)
	require.Equal(t, 2, plan.TimeoutSeconds)
	require.Equal(t, 10, plan.Retries)
}

// A check that states only what it cares about gets sensible timings rather
// than zeros, which a runtime would read as "probe constantly" or "never".
func TestUnstatedHealthTimingsFallBackRatherThanBeingZero(t *testing.T) {
	plan := healthPlan(&spec.AppSpec{}, spec.Workload{
		Name: "web", Health: &spec.Healthcheck{Path: "/healthz", Port: 3000},
	})
	require.NotNil(t, plan)
	require.Equal(t, 30, plan.IntervalSeconds)
	require.Equal(t, 5, plan.TimeoutSeconds)
	require.Equal(t, 3, plan.Retries)

	require.Equal(t, 7, orDefault(7, 30))
	require.Equal(t, 30, orDefault(0, 30))
	require.Equal(t, 30, orDefault(-1, 30), "a negative interval is not an interval")
}

// A prebuilt image is run as it is, and an image source has nothing to build
// from. Building either would be work nobody asked for, against a source that
// is not there.
func TestNothingIsBuiltForAnImageThatAlreadyExists(t *testing.T) {
	require.False(t, needsBuild(&spec.AppSpec{
		Build: spec.Build{Strategy: spec.BuildPrebuilt},
	}))
	require.False(t, needsBuild(&spec.AppSpec{
		Source: spec.Source{Type: spec.SourceImage},
	}))

	require.True(t, needsBuild(&spec.AppSpec{
		Source: spec.Source{Type: spec.SourceGit},
		Build:  spec.Build{Strategy: spec.BuildDockerfile},
	}))
	require.True(t, needsBuild(&spec.AppSpec{
		Source: spec.Source{Type: spec.SourceUpload},
		Build:  spec.Build{Strategy: spec.BuildBuildpack},
	}))
}

// Build failures are the one place an internal cause is worth showing: the
// caller is looking at their own build, and "the build failed" with nothing
// further is unactionable.
func TestABuildFailureCarriesItsUnderlyingCause(t *testing.T) {
	cause := errors.New("exit status 1: npm ERR! missing script: build")

	require.Equal(t, cause.Error(),
		detailOf(errs.Wrap(errs.BuildFailed, "The build failed.", cause)))

	// An envelope with nothing wrapped has no further detail to give, and
	// inventing one would be worse than saying nothing.
	require.Empty(t, detailOf(errs.New(errs.BuildFailed, "The build failed.")))

	// An error carrying no envelope at all is still rendered rather than lost.
	require.Equal(t, "connection refused", detailOf(errors.New("connection refused")))
}

// The message is the one held to the R-105 standard; an adapter under no
// obligation to produce an envelope must not leave the caller with nothing.
func TestTheDeployMessageIsAlwaysSomethingAPersonCanRead(t *testing.T) {
	require.Equal(t, "The build timed out after 30 minutes.",
		messageOf(errs.New(errs.BuildTimeout, "The build timed out after 30 minutes.")))

	require.Equal(t, "The deploy failed.", messageOf(errors.New("dial unix /var/run/docker.sock")))
}

func TestFirstNonEmptyPicksTheFirstThingThatWasActuallySet(t *testing.T) {
	require.Equal(t, "explicit", firstNonEmpty("", "explicit", "fallback"))
	require.Equal(t, "fallback", firstNonEmpty("", "", "fallback"))
	require.Empty(t, firstNonEmpty("", "", ""))
	require.Empty(t, firstNonEmpty())
}

func TestACommitIsShortenedForDisplayWithoutBeingCorrupted(t *testing.T) {
	require.Equal(t, "1234abcd", short("1234abcdef567890"))
	require.Equal(t, "1234abcd", short("1234abcd"), "exactly eight is already short")
	require.Equal(t, "abc", short("abc"), "an upload has no commit, and a short one is not padded")
	require.Empty(t, short(""))
}

// observing is a runtime that answers Observe and nothing else.
type observing struct {
	api.RuntimeAdapter
	bundle api.ObservedBundle
	err    error
}

func (o observing) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	return o.bundle, o.err
}

// The runtime's own observation rather than anything Pando computed: a tag can
// move, and the only thing worth comparing against a running container later is
// what was running now.
func TestThePrimaryDigestComesFromTheRuntime(t *testing.T) {
	got := primaryDigest(context.Background(), observing{bundle: api.ObservedBundle{
		Workloads: []api.ObservedWorkload{
			{Name: "worker", Present: true, Running: false, ImageDigest: "sha256:stopped"},
			{Name: "web", Present: true, Running: true, ImageDigest: "sha256:running"},
		},
	}}, digestSpec("web"), "app_01HQ8")

	require.Equal(t, "sha256:running", got, "a stopped workload's image is not what is running")
}

// TestThePrimaryDigestIsThePrimaryWorkloads asserts that it is that workload's.
//
// It used to be whichever running workload the runtime listed first, which for
// a two-service app is a coin toss — and this digest is what "running the wrong
// image" is decided by, so taking it from the proxy made the application
// container permanently wrong, and the correction for that replaced it with a
// second copy of the proxy.
func TestThePrimaryDigestIsThePrimaryWorkloads(t *testing.T) {
	got := primaryDigest(context.Background(), observing{bundle: api.ObservedBundle{
		Workloads: []api.ObservedWorkload{
			{Name: "proxy", Present: true, Running: true, ImageDigest: "sha256:caddy"},
			{Name: "app", Present: true, Running: true, ImageDigest: "sha256:theapp"},
		},
	}}, digestSpec("app"), "app_01HQ8")

	require.Equal(t, "sha256:theapp", got)
}

// A spec whose primary workload is the named one.
func digestSpec(primary string) *spec.AppSpec {
	return &spec.AppSpec{Workloads: []spec.Workload{
		{Name: "proxy"},
		{Name: "app"},
		{Name: primary, Primary: true},
	}}
}

// Empty on any failure, which makes image drift undetectable rather than making
// every workload look wrong.
func TestAnUnobservableRuntimeYieldsNoDigestRatherThanAWrongOne(t *testing.T) {
	ctx := context.Background()

	s := digestSpec("web")
	require.Empty(t, primaryDigest(ctx, observing{err: errors.New("daemon is not running")}, s, "app_01HQ8"))
	require.Empty(t, primaryDigest(ctx, observing{bundle: api.ObservedBundle{}}, s, "app_01HQ8"))

	// Running, but the runtime does not report a digest.
	require.Empty(t, primaryDigest(ctx, observing{bundle: api.ObservedBundle{
		Workloads: []api.ObservedWorkload{{Name: "web", Present: true, Running: true}},
	}}, s, "app_01HQ8"))
}

// TestR105_AFailedStepSaysWhyInTheLogSomebodyIsWatching asserts R-105.
//
// `!! apply failed: Could not create "proxy".` was the whole of what a real
// deploy said. The reason — a mount the Docker daemon refused — was in the
// error that message wrapped, and went only to the server's log. The person
// deploying has the deploy log and nothing else.
func TestR105_AFailedStepSaysWhyInTheLogSomebodyIsWatching(t *testing.T) {
	err := errs.Wrap(errs.AdapterFailed, `Could not create "proxy".`,
		errors.New("Error response from daemon: source /var/lib/docker/x is not directory")).
		WithRemedy("Remove that mount in the app's storage settings.")

	var log strings.Builder
	writeFailure(&log, "apply failed: "+messageOf(err), err)

	out := log.String()
	require.Contains(t, out, `!! apply failed: Could not create "proxy".`)
	require.Contains(t, out, "is not directory", "why")
	require.Contains(t, out, "storage settings", "and what to do")
}

// A cause already contained in the headline is not repeated. Two lines saying
// the same thing read as two problems.
func TestAFailureDoesNotSayTheSameThingTwice(t *testing.T) {
	err := errs.Newf(errs.PlanSlotUnfilled, "This app needs a PostgreSQL database.")

	var log strings.Builder
	writeFailure(&log, messageOf(err), err)

	require.Equal(t, "\n!! This app needs a PostgreSQL database.\n", log.String())
}

// TestR130_AVariableNobodyFilledInIsNotSetToNothing asserts that a declared
// hole stays a hole.
//
// Detection reads the names of an app's variables out of `.env.example` and has
// no values to put in them. Passing those through as empty strings is not the
// same as leaving them unset, and the difference decides what the app does:
// `if "APP_BASE_URL" in os.environ` is true for an empty one. An empty value a
// person typed is an answer and is kept.
func TestR130_AVariableNobodyFilledInIsNotSetToNothing(t *testing.T) {
	empty := ""
	filled := "info"

	w := spec.Workload{
		Name: "web",
		Env: []spec.EnvEntry{
			{Key: "APP_BASE_URL", Value: &empty, Source: spec.EnvFromDetection},
			{Key: "LOG_LEVEL", Value: &filled, Source: spec.EnvFromDetection},
			{Key: "EMPTY_ON_PURPOSE", Value: &empty, Source: spec.EnvFromUser},
		},
	}

	env, err := resolveEnv(&spec.AppSpec{Workloads: []spec.Workload{w}}, w, nil, provisioned{})
	require.NoError(t, err)

	_, declared := env["APP_BASE_URL"]
	require.False(t, declared, "a name with no value never reaches the container")
	require.Equal(t, "info", env["LOG_LEVEL"].Reveal())

	set, ok := env["EMPTY_ON_PURPOSE"]
	require.True(t, ok, "somebody chose this")
	require.Equal(t, "", set.Reveal())
}

// TestR131_EachPartOfADatabaseComesFromItsOneSlot asserts R-131. PGHOST,
// PGUSER and PGPASSWORD are three variables and one database: each takes its
// part of the slot's connection string, whichever way the slot was filled, and
// a value that is not a URL is refused by name rather than set to nothing.
func TestR131_EachPartOfADatabaseComesFromItsOneSlot(t *testing.T) {
	ref := "PGHOST"
	w := spec.Workload{Name: "web", Env: []spec.EnvEntry{
		{Key: "PGHOST", SlotRef: &ref, SlotPart: spec.SlotPartHost},
		{Key: "PGPORT", SlotRef: &ref, SlotPart: spec.SlotPartPort},
		{Key: "PGUSER", SlotRef: &ref, SlotPart: spec.SlotPartUser},
		{Key: "PGPASSWORD", SlotRef: &ref, SlotPart: spec.SlotPartPassword},
		{Key: "PGDATABASE", SlotRef: &ref, SlotPart: spec.SlotPartDatabase},
	}}
	s := &spec.AppSpec{Workloads: []spec.Workload{w}, Slots: []spec.Slot{{Key: "PGHOST", Type: spec.SlotPostgres,
		Required: true, Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned}}}}
	svcs := provisioned{connections: map[string]secret.Value{
		"PGHOST": secret.New("postgresql://app:s3cret@svc-01hq9/app"),
	}}
	env, err := resolveEnv(s, w, nil, svcs)
	require.NoError(t, err)
	got := map[string]string{}
	for k, v := range env {
		got[k] = v.Reveal()
	}
	require.Equal(t, map[string]string{
		"PGHOST": "svc-01hq9", "PGPORT": "5432", "PGUSER": "app", "PGPASSWORD": "s3cret", "PGDATABASE": "app",
	}, got)

	s.Slots[0].Resolution = &spec.Resolution{Mode: spec.ResolutionBound, Target: "not a url"}
	_, err = resolveEnv(s, w, nil, svcs)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "PGHOST")
}

// The primary workload is told which port Pando routes to, unless the spec says
// otherwise. A Procfile with `--bind 0.0.0.0:$PORT` started with an empty port
// before this (issue #55).
func TestR097_ThePrimaryWorkloadIsToldItsPort(t *testing.T) {
	web := spec.Workload{Name: "web", Primary: true, Ports: []spec.Port{{Number: 8000}}}
	env, err := resolveEnv(&spec.AppSpec{Workloads: []spec.Workload{web}}, web, nil, provisioned{})
	require.NoError(t, err)
	require.Equal(t, "8000", env["PORT"].Reveal())

	worker := spec.Workload{Name: "worker", Ports: []spec.Port{{Number: 9000}}}
	env, err = resolveEnv(&spec.AppSpec{Workloads: []spec.Workload{worker}}, worker, nil, provisioned{})
	require.NoError(t, err)
	_, set := env["PORT"]
	require.False(t, set, "only the workload traffic is routed to")

	chosen := "5000"
	own := spec.Workload{Name: "web", Primary: true, Ports: []spec.Port{{Number: 8000}},
		Env: []spec.EnvEntry{{Key: "PORT", Value: &chosen, Source: spec.EnvFromUser}}}
	env, err = resolveEnv(&spec.AppSpec{Workloads: []spec.Workload{own}}, own, nil, provisioned{})
	require.NoError(t, err)
	require.Equal(t, "5000", env["PORT"].Reveal(), "a PORT the spec sets is kept")
}

// TestR148_EachPartOfAnAppRecordsItsOwnImage asserts R-148.
//
// The reconciler restores a missing workload from the image the deployment
// recorded. With one image for the whole app that was the primary workload's,
// so a compose app's application container was restored from its proxy's
// image — and then, being "wrong", recreated from it on every tick.
func TestR148_EachPartOfAnAppRecordsItsOwnImage(t *testing.T) {
	s := &spec.AppSpec{Workloads: []spec.Workload{
		{Name: "app", Build: &spec.WorkloadBuild{Context: "."}},
		{Name: "proxy", Image: "caddy:2-alpine", Primary: true},
	}}

	got := workloadImages(context.Background(), observing{bundle: api.ObservedBundle{
		Workloads: []api.ObservedWorkload{
			{Name: "app", ImageDigest: "sha256:theapp"},
			{Name: "proxy", ImageDigest: "sha256:caddy"},
		},
	}}, s, map[string]string{"app": "pando/app:latest"}, "", "app_01HQ8")

	require.Equal(t, "pando/app:latest", got["app"].Ref)
	require.Equal(t, "sha256:theapp", got["app"].Digest)
	require.Equal(t, "caddy:2-alpine", got["proxy"].Ref)
	require.Equal(t, "sha256:caddy", got["proxy"].Digest)
}

// A workload that builds its own image never takes the app's.
//
// The app-level image belongs to the app-level build, and a compose app has
// none — so for one of its services, "the app's image" is another service's.
// Empty is the honest answer: nothing starts a workload from an image Pando
// cannot name.
func TestAWorkloadThatBuildsItsOwnDoesNotTakeTheApps(t *testing.T) {
	building := spec.Workload{Name: "app", Build: &spec.WorkloadBuild{Context: "."}}
	require.Empty(t, workloadImage(building, nil, "caddy:2-alpine"))
	require.Equal(t, "pando/app:latest",
		workloadImage(building, map[string]string{"app": "pando/app:latest"}, "caddy:2-alpine"))

	// One that names an image keeps it, and one that does neither is the
	// single-image app the app-level build produced.
	named := spec.Workload{Name: "proxy", Image: "caddy:2-alpine"}
	require.Equal(t, "caddy:2-alpine", workloadImage(named, nil, "pando/app:latest"))

	plain := spec.Workload{Name: "web"}
	require.Equal(t, "pando/app:latest", workloadImage(plain, nil, "pando/app:latest"))
}
