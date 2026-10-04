package docker

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// Replacing Pando's own container (R-355, R-359).
//
// Pando cannot replace itself: the process that stops the container is in
// it. So it starts a helper from its own image — the version already running,
// already trusted — that does the replacing, the way Watchtower and
// Portainer's agent do: inspect the container, stop it, create a new one with
// the same configuration under the new image, start it, and check it. Unlike
// Watchtower, the helper waits for the new one to say it is ready and puts the
// old one back when it does not.
//
// The database copy and its restore are core's (internal/core/state), called
// by the helper between these steps: an adapter never touches the state store
// (R-027).

// labelUpgradeHelper marks the helper, so a leftover one can be found.
const labelUpgradeHelper = "io.pando.upgrade-helper"

// preUpgradeSuffix names the old container while the new one is checked.
const preUpgradeSuffix = "-pre-upgrade"

var _ api.SelfUpgrader = (*Adapter)(nil)

// Self implements api.SelfUpgrader.
func (a *Adapter) Self(ctx context.Context) (api.SelfWorkload, error) {
	self := a.inspectSelf(ctx)
	if self == nil {
		return api.SelfWorkload{}, errs.New(errs.ValidInvalid,
			"Pando is not running in a container this Docker daemon manages, so it cannot replace itself.").
			WithRemedy("Upgrade by replacing the pando binary with the new release's, then start it again.")
	}
	return api.SelfWorkload{ID: self.ID, Image: self.Config.Image}, nil
}

// PullImage implements api.SelfUpgrader.
func (a *Adapter) PullImage(ctx context.Context, ref string) error {
	if err := a.pullOnce(ctx, ref); err != nil {
		return errs.Wrap(errs.AdapterFailed, fmt.Sprintf("Could not pull %s.", ref), err)
	}
	return nil
}

// StartHelper implements api.SelfUpgrader.
func (a *Adapter) StartHelper(ctx context.Context, spec api.HelperSpec) (string, error) {
	self := a.inspectSelf(ctx)
	if self == nil {
		return "", errs.New(errs.ValidInvalid, "Pando is not running in a container, so there is nothing to start a helper beside.")
	}

	env := make([]string, 0, len(spec.Env))
	for k, v := range spec.Env {
		env = append(env, k+"="+v.Reveal())
	}
	slices.Sort(env)

	// The same mounts — the data directory the outcome is written to, and the
	// runtime socket — and the same networks, which is how the helper reaches
	// Postgres by the name in the database URL.
	hostCfg := &container.HostConfig{Mounts: mountsOf(self)}
	netCfg, rest := networksOf(self)

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			// The image the running container was created from, by ID: the
			// helper is the version already trusted, whatever a tag names now.
			Image:  self.Image,
			Cmd:    spec.Args,
			Env:    env,
			Labels: map[string]string{labelManaged: "true", labelUpgradeHelper: "true"},
		},
		HostConfig:       hostCfg,
		NetworkingConfig: netCfg,
		Name:             strings.TrimPrefix(self.Name, "/") + "-upgrade",
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Could not create the upgrade helper.", err)
	}
	for _, id := range rest {
		if _, err := a.cli.NetworkConnect(ctx, id, client.NetworkConnectOptions{Container: created.ID}); err != nil {
			_, _ = a.cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
			return "", errs.Wrap(errs.AdapterFailed, "Could not attach the upgrade helper to Pando's networks.", err)
		}
	}
	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = a.cli.ContainerRemove(ctx, created.ID, client.ContainerRemoveOptions{Force: true})
		return "", errs.Wrap(errs.AdapterFailed, "Could not start the upgrade helper.", err)
	}
	return created.ID, nil
}

// RemoveHelpers removes upgrade helpers that have finished. The new Pando
// calls it once it has read the outcome.
func (a *Adapter) RemoveHelpers(ctx context.Context) error {
	list, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelUpgradeHelper+"=true"),
	})
	if err != nil {
		return err
	}
	for _, c := range list.Items {
		if c.State != container.StateRunning {
			_, _ = a.cli.ContainerRemove(ctx, c.ID, client.ContainerRemoveOptions{})
		}
	}
	return nil
}

func mountsOf(c *container.InspectResponse) []mount.Mount {
	out := make([]mount.Mount, 0, len(c.Mounts))
	for _, m := range c.Mounts {
		ms := mount.Mount{Type: m.Type, Target: m.Destination, ReadOnly: !m.RW}
		if m.Name != "" {
			ms.Source = m.Name
		} else {
			ms.Source = m.Source
		}
		out = append(out, ms)
	}
	return out
}

