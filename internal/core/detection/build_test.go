package detection

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// TestR311_ProgressReportsEveryStepUnderWay asserts that a running detection
// says what it is doing when two steps run at once — the source scan beside the
// build, the image scan beside the trial run — in the order they happen, and
// keeps the proposal as far as it has got.
func TestR311_ProgressReportsEveryStepUnderWay(t *testing.T) {
	t.Parallel()
	var saved []map[string]any
	p := &progress{side: map[string]bool{}, save: func(body map[string]any) { saved = append(saved, body) }}

	p.during(detect.StageScanning, true) // before any stage: nothing to say yet
	require.Empty(t, saved)

	p.report(detect.StageBuilding, &detect.Proposal{Status: "running"})
	require.Equal(t, []string{detect.StageBuilding, detect.StageScanning}, saved[len(saved)-1]["running"])
	require.Equal(t, "running", saved[len(saved)-1]["status"], "the proposal so far")

	p.report(detect.StageTrying, nil)
	last := saved[len(saved)-1]
	require.Equal(t, detect.StageTrying, last["stage"])
	require.Equal(t, []string{detect.StageTrying, detect.StageScanning}, last["running"])
	require.Equal(t, "running", last["status"], "kept from the last report that had one")

	p.during(detect.StageScanning, false)
	require.Equal(t, []string{detect.StageTrying}, saved[len(saved)-1]["running"])

	var none *progress
	none.during(detect.StageScanning, true)
}

// partScanner scans the source until it is canceled, and fails the image.
type partScanner struct {
	recorded []api.ScanRequest
	failed   error
	started  chan struct{}
}

func (p *partScanner) Scans() bool                                 { return true }
func (p *partScanner) ScanSource(context.Context, api.ScanRequest) {}
func (p *partScanner) ScanPart(ctx context.Context, req api.ScanRequest) (api.ScanResult, error) {
	if req.SourceDir != "" {
		close(p.started)
		<-ctx.Done()
		return api.ScanResult{}, ctx.Err()
	}
	return api.ScanResult{}, errs.New(errs.AdapterFailed, "The scanner could not read the image.")
}
func (p *partScanner) RecordParts(_ context.Context, req api.ScanRequest, _ []api.ScanResult, failed error) {
	p.recorded = append(p.recorded, req)
	p.failed = failed
}

// TestR311_AScanBesideABuildThatFailedIsStopped asserts that a detection whose
// build failed does not leave the scan beside it running: stop cancels it and
// waits for it, and nothing is recorded for an app that will not be proposed.
// A scan that failed is recorded as failed, with no score (R-318).
func TestR311_AScanBesideABuildThatFailedIsStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	scanner := &partScanner{started: make(chan struct{})}
	r := &Runner{Scanner: scanner}

	scans := r.newScans("app_1", "abc", nil)
	scans.startSource(ctx, "/tmp/checkout")
	scans.startSource(ctx, "/tmp/checkout") // once
	<-scanner.started
	stopped := make(chan struct{})
	go func() { scans.stop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("stop did not cancel the scan under way")
	}

	failing := r.newScans("app_1", "abc", nil)
	failing.startImage(ctx, "sha256:1", nil)
	require.True(t, failing.record(ctx))
	require.Len(t, scanner.recorded, 1)
	require.Equal(t, "sha256:1", scanner.recorded[0].Image)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(scanner.failed))

	var none *scanParts
	none.startSource(ctx, "/tmp")
	none.startImage(ctx, "sha256:1", nil)
	none.stop()
	require.False(t, none.began())
	require.False(t, none.record(ctx))
	require.Nil(t, (&Runner{}).newScans("app_1", "abc", nil))
}

func TestR107_ABuildLogKeepsItsEnd(t *testing.T) {
	t.Parallel()
	log := &tail{max: 8}
	_, _ = log.Write([]byte("0123456789"))
	_, _ = log.Write([]byte("ab"))
	require.Equal(t, "456789ab", log.String())
}

// TestR107_ABuildFailureSaysWhyAndCarriesItsLog asserts R-107's pattern for a
// build at detection, held to R-105: a sentence that stands alone, what to do,
// and the build's own output.
func TestR107_ABuildFailureSaysWhyAndCarriesItsLog(t *testing.T) {
	t.Parallel()
	err := buildFailure(errs.New(errs.BuildFailed, "The RUN npm ci step exited 1.").WithRemedy("Fix the lockfile."), "#7 ERROR")
	e := errs.As(err)
	require.Equal(t, errs.BuildFailed, e.Code)
	require.True(t, strings.HasSuffix(e.Message, "The RUN npm ci step exited 1."))
	require.Equal(t, "Fix the lockfile.", e.Remedy)
	require.Equal(t, "#7 ERROR", e.Details["build_log"])

	plain := errs.As(buildFailure(errors.New("boom"), ""))
	require.Contains(t, plain.Message, "the builder did not say why")
	require.Contains(t, plain.Remedy, "build log")

	require.Equal(t, map[string]any{"message": "boom"}, failureBody(errors.New("boom"))["error"])
	require.Equal(t, e, failureBody(err)["error"])
}
