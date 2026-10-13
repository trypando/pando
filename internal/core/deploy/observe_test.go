package deploy

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// trialRuntime answers a trial with a fixed result and records what it was
// asked to start.
type trialRuntime struct {
	api.RuntimeAdapter
	result api.TrialResult
	asked  api.TrialRequest
}

func (r *trialRuntime) Capabilities(context.Context) (api.RuntimeCapabilities, error) {
	return api.RuntimeCapabilities{SupportsTrialRun: true, SupportsPortObservation: true}, nil
}

func (r *trialRuntime) Trial(_ context.Context, req api.TrialRequest) (api.TrialResult, error) {
	r.asked = req
	return r.result, nil
}

func assumedPortApp(port int) *spec.AppSpec {
	return &spec.AppSpec{
		Build: spec.Build{Strategy: spec.BuildBuildpack},
		Workloads: []spec.Workload{{
			Name: "web", Primary: true, Exposed: true,
			Ports: []spec.Port{{Number: port, Protocol: "http", Source: spec.PortFramework}},
		}},
	}
}

// TestR097_ABuildpackAppsAssumedPortIsCheckedAgainstTheBuiltImage asserts R-097.
//
// A source build has no image at detection, so its port is the framework's
// usual one. Apps on 5000 or 4000 deployed, reported success, and answered 502
// (issue #55). The built image is watched, and the port it listens on wins.
func TestR097_ABuildpackAppsAssumedPortIsCheckedAgainstTheBuiltImage(t *testing.T) {
	app := assumedPortApp(8000)
	require.True(t, needsPortCheck(app))

	rt := &trialRuntime{result: api.TrialResult{Started: true, ObservedPorts: []int{5000}}}
	check := checkPort(context.Background(), rt, app, "img", "t1", io.Discard)
	require.Equal(t, 5000, check.Port)
	require.NoError(t, check.Refusal)
	require.Equal(t, "8000", rt.asked.Env["PORT"].Reveal(), "watched the way it will run")

	corrected := withObservedPort(app, check.Port)
	require.Equal(t, 5000, corrected.Workloads[0].Ports[0].Number)
	require.Equal(t, spec.PortObserved, corrected.Workloads[0].Ports[0].Source)
	require.Equal(t, 8000, app.Workloads[0].Ports[0].Number, "the original spec is not modified")
	require.False(t, needsPortCheck(corrected), "an observed port is not checked again")
}

func TestAnAppListeningWhereItWasToldIsLeftAlone(t *testing.T) {
	rt := &trialRuntime{result: api.TrialResult{Started: true, ObservedPorts: []int{3000}}}
	check := checkPort(context.Background(), rt, assumedPortApp(3000), "img", "t1", io.Discard)
	require.Zero(t, check.Port)
	require.NoError(t, check.Refusal)
}

// An app that listens only on 127.0.0.1 cannot be reached from outside its
// container. It used to deploy, report success and answer 502 with nothing
// saying why.
func TestR105_AnAppListeningOnlyOnLoopbackIsRefusedWithTheFix(t *testing.T) {
	rt := &trialRuntime{result: api.TrialResult{Started: true, LoopbackPorts: []int{8000}}}
	check := checkPort(context.Background(), rt, assumedPortApp(8000), "img", "t1", io.Discard)

	e := errs.As(check.Refusal)
	require.NotNil(t, e)
	require.Equal(t, errs.BuildListensOnLoopback, e.Code)
	require.Contains(t, e.Message, "127.0.0.1")
	require.Contains(t, e.Remedy, "0.0.0.0")
}

// A primary workload that stopped is a failed deploy, not a degraded one.
// Every static site's server exited at once on a broken config and the deploy
// reported success (issue #55).
func TestR146_AnAppWhosePrimaryExitedIsNotReportedAsDeployed(t *testing.T) {
	code := 1
	bundle := api.BundlePlan{Workloads: []api.WorkloadPlan{{Name: "web", Exposed: true}, {Name: "worker"}}}

	exited, ok := primaryExited(bundle, api.ObservedBundle{Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, ExitCode: &code},
		{Name: "worker", Present: true, Running: true},
	}})
	require.True(t, ok)
	require.Equal(t, "web", exited.Name)

	_, ok = primaryExited(bundle, api.ObservedBundle{Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true, Running: true},
		{Name: "worker", Present: true, ExitCode: &code},
	}})
	require.False(t, ok, "only the workload traffic goes to decides this")

	_, ok = primaryExited(bundle, api.ObservedBundle{Workloads: []api.ObservedWorkload{
		{Name: "web", Present: true},
	}})
	require.False(t, ok, "not started yet is not exited")
}

func TestOnlyAGuessedBuildpackPortIsChecked(t *testing.T) {
	declared := assumedPortApp(8080)
	declared.Workloads[0].Ports[0].Source = spec.PortUser
	require.False(t, needsPortCheck(declared), "a person's answer is not second-guessed")

	dockerfile := assumedPortApp(8080)
	dockerfile.Build.Strategy = spec.BuildDockerfile
	require.False(t, needsPortCheck(dockerfile), "an image-backed app was watched at detection")
}

// TestR097_ThePortThatServesAPageIsRoutedToFirst asserts R-097 at deploy: of
// the ports that answered HTTP, one that serves a page is chosen over a lower
// one answering 404, as detection ranks them. A metrics port on 3001 was routed
// to over the site on 9000 for being the lower number.
func TestR097_ThePortThatServesAPageIsRoutedToFirst(t *testing.T) {
	got := listeningPorts(api.TrialResult{
		ObservedPorts: []int{3001, 9000},
		HTTPAnswers:   []api.HTTPAnswer{{Port: 3001, Status: 404}, {Port: 9000, Status: 200}},
	}, true)
	require.Equal(t, []int{9000, 3001}, got)

	got = listeningPorts(api.TrialResult{ObservedPorts: []int{3001, 9000}}, false)
	require.Equal(t, []int{3001, 9000}, got, "without a probe, what was observed stands")
}