// networksOf returns the networking config to create with — one network, the
// most the API takes at create — and the IDs of the rest to connect after.
func networksOf(c *container.InspectResponse) (*network.NetworkingConfig, []string) {
	names := make([]string, 0, len(c.NetworkSettings.Networks))
	for n := range c.NetworkSettings.Networks {
		names = append(names, n)
	}
	slices.Sort(names)
	if len(names) == 0 {
		return nil, nil
	}
	first := c.NetworkSettings.Networks[names[0]]
	cfg := &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{
		names[0]: {NetworkID: first.NetworkID},
	}}
	var rest []string
	for _, n := range names[1:] {
		rest = append(rest, c.NetworkSettings.Networks[n].NetworkID)
	}
	return cfg, rest
}

// Replacer is the helper's half: the container steps of R-359, in order. It
// holds no state of Pando's; the helper calls core between steps.
type Replacer struct {
	cli *client.Client
}

// NewReplacer connects to the runtime the helper was started beside.
func NewReplacer() (*Replacer, error) {
	cli, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Replacer{cli: cli}, nil
}

// Old is the container being replaced, as it was before anything changed.
type Old struct {
	ID      string
	Name    string
	Image   string // the image ID it ran
	inspect container.InspectResponse
}

// Inspect reads the container to be replaced.
func (r *Replacer) Inspect(ctx context.Context, id string) (Old, error) {
	got, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		return Old{}, err
	}
	return Old{ID: got.Container.ID, Name: strings.TrimPrefix(got.Container.Name, "/"),
		Image: got.Container.Image, inspect: got.Container}, nil
}

// Stop stops the old container and moves its name aside, so the new one can
// take it — Compose finds its service's container by name and labels.
func (r *Replacer) Stop(ctx context.Context, old Old) error {
	timeout := 30
	if _, err := r.cli.ContainerStop(ctx, old.ID, client.ContainerStopOptions{Timeout: &timeout}); err != nil {
		return fmt.Errorf("stopping Pando: %w", err)
	}
	if _, err := r.cli.ContainerRename(ctx, old.ID, client.ContainerRenameOptions{NewName: old.Name + preUpgradeSuffix}); err != nil {
		return fmt.Errorf("moving the old container's name aside: %w", err)
	}
	return nil
}

// Recreate creates and starts a container with the old one's configuration
// under image, and returns its ID.
//
// Environment variables and labels the old image set are dropped rather than
// copied: they belong to the image, and copying them would pin the new
// container to the old image's values — its version label, its PATH.
func (r *Replacer) Recreate(ctx context.Context, old Old, image string) (string, error) {
	oldImage, err := r.cli.ImageInspect(ctx, old.Image)
	if err != nil {
		return "", fmt.Errorf("reading the old image: %w", err)
	}
	cfg := *old.inspect.Config
	cfg.Image = image
	cfg.Hostname = ""
	cfg.Env = without(cfg.Env, oldImage.Config.Env)
	cfg.Labels = withoutLabels(cfg.Labels, oldImage.Config.Labels)
	if slices.Equal(cfg.Entrypoint, oldImage.Config.Entrypoint) {
		cfg.Entrypoint = nil
	}
	if slices.Equal(cfg.Cmd, oldImage.Config.Cmd) {
		cfg.Cmd = nil
	}

	netCfg, rest := networksOf(&old.inspect)
	created, err := r.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &cfg, HostConfig: old.inspect.HostConfig, NetworkingConfig: withAliases(netCfg, &old.inspect),
		Name: old.Name,
	})
	if err != nil {
		return "", fmt.Errorf("creating the new container: %w", err)
	}
	for _, id := range rest {
		if _, err := r.cli.NetworkConnect(ctx, id, client.NetworkConnectOptions{
			Container: created.ID, EndpointConfig: aliasesFor(id, &old.inspect),
		}); err != nil {
			return created.ID, fmt.Errorf("attaching the new container to its networks: %w", err)
		}
	}
	if _, err := r.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return created.ID, fmt.Errorf("starting the new container: %w", err)
	}
	return created.ID, nil
}

