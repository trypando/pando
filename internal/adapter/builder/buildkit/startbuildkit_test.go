package buildkit

import (
	"context"
	"testing"

	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/api/types/container"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// TestR111_ABuildStartsTheBuildKitPandoRuns asserts that a build starts a
// stopped BuildKit itself, rather than dialing one that is not there. After a
// reboot on rootless Podman every container is stopped and nothing starts it
// again, and Configure, the only place that did, had run already (issue #179).
func TestR111_ABuildStartsTheBuildKitPandoRuns(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	_, err := m.ensure(ctx)
	require.NoError(t, err)
	f.containers[managedName].State.Running = false
	f.containers[managedName].State.Status = container.StateExited

	cli, err := bkclient.New(ctx, "tcp://127.0.0.1:1")
	require.NoError(t, err)
	t.Cleanup(func() { _ = cli.Close() })
	a := &Adapter{cli: cli, managed: m}

	err = a.startBuildKit(ctx, 0)
	require.True(t, f.containers[managedName].State.Running, "the stopped one is started")
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "and one that does not answer says so")
	require.Contains(t, errs.As(err).Remedy, "docker logs")

	require.NoError(t, (&Adapter{cli: cli}).startBuildKit(ctx, 0), "a BuildKit somebody else runs is theirs to start")
}
