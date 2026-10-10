package buildkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

var errDaemon = errors.New("the daemon said no")

// TestPandosBuildKitSaysWhichStepFailed asserts every Docker call ensure makes
// that fails is reported as what Pando was doing, with an adapter code the
// planner turns into a readable refusal — never a BuildKit claimed to be up.
func TestPandosBuildKitSaysWhichStepFailed(t *testing.T) {
	cases := []struct {
		method string
		code   errs.Code
		setup  func(f *fakeDocker, m *managed)
	}{
		{method: "NetworkList", code: errs.AdapterUnavailable},
		{method: "NetworkCreate", code: errs.AdapterFailed},
		{method: "NetworkConnect", code: errs.AdapterFailed},
		{method: "ContainerInspect", code: errs.AdapterUnavailable},
		{method: "ImageInspect", code: errs.AdapterFailed}, // then the pull fails too
		{method: "VolumeCreate", code: errs.AdapterFailed},
		{method: "ContainerCreate", code: errs.AdapterFailed},
		{method: "ContainerStart", code: errs.AdapterFailed},
		{method: "CopyToContainer", code: errs.AdapterFailed, setup: func(_ *fakeDocker, m *managed) {
			m.files = registryFiles("registry:5000", []byte("ca"))
		}},
		{method: "ContainerRemove", code: errs.AdapterFailed, setup: func(f *fakeDocker, _ *managed) {
			f.containers[managedName] = &container.InspectResponse{ID: "old",
				Config: &container.Config{Labels: map[string]string{labelManagedBuild: "an-older-pando"}}, State: &container.State{}}
		}},
	}
	for _, c := range cases {
		f := newFakeDocker("pando-1")
		m := newManaged(f)
		m.self = "pando-1"
		if c.setup != nil {
			c.setup(f, m)
		}
		f.fail = map[string]error{c.method: errDaemon}
		_, err := m.ensure(context.Background())
		require.Error(t, err, c.method)
		require.Equal(t, c.code, errs.CodeOf(err), c.method)
	}

	// A registry network that is not there.
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	m.networks = []string{"pando-registry"}
	m.cli = &missingNetworkDocker{fakeDocker: f}
	_, err := m.ensure(context.Background())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "PANDO_BUILDKIT_NETWORKS")
}

// missingNetworkDocker refuses to connect BuildKit to any network but its own.
type missingNetworkDocker struct{ *fakeDocker }

func (d *missingNetworkDocker) NetworkConnect(ctx context.Context, id string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if id == "pando-registry" {
		return client.NetworkConnectResult{}, fmt.Errorf("network pando-registry: %w", cerrdefs.ErrNotFound)
	}
	return d.fakeDocker.NetworkConnect(ctx, id, o)
}

// racingDocker is a daemon where another replica creates the container
// between this one looking for it and creating it.
type racingDocker struct{ *fakeDocker }

func (d *racingDocker) ContainerCreate(ctx context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if _, err := d.fakeDocker.ContainerCreate(ctx, o); err != nil {
		return client.ContainerCreateResult{}, err
	}
	return client.ContainerCreateResult{}, fmt.Errorf("name in use: %w", cerrdefs.ErrConflict)
}

// TestTwoReplicasStartingTogetherShareOneBuildKit asserts a replica whose
// create loses the race uses the winner's container rather than failing.
func TestTwoReplicasStartingTogetherShareOneBuildKit(t *testing.T) {
	f := newFakeDocker("pando-2")
	m := newManaged(&racingDocker{fakeDocker: f})
	m.self = "pando-2"
	addr, err := m.ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tcp://pando-buildkit:1234", addr)
	require.True(t, f.containers[managedName].State.Running, "started the winner's")
}

// TestTheRegistrySettingsGoTogether asserts half a registry, or a CA Pando
// cannot read, is refused at configure with what to set, rather than starting
// a BuildKit that cannot push.
func TestTheRegistrySettingsGoTogether(t *testing.T) {
	fake := newFakeDocker("")
	orig := dockerClient
	dockerClient = func() (dockerAPI, error) { return fake, nil }
	t.Cleanup(func() { dockerClient = orig })

	t.Setenv("PANDO_BUILDKIT_REGISTRY_HOST", "registry:5000")
	err := New().Configure(context.Background(), nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "go together")

	t.Setenv("PANDO_BUILDKIT_REGISTRY_CA", filepath.Join(t.TempDir(), "missing.crt"))
	err = New().Configure(context.Background(), nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "Mount the registry's CA")

	ca := filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(ca, []byte("-----BEGIN CERTIFICATE-----\n"), 0o600))
	t.Setenv("PANDO_BUILDKIT_REGISTRY_CA", ca)
	t.Setenv("PANDO_BUILDKIT_NETWORKS", "pando-registry, ")
	a := New()
	require.NoError(t, a.Configure(context.Background(), nil))
	require.Equal(t, []string{"pando-registry"}, a.managed.networks)
	require.Contains(t, string(a.managed.files[".config/buildkit/buildkitd.toml"]), `registry."registry:5000"`)
}

// TestAHealthCheckOnPandosBuildKitReportsWhatEnsureCouldNotDo asserts a
// failure to start BuildKit reaches the planner through the health check.
func TestAHealthCheckOnPandosBuildKitReportsWhatEnsureCouldNotDo(t *testing.T) {
	fake := newFakeDocker("")
	orig := dockerClient
	dockerClient = func() (dockerAPI, error) { return fake, nil }
	t.Cleanup(func() { dockerClient = orig })

	a := New()
	require.NoError(t, a.Configure(context.Background(), nil))
	fake.fail = map[string]error{"NetworkList": errDaemon}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.HealthCheck(context.Background())))

	dockerClient = func() (dockerAPI, error) { return nil, errDaemon }
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(New().Configure(context.Background(), nil)))
}
