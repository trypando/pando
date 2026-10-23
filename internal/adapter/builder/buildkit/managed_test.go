package buildkit

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	cerrdefs "github.com/containerd/errdefs"
	bkclient "github.com/moby/buildkit/client"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/errs"
)

// fakeDocker is the part of a Docker daemon the managed BuildKits use, in
// memory: containers by name, networks, and who joined them.
type fakeDocker struct {
	mu         sync.Mutex
	containers map[string]*container.InspectResponse // by name
	networks   map[string]network.Summary            // by name
	joined     map[string]bool                       // "<container>@<network>"
	addrs      int
	created    []client.ContainerCreateOptions
	netOpts    map[string]client.NetworkCreateOptions
	started    int
	removed    []string
	volumes    []string // removed by name
	copied     [][]byte
	self       string // the container Pando is in; "" is on the host
	attached   string // what joining a network already joined says; Docker's when empty
	pathOnly   bool   // takes seccomp= only as a path, as Podman does
	noImage    bool   // has not got the pinned image
	pulled     int
	logs       string

	// fail makes a method return an error, by its name.
	fail map[string]error
}

func (f *fakeDocker) failing(method string) error { return f.fail[method] }

func newFakeDocker(self string) *fakeDocker {
	f := &fakeDocker{containers: map[string]*container.InspectResponse{}, networks: map[string]network.Summary{},
		joined: map[string]bool{}, netOpts: map[string]client.NetworkCreateOptions{}, self: self}
	if self != "" {
		f.containers[self] = &container.InspectResponse{ID: "full-" + self, Name: "/" + self,
			Config: &container.Config{}, State: &container.State{Running: true, Status: container.StateRunning}}
	}
	return f
}

// find is a container by name or ID. Called with the lock held.
func (f *fakeDocker) find(id string) (string, *container.InspectResponse) {
	if c, ok := f.containers[id]; ok {
		return id, c
	}
	for name, c := range f.containers {
		if c.ID == id {
			return name, c
		}
	}
	return "", nil
}

