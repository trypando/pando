//go:build integration

package detection_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
)

// reviser is an AI adapter that revises plans, as a test needs it to.
type reviser struct {
	result adapterapi.ScreenResult
	got    adapterapi.ScreenRequest
	// sawDockerfile is whether the checkout it was handed held the file,
	// looked at during the call: the checkout is gone once Revise returns.
	sawDockerfile bool
}

func (r *reviser) Capabilities(context.Context) (adapterapi.AICapabilities, error) {
	return adapterapi.AICapabilities{Functions: []adapterapi.AIFunction{adapterapi.AIFunctionRevisePlan}}, nil
}

func (r *reviser) RepairPlan(context.Context, adapterapi.ScreenRequest) (adapterapi.ScreenResult, error) {
	return adapterapi.ScreenResult{}, nil
}

func (r *reviser) AnswerQuestions(context.Context, adapterapi.ScreenRequest) (adapterapi.ScreenResult, error) {
	return adapterapi.ScreenResult{}, nil
}

func (r *reviser) RevisePlan(_ context.Context, req adapterapi.ScreenRequest) (adapterapi.ScreenResult, error) {
	r.got = req
	_, err := req.Source.Stat("Dockerfile")
	r.sawDockerfile = err == nil
	return r.result, nil
}

// sourceScans records what detection asked the scanner to scan.
type sourceScans struct {
	commits  []string
	requests []adapterapi.ScanRequest
}

func (s *sourceScans) ScanSource(_ context.Context, req adapterapi.ScanRequest) {
	s.commits = append(s.commits, req.Commit)
	s.requests = append(s.requests, req)
}

// TestR336_ARevisionReadsTheReviewedCommitAndIsStored asserts R-336's third
// trigger end to end: after detection, a person's request reaches the adapter
// with a checkout of the commit that was reviewed, its change lands in the
// stored proposal, and the exchange is kept on it. Detection hands the
// scanner that same commit, so a deploy of it can reuse the scan (R-312).
func TestR336_ARevisionReadsTheReviewedCommitAndIsStored(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ctx := context.Background()

	repo := repoWith(t, map[string]string{
		"Dockerfile": "FROM nginx:alpine\nEXPOSE 8080\n",
		"index.html": "<h1>notes</h1>",
	})
	appID := appFrom(t, db, spec.Source{Type: spec.SourceGit, URL: repo})
	runner := runnerOver(t, db, corepolicy.Static(corepolicy.Default()))
	scans := &sourceScans{}
	runner.Scanner = scans

	detected, err := runner.Detect(ctx, appID)
	require.NoError(t, err)
	require.Equal(t, []string{detected.Commit}, scans.commits, "the scan is of the commit detection read")

	ai := &reviser{result: adapterapi.ScreenResult{
		Reply: "The Dockerfile serves nginx; I set NGINX_PORT to match EXPOSE 8080.",
		Amendments: []adapterapi.Amendment{{
			Kind: adapterapi.AmendSetEnv, Key: "NGINX_PORT", Value: "8080",
			Reason: "The Dockerfile exposes 8080.", Evidence: []string{"Dockerfile"},
		}},
	}}
	runner.Screener, runner.ScreenerRef = ai, "ai_test"

	revised, err := runner.Revise(ctx, appID, "  It listens on 8080.  ")
	require.NoError(t, err)
	require.Equal(t, "It listens on 8080.", ai.got.Instruction)

	// The adapter read the reviewed commit: the repository's files were there.
	require.True(t, ai.sawDockerfile)

	var proposal detect.Proposal
	require.NoError(t, json.Unmarshal(revised.Body, &proposal))
	require.Len(t, proposal.Conversation, 2)
	require.Equal(t, detect.TurnPerson, proposal.Conversation[0].From)
	require.Equal(t, []string{"web: NGINX_PORT set to 8080"}, proposal.Conversation[1].Changes)

	stored, err := state.NewDetections(db).Get(ctx, appID)
	require.NoError(t, err)
	require.Contains(t, string(stored.Body), "NGINX_PORT", "the change is stored, not only returned")
}

// TestR336_ARevisionNeedsAFinishedPlan asserts the refusal before any fetch:
// an app whose detection never finished has no plan to change.
func TestR336_ARevisionNeedsAFinishedPlan(t *testing.T) {
	t.Parallel()
	db := connected(t)
	ctx := context.Background()

	repo := repoWith(t, map[string]string{"Dockerfile": "FROM nginx:alpine\n"})
	appID := appFrom(t, db, spec.Source{Type: spec.SourceGit, URL: repo})
	runner := runnerOver(t, db, corepolicy.Static(corepolicy.Default()))
	runner.Screener = &reviser{}

	require.NoError(t, state.NewDetections(db).Start(ctx, appID))
	_, err := runner.Revise(ctx, appID, "Add Redis.")
	require.Equal(t, errs.StateInvalid, errs.CodeOf(err))

	_, err = runner.Revise(ctx, "app_missing", "Add Redis.")
	require.Equal(t, errs.NotFound, errs.CodeOf(err))
}
