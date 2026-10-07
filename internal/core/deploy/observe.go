package deploy

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// observeTimeout bounds the post-build trial. A working app is seen listening in
// a few seconds and the trial returns then; this is how long one that never
// listens is waited for.
const observeTimeout = 30 * time.Second

// portCheck is what watching a freshly built app start said about its port.
type portCheck struct {
	// Port replaces the assumed one. Zero means the assumption stands, or
	// nothing better was learned.
	Port int

	// Refusal is set when the app can be seen to be unreachable whatever port
	// Pando picks.
	Refusal error
}

// needsPortCheck reports whether this app's port is a guess a trial can settle.
//
// R-097 says a port is observed, not assumed. For an app with an image the trial
// runs at detection; a source build has nothing to start until it is built, so
// detection falls back on the framework's usual port and says so (design 01
// §2.3). Node gets 3000, Python 8000, and an app on 5000 or 4000 deployed,
// reported success, and answered 502 through the proxy (issue #55). The build is
// the first moment there is something to watch, so this is where the guess is
// checked.
func needsPortCheck(s *spec.AppSpec) bool {
	if s.Build.Strategy != spec.BuildBuildpack {
		return false
	}
	primary, ok := s.PrimaryWorkload()
	if !ok || len(primary.Ports) == 0 {
		return false
	}
	return primary.Ports[0].Source == spec.PortFramework
}

// checkPort starts the built image once and watches where it listens.
//
// It is told the assumed port through PORT, the same as the real start, so an
// app that honors PORT is seen on it and nothing changes. Only an app that
// ignores PORT and listens somewhere else has its port corrected — to the one
// it was watched using, recorded as observed.
//
// Configuration the spec holds as plain values is passed; secrets and slots are
// not, because resolving them is a step of its own (step 11) and this runs
// before it. An app that cannot start without them tells this nothing, and the
// assumption stands.
func checkPort(ctx context.Context, runtime api.RuntimeAdapter, s *spec.AppSpec, image, trialID string, sink io.Writer) portCheck {
	return checkPortWith(ctx, runtime, s, image, nil, trialID, sink)
}

// checkPortWith is checkPort for an image the runtime pulls with auth: a
// build pushed to the install's registry (issue #72, PR 5).
func checkPortWith(ctx context.Context, runtime api.RuntimeAdapter, s *spec.AppSpec, image string, auth *api.RegistryAuth, trialID string, sink io.Writer) portCheck {
	caps, err := runtime.Capabilities(ctx)
	if err != nil || !caps.SupportsTrialRun || !caps.SupportsPortObservation {
		return portCheck{}
	}
	primary, _ := s.PrimaryWorkload()
	assumed := primary.Ports[0].Number

	env := map[string]secret.Value{}
	for _, e := range primary.Env {
		if e.Value != nil && *e.Value != "" {
			env[e.Key] = secret.New(*e.Value)
		}
	}
	if port, ok := defaultPort(primary); ok {
		env["PORT"] = secret.New(port)
	}

	fmt.Fprintf(sink, "=> Checking which port the app listens on\n")
	result, err := runtime.Trial(ctx, api.TrialRequest{
		TrialID:        trialID,
		Image:          image,
		PullAuth:       auth,
		Command:        primary.Command,
		Entrypoint:     primary.Entrypoint,
		WorkingDir:     primary.WorkingDir,
		Env:            env,
		Timeout:        observeTimeout,
		IsolationFloor: s.Runtime.IsolationFloor,
	})
	if err != nil {
		// Not a reason to stop the deploy: the assumption is what would have
		// been used without the check.
		return portCheck{}
	}

	for _, p := range result.ObservedPorts {
		if p == assumed {
			return portCheck{}
		}
	}
	if len(result.ObservedPorts) > 0 {
		observed := result.ObservedPorts[0]
		fmt.Fprintf(sink, "   The app listens on port %d, not %d. Pando will send traffic to %d.\n",
			observed, assumed, observed)
		return portCheck{Port: observed}
	}
	if len(result.LoopbackPorts) > 0 {
		return portCheck{Refusal: loopbackRefusal(result.LoopbackPorts)}
	}
	return portCheck{}
}

