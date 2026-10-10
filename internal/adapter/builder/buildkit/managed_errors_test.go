package buildkit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// TestAHalfMadeBuildKitIsNotKept asserts a container whose files or networks
// could not be given to it after it was created is removed, and the next
// ensure makes it again — not started later as though it had them, where every
// push would fail and nothing would replace it (CodeRabbit on #173).
func TestAHalfMadeBuildKitIsNotKept(t *testing.T) {
	ctx := context.Background()

	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	m.files = registryFiles("registry:5000", []byte("ca"))
	f.fail = map[string]error{"CopyToContainer": errDaemon}
	_, err := m.ensure(ctx)
	require.Error(t, err)
	require.NotContains(t, f.containers, managedName, "the container without its files is gone")
	f.fail = nil
	_, err = m.ensure(ctx)
	require.NoError(t, err)
	require.Len(t, f.created, 2, "made again, files and all")
	require.Len(t, f.copied, 1)

	f = newFakeDocker("pando-1")
	m = newManaged(&missingNetworkDocker{fakeDocker: f})
	m.self = "pando-1"
	m.networks = []string{"pando-registry"}
	_, err = m.ensure(ctx)
	require.Error(t, err)
	require.NotContains(t, f.containers, managedName, "the container without its network is gone")
	m.cli = f // the network exists now
	_, err = m.ensure(ctx)
	require.NoError(t, err)
	require.Len(t, f.created, 2)
	require.True(t, f.joined["c-pando-buildkit@pando-registry"])
}

// TestAFailedFirstStartIsRetriedAtTheNextCheck asserts a container whose
// first start failed is removed, so the next ensure makes and starts it at once
// rather than leaving a never-started container alone for setupGrace
// (CodeRabbit on #173).
func TestAFailedFirstStartIsRetriedAtTheNextCheck(t *testing.T) {
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	f.fail = map[string]error{"ContainerStart": errDaemon}
	_, err := m.ensure(context.Background())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.NotContains(t, f.containers, managedName, "not left never-started")

	f.fail = nil
	_, err = m.ensure(context.Background())
	require.NoError(t, err)
	require.Len(t, f.created, 2)
	require.True(t, f.containers[managedName].State.Running)
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
	require.False(t, f.containers[managedName].State.Running,
		"the winner may still be giving it its files: the replica that made it starts it")
	require.Zero(t, f.started)

	// Its next check, while the winner is still setting it up: never started,
	// so not started here either (CodeRabbit on #173).
	m.cli = f
	_, err = m.ensure(context.Background())
	require.NoError(t, err)
	require.Zero(t, f.started, "a never-started container is its maker's to start")
	require.Len(t, f.created, 1, "the winner's, not a second")

	// The winner finishes and starts it; the loser's next check uses it.
	_, err = f.ContainerStart(context.Background(), f.containers[managedName].ID, client.ContainerStartOptions{})
	require.NoError(t, err)
	_, err = m.ensure(context.Background())
	require.NoError(t, err)
	require.Len(t, f.created, 1)
	require.Equal(t, 1, f.started)
}

// TestABuildKitNobodyFinishedSettingUpIsMadeAgain asserts a container that
// was created and never started, long past any setup — its maker stopped
// partway — is replaced rather than waited on for ever or started without
// what it lacks.
func TestABuildKitNobodyFinishedSettingUpIsMadeAgain(t *testing.T) {
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	f.containers[managedName] = &container.InspectResponse{ID: "half-made",
		Config:  &container.Config{Labels: map[string]string{labelManagedBuild: m.digest()}},
		State:   &container.State{Status: container.StateCreated},
		Created: time.Now().Add(-10 * time.Minute).UTC().Format(time.RFC3339Nano)}

	_, err := m.ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, f.removed, "the half-made one is gone")
	require.Len(t, f.created, 1, "and made again")
	require.True(t, f.containers[managedName].State.Running)

	require.True(t, abandoned("not a time"), "an unreadable time is never waited on")
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
