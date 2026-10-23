package buildkit

import (
	"context"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/errs"
)

// What a Pando that stopped without removing its daemons left behind is
// removed by the next one to start (R-117): a crash, a kill, a host reboot.

// sweepTimeout bounds a sweep, which runs as Pando starts.
const sweepTimeout = time.Minute

// sweep removes this installation's daemons, and their networks, that no
// running Pando is using: those this Pando's previous run started (startup
// is true), and those whose Pando's container is gone or stopped. It also
// removes the one BuildKit every app shared before daemons were per app.
func (m *managed) sweep(ctx context.Context, startup bool) {
	ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
	defer cancel()
	if err := m.setup(ctx); err != nil {
		return
	}
	if startup && m.inContainer {
		// Only a Pando in a container: that is what an upgrade replaced. One
		// run on the host, a developer's, shares the Docker daemon with an
		// installation it is not.
		m.removeShared(ctx)
	}
	gone := m.ownerGone(ctx, startup)

	containers, err := m.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelManagedBuild+"="+labelDaemon),
	})
	if err == nil {
		for _, c := range containers.Items {
			if m.ours(c.Names, c.Labels) && gone(c.Labels[labelBuildOwner]) {
				m.remove(ctx, &instance{container: c.ID})
			}
		}
	}
	networks, err := m.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("label", labelManagedBuild+"="+labelNetwork),
	})
	if err == nil {
		for _, n := range networks.Items {
			if m.ours([]string{n.Name}, n.Labels) && gone(n.Labels[labelBuildOwner]) {
				_, _ = m.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
			}
		}
	}
}

// ownerGone reports, for the owner label of a daemon, whether the Pando that
// started it is no longer running: this one's previous run, when startup is
// true, or another whose container is gone or stopped. A Pando on a host, or
// one Docker cannot answer for, is taken to be running.
func (m *managed) ownerGone(ctx context.Context, startup bool) func(owner string) bool {
	alive := map[string]bool{}
	return func(owner string) bool {
		switch {
		case owner == m.owner:
			return startup
		case strings.HasPrefix(owner, hostOwnerPrefix), owner == "":
			// Another Pando on a host: only it knows whether it is running.
			return false
		}
		if known, ok := alive[owner]; ok {
			return !known
		}
		alive[owner] = m.running(ctx, owner)
		return !alive[owner]
	}
}

// running is whether a Pando's container is running, or cannot be told not
// to be.
func (m *managed) running(ctx context.Context, id string) bool {
	got, err := m.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return !cerrdefs.IsNotFound(err)
	}
	return got.Container.State != nil && got.Container.State.Running
}

// ours is whether something with these names and labels is a daemon, or a
// daemon's network, of this installation: never another installation's on the
// same Docker daemon.
func (m *managed) ours(names []string, labels map[string]string) bool {
	if labels[labelBuildApp] == "" {
		return false
	}
	for _, n := range names {
		if strings.HasPrefix(strings.TrimPrefix(n, "/"), m.daemonPrefix()) {
			return true
		}
	}
	return false
}

// removeShared removes the BuildKit every app shared before each had its own,
// with its cache volume and its network, by the names it had.
func (m *managed) removeShared(ctx context.Context) {
	name := m.project + "-buildkit"
	got, err := m.cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if err == nil && got.Container.Config != nil && got.Container.Config.Labels[labelManagedBuild] != "" {
		_, _ = m.cli.ContainerRemove(ctx, got.Container.ID, client.ContainerRemoveOptions{Force: true, RemoveVolumes: true})
	}
	_, _ = m.cli.VolumeRemove(ctx, m.project+"-buildkit-cache", client.VolumeRemoveOptions{})
	// Refused while Pando's container is still on it, as it is when this
	// Pando was upgraded in place; one that was re-created left only a dead
	// endpoint, and goes.
	_, _ = m.cli.NetworkRemove(ctx, m.project+"-build", client.NetworkRemoveOptions{})
}

// removeApp removes a deleted app's daemons that other Pandos started. This
// Pando's own is the pool's (daemons.forgetApp).
func (m *managed) removeApp(ctx context.Context, app string) error {
	if err := m.setup(ctx); err != nil {
		return err
	}
	containers, err := m.cli.ContainerList(ctx, client.ContainerListOptions{
		All: true, Filters: make(client.Filters).Add("label", labelBuildApp+"="+app),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Could not list the app's build services.", err)
	}
	for _, c := range containers.Items {
		if m.ours(c.Names, c.Labels) && c.Labels[labelBuildOwner] != m.owner {
			name := strings.TrimPrefix(firstName(c.Names), "/")
			m.remove(ctx, &instance{container: c.ID, network: name})
		}
	}
	return nil
}

func firstName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return names[0]
}

// healthy is the managed builder's health check, which the planner calls
// before every plan: whether Docker answers, the build authority is there,
// and the pinned image is — fetched in the background when it is not, so the
// first build does not wait for it. It starts no daemon and waits on none.
func (m *managed) healthy(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := m.cli.Ping(ctx, client.PingOptions{}); err != nil {
		return errs.Wrap(errs.AdapterUnavailable, "Pando could not reach Docker, which it runs its build services on.", err).
			WithRemedy("Check that Docker is running and that Pando can reach it, as it does to run apps.")
	}
	if err := m.setup(ctx); err != nil {
		return err
	}
	if _, err := m.cli.ImageInspect(ctx, managedImage); err != nil {
		go m.prefetch(context.WithoutCancel(ctx))
	}
	return nil
}

// prefetch pulls the pinned image, once at a time.
func (m *managed) prefetch(ctx context.Context) {
	if !m.pulling.TryLock() {
		return
	}
	defer m.pulling.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_ = m.ensureImage(ctx)
}