// loopbackRefusal explains an app that listens where nothing else can reach it.
//
// gunicorn, uvicorn and several Node servers default to 127.0.0.1. Inside a
// container that is the container's own loopback, which Pando's proxy cannot
// reach, so the app deployed, reported success and answered 502 with nothing
// saying why (issue #55). Deploying it anyway produces exactly that again, so
// this stops before the running version is replaced.
func loopbackRefusal(ports []int) error {
	listed := make([]string, 0, len(ports))
	for _, p := range ports {
		listed = append(listed, strconv.Itoa(p))
	}
	return errs.Newf(errs.BuildListensOnLoopback,
		"This app started, but it listens only on 127.0.0.1 (port %s) inside its container, "+
			"so nothing outside the container can reach it, including Pando.",
		strings.Join(listed, ", ")).
		WithRemedy("Make the app listen on 0.0.0.0 instead of 127.0.0.1 or localhost. For example: " +
			"`uvicorn main:app --host 0.0.0.0`, `gunicorn app:app --bind 0.0.0.0:$PORT`, " +
			"or `app.listen(process.env.PORT, '0.0.0.0')` in Node.")
}

// exitSettle is how long every part of a new app has to stay up before the
// deploy counts it as running.
const exitSettle = 10 * time.Second

// exitRestartEvery is how often a deploy starts again a part of the app that
// stopped while it was waiting for the app to come up.
const exitRestartEvery = 5 * time.Second

// primaryExited returns the primary workload if it has stopped with an exit
// code, which is a process that ran and ended, not one still starting.
//
// The primary workload is the one traffic goes to (R-030), and the plan marks
// it as the exposed one. Every static site deployed a server that exited at
// once on a broken config, and the deploy reported success (issue #55):
// "degraded" after two minutes is what an app slow to become healthy gets, and
// an app that is not running at all is a different thing.
func primaryExited(bundle api.BundlePlan, observed api.ObservedBundle) (api.ObservedWorkload, bool) {
	var primary string
	for _, w := range bundle.Workloads {
		if w.Exposed {
			primary = w.Name
			break
		}
	}
	for _, w := range observed.Workloads {
		if w.Name == primary && w.Present && !w.Running && !w.Restarting && w.ExitCode != nil {
			return w, true
		}
	}
	return api.ObservedWorkload{}, false
}

// exitedFailure says the app stopped, and shows what it said before it did.
// The last lines of its own output are nearly always the reason, and a deploy
// log that ends without them sends somebody looking for the logs of an app
// that is no longer running.
func exitedFailure(ctx context.Context, runtime api.RuntimeAdapter, appID string, w api.ObservedWorkload, sink io.Writer) error {
	if logs, err := runtime.Logs(ctx, api.WorkloadRef{BundleID: appID, Workload: w.Name}, api.LogOptions{Tail: 30}); err == nil {
		fmt.Fprintf(sink, "\n-- the last lines %q wrote before it stopped --\n", w.Name)
		_, _ = io.Copy(sink, io.LimitReader(logs, 16<<10))
		_ = logs.Close()
	}
	return errs.Newf(errs.StateAppExited,
		"The app started and then stopped with exit code %d, so there is nothing to send traffic to.", *w.ExitCode).
		WithDetail("workload", w.Name).
		WithRemedy("The app's last output is in the deploy log above, and usually names the reason — a missing setting, " +
			"a port already in use, or a command that finishes instead of running a server.")
}

// withObservedPort is the spec with the primary workload's port replaced by the
// one the app was watched listening on.
func withObservedPort(s *spec.AppSpec, port int) *spec.AppSpec {
	out := *s
	out.Workloads = append([]spec.Workload(nil), s.Workloads...)
	for i := range out.Workloads {
		if !out.Workloads[i].Primary {
			continue
		}
		ports := append([]spec.Port(nil), out.Workloads[i].Ports...)
		ports[0] = spec.Port{Number: port, Protocol: "http", Source: spec.PortObserved}
		out.Workloads[i].Ports = ports
		break
	}
	return &out
}
