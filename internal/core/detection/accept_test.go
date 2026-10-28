package detection

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/detect"
)

func acceptProposal() detect.Proposal {
	return detect.Proposal{
		Status: "ready",
		DraftSpec: spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			Build:         spec.Build{Strategy: "dockerfile", Dockerfile: "Dockerfile"},
			Workloads: []spec.Workload{{
				Name:    "web",
				Primary: true,
				Env:     []spec.EnvEntry{{Key: "NODE_ENV", Value: ptr("production"), Source: spec.EnvFromDetection}},
			}},
		},
	}
}

func ptr(s string) *string { return &s }

// TestR022_AcceptedCarriesWhatAPersonDecided asserts the spec accepting
// pins keeps a person's variables from the pinned spec and gets the
// install's defaults, which is what the detection diff compares (issue
// #350).
func TestR022_AcceptedCarriesWhatAPersonDecided(t *testing.T) {
	pinned := &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		Workloads: []spec.Workload{{
			Name: "web",
			Env: []spec.EnvEntry{
				{Key: "NODE_ENV", Value: ptr("production"), Source: spec.EnvFromDetection},
				{Key: "GITHUB_TOKEN", SecretRef: ptr("GITHUB_TOKEN"), Source: spec.EnvFromUser},
			},
		}},
	}
	defaults := &spec.Defaults{BuilderAdapter: "builder_railpack", BuildTimeoutSeconds: 900}

	got := Accepted(acceptProposal(), nil, "app_1", "notes", defaults, pinned)

	require.Equal(t, "app_1", got.AppID)
	require.Equal(t, "builder_railpack", got.Build.AdapterRef, "the install's defaults are applied")
	require.Equal(t, 900, got.Build.TimeoutSeconds)

	keys := map[string]bool{}
	for _, e := range got.Workloads[0].Env {
		keys[e.Key] = true
	}
	require.True(t, keys["GITHUB_TOKEN"], "a person's variable carries")
	require.True(t, keys["NODE_ENV"], "what detection found stays")

	for _, c := range spec.Compare(pinned, &got).Changes {
		require.NotEqual(t, "workloads.web.env", c.Path, "the carried variable is not reported as removed: %+v", c)
	}
}

// TestAcceptedWithNothingPinnedOrDefaultedIsTheProposal asserts the first
// acceptance on an install with no defaults is the proposal itself.
func TestAcceptedWithNothingPinnedOrDefaultedIsTheProposal(t *testing.T) {
	p := acceptProposal()
	got := Accepted(p, nil, "app_1", "notes", nil, nil)

	want := p.DraftSpec
	want.AppID = "app_1"
	require.Equal(t, want, got)
}