func (f *fakeDocker) ContainerInspect(_ context.Context, id string, _ client.ContainerInspectOptions) (client.ContainerInspectResult, error) {
	if err := f.failing("ContainerInspect"); err != nil {
		return client.ContainerInspectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	_, c := f.find(id)
	if c == nil {
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
	f.containers[o.Name] = &container.InspectResponse{ID: "c-" + o.Name, Name: "/" + o.Name, Config: o.Config,
		HostConfig: o.HostConfig, State: &container.State{Status: container.StateCreated}}
	for n := range o.NetworkingConfig.EndpointsConfig {
		f.joined["c-"+o.Name+"@"+n] = true
	}
	return client.ContainerCreateResult{ID: "c-" + o.Name}, nil
}

func (f *fakeDocker) ContainerStart(_ context.Context, id string, _ client.ContainerStartOptions) (client.ContainerStartResult, error) {
	if err := f.failing("ContainerStart"); err != nil {
		return client.ContainerStartResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started++
	_, c := f.find(id)
	if c == nil {
		return client.ContainerStartResult{}, cerrdefs.ErrNotFound
	}
	c.State.Running, c.State.Status = true, container.StateRunning
	c.NetworkSettings = &container.NetworkSettings{Networks: map[string]*network.EndpointSettings{}}
	if len(c.HostConfig.PortBindings) > 0 {
		c.NetworkSettings.Ports = network.PortMap{
			network.MustParsePort(managedPort + "/tcp"): {{HostPort: "32768"}},
		}
	}
	// An address on every network it joined, as the engine gives one.
	for key := range f.joined {
		if n, ok := strings.CutPrefix(key, c.ID+"@"); ok {
			f.addrs++
			c.NetworkSettings.Networks[strings.TrimPrefix(n, "n-")] = &network.EndpointSettings{
				NetworkID: n, IPAddress: netip.AddrFrom4([4]byte{10, 99, 0, byte(f.addrs)}),
			}
		}
	}
	return client.ContainerStartResult{}, nil
}

func (f *fakeDocker) ContainerRemove(_ context.Context, id string, o client.ContainerRemoveOptions) (client.ContainerRemoveResult, error) {
	if err := f.failing("ContainerRemove"); err != nil {
		return client.ContainerRemoveResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	name, c := f.find(id)
	if c == nil {
		return client.ContainerRemoveResult{}, cerrdefs.ErrNotFound
	}
	if !o.RemoveVolumes {
		return client.ContainerRemoveResult{}, errors.New("the fake wants anonymous volumes removed with the container")
	}
	delete(f.containers, name)
	f.removed = append(f.removed, name)
	return client.ContainerRemoveResult{}, nil
}

func (f *fakeDocker) ContainerList(_ context.Context, o client.ContainerListOptions) (client.ContainerListResult, error) {
	if err := f.failing("ContainerList"); err != nil {
		return client.ContainerListResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.ContainerListResult
	for name, c := range f.containers {
		if c.Config == nil || !matchesLabels(o.Filters, c.Config.Labels) {
			continue
		}
		out.Items = append(out.Items, container.Summary{ID: c.ID, Names: []string{"/" + name}, Labels: c.Config.Labels})
	}
	return out, nil
}

// matchesLabels applies a filter's label=value terms.
func matchesLabels(filters client.Filters, labels map[string]string) bool {
	for term := range filters["label"] {
		k, v, _ := strings.Cut(term, "=")
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (f *fakeDocker) ContainerLogs(context.Context, string, client.ContainerLogsOptions) (client.ContainerLogsResult, error) {
	if f.logs == "" {
		return nil, cerrdefs.ErrNotFound
	}
	// One frame of Docker's multiplexed log stream, on stderr.
	body := []byte(f.logs + "\n")
	frame := []byte{byte(stdcopy.Stderr), 0, 0, 0, 0, 0, 0, byte(len(body))}
	return io.NopCloser(bytes.NewReader(append(frame, body...))), nil
}

func (f *fakeDocker) ImageInspect(context.Context, string, ...client.ImageInspectOption) (client.ImageInspectResult, error) {
	if err := f.failing("ImageInspect"); err != nil {
		return client.ImageInspectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.noImage {
		return client.ImageInspectResult{}, cerrdefs.ErrNotFound
	}
	return client.ImageInspectResult{}, nil
}

func (f *fakeDocker) ImagePull(context.Context, string, client.ImagePullOptions) (client.ImagePullResponse, error) {
	if err := f.failing("ImagePull"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pulled++
	f.noImage = false
	return fakePull{r: strings.NewReader("{}")}, nil
}

// fakePull is a finished pull: its stream, and nothing else asked of it.
type fakePull struct {
	client.ImagePullResponse
	r io.Reader
}

func (p fakePull) Read(b []byte) (int, error) { return p.r.Read(b) }
func (fakePull) Close() error                 { return nil }

func (f *fakeDocker) NetworkList(_ context.Context, o client.NetworkListOptions) (client.NetworkListResult, error) {
	if err := f.failing("NetworkList"); err != nil {
		return client.NetworkListResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out client.NetworkListResult
	for _, n := range f.networks {
		if matchesLabels(o.Filters, n.Labels) {
			out.Items = append(out.Items, n)
		}
	}
	return out, nil
}

func (f *fakeDocker) NetworkCreate(_ context.Context, name string, o client.NetworkCreateOptions) (client.NetworkCreateResult, error) {
	if err := f.failing("NetworkCreate"); err != nil {
		return client.NetworkCreateResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.networks[name]; ok {
		return client.NetworkCreateResult{}, fmt.Errorf("network %s exists: %w", name, cerrdefs.ErrConflict)
	}
	summary := network.Summary{Network: network.Network{Name: name, ID: "n-" + name, Labels: o.Labels}}
	if o.IPAM != nil {
		summary.IPAM = *o.IPAM
	}
	f.networks[name] = summary
	f.netOpts[name] = o
	return client.NetworkCreateResult{ID: "n-" + name}, nil
}

func (f *fakeDocker) NetworkRemove(_ context.Context, id string, _ client.NetworkRemoveOptions) (client.NetworkRemoveResult, error) {
	if err := f.failing("NetworkRemove"); err != nil {
		return client.NetworkRemoveResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, n := range f.networks {
		if name == id || n.ID == id {
			delete(f.networks, name)
			return client.NetworkRemoveResult{}, nil
		}
	}
	return client.NetworkRemoveResult{}, cerrdefs.ErrNotFound
}

func (f *fakeDocker) networkNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for name := range f.networks {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (f *fakeDocker) NetworkConnect(_ context.Context, id string, o client.NetworkConnectOptions) (client.NetworkConnectResult, error) {
	if err := f.failing("NetworkConnect"); err != nil {
		return client.NetworkConnectResult{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := o.Container + "@" + id
	if f.joined[key] {
		if f.attached != "" {
			return client.NetworkConnectResult{}, errors.New(f.attached)
		}
		return client.NetworkConnectResult{}, fmt.Errorf("endpoint with name %s already exists in network", o.Container)
	}
	if _, c := f.find(o.Container); c == nil {
		return client.NetworkConnectResult{}, fmt.Errorf("no such container %s: %w", o.Container, cerrdefs.ErrNotFound)
	}
	known := false
	for name, n := range f.networks {
		known = known || name == id || n.ID == id
	}
	if !known {
		return client.NetworkConnectResult{}, fmt.Errorf("network %s not found: %w", id, cerrdefs.ErrNotFound)
	}
	f.joined[key] = true
	return client.NetworkConnectResult{}, nil
}

func (f *fakeDocker) VolumeRemove(_ context.Context, id string, _ client.VolumeRemoveOptions) (client.VolumeRemoveResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes = append(f.volumes, id)
	return client.VolumeRemoveResult{}, nil
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

func (f *fakeDocker) Ping(context.Context, client.PingOptions) (client.PingResult, error) {
	if err := f.failing("Ping"); err != nil {
		return client.PingResult{}, err
	}
	return client.PingResult{}, nil
}

// testManaged is a managed on f whose daemons answer at once, with Pando's
// data directory a temporary one.
func testManaged(t *testing.T, f *fakeDocker) *managed {
	t.Helper()
	m := newManaged(f)
	m.self = f.self
	if m.self == "" {
		m.self = "not-a-container"
	}
	m.dataDir = t.TempDir()
	m.answers = func(context.Context, *bkclient.Client) error { return nil }
	return m
}

// untar reads an archive CopyToContainer was given: bodies and modes by name.
func untar(t *testing.T, b []byte) (map[string]string, map[string]int64) {
	t.Helper()
	bodies, modes := map[string]string{}, map[string]int64{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		body, _ := io.ReadAll(tr)
		require.Equal(t, 1000, h.Uid, "owned by the rootless BuildKit's user")
		bodies[h.Name], modes[h.Name] = string(body), h.Mode
	}
	return bodies, modes
}

// TestR117_EachAppGetsABuildKitOfItsOwn asserts R-117 and R-111 for what is
// asked of Docker: two apps' daemons are two containers on two networks of
// their own, each the pinned image under the narrowed seccomp profile,
// listening for certified clients only, with no volume of a name that could
// outlive it, no restart policy, and never a runtime socket (R-112).
func TestR117_EachAppGetsABuildKitOfItsOwn(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	m := testManaged(t, f)

	a, err := m.launch(ctx, "app_01AAA")
	require.NoError(t, err)
	b, err := m.launch(ctx, "app_01BBB")
	require.NoError(t, err)
	t.Cleanup(func() { _ = a.cli.Close(); _ = b.cli.Close() })

	require.Len(t, f.created, 2)
	require.NotEqual(t, f.created[0].Name, f.created[1].Name, "two daemons")
	require.Equal(t, "pando-buildkit-app_01aaa-full-pando-1", f.created[0].Name)
	require.Equal(t, "c-"+f.created[0].Name, a.container)
	require.Equal(t, f.created[0].Name, a.network, "and its network goes with it")

	for i, o := range f.created {
		app := []string{"app_01AAA", "app_01BBB"}[i]
		require.Equal(t, managedImage, o.Config.Image)
		require.Contains(t, o.Config.Image, "@sha256:", "pinned by digest")
		require.Equal(t, managedArgs, []string(o.Config.Cmd))
		require.Contains(t, o.Config.Cmd, "--tlscacert", "the listener requires a client certificate")
		require.Equal(t, map[string]string{labelManagedBuild: labelDaemon, labelBuildApp: app, labelBuildOwner: "full-pando-1"}, o.Config.Labels)
		require.Contains(t, o.HostConfig.SecurityOpt, "apparmor=unconfined")
		var seccomp string
		for _, opt := range o.HostConfig.SecurityOpt {
			if p, ok := strings.CutPrefix(opt, "seccomp="); ok {
				seccomp = p
			}
		}
		require.Contains(t, seccomp, `"defaultAction":"SCMP_ACT_ERRNO"`, "the profile itself, not a path")
		require.True(t, json.Valid([]byte(seccomp)))
		require.Empty(t, o.HostConfig.RestartPolicy.Name, "a stopped daemon is replaced by the next build, never restarted")
		require.Empty(t, o.HostConfig.Binds, "nothing of the host's")
		require.Empty(t, o.HostConfig.Mounts, "no named volume: its state goes with the container")
		require.False(t, o.HostConfig.Privileged)
		require.Empty(t, o.HostConfig.PortBindings, "nothing published when Pando is in a container")
		require.Len(t, o.NetworkingConfig.EndpointsConfig, 1)
		require.Contains(t, o.NetworkingConfig.EndpointsConfig, o.Name, "created on its own network")

		netOpts := f.netOpts[o.Name]
		require.False(t, netOpts.Internal, "its own network is its way out")
		require.Equal(t, labelNetwork, netOpts.Labels[labelManagedBuild])
		require.Equal(t, 29, netOpts.IPAM.Config[0].Subnet.Bits(), "a /29 of Pando's pool")
		require.True(t, f.joined["c-"+o.Name+"@n-pando-buildctl"], "and on the network Pando reaches it on")
	}
	require.True(t, f.netOpts["pando-buildctl"].Internal, "the network Pando reaches daemons on leads nowhere")
	require.True(t, f.joined["full-pando-1@n-pando-buildctl"], "Pando joined it")
	require.Equal(t, 2, f.started)

	// Each got the authority's certificate and a server certificate of its
	// own: server authentication only, so a step that read it could not
	// present it as a client; and its key readable by its owner alone.
	require.Len(t, f.copied, 2)
	first, modes := untar(t, f.copied[0])
	second, _ := untar(t, f.copied[1])
	require.Contains(t, first[tlsCAFile], "BEGIN CERTIFICATE")
	require.Equal(t, int64(0o600), modes[tlsKeyFile])
	require.NotEqual(t, first[tlsCrtFile], second[tlsCrtFile], "a fresh leaf per daemon")
	block, _ := pem.Decode([]byte(first[tlsCrtFile]))
	leafCert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err)
	require.Equal(t, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, leafCert.ExtKeyUsage)
	require.NotContains(t, first[tlsCAFile], "PRIVATE KEY", "never the authority's key")
}

// TestAHostRunPandoReachesItsBuildKitsOnLoopback covers a developer running
// the binary on the host: there is no container to join a network, so each
// daemon is published on the host's loopback, on a port Docker picks, and
// nowhere else.
func TestAHostRunPandoReachesItsBuildKitsOnLoopback(t *testing.T) {
	f := newFakeDocker("")
	m := testManaged(t, f)
	inst, err := m.launch(context.Background(), "app_01AAA")
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.cli.Close() })
	require.Len(t, f.created, 1)
	require.NotContains(t, f.networks, "pando-buildctl", "no network to reach it on: Pando is not in a container")
	for _, bindings := range f.created[0].HostConfig.PortBindings {
		for _, b := range bindings {
			require.Equal(t, "127.0.0.1", b.HostIP.String(), "loopback only")
			require.Empty(t, b.HostPort, "a port Docker picks, so daemons never collide")
		}
	}
	addr, err := m.address(context.Background(), inst.container, "")
	require.NoError(t, err)
	require.Equal(t, "tcp://127.0.0.1:32768", addr)
	require.Equal(t, "host-not-a-container", f.created[0].Config.Labels[labelBuildOwner])
}

// TestPandosBuildKitsTrustTheInstallationsRegistry covers
// docker-compose.registry.yml: the registry's CA and a configuration naming it
// are written into each daemon before it starts, and it joins the registry's
// network because, and only because, it was named.
func TestPandosBuildKitsTrustTheInstallationsRegistry(t *testing.T) {
	f := newFakeDocker("pando-1")
	f.networks["pando-registry"] = network.Summary{Network: network.Network{Name: "pando-registry", ID: "pando-registry"}}
	m := testManaged(t, f)
	m.files = registryFiles("registry:5000", []byte("-----BEGIN CERTIFICATE-----\nca\n"))
	m.networks = []string{"pando-registry"}

	inst, err := m.launch(context.Background(), "app_01AAA")
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.cli.Close() })
	require.True(t, f.joined[inst.container+"@pando-registry"])

	got, _ := untar(t, f.copied[0])
	require.Equal(t, "[registry.\"registry:5000\"]\n  ca = [\"/home/user/.config/buildkit/registry-ca.crt\"]\n",
		got[".config/buildkit/buildkitd.toml"])
	require.Contains(t, got[".config/buildkit/registry-ca.crt"], "BEGIN CERTIFICATE")
	require.Nil(t, registryFiles("", []byte("ca")), "half a registry is none")

	// A registry network that is not there.
	g := newFakeDocker("pando-1")
	n := testManaged(t, g)
	n.networks = []string{"missing"}
	_, err = n.launch(context.Background(), "app_01AAA")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "PANDO_BUILDKIT_NETWORKS")
}

// TestEachInstallationOnADaemonRunsItsOwnBuildKits asserts the names come
// from the Compose project of Pando's own container, so a second installation
// on the same daemon — the deploy QA run's beside an operator's — has its own
// daemons and control network, and sweeping one never removes the other's.
func TestEachInstallationOnADaemonRunsItsOwnBuildKits(t *testing.T) {
	f := newFakeDocker("qa-pando-1")
	f.containers["qa-pando-1"].Config.Labels = map[string]string{labelComposeProject: "pandoqa"}
	m := testManaged(t, f)
	inst, err := m.launch(context.Background(), "app_01AAA")
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.cli.Close() })
	require.True(t, strings.HasPrefix(f.created[0].Name, "pandoqa-buildkit-app_01aaa-"), f.created[0].Name)
	require.Contains(t, f.networks, "pandoqa-buildctl")
}

// TestR111_OnPodmanPandoRejoinsItsBuildNetwork asserts Pando, back from a
// reboot and still on the network it reaches its daemons on, carries on when
// Podman says so in its own words ("network is already connected"); taken as
// a failure, no app could be built (issue #179).
func TestR111_OnPodmanPandoRejoinsItsBuildNetwork(t *testing.T) {
	f := newFakeDocker("pando-1")
	f.attached = "network is already connected"
	f.joined["full-pando-1@n-pando-buildctl"] = true
	m := testManaged(t, f)
	inst, err := m.launch(context.Background(), "app_01AAA")
	require.NoError(t, err)
	_ = inst.cli.Close()

	g := newFakeDocker("pando-1")
	g.attached = "boom"
	g.joined["full-pando-1@n-pando-buildctl"] = true
	_, err = testManaged(t, g).launch(context.Background(), "app_01AAA")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), "any other refusal to join is still one")
}

// TestR111_OnPodmanBuildKitGetsItsProfileAsAFile asserts that a BuildKit
// Pando runs is made under the narrowed seccomp profile on an engine that
// takes the profile only as a path. Podman's Docker-compatible API reads
// seccomp= as a file to open, so Docker's inline profile failed every create
// there (issue #179).
func TestR111_OnPodmanBuildKitGetsItsProfileAsAFile(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	f.pathOnly = true
	m := testManaged(t, f)
	f.containers["pando-1"].Mounts = []container.MountPoint{
		{Destination: "/etc/traefik/dynamic", Source: "/host/volumes/traefik/_data"},
		{Destination: m.dataDir, Source: "/host/volumes/pando-data/_data"},
	}

	inst, err := m.launch(ctx, "app_01AAA")
	require.NoError(t, err)
	_ = inst.cli.Close()
	require.Contains(t, f.created[0].HostConfig.SecurityOpt, "seccomp=/host/volumes/pando-data/_data/"+profileFileName, "the profile, by its path on the host")
	written, err := os.ReadFile(filepath.Join(m.dataDir, profileFileName))
	require.NoError(t, err)
	require.Equal(t, compactProfile(), string(written), "the same narrowed profile Docker is given inline")

	// With no mount holding the data directory there is nowhere to put it the
	// engine can see, and the refusal says what to change.
	g := newFakeDocker("pando-1")
	g.pathOnly = true
	_, err = testManaged(t, g).launch(ctx, "app_01AAA")
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Remedy, "on a volume")
}

// The profile file goes where the engine can see it, or the create is refused
// saying what to change: no directory to write it to, no mount holding it.
func TestTheProfileFileIsRefusedWhereTheEngineCouldNotReadIt(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))
	for name, setup := range map[string]func(m *managed){
		"cannot write it": func(m *managed) {
			require.NoError(t, os.Mkdir(filepath.Join(m.dataDir, profileFileName), 0o755))
		},
		"no mount": func(*managed) {},
	} {
		f := newFakeDocker("pando-1")
		f.pathOnly = true
		m := testManaged(t, f)
		setup(m)
		_, err := m.launch(context.Background(), "app_01AAA")
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err), name)
		require.Contains(t, errs.As(err).Message, "seccomp profile only as a file", name)
	}
}

func TestTheProfileDirIsBesideTheBuildCache(t *testing.T) {
	t.Setenv("PANDO_BUILD_CACHE_DIR", "/srv/pando/buildcache")
	require.Equal(t, "/srv/pando", (&managed{}).profileDir())
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

var errDaemon = errors.New("the daemon said no")

// TestR117_ADaemonThatCouldNotStartLeavesNothing asserts every Docker call a
// launch makes that fails is reported as what Pando was doing, with an adapter
// code — and that whatever it had made by then is removed, so a failed start
// leaves no container or network behind.
func TestR117_ADaemonThatCouldNotStartLeavesNothing(t *testing.T) {
	for method, code := range map[string]errs.Code{
		"NetworkList":     errs.AdapterUnavailable,
		"NetworkCreate":   errs.AdapterFailed,
		"NetworkConnect":  errs.AdapterFailed,
		"ImageInspect":    errs.AdapterFailed, // then the pull fails too
		"ContainerCreate": errs.AdapterFailed,
		"ContainerStart":  errs.AdapterFailed,
		"CopyToContainer": errs.AdapterFailed,
	} {
		f := newFakeDocker("pando-1")
		m := testManaged(t, f)
		f.fail = map[string]error{method: errDaemon}
		if method == "ImageInspect" {
			f.fail["ImagePull"] = errDaemon
		}
		_, err := m.launch(context.Background(), "app_01AAA")
		require.Equal(t, code, errs.CodeOf(err), method)
		f.fail = nil
		for name := range f.containers {
			require.Equal(t, "pando-1", name, "%s: nothing of the daemon's is left", method)
		}
		for name := range f.networks {
			require.Equal(t, "pando-buildctl", name, "%s: nothing of the daemon's is left", method)
		}
	}
}

// TestR117_PandoSweepsDaemonsNobodyIsUsing asserts crash recovery: as Pando
// starts it removes what its own last run left, what a Pando whose container
// is gone left, and the one BuildKit every app used to share — and leaves a
// running replica's daemons and another installation's alone.
func TestR117_PandoSweepsDaemonsNobodyIsUsing(t *testing.T) {
	f := newFakeDocker("pando-1")
	f.containers["pando-2"] = &container.InspectResponse{ID: "full-pando-2", Config: &container.Config{},
		State: &container.State{Running: true}}
	daemonOf := func(name, app, owner string) {
		labels := map[string]string{labelManagedBuild: labelDaemon, labelBuildApp: app, labelBuildOwner: owner}
		f.containers[name] = &container.InspectResponse{ID: "c-" + name, Config: &container.Config{Labels: labels},
			State: &container.State{Running: true}}
		f.networks[name] = network.Summary{Network: network.Network{Name: name, ID: "n-" + name,
			Labels: map[string]string{labelManagedBuild: labelNetwork, labelBuildApp: app, labelBuildOwner: owner}}}
	}
	daemonOf("pando-buildkit-app_1-mine", "app_1", "full-pando-1")
	daemonOf("pando-buildkit-app_2-gone", "app_2", "full-pando-9")
	daemonOf("pando-buildkit-app_3-live", "app_3", "full-pando-2")
	daemonOf("pando-buildkit-app_4-host", "app_4", "host-laptop")
	daemonOf("other-buildkit-app_5-gone", "app_5", "full-pando-9")
	f.containers["pando-buildkit"] = &container.InspectResponse{ID: "c-shared",
		Config: &container.Config{Labels: map[string]string{labelManagedBuild: "0123abcd"}}, State: &container.State{Running: true}}

	m := testManaged(t, f)
	m.sweep(context.Background(), true)

	require.ElementsMatch(t, []string{"pando-buildkit-app_1-mine", "pando-buildkit-app_2-gone", "pando-buildkit"}, f.removed)
	require.Contains(t, f.volumes, "pando-buildkit-cache", "the shared one's cache goes with it")
	require.ElementsMatch(t, []string{"other-buildkit-app_5-gone", "pando-buildctl", "pando-buildkit-app_3-live", "pando-buildkit-app_4-host"}, f.networkNames())

	// Later sweeps leave this Pando's own.
	f.removed = nil
	daemonOf("pando-buildkit-app_6-mine", "app_6", "full-pando-1")
	m.sweep(context.Background(), false)
	require.Empty(t, f.removed)
}

// TestR117_DeletingAnAppRemovesItsDaemons asserts a deleted app's daemons
// started by other replicas are removed with it, and no other app's.
func TestR117_DeletingAnAppRemovesItsDaemons(t *testing.T) {
	f := newFakeDocker("pando-1")
	for _, d := range []struct{ name, app, owner string }{
		{"pando-buildkit-app_1-other", "app_1", "full-pando-2"},
		{"pando-buildkit-app_1-mine", "app_1", "full-pando-1"},
		{"pando-buildkit-app_2-other", "app_2", "full-pando-2"},
	} {
		f.containers[d.name] = &container.InspectResponse{ID: "c-" + d.name, Config: &container.Config{
			Labels: map[string]string{labelManagedBuild: labelDaemon, labelBuildApp: d.app, labelBuildOwner: d.owner}},
			State: &container.State{Running: true}}
	}
	m := testManaged(t, f)
	require.NoError(t, m.removeApp(context.Background(), "app_1"))
	require.Equal(t, []string{"pando-buildkit-app_1-other"}, f.removed, "this Pando's own is its pool's to remove")

	f.fail = map[string]error{"ContainerList": errDaemon}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(m.removeApp(context.Background(), "app_1")))
}

// TestR117_PlanTimeStartsNoBuildKit asserts the health check the planner
// calls before every plan starts no daemon and waits on none: it asks Docker,
// readies the build authority, and fetches a missing image in the background.
func TestR117_PlanTimeStartsNoBuildKit(t *testing.T) {
	f := newFakeDocker("pando-1")
	f.noImage = true
	m := testManaged(t, f)
	require.NoError(t, m.healthy(context.Background()))
	require.Empty(t, f.created, "no daemon at plan time")
	require.Eventually(t, func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.pulled == 1
	}, testWait, testTick, "the image is fetched in the background")
	_, err := os.Stat(filepath.Join(m.dataDir, buildAuthorityFile))
	require.NoError(t, err, "the build authority is ready")

	f.fail = map[string]error{"Ping": errDaemon}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(m.healthy(context.Background())))

	g := newFakeDocker("pando-1")
	n := testManaged(t, g)
	blocked := filepath.Join(n.dataDir, "a-file")
	require.NoError(t, os.WriteFile(blocked, nil, 0o600))
	n.dataDir = filepath.Join(blocked, "data")
	err = n.healthy(context.Background())
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err), "an authority it cannot make")
	require.Contains(t, errs.As(err).Message, buildAuthorityFile)
}

