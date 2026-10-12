package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

func buildSpec() *spec.AppSpec {
	return &spec.AppSpec{
		Source: spec.Source{Type: spec.SourceGit, URL: "https://example.test/app", Commit: "abc"},
		Build: spec.Build{Strategy: spec.BuildDockerfile, Dockerfile: "Dockerfile", AdapterRef: "bld",
			Args: []spec.KV{{Key: "B", Value: "2"}, {Key: "A", Value: "1"}}, TimeoutSeconds: 600},
		Runtime: spec.RuntimeRef{AdapterRef: "rt"},
		Workloads: []spec.Workload{{Name: "web", Primary: true, Command: []string{"npm", "start"},
			Ports: []spec.Port{{Number: 3000}}}},
	}
}

// TestR311_BuildInputsChangeOnlyWithWhatIsBuilt asserts what decides whether a
// deploy may run the image detection built (R-311, design 09 §4.1): anything
// that changes what a build produces, and nothing that only changes how the
// image runs — answering a port must not cost a rebuild.
func TestR311_BuildInputsChangeOnlyWithWhatIsBuilt(t *testing.T) {
	t.Parallel()
	base := spec.BuildInputs(buildSpec())
	require.NotEmpty(t, base)
	require.Empty(t, spec.BuildInputs(nil))

	same := map[string]func(s *spec.AppSpec){
		"a port":                 func(s *spec.AppSpec) { s.Workloads[0].Ports[0].Number = 8080 },
		"the environment":        func(s *spec.AppSpec) { s.Workloads[0].Env = []spec.EnvEntry{{Key: "X"}} },
		"the commit":             func(s *spec.AppSpec) { s.Source.Commit = "def" },
		"the build's timeout":    func(s *spec.AppSpec) { s.Build.TimeoutSeconds = 60 },
		"arguments in any order": func(s *spec.AppSpec) { s.Build.Args[0], s.Build.Args[1] = s.Build.Args[1], s.Build.Args[0] },
		"a dockerfile's command": func(s *spec.AppSpec) { s.Workloads[0].Command = []string{"node", "x"} },
	}
	for name, change := range same {
		s := buildSpec()
		change(s)
		require.Equal(t, base, spec.BuildInputs(s), name)
	}

	differ := map[string]func(s *spec.AppSpec){
		"a build argument": func(s *spec.AppSpec) { s.Build.Args = append(s.Build.Args, spec.KV{Key: "C", Value: "3"}) },
		"the Dockerfile":   func(s *spec.AppSpec) { s.Build.Dockerfile = "web.Dockerfile" },
		"the target":       func(s *spec.AppSpec) { s.Build.Target = "prod" },
		"the builder":      func(s *spec.AppSpec) { s.Build.AdapterRef = "other" },
		"the runtime":      func(s *spec.AppSpec) { s.Runtime.AdapterRef = "other" },
		"the folder":       func(s *spec.AppSpec) { s.Source.Subdir = "web" },
		"a buildpack's start command": func(s *spec.AppSpec) {
			s.Build.Strategy = spec.BuildBuildpack
		},
		"a service's build": func(s *spec.AppSpec) {
			s.Workloads = append(s.Workloads, spec.Workload{Name: "worker",
				Build: &spec.WorkloadBuild{Dockerfile: "worker.Dockerfile", Args: []spec.KV{{Key: "Z"}, {Key: "Y"}}}})
		},
	}
	for name, change := range differ {
		s := buildSpec()
		change(s)
		require.NotEqual(t, base, spec.BuildInputs(s), name)
	}

	// A buildpack is planned around the start command, so there it counts.
	pack := buildSpec()
	pack.Build.Strategy = spec.BuildBuildpack
	other := buildSpec()
	other.Build.Strategy = spec.BuildBuildpack
	other.Workloads[0].Command = []string{"node", "server.js"}
	require.NotEqual(t, spec.BuildInputs(pack), spec.BuildInputs(other))
}
