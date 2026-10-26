package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

func changeAt(d spec.Diff, path string) (spec.Change, bool) {
	for _, c := range d.Changes {
		if c.Path == path {
			return c, true
		}
	}
	return spec.Change{}, false
}

func TestIdenticalSpecsProduceNoDiff(t *testing.T) {
	d := spec.Compare(valid(), valid())
	require.True(t, d.Empty())
	require.Equal(t, spec.Benign, d.Class())
	require.False(t, d.RequiresConfirmation())
}

// The changes that can lose data or break access must require explicit
// confirmation (design 01 §4).
func TestDestructiveChangesRequireConfirmation(t *testing.T) {
	withVolume := func() *spec.AppSpec {
		s := valid()
		s.Volumes = []spec.Volume{{ID: "vol_01HQ8", Name: "data", Declared: spec.VolumeFromUser}}
		return s
	}

	cases := map[string]struct {
		old, next *spec.AppSpec
		path      string
	}{
		"volume removed": {
			old: withVolume(), next: valid(), path: "volumes.vol_01HQ8",
		},
		"routing mode changed": {
			old: valid(),
			next: func() *spec.AppSpec {
				s := valid()
				s.Routing = spec.Routing{AdapterRef: "rte_traefik", Mode: spec.RoutingSubdomain, Hostname: "notes.corp.com"}
				return s
			}(),
			path: "routing.mode",
		},
		"runtime adapter swapped": {
			old: valid(),
			next: func() *spec.AppSpec {
				s := valid()
				s.Runtime.AdapterRef = "rt_incus"
				return s
			}(),
			path: "runtime.adapter_ref",
		},
		"workload removed": {
			old: func() *spec.AppSpec {
				s := valid()
				s.Workloads = append(s.Workloads, spec.Workload{Name: "worker"})
				return s
			}(),
			next: valid(),
			path: "workloads.worker",
		},
	}

	for name, tc := range cases {
		d := spec.Compare(tc.old, tc.next)
		require.Equal(t, spec.Destructive, d.Class(), name)
		require.True(t, d.RequiresConfirmation(), name)

		c, ok := changeAt(d, tc.path)
		require.True(t, ok, "%s: expected a change at %s", name, tc.path)
		require.Equal(t, spec.Destructive, c.Class, name)
		require.NotEmpty(t, c.Summary, name)
	}
}

// O-8/R-257: swapping the runtime is neither a migration nor a plain redeploy.
// The summary has to say that storage does not move, because that is the part a
// user would otherwise assume.
func TestR257_RuntimeSwapSaysStorageDoesNotMove(t *testing.T) {
	next := valid()
	next.Runtime.AdapterRef = "rt_incus"

	c, ok := changeAt(spec.Compare(valid(), next), "runtime.adapter_ref")
	require.True(t, ok)
	require.Equal(t, spec.Destructive, c.Class)
	require.Contains(t, c.Summary, "storage does not move with it")
}

// A slot moving off a provisioned service leaves that service's data behind.
func TestSlotLeavingProvisionedIsDestructive(t *testing.T) {
	old := valid()
	old.Slots = []spec.Slot{{Key: "DATABASE_URL", Type: spec.SlotPostgres,
		Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned}}}

	next := valid()
	next.Slots = []spec.Slot{{Key: "DATABASE_URL", Type: spec.SlotPostgres,
		Resolution: &spec.Resolution{Mode: spec.ResolutionBound, Target: "postgres://elsewhere"}}}

	d := spec.Compare(old, next)
	require.Equal(t, spec.Destructive, d.Class())

	// The reverse direction is not destructive: nothing existing is abandoned.
	require.Equal(t, spec.Restart, spec.Compare(next, old).Class())
}

func TestRebuildChanges(t *testing.T) {
	for name, mutate := range map[string]func(*spec.AppSpec){
		"different repo":   func(s *spec.AppSpec) { s.Source.URL = "https://github.com/acme/other" },
		"different branch": func(s *spec.AppSpec) { s.Source.Ref = "develop" },
		"different commit": func(s *spec.AppSpec) { s.Source.Commit = "abc123" },
		"different strategy": func(s *spec.AppSpec) {
			s.Build.Strategy = spec.BuildBuildpack
		},
		"different dockerfile": func(s *spec.AppSpec) { s.Build.Dockerfile = "Dockerfile.prod" },
		"different build args": func(s *spec.AppSpec) {
			s.Build.Args = []spec.KV{{Key: "NODE_ENV", Value: "production"}}
		},
	} {
		next := valid()
		mutate(next)
		require.Equal(t, spec.Rebuild, spec.Compare(valid(), next).Class(), name)
	}
}

