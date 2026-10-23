package spec_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/spec"
)

// pinnedApp is an app somebody has deployed and then configured: a couple of
// environment variables, a database they chose to have Pando provision, and a
// volume they added.
func pinnedApp() *spec.AppSpec {
	return &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		Build:         spec.Build{Strategy: spec.BuildBuildpack},
		Workloads: []spec.Workload{{
			Name: "web", Primary: true, Exposed: true,
			Env: []spec.EnvEntry{
				{Key: "FEATURE_FLAGS", Value: ptr("new-scoring"), Source: spec.EnvFromUser},
				{Key: "STRIPE_KEY", SecretRef: ptr("STRIPE_KEY"), Source: spec.EnvFromUser},
				{Key: "NODE_ENV", Value: ptr("production"), Source: spec.EnvFromDetection},
			},
		}},
		Slots: []spec.Slot{{
			Key: "DATABASE_URL", Type: spec.SlotPostgres, Required: true,
			Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
		}},
		Volumes: []spec.Volume{{ID: "vol_1", Name: "uploads", Declared: spec.VolumeFromUser}},
	}
}

// redetected is what detection proposes after a Dockerfile is added: it
// describes the repository, and knows nothing about the app's configuration.
func redetected() *spec.AppSpec {
	return &spec.AppSpec{
		SchemaVersion: spec.SchemaVersion,
		Build:         spec.Build{Strategy: spec.BuildDockerfile, Dockerfile: "Dockerfile"},
		Workloads: []spec.Workload{{
			Name: "web", Primary: true, Exposed: true,
			Env: []spec.EnvEntry{{Key: "NODE_ENV", Value: ptr("production"), Source: spec.EnvFromDetection}},
		}},
		Slots: []spec.Slot{{Key: "DATABASE_URL", Type: spec.SlotPostgres, Required: true}},
	}
}

// TestR022_ReDetectingKeepsWhatAPersonDecided asserts the scenario this exists
// for: deploy an app, configure it, add a Dockerfile, re-detect.
func TestR022_ReDetectingKeepsWhatAPersonDecided(t *testing.T) {
	out := spec.Carry(pinnedApp(), redetected())

	// The build is taken from the repository. That is the point of re-detecting.
	require.Equal(t, spec.BuildDockerfile, out.Build.Strategy)
	require.Equal(t, "Dockerfile", out.Build.Dockerfile)

	// The variables somebody typed survive, secret references included.
	env := map[string]spec.EnvEntry{}
	for _, e := range out.Workloads[0].Env {
		env[e.Key] = e
	}
	require.Contains(t, env, "FEATURE_FLAGS")
	require.Equal(t, "new-scoring", *env["FEATURE_FLAGS"].Value)
	require.Contains(t, env, "STRIPE_KEY")
	require.Equal(t, "STRIPE_KEY", *env["STRIPE_KEY"].SecretRef)

	// And what detection found is still there.
	require.Contains(t, env, "NODE_ENV")
}

// The database is the part that loses data rather than configuration.
//
// A proposal arrives with every slot unfilled. Pinning it unresolved leaves the
// app with no database and the old one sitting unreferenced on disk.
func TestR022_ReDetectingKeepsHowADependencyIsFilled(t *testing.T) {
	out := spec.Carry(pinnedApp(), redetected())

	require.Len(t, out.Slots, 1)
	require.NotNil(t, out.Slots[0].Resolution, "the dependency is still filled")
	require.Equal(t, spec.ResolutionProvisioned, out.Slots[0].Resolution.Mode)
}

// Storage somebody added is not silently unmounted.
func TestR022_ReDetectingKeepsStorageSomebodyAdded(t *testing.T) {
	out := spec.Carry(pinnedApp(), redetected())

	require.Len(t, out.Volumes, 1)
	require.Equal(t, "uploads", out.Volumes[0].Name)
}

