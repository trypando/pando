package buildkit

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// fakeDocker is the part of a Docker daemon the managed BuildKit uses, in
// memory: containers by name, networks, and who joined them.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*container.InspectResponse
	networks   map[string]string // name -> ID
	joined     map[string]bool   // container that joined the build network
	created    []client.ContainerCreateOptions
	started    int
	removed    int
	copied     [][]byte
	self       string // the container Pando is in; "" is on the host
	attached   string // what joining a network already joined says; Docker's when empty
	pathOnly   bool   // takes seccomp= only as a path, as Podman does

	// fail makes a method return an error, by its name.
	fail map[string]error
}

func (f *fakeDocker) failing(method string) error { return f.fail[method] }

func newFakeDocker(self string) *fakeDocker {
	return &fakeDocker{containers: map[string]*container.InspectResponse{}, networks: map[string]string{},
		joined: map[string]bool{}, self: self}
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := f.failing("ContainerInspect"); err != nil {
		return client.ContainerInspectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return client.ContainerInspectResult{}, fmt.Errorf("no such container %s: %w", id, cerrdefs.ErrNotFound)
	}
	return client.ContainerInspectResult{Container: *c}, nil
}

func (f *fakeDocker) ContainerCreate(_ context.Context, o client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := f.failing("ContainerCreate"); err != nil {
		return client.ContainerCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.containers[o.Name]; ok {
		return client.ContainerCreateResult{}, fmt.Errorf("name in use: %w", cerrdefs.ErrConflict)
	}
	if f.pathOnly && o.HostConfig != nil {
		for _, opt := range o.HostConfig.SecurityOpt {
			if p, ok := strings.CutPrefix(opt, "seccomp="); ok && !strings.HasPrefix(p, "/") {
				return client.ContainerCreateResult{}, fmt.Errorf("container create: opening seccomp profile failed: open %s: no such file or directory", p)
			}
		}
	}
	f.created = append(f.created, o)
	f.containers[o.Name] = &container.InspectResponse{ID: "c-" + o.Name, Name: o.Name, Config: o.Config,
		HostConfig: o.HostConfig, State: &container.State{Status: container.StateCreated},
		Created: time.Now().UTC().Format(time.RFC3339Nano)}
	return client.ContainerCreateResult{ID: "c-" + o.Name}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if err := f.failing("ContainerStart"); err != nil {
		return client.ContainerStartResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	for _, c := range f.containers {
		if c.ID == id {
			c.State.Running, c.State.Status = true, container.StateRunning
		}
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, _ client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := f.failing("ContainerRemove"); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed++
	for name, c := range f.containers {
		if c.ID == id {
			delete(f.containers, name)
		}
	}
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if err := f.failing("ImageInspect"); err != nil {
		return client.ImageInspectResult{}, err
	}
	return client.ImageInspectResult{}, nil
}

func (f *fakeDocker) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	return nil, fmt.Errorf("the fake has every image")
}

func (f *fakeDocker) NetworkList(_ context.Context, _ client.NetworkListOptions) (client.NetworkListResult, error) {
	if err := f.failing("NetworkList"); err != nil {
		return client.NetworkListResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.NetworkListResult
	for name, id := range f.networks {
		out.Items = append(out.Items, network.Summary{Network: network.Network{Name: name, ID: id}})
	}
	return out, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, _ client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if err := f.failing("NetworkCreate"); err != nil {
		return client.NetworkCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networks[name] = "n-" + name
	return client.NetworkCreateResult{ID: "n-" + name}, nil
}

func (f *fakeDocker) NetworkConnect(_ context.Context, id string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if err := f.failing("NetworkConnect"); err != nil {
		return client.NetworkConnectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if strings.HasPrefix(o.Container, "c-") { // a container the fake made
		f.joined[o.Container+"@"+id] = true
		return client.NetworkConnectResult{}, nil
	}
	if o.Container != f.self || f.self == "" {
		return client.NetworkConnectResult{}, fmt.Errorf("no such container %s: %w", o.Container, cerrdefs.ErrNotFound)
	}
	if f.joined[o.Container] {
		if f.attached != "" {
			return client.NetworkConnectResult{}, errors.New(f.attached)
		}
		return client.NetworkConnectResult{}, fmt.Errorf("endpoint with name %s already exists in network", o.Container)
	}
	f.joined[o.Container] = true
	return client.NetworkConnectResult{}, nil
}

func (f *fakeDocker) CopyToContainer(_ context.Context, _ string, o client.CopyToContainerOptions) (client.CopyToContainerResult, error) {
	if err := f.failing("CopyToContainer"); err != nil {
		return client.CopyToContainerResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(o.Content)
	f.copied = append(f.copied, b)
	return client.CopyToContainerResult{}, nil
}

func (f *fakeDocker) VolumeCreate(context.Context, client.VolumeCreateOptions) (client.VolumeCreateResult, error) {
	if err := f.failing("VolumeCreate"); err != nil {
		return client.VolumeCreateResult{}, err
	}
	return client.VolumeCreateResult{}, nil
}

// TestR111_PandoStartsItsOwnBuildKitUnderTheNarrowedProfile asserts R-111 —
// "Pando starts it" — and issue #130: the container runs the pinned image
// under the compiled-in seccomp profile, passed inline so the install needs no
// file beside its Compose file, with its cache volume, on a network Pando's
// container joins, and never a runtime socket (R-112).
func TestR111_PandoStartsItsOwnBuildKitUnderTheNarrowedProfile(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"

	addr, err := m.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, "tcp://pando-buildkit:1234", addr, "reached by name on the network it shares with Pando")
	require.True(t, f.joined["pando-1"], "Pando's container joined the build network")
	require.Len(t, f.created, 1)

	o := f.created[0]
	require.Equal(t, managedImage, o.Config.Image)
	require.Contains(t, o.Config.Image, "@sha256:", "pinned by digest")
	require.Equal(t, managedArgs, []string(o.Config.Cmd))
	require.Contains(t, o.HostConfig.SecurityOpt, "apparmor=unconfined")
	var seccomp string
	for _, opt := range o.HostConfig.SecurityOpt {
		if p, ok := strings.CutPrefix(opt, "seccomp="); ok {
			seccomp = p
		}
	}
	require.Contains(t, seccomp, `"defaultAction":"SCMP_ACT_ERRNO"`, "the profile itself, not a path")
	require.True(t, json.Valid([]byte(seccomp)))
	require.Equal(t, container.RestartPolicyUnlessStopped, o.HostConfig.RestartPolicy.Name)
	require.Empty(t, o.HostConfig.Binds, "nothing of the host's")
	for _, mnt := range o.HostConfig.Mounts {
		require.NotContains(t, mnt.Source, ".sock", "no runtime socket (R-112)")
	}
	require.Empty(t, o.HostConfig.PortBindings, "nothing published when Pando is in a container")
	require.Equal(t, 1, f.started)

	// Again: found, running, left alone.
	_, err = m.ensure(ctx)
	require.NoError(t, err)
	require.Len(t, f.created, 1)
	require.Equal(t, 1, f.started)
}

// TestTheManagedBuildKitIsStartedReplacedOrSharedAsItIsFound covers what the
// ensure finds: a stopped one is started, one made by another Pando version
// is replaced, and one another replica made first is used.
func TestTheManagedBuildKitIsStartedReplacedOrSharedAsItIsFound(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	_, err := m.ensure(ctx)
	require.NoError(t, err)

	f.containers[managedName].State.Running = false
	f.containers[managedName].State.Status = container.StateExited
	_, err = m.ensure(ctx)
	require.NoError(t, err)
	require.True(t, f.containers[managedName].State.Running, "a stopped one is started")
	require.Len(t, f.created, 1)

	f.containers[managedName].Config.Labels[labelManagedBuild] = "an-older-pando"
	_, err = m.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, f.removed, "another version's is replaced")
	require.Len(t, f.created, 2)
	require.Equal(t, m.digest(), f.containers[managedName].Config.Labels[labelManagedBuild])

	// A second replica in its own container, with the first one's BuildKit
	// already there: joins the network and uses it.
	f.self = "pando-2"
	second := newManaged(f)
	second.self = "pando-2"
	addr, err := second.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, "tcp://pando-buildkit:1234", addr)
	require.True(t, f.joined["pando-2"])
	require.Len(t, f.created, 2)
}

// TestAHostRunPandoReachesItsBuildKitOnLoopback covers a developer running the
// binary on the host: there is no container to join the network, so BuildKit
// is published on the host's loopback and nowhere else.
func TestAHostRunPandoReachesItsBuildKitOnLoopback(t *testing.T) {
	f := newFakeDocker("")
	m := newManaged(f)
	m.self = "not-a-container"
	addr, err := m.ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tcp://127.0.0.1:1234", addr)
	for _, bindings := range f.created[0].HostConfig.PortBindings {
		for _, b := range bindings {
			require.Equal(t, "127.0.0.1", b.HostIP.String(), "loopback only")
		}
	}
}

// TestPandosBuildKitTrustsTheInstallationsRegistry covers
// docker-compose.registry.yml: the registry's CA and a configuration naming it
// are written into the container before it starts, and a different CA is a
// different container.
func TestPandosBuildKitTrustsTheInstallationsRegistry(t *testing.T) {
	f := newFakeDocker("pando-1")
	m := newManaged(f)
	m.self = "pando-1"
	plain := m.digest()
	m.files = registryFiles("registry:5000", []byte("-----BEGIN CERTIFICATE-----\nca\n"))
	m.networks = []string{"pando-registry"}
	require.NotEqual(t, plain, m.digest(), "the files are part of what it was made from")

	_, err := m.ensure(context.Background())
	require.NoError(t, err)
	require.Len(t, f.copied, 1)
	require.True(t, f.joined["c-pando-buildkit@pando-registry"], "joined the registry's network, and only because it was named")

	got := map[string]string{}
	tr := tar.NewReader(bytes.NewReader(f.copied[0]))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		body, _ := io.ReadAll(tr)
		require.Equal(t, 1000, h.Uid, "owned by the rootless BuildKit's user")
		got[h.Name] = string(body)
	}
	require.Equal(t, "[registry.\"registry:5000\"]\n  ca = [\"/home/user/.config/buildkit/registry-ca.crt\"]\n",
		got[".config/buildkit/buildkitd.toml"])
	require.Contains(t, got[".config/buildkit/registry-ca.crt"], "BEGIN CERTIFICATE")

	require.Nil(t, registryFiles("", []byte("ca")), "half a registry is none")
}

// TestEachInstallationOnADaemonRunsItsOwnBuildKit asserts the names come from
// the Compose project of Pando's own container, so a second installation on
// the same daemon — the deploy QA run's beside an operator's — has its own
// BuildKit, network and cache, and removing one never removes the other's.
func TestEachInstallationOnADaemonRunsItsOwnBuildKit(t *testing.T) {
	f := newFakeDocker("qa-pando-1")
	f.containers["qa-pando-1"] = &container.InspectResponse{ID: "qa-pando-1",
		Config: &container.Config{Labels: map[string]string{labelComposeProject: "pandoqa"}}, State: &container.State{Running: true}}
	m := newManaged(f)
	m.self = "qa-pando-1"

	addr, err := m.ensure(context.Background())
	require.NoError(t, err)
	require.Equal(t, "tcp://pandoqa-buildkit:1234", addr)
	require.Equal(t, "pandoqa-buildkit", f.created[0].Name)
	require.Contains(t, f.networks, "pandoqa-build")
	require.Equal(t, "pandoqa-buildkit-cache", f.created[0].HostConfig.Mounts[0].Source)
}

// TestR111_AfterARebootOnPodmanBuildKitIsStartedAgain asserts that Pando, back
// from a reboot and still on the build network, starts its stopped BuildKit
// when Podman says so in its own words. Podman answers the join with "network
// is already connected"; taken as a failure, ensure stopped there, BuildKit
// stayed down, and no app could be built (issue #179).
func TestR111_AfterARebootOnPodmanBuildKitIsStartedAgain(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	f.attached = "network is already connected"
	m := newManaged(f)
	m.self = "pando-1"
	_, err := m.ensure(ctx)
	require.NoError(t, err)

	f.containers[managedName].State.Running = false
	f.containers[managedName].State.Status = container.StateExited
	addr, err := m.ensure(ctx)
	require.NoError(t, err)
	require.Equal(t, "tcp://pando-buildkit:1234", addr)
	require.True(t, f.containers[managedName].State.Running, "the stopped one is started")

	f.attached = "boom"
	_, err = m.ensure(ctx)
	require.Error(t, err, "any other refusal to join is still one")
}

// TestR111_OnPodmanBuildKitGetsItsProfileAsAFile asserts that the BuildKit
// Pando runs is made under the narrowed seccomp profile on an engine that
// takes the profile only as a path. Podman's Docker-compatible API reads
// seccomp= as a file to open, so Docker's inline profile failed every create
// there, and no app could be built on Podman (issue #179).
func TestR111_OnPodmanBuildKitGetsItsProfileAsAFile(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	f.pathOnly = true
	dataDir := t.TempDir()
	f.containers["pando-1"] = &container.InspectResponse{ID: "pando-1", Config: &container.Config{},
		State: &container.State{Running: true},
		Mounts: []container.MountPoint{
			{Destination: "/etc/traefik/dynamic", Source: "/host/volumes/traefik/_data"},
			{Destination: dataDir, Source: "/host/volumes/pando-data/_data"},
		}}
	m := newManaged(f)
	m.self = "pando-1"
	m.dataDir = dataDir

	_, err := m.ensure(ctx)
	require.NoError(t, err)
	require.True(t, f.containers[managedName].State.Running)
	opts := f.containers[managedName].HostConfig.SecurityOpt
	require.Contains(t, opts, "seccomp=/host/volumes/pando-data/_data/"+profileFileName, "the profile, by its path on the host")

	written, err := os.ReadFile(filepath.Join(dataDir, profileFileName))
	require.NoError(t, err)
	require.Equal(t, compactProfile(), string(written), "the same narrowed profile Docker is given inline")

	// With no mount holding the data directory there is nowhere to put it the
	// engine can see, and the refusal says what to change.
	g := newFakeDocker("pando-1")
	g.pathOnly = true
	g.containers["pando-1"] = &container.InspectResponse{ID: "pando-1", Config: &container.Config{}, State: &container.State{Running: true}}
	n := newManaged(g)
	n.self = "pando-1"
	n.dataDir = t.TempDir()
	_, err = n.ensure(ctx)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "on a volume")
}

func TestHostPathTakesTheDeepestMount(t *testing.T) {
	mounts := []container.MountPoint{
		{Destination: "/var/lib/pando", Source: ""}, // tmpfs: on no path of the host's
		{Destination: "/var/lib", Source: "/a"},
		{Destination: "/var/lib/pando", Source: "/b"},
		{Destination: "/var/lib/pandora", Source: "/c"},
	}
	got, ok := hostPath(mounts, "/var/lib/pando/x.json")
	require.True(t, ok)
	require.Equal(t, "/b/x.json", got)
	_, ok = hostPath(mounts, "/etc/x")
	require.False(t, ok)
}