// A launch canceled while its daemon starts stops waiting, and leaves nothing.
func TestALaunchCanceledWhileBuildKitStartsStopsWaiting(t *testing.T) {
	f := newFakeDocker("pando-1")
	m := testManaged(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	m.answers = func(context.Context, *bkclient.Client) error {
		cancel()
		return errors.New("connection refused")
	}
	_, err := m.launch(ctx, "app_01AAA")
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, f.removed, 1)
}

func TestDaemonNamesAreSafeForDocker(t *testing.T) {
	m := &managed{project: "pando", owner: "host-My Laptop.local"}
	require.Equal(t, "pando-buildkit-none-my-laptop.lo", m.daemonName(""))
	require.Equal(t, "pando-buildkit-app_01abc-my-laptop.lo", m.daemonName("app_01ABC"))
}

// TestAPandoInAContainerReachesItsBuildKitsByAddress covers a daemon whose
// name is too long to be a DNS label, as one under a Compose project of
// pando-smoke was: Pando dials its address on the control network, never its
// name, unless the engine reports none.
func TestAPandoInAContainerReachesItsBuildKitsByAddress(t *testing.T) {
	ctx := context.Background()
	f := newFakeDocker("pando-1")
	m := testManaged(t, f)
	m.project, m.named = "a-compose-project-with-a-long-name", true
	inst, err := m.launch(ctx, "app_01M5JDHNXNGDAMD4PX1PP9G75N")
	require.NoError(t, err)
	t.Cleanup(func() { _ = inst.cli.Close() })
	name := m.daemonName("app_01M5JDHNXNGDAMD4PX1PP9G75N")
	require.Greater(t, len(name), 63, "not a DNS label")

	addr, err := m.address(ctx, inst.container, name)
	require.NoError(t, err)
	require.Regexp(t, `^tcp://10\.99\.0\.\d+:1234$`, addr)

	f.mu.Lock()
	f.containers[name].NetworkSettings = nil
	f.mu.Unlock()
	addr, err = m.address(ctx, inst.container, name)
	require.NoError(t, err)
	require.Equal(t, "tcp://"+name+":1234", addr, "the name, when the engine reports no address")

	_, err = m.address(ctx, "gone", name)
	require.Error(t, err)
}

// A Pando in a container that Docker could not answer for once is not taken
// to run on the host: setup fails, and the next try finds its container.
func TestADockerHiccupDoesNotMakeAContainerizedPandoHostRun(t *testing.T) {
	f := newFakeDocker("pando-1")
	m := testManaged(t, f)
	f.fail = map[string]error{"ContainerInspect": errDaemon}
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(m.setup(context.Background())))
	require.False(t, m.ready)

	f.fail = nil
	require.NoError(t, m.setup(context.Background()))
	require.True(t, m.inContainer)
	require.Equal(t, "full-pando-1", m.owner)
}
