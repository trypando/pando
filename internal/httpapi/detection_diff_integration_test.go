//go:build integration

package httpapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/detect"
)

// TestR022_TheDetectionDiffShowsWhatAcceptingChanges asserts R-022's diff is
// of what accepting would pin. A variable a person added outlives a
// re-detection, so the diff does not list it as removed, and accepting keeps
// it (issue #350).
func TestR022_TheDetectionDiffShowsWhatAcceptingChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	i := newInstall(t)
	admin := i.admin()
	appID := i.createApp(admin, "notes")

	pinned := minimalSpec()
	pinned["workloads"].([]map[string]any)[0]["env"] = []map[string]any{
		{"key": "GITHUB_TOKEN", "secret_ref": "GITHUB_TOKEN", "source": "user"},
	}
	i.pinSpec(admin, appID, i.writeSpec(admin, appID, pinned))

	// The re-detection, which knows nothing of the variable.
	proposal := detect.Proposal{
		Status: "ready",
		Winner: detect.Candidate{Detector: "image", Strategy: "prebuilt"},
		DraftSpec: spec.AppSpec{
			SchemaVersion: spec.SchemaVersion,
			Source:        spec.Source{Type: "image", Image: "nginx:alpine"},
			Build:         spec.Build{Strategy: spec.BuildPrebuilt},
			Workloads: []spec.Workload{{
				Name:    "web",
				Image:   "nginx:alpine",
				Primary: true,
				Exposed: true,
				Ports:   []spec.Port{{Number: 8080, Protocol: "http", Source: "user"}},
			}},
		},
	}
	require.NoError(t, state.NewDetections(i.db).Save(ctx, appID, "ready", proposal, "abc123"))

	got := i.do(admin, http.MethodGet, "/apps/"+appID+"/detection/diff", nil)
	require.Equal(t, http.StatusOK, got.Code, got.String())
	var diff struct {
		Changes spec.Diff `json:"changes"`
		IsFirst bool      `json:"is_first"`
	}
	got.JSON(t, &diff)
	require.False(t, diff.IsFirst)
	for _, c := range diff.Changes.Changes {
		require.NotEqual(t, "workloads.web.env", c.Path, "the diff reports a change accepting does not make: %+v", c)
	}

	accepted := i.do(admin, http.MethodPost, "/apps/"+appID+"/detection/accept", map[string]any{"confirm": true})
	require.Contains(t, []int{http.StatusOK, http.StatusCreated}, accepted.Code, accepted.String())

	rev := i.do(admin, http.MethodGet, "/apps/"+appID+"/specs/2", nil)
	require.Equal(t, http.StatusOK, rev.Code, rev.String())
	var body struct {
		Body spec.AppSpec `json:"body"`
	}
	require.NoError(t, json.Unmarshal(rev.Body, &body))
	require.Len(t, body.Body.Workloads[0].Env, 1)
	kept := body.Body.Workloads[0].Env[0]
	require.Equal(t, "GITHUB_TOKEN", kept.Key)
	require.Equal(t, "GITHUB_TOKEN", *kept.SecretRef)
	require.Equal(t, spec.EnvFromUser, kept.Source)
}