// Ready waits until the new container answers /readyz, which it does only
// after migrations and the audit-log check have passed, or until timeout or
// its exit.
func (r *Replacer) Ready(ctx context.Context, id string, port string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	httpc := &http.Client{Timeout: 5 * time.Second}
	for time.Now().Before(deadline) {
		got, err := r.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
		if err != nil {
			return err
		}
		if got.Container.State != nil && !got.Container.State.Running {
			return fmt.Errorf("the new version exited with status %d", got.Container.State.ExitCode)
		}
		for _, ep := range got.Container.NetworkSettings.Networks {
			if !ep.IPAddress.IsValid() {
				continue
			}
			resp, err := httpc.Get("http://" + ep.IPAddress.String() + ":" + port + "/readyz")
			if err == nil {
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK {
					return nil
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("the new version did not report ready within %s", timeout)
}

// Logs returns the end of a container's output, for the report of why an
// upgrade was rolled back.
func (r *Replacer) Logs(ctx context.Context, id string) string {
	rc, err := r.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "40"})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()
	b, _ := io.ReadAll(io.LimitReader(rc, 64<<10))
	return stripStreamHeaders(b)
}

// Discard removes the new container, after a failed check.
func (r *Replacer) Discard(ctx context.Context, id string) {
	if id != "" {
		_, _ = r.cli.ContainerRemove(ctx, id, client.ContainerRemoveOptions{Force: true})
	}
}

// Restore gives the old container its name back and starts it.
func (r *Replacer) Restore(ctx context.Context, old Old) error {
	if _, err := r.cli.ContainerRename(ctx, old.ID, client.ContainerRenameOptions{NewName: old.Name}); err != nil {
		return fmt.Errorf("giving the old container its name back: %w", err)
	}
	if _, err := r.cli.ContainerStart(ctx, old.ID, client.ContainerStartOptions{}); err != nil {
		return fmt.Errorf("starting the old container: %w", err)
	}
	return nil
}

// Finish points the deployment's moving tag at the new image and removes the
// old container. Only now: a tag moved before the check, then a rollback, and
// the next `docker compose up` starts the version that just failed.
func (r *Replacer) Finish(ctx context.Context, old Old, image, tag string) error {
	if tag != "" {
		if _, err := r.cli.ImageTag(ctx, client.ImageTagOptions{Source: image, Target: tag}); err != nil {
			return fmt.Errorf("pointing %s at the new image: %w", tag, err)
		}
	}
	_, err := r.cli.ContainerRemove(ctx, old.ID, client.ContainerRemoveOptions{})
	return err
}

func without(env, defaults []string) []string {
	out := []string{}
	for _, e := range env {
		if !slices.Contains(defaults, e) {
			out = append(out, e)
		}
	}
	return out
}

func withoutLabels(labels, defaults map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range labels {
		if d, ok := defaults[k]; ok && d == v {
			continue
		}
		out[k] = v
	}
	return out
}

func withAliases(cfg *network.NetworkingConfig, old *container.InspectResponse) *network.NetworkingConfig {
	if cfg == nil {
		return nil
	}
	for name, ep := range cfg.EndpointsConfig {
		if prev, ok := old.NetworkSettings.Networks[name]; ok {
			ep.Aliases = keptAliases(prev.Aliases, old.ID)
		}
	}
	return cfg
}

func aliasesFor(networkID string, old *container.InspectResponse) *network.EndpointSettings {
	for _, ep := range old.NetworkSettings.Networks {
		if ep.NetworkID == networkID {
			return &network.EndpointSettings{Aliases: keptAliases(ep.Aliases, old.ID)}
		}
	}
	return nil
}

// keptAliases drops the alias Docker derives from the container ID, which
// would otherwise name the old container on the new one.
func keptAliases(aliases []string, id string) []string {
	var out []string
	for _, a := range aliases {
		if !strings.HasPrefix(id, a) {
			out = append(out, a)
		}
	}
	return out
}

// stripStreamHeaders removes the 8-byte frame headers Docker puts on a
// non-TTY container's log stream.
func stripStreamHeaders(b []byte) string {
	var out strings.Builder
	for len(b) >= 8 && (b[0] == 1 || b[0] == 2) && b[1] == 0 && b[2] == 0 && b[3] == 0 {
		n := int(b[4])<<24 | int(b[5])<<16 | int(b[6])<<8 | int(b[7])
		b = b[8:]
		if n > len(b) {
			n = len(b)
		}
		out.Write(b[:n])
		b = b[n:]
	}
	out.Write(b)
	return out.String()
}
