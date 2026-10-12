//go:build integration

package deploy

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// answeringScreener is an AI adapter that answers the build argument a
// Dockerfile needs, citing the Dockerfile.
type answeringScreener struct{}

func (answeringScreener) Capabilities(context.Context) (api.AICapabilities, error) {
	return api.AICapabilities{Functions: []api.AIFunction{api.AIFunctionAnswerQuestions}}, nil
}
func (answeringScreener) RepairPlan(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{}, nil
}
func (answeringScreener) RevisePlan(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{}, nil
}
func (answeringScreener) AnswerQuestions(context.Context, api.ScreenRequest) (api.ScreenResult, error) {
	return api.ScreenResult{Amendments: []api.Amendment{{
		Kind: api.AmendAnswerQuestion, Key: detect.BuildArgKeyPrefix + "TOKEN", Value: "from-the-readme",
		Reason:   "The Dockerfile stops without TOKEN, and any non-empty value lets the build finish.",
		Evidence: []string{"Dockerfile"},
	}}}, nil
}

func proposalOf(t *testing.T, d state.Detection) detect.Proposal {
	t.Helper()
	var p detect.Proposal
	require.NoError(t, json.Unmarshal(d.Body, &p))
	return p
}

// TestR311_AnAnswerFromScreeningBuildsThePlanAsItStands asserts R-311 after
// screening (R-336): an AI adapter's suggestion counts as an answer until a
// person gives one (R-338), so a build argument it answered is built with,
// and the plan carries that image's score.
func TestR311_AnAnswerFromScreeningBuildsThePlanAsItStands(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := repoWith(t, buildArgRepo)
	h := newScanHarnessAt(t, repo, commit)
	d := h.detector()
	d.Screener, d.ScreenerRef = answeringScreener{}, "ai"

	det, err := d.Detect(ctx, h.app.ID)
	require.NoError(t, err)
	p := proposalOf(t, det)
	require.NotNil(t, p.Built, "built with the suggested answer")
	builds := h.builder.builds()
	require.Len(t, builds, 1)
	require.Equal(t, "from-the-readme", builds[0].Args["TOKEN"])

	plan, err := h.service.Report(ctx, h.app.ID, "")
	require.NoError(t, err)
	require.False(t, plan.AwaitingImage)
	require.Equal(t, security.ScopeImageAndSource, plan.Scope)
}

// TestR107_ABuildThatFailsAfterAnAnswerShowsItsLog asserts R-107 for the
// build an answer starts: it fails the detection with the build's log, as
// the first one would.
func TestR107_ABuildThatFailsAfterAnAnswerShowsItsLog(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo, commit := repoWith(t, buildArgRepo)
	h := newScanHarnessAt(t, repo, commit)
	d := h.detector()

	_, err := d.Detect(ctx, h.app.ID)
	require.NoError(t, err)
	h.builder.fail = "ERROR: TOKEN is required"
	require.NoError(t, d.Detections.SaveAnswers(ctx, h.app.ID, map[string]string{"build_arg.TOKEN": "x"}))
	started, err := d.Rebuild(ctx, h.app.ID)
	require.NoError(t, err)
	require.True(t, started)

	det, err := d.Detections.Get(ctx, h.app.ID)
	require.NoError(t, err)
	require.Equal(t, state.DetectionFailed, det.Status)
	require.Contains(t, string(det.Body), "TOKEN is required")

	built, err := d.Built(ctx, h.app.ID)
	require.NoError(t, err)
	require.Nil(t, built, "a failed detection built nothing to reuse")
	again, err := d.Rebuild(ctx, h.app.ID)
	require.NoError(t, err)
	require.False(t, again, "nothing to build again from a failed detection")
}

// TestR311_WhatADeployFindsDetectionBuilt covers the answers a deploy gets
// when it asks for detection's build: none for an app never read, and none
// for a detection whose body cannot be read.
func TestR311_WhatADeployFindsDetectionBuilt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newScanHarness(t)
	d := h.detector()

	built, err := d.Built(ctx, h.app.ID)
	require.NoError(t, err)
	require.Nil(t, built, "never read")

	require.NoError(t, d.Detections.Save(ctx, h.app.ID, state.DetectionReady, []string{"not a proposal"}, ""))
	built, err = d.Built(ctx, h.app.ID)
	require.NoError(t, err)
	require.Nil(t, built)

	_, err = d.Rebuild(ctx, h.app.ID)
	require.Equal(t, errs.Internal, errs.CodeOf(err), "an unreadable plan is said")

	_, err = d.Rebuild(ctx, "app_missing")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}
