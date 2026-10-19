package buildkit

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// stoppedBuildKit is a fake daemon with the BuildKit Pando runs made and
// stopped, as a reboot on rootless Podman leaves it.
func stoppedBuildKit(t *testing.T) (*fakeDocker, *managed) {
	t.Helper()
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	_, err := m.ensure(context.Background())
	require.NoError(t, err)
	f.containers[managedName].State.Running = false
	f.containers[managedName].State.Status = container.StateExited
	return f, m
}

func unreachableClient(t *testing.T) *bkclient.Client {
	t.Helper()
	cli, err := bkclient.New(context.Background(), "tcp://127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// TestR111_ABuildStartsTheBuildKitPandoRuns asserts that a build starts a
// stopped BuildKit itself, rather than dialing one that is not there. After a
// reboot on rootless Podman every container is stopped and nothing starts it
// again, and Configure, the only place that did, had run already (issue #179).
func TestR111_ABuildStartsTheBuildKitPandoRuns(t *testing.T) {
	ctx := context.Background()
	f, m := stoppedBuildKit(t)
	a := &Adapter{cli: unreachableClient(t), managed: m}

	err := a.startBuildKit(ctx, 0)
	require.True(t, f.containers[managedName].State.Running, "the stopped one is started")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "and one that does not answer says so")
	require.Contains(t, errs.As(err).Remedy, "docker logs")

	require.NoError(t, (&Adapter{cli: a.cli}).startBuildKit(ctx, 0), "a BuildKit the install topology runs is the topology's to start")
}

// A BuildKit just started takes a moment to answer; the build waits for it.
func TestABuildWaitsForTheBuildKitItStarted(t *testing.T) {
	old := buildKitRetryEvery
	buildKitRetryEvery = time.Millisecond
	t.Cleanup(func() { buildKitRetryEvery = old })

	_, m := stoppedBuildKit(t)
	asked := 0
	a := &Adapter{managed: m, answers: func(context.Context) error {
		asked++
		if asked < 3 {
			return errors.New("connection refused")
		}
		return nil
	}}
	require.NoError(t, a.startBuildKit(context.Background(), time.Minute))
	require.Equal(t, 3, asked)
}

// A build stops at a BuildKit Pando could not start, and says why.
func TestABuildStopsWhereTheBuildKitCouldNotBeStarted(t *testing.T) {
	f, m := stoppedBuildKit(t)
	f.fail = map[string]error{"ContainerStart": errors.New("crun: permission denied")}
	a := &Adapter{cli: unreachableClient(t), managed: m}

	_, err := a.Build(context.Background(), api.BuildRequest{})
	require.ErrorContains(t, err, "Could not start Pando's build service")
}

// The profile file goes where the engine can see it, or the create is refused
// saying what to change: no directory to write it to, no container to read the
// mounts of, no mount holding it.
func TestTheProfileFileIsRefusedWhereTheEngineCouldNotReadIt(t *testing.T) {
	ctx := context.Background()
	file := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	for name, setup := range map[string]func(f *fakeDocker, m *managed){
		"no directory": func(f *fakeDocker, m *managed) {
			m.dataDir = filepath.Join(file, "under-a-file")
			f.containers["pando-1"] = &container.InspectResponse{ID: "pando-1", Config: &container.Config{}}
		},
		"no container": func(_ *fakeDocker, m *managed) {
			m.self = "gone"
		},
		"cannot write it": func(f *fakeDocker, m *managed) {
			require.NoError(t, os.Mkdir(filepath.Join(m.dataDir, profileFileName), 0o755))
			f.containers["pando-1"] = &container.InspectResponse{ID: "pando-1", Config: &container.Config{}}
		},
	} {
		f := newFakeDocker("pando-1")
		f.pathOnly = true
		m := newManaged(f)
		m.self = "pando-1"
		m.dataDir = t.TempDir()
		setup(f, m)
		_, err := m.ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), name)
		require.Contains(t, errs.As(err).Message, "seccomp profile only as a file", name)
	}
}

// Unnamed, Pando is the container its hostname names, as Docker and Podman
// set it.
func TestTheProfileFileIsFoundThroughTheHostname(t *testing.T) {
	host, err := os.Hostname()
	require.NoError(t, err)
	dataDir := t.TempDir()
	f := newFakeDocker(host)
	f.pathOnly = true
	f.containers[host] = &container.InspectResponse{ID: host, Config: &container.Config{},
		Mounts: []container.MountPoint{{Destination: dataDir, Source: "/host/data"}}}
	m := newManaged(f)
	m.dataDir = dataDir

	_, err = m.ensure(context.Background())
	require.NoError(t, err)
	require.Contains(t, f.containers[managedName].HostConfig.SecurityOpt, "seccomp=/host/data/"+profileFileName)
}

func TestTheProfileDirIsBesideTheBuildCache(t *testing.T) {
	t.Setenv("PANDO_BUILD_CACHE_DIR", "/srv/pando/buildcache")
	require.Equal(t, "/srv/pando", (&managed{}).profileDir())
}

// A build canceled while it waits for BuildKit stops waiting.
func TestABuildCanceledWhileBuildKitStartsStopsWaiting(t *testing.T) {
	_, m := stoppedBuildKit(t)
	ctx, cancel := context.WithCancel(context.Background())
	a := &Adapter{managed: m, answers: func(context.Context) error {
		cancel()
		return errors.New("connection refused")
	}}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(a.startBuildKit(ctx, time.Minute)))
}