func TestRestartChanges(t *testing.T) {
	for name, mutate := range map[string]func(*spec.AppSpec){
		"env changed": func(s *spec.AppSpec) {
			s.Workloads[0].Env = []spec.EnvEntry{{Key: "LOG_LEVEL", Value: ptr("debug")}}
		},
		"resources changed": func(s *spec.AppSpec) { s.Resources.CPUMillis = 500 },
		"health changed":    func(s *spec.AppSpec) { s.Health.Path = "/healthz" },
		"volume added": func(s *spec.AppSpec) {
			s.Volumes = []spec.Volume{{ID: "vol_new", Name: "cache", Declared: spec.VolumeFromUser}}
		},
		"egress changed": func(s *spec.AppSpec) {
			s.Egress = spec.Egress{Mode: spec.EgressAllowlist, List: []string{"api.stripe.com"}}
		},
		"egress addition": func(s *spec.AppSpec) {
			s.Egress = spec.Egress{Add: []string{"api.stripe.com"}}
		},
		"private addresses blocked": func(s *spec.AppSpec) {
			s.Egress = spec.Egress{BlockPrivate: ptr(true)}
		},
	} {
		next := valid()
		mutate(next)
		require.Equal(t, spec.Restart, spec.Compare(valid(), next).Class(), name)
	}
}

func TestBenignChangesCollapse(t *testing.T) {
	for name, mutate := range map[string]func(*spec.AppSpec){
		"retention":     func(s *spec.AppSpec) { s.Retention.LogBytes = 1 << 20 },
		"auto deploy":   func(s *spec.AppSpec) { s.Deploy.AutoDeploy = spec.AutoDeploy{Enabled: true, Branch: "main"} },
		"auto rollback": func(s *spec.AppSpec) { s.Deploy.AutoRollback = true },
		"approval":      func(s *spec.AppSpec) { s.Deploy.RequireApproval = true },
	} {
		next := valid()
		mutate(next)
		d := spec.Compare(valid(), next)
		require.False(t, d.Empty(), name)
		require.Equal(t, spec.Benign, d.Class(), name)
		require.False(t, d.RequiresConfirmation(), name)
	}
}

// An egress written before issue #79, saved again unchanged in today's
// shape, is not a change (R-182).
func TestEgressInAnOlderShapeIsNotAChange(t *testing.T) {
	old := valid()
	old.Egress = spec.Egress{Mode: spec.EgressAllowlist, Allowlist: []string{"api.stripe.com"}}
	next := valid()
	next.Egress = spec.Egress{Mode: spec.EgressAllowlist, List: []string{"api.stripe.com"}}
	_, found := changeAt(spec.Compare(old, next), "egress")
	require.False(t, found)
}

// The overall class is the worst change present, not the most common.
func TestClassIsTheWorstChange(t *testing.T) {
	next := valid()
	next.Retention.LogBytes = 1 << 20 // benign
	next.Resources.CPUMillis = 500    // restart
	next.Source.Ref = "develop"       // rebuild
	next.Volumes = nil                // no change
	next.Routing.Port = 9090          // destructive

	d := spec.Compare(valid(), next)
	require.Equal(t, spec.Destructive, d.Class())
	require.Equal(t, spec.Destructive, d.Changes[0].Class, "worst changes sort first")
}

// The spec holds no secret values, so the differ compares a secret's identity
// rather than resolving it.
func TestEnvDiffComparesReferencesNotValues(t *testing.T) {
	old := valid()
	old.Workloads[0].Env = []spec.EnvEntry{{Key: "TOKEN", SecretRef: ptr("sec_A")}}

	same := valid()
	same.Workloads[0].Env = []spec.EnvEntry{{Key: "TOKEN", SecretRef: ptr("sec_A")}}
	require.True(t, spec.Compare(old, same).Empty(), "the same reference is not a change")

	moved := valid()
	moved.Workloads[0].Env = []spec.EnvEntry{{Key: "TOKEN", SecretRef: ptr("sec_B")}}
	require.Equal(t, spec.Restart, spec.Compare(old, moved).Class())
}

// Every change carries text written for the person confirming it.
func TestEveryChangeHasAReadableSummary(t *testing.T) {
	next := valid()
	next.Source.Ref = "develop"
	next.Resources.CPUMillis = 500
	next.Routing.Port = 9090

	d := spec.Compare(valid(), next)
	require.NotEmpty(t, d.Changes)
	for _, c := range d.Changes {
		require.NotEmpty(t, c.Summary, "change at %s has no summary", c.Path)
		require.NotEmpty(t, c.Path)
	}
}

// TestR422_AFailoverPriorityChangeIsBenign asserts that changing an app's
// availability.priority restarts nothing: it is read only at failover.
func TestR422_AFailoverPriorityChangeIsBenign(t *testing.T) {
	next := valid()
	next.Availability.Priority = 5
	c, ok := changeAt(spec.Compare(valid(), next), "availability.priority")
	require.True(t, ok)
	require.Equal(t, spec.Benign, c.Class)
	require.Equal(t, "This app's failover priority is now 5.", c.Summary)
}