// Detection wins where it should: a value it infers does not overwrite one a
// person set, but everything else about the workload is the new reading.
func TestAPersonsValueWinsOverAnInferredOne(t *testing.T) {
	pinned := pinnedApp()
	next := redetected()
	next.Workloads[0].Env = append(next.Workloads[0].Env,
		spec.EnvEntry{Key: "FEATURE_FLAGS", Value: ptr("guessed"), Source: spec.EnvFromDetection})

	out := spec.Carry(pinned, next)

	for _, e := range out.Workloads[0].Env {
		if e.Key == "FEATURE_FLAGS" {
			require.Equal(t, "new-scoring", *e.Value, "the typed value wins")
			require.Equal(t, spec.EnvFromUser, e.Source)
		}
	}
	// Two the person set, plus NODE_ENV. The inferred FEATURE_FLAGS is gone,
	// not kept alongside theirs.
	require.Len(t, out.Workloads[0].Env, 3, "no duplicate key")
}

// TestR165_ReDetectingKeepsTheAppsAddress asserts R-165: an address somebody
// bookmarked survives re-running Configuration, rather than falling back to
// <slug>.<base domain>.
func TestR165_ReDetectingKeepsTheAppsAddress(t *testing.T) {
	pinned := pinnedApp()
	pinned.Routing = spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain,
		ModeSource: spec.ModeFromAdapterDefault, Hostname: "notes.bemeek.io"}
	next := redetected()
	next.Routing = spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain,
		ModeSource: spec.ModeFromAdapterDefault, Hostname: "crew-notes.bemeek.io"}

	out := spec.Carry(pinned, next)
	require.Equal(t, pinned.Routing, out.Routing)
}

// Nothing is carried on the first acceptance — there is nothing to carry.
func TestTheFirstAcceptanceCarriesNothing(t *testing.T) {
	out := spec.Carry(nil, redetected())
	require.Equal(t, redetected(), out)
}

// A variable on a workload the new reading no longer has goes with it. There is
// nowhere to put it, and inventing a workload to hold it would be worse.
func TestAVariableOnAVanishedWorkloadIsNotResurrected(t *testing.T) {
	pinned := pinnedApp()
	pinned.Workloads[0].Name = "old-name"

	out := spec.Carry(pinned, redetected())

	require.Len(t, out.Workloads, 1)
	require.Equal(t, "web", out.Workloads[0].Name)
	for _, e := range out.Workloads[0].Env {
		require.NotEqual(t, "FEATURE_FLAGS", e.Key)
	}
}

// Compose-sourced entries are replaced, not carried: the file is the record,
// and somebody editing it expects the change to land.
func TestComposeSourcedEntriesAreNotCarried(t *testing.T) {
	pinned := pinnedApp()
	pinned.Workloads[0].Env = []spec.EnvEntry{
		{Key: "PORT", Value: ptr("3000"), Source: spec.EnvFromCompose},
	}

	out := spec.Carry(pinned, redetected())

	for _, e := range out.Workloads[0].Env {
		require.NotEqual(t, "PORT", e.Key)
	}
}

// TestR241_ReDetectingKeepsTheAppsResourceLimits asserts that accepting a
// re-detection keeps the limits somebody set, the app's and a workload's, so
// that re-detecting changes no limit and needs no app.resources.override.
func TestR241_ReDetectingKeepsTheAppsResourceLimits(t *testing.T) {
	pinned := pinnedApp()
	pinned.Resources = spec.Resources{CPUMillis: 2000, MemoryBytes: 2 << 30, DiskBytes: 10 << 30, Overridden: true}
	pinned.Workloads[0].Resources = &spec.ResourceLimits{MemoryBytes: 1 << 30}

	next := redetected()
	next.Resources = spec.Resources{CPUMillis: 1000, MemoryBytes: 512 << 20, DiskBytes: 5 << 30}
	got := spec.Carry(pinned, next)

	require.Equal(t, pinned.Resources, got.Resources)
	require.Equal(t, &spec.ResourceLimits{MemoryBytes: 1 << 30}, got.Workloads[0].Resources)
	require.Nil(t, next.Workloads[0].Resources, "the proposal itself is not changed")
	require.False(t, spec.LimitsChanged(pinned, got))

	// A pinned spec with no limits recorded keeps the proposal's defaults.
	bare := pinnedApp()
	got = spec.Carry(bare, redetected())
	require.Equal(t, redetected().Resources, got.Resources)
}
