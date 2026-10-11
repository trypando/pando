package incus

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/pkg/sftp"
	"github.com/stretchr/testify/require"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/hostagent"
)

// fakeIncus is incusAPI in memory.
type fakeIncus struct {
	mu sync.Mutex

	extensions []string
	project    string
	members    []incusapi.ClusterMember
	resources  map[string]*incusapi.Resources
	networks   map[string]*incusapi.Network
	profiles   map[string]*incusapi.Profile
	drivers    map[string]string
	instances  map[string]*incusapi.InstanceFull
	acls       map[string]*incusapi.NetworkACL
	volumes    map[string]*incusapi.StorageVolume
	aliases    map[string]bool
	files      map[string]*memFS

	created  []incusapi.InstancesPost
	targets  []string
	states   []string // "name action"
	execs    [][]string
	execCode int

	// refuseLive refuses an update to a running instance, as Incus does a
	// memory decrease; refuseCleanStop refuses a stop that is not forced.
	refuseLive      bool
	refuseCleanStop bool

	// fail makes the named method fail with that error.
	fail map[string]error
}

func newFakeIncus() *fakeIncus {
	return &fakeIncus{
		extensions: append([]string{}, required...),
		project:    "default",
		resources: map[string]*incusapi.Resources{
			"": res(16, 64<<30, 16<<30),
		},
		networks: map[string]*incusapi.Network{"incusbr0": {Name: "incusbr0", Type: "bridge", Managed: true}},
		profiles: map[string]*incusapi.Profile{"default": {Name: "default", ProfilePut: incusapi.ProfilePut{
			Devices: map[string]map[string]string{"root": {"type": "disk", "path": "/", "pool": "default"}},
		}}},
		drivers:   map[string]string{"default": "zfs", "local": "lvm"},
		instances: map[string]*incusapi.InstanceFull{},
		acls:      map[string]*incusapi.NetworkACL{},
		volumes:   map[string]*incusapi.StorageVolume{},
		aliases:   map[string]bool{guestAlias(): true},
		files:     map[string]*memFS{},
	}
}

func res(cpus int, memory, used uint64) *incusapi.Resources {
	r := &incusapi.Resources{}
	r.CPU.Total = uint64(cpus)
	r.Memory.Total = memory
	r.Memory.Used = used
	return r
}

func (f *fakeIncus) Server(context.Context) (*incusapi.Server, error) {
	if err := f.failure("Server"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	s := &incusapi.Server{}
	s.APIExtensions = f.extensions
	s.Environment.Project = f.project
	s.Environment.ServerVersion = "6.21"
	s.Environment.Architectures = []string{"x86_64"}
	return s, nil
}

func (f *fakeIncus) Members(context.Context) ([]incusapi.ClusterMember, error) {
	if err := f.failure("Members"); err != nil {
		return nil, err
	}
	return f.members, nil
}

func (f *fakeIncus) Resources(_ context.Context, member string) (*incusapi.Resources, error) {
	if err := f.failure("Resources"); err != nil {
		return nil, err
	}
	r, ok := f.resources[member]
	if !ok {
		return nil, errNotFound
	}
	return r, nil
}

func (f *fakeIncus) Instances(context.Context) ([]incusapi.InstanceFull, error) {
	if err := f.failure("Instances"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []incusapi.InstanceFull
	for _, i := range f.instances {
		out = append(out, *i)
	}
	return out, nil
}

func (f *fakeIncus) Instance(_ context.Context, name string) (*incusapi.Instance, string, error) {
	if err := f.failure("Instance"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[name]
	if !ok {
		return nil, "", errNotFound
	}
	c := i.Instance
	return &c, "etag", nil
}

func (f *fakeIncus) InstanceState(_ context.Context, name string) (*incusapi.InstanceState, error) {
	if err := f.failure("InstanceState"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[name]
	if !ok {
		return nil, errNotFound
	}
	st := &incusapi.InstanceState{Status: i.Status}
	if i.Status == "Running" {
		st.Network = map[string]incusapi.InstanceStateNetwork{
			"podman0": {Hwaddr: "aa:aa:aa:aa:aa:aa", Addresses: []incusapi.InstanceStateNetworkAddress{
				{Family: "inet", Address: "10.213.0.1", Scope: "global"},
			}},
			"enp5s0": {Hwaddr: "00:16:3e:00:00:01", Addresses: []incusapi.InstanceStateNetworkAddress{
				{Family: "inet6", Address: "fe80::1", Scope: "link"},
				{Family: "inet", Address: "10.1.0.42", Scope: "global"},
			}},
		}
	}
	return st, nil
}

func (f *fakeIncus) CreateInstance(_ context.Context, target string, post incusapi.InstancesPost) error {
	if err := f.failure("CreateInstance"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, post)
	f.targets = append(f.targets, target)
	location := target
	if location == "" && len(f.members) > 0 {
		location = f.members[0].ServerName
	}
	f.instances[post.Name] = &incusapi.InstanceFull{Instance: incusapi.Instance{
		Name: post.Name, Type: string(post.Type), Status: "Stopped", Location: location,
		InstancePut: post.InstancePut,
	}, State: &incusapi.InstanceState{}}
	if f.instances[post.Name].Config == nil {
		f.instances[post.Name].Config = map[string]string{}
	}
	f.instances[post.Name].Config["volatile."+nicName+".hwaddr"] = "00:16:3e:00:00:01"
	return nil
}

func (f *fakeIncus) UpdateInstance(_ context.Context, name string, put incusapi.InstancePut, _ string) error {
	if err := f.failure("UpdateInstance"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[name]
	if !ok {
		return errNotFound
	}
	if f.refuseLive && i.Status == "Running" {
		return errors.New("Cannot reduce memory of a running VM")
	}
	i.InstancePut = put
	return nil
}

func (f *fakeIncus) SetState(_ context.Context, name, action string, force bool) error {
	if err := f.failure("SetState"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.instances[name]
	if !ok {
		return errNotFound
	}
	f.states = append(f.states, name+" "+action+map[bool]string{true: " force"}[force])
	if action == "stop" && !force && f.refuseCleanStop {
		return errors.New("Instance didn't stop within the timeout")
	}
	switch action {
	case "start":
		i.Status = "Running"
	case "stop":
		i.Status = "Stopped"
	}
	return nil
}

func (f *fakeIncus) DeleteInstance(_ context.Context, name string) error {
	if err := f.failure("DeleteInstance"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[name]; !ok {
		return errNotFound
	}
	delete(f.instances, name)
	return nil
}

func (f *fakeIncus) Exec(_ context.Context, name string, command []string, _ io.Reader, _, _ io.Writer) (int, error) {
	if err := f.failure("Exec"); err != nil {
		return -1, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.instances[name]; !ok {
		return -1, errNotFound
	}
	f.execs = append(f.execs, command)
	return f.execCode, nil
}

func (f *fakeIncus) Network(_ context.Context, name string) (*incusapi.Network, string, error) {
	if err := f.failure("Network"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return nil, "", errNotFound
	}
	c := *n
	return &c, "etag", nil
}

func (f *fakeIncus) UpdateNetwork(_ context.Context, name string, put incusapi.NetworkPut, _ string) error {
	if err := f.failure("UpdateNetwork"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.networks[name]
	if !ok {
		return errNotFound
	}
	n.NetworkPut = put
	return nil
}

func (f *fakeIncus) ACL(_ context.Context, name string) (*incusapi.NetworkACL, string, error) {
	if err := f.failure("ACL"); err != nil {
		return nil, "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	acl, ok := f.acls[name]
	if !ok {
		return nil, "", errNotFound
	}
	c := *acl
	return &c, "etag", nil
}

func (f *fakeIncus) CreateACL(_ context.Context, acl incusapi.NetworkACLsPost) error {
	if err := f.failure("CreateACL"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acls[acl.Name] = &incusapi.NetworkACL{NetworkACLPost: acl.NetworkACLPost, NetworkACLPut: acl.NetworkACLPut}
	return nil
}

func (f *fakeIncus) UpdateACL(_ context.Context, name string, put incusapi.NetworkACLPut, _ string) error {
	if err := f.failure("UpdateACL"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	acl, ok := f.acls[name]
	if !ok {
		return errNotFound
	}
	acl.NetworkACLPut = put
	return nil
}

func (f *fakeIncus) Profile(_ context.Context, name string) (*incusapi.Profile, error) {
	if err := f.failure("Profile"); err != nil {
		return nil, err
	}
	p, ok := f.profiles[name]
	if !ok {
		return nil, errNotFound
	}
	return p, nil
}

func (f *fakeIncus) PoolDriver(_ context.Context, pool string) (string, error) {
	if err := f.failure("PoolDriver"); err != nil {
		return "", err
	}
	d, ok := f.drivers[pool]
	if !ok {
		return "", errNotFound
	}
	return d, nil
}

func (f *fakeIncus) Volume(_ context.Context, pool, name string) (*incusapi.StorageVolume, error) {
	if err := f.failure("Volume"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[pool+"/"+name]
	if !ok {
		return nil, errNotFound
	}
	c := *v
	for _, i := range f.instances {
		for _, d := range i.Devices {
			if d["type"] == "disk" && d["source"] == name {
				c.UsedBy = append(c.UsedBy, "/1.0/instances/"+i.Name)
			}
		}
	}
	return &c, nil
}

func (f *fakeIncus) Volumes(_ context.Context, pool string) ([]incusapi.StorageVolume, error) {
	if err := f.failure("Volumes"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []incusapi.StorageVolume
	for k, v := range f.volumes {
		if strings.HasPrefix(k, pool+"/") {
			out = append(out, *v)
		}
	}
	return out, nil
}

func (f *fakeIncus) CreateVolume(_ context.Context, _, pool string, post incusapi.StorageVolumesPost) error {
	if err := f.failure("CreateVolume"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.volumes[pool+"/"+post.Name] = &incusapi.StorageVolume{Name: post.Name, Type: post.Type, StorageVolumePut: post.StorageVolumePut}
	return nil
}

func (f *fakeIncus) UpdateVolume(_ context.Context, pool, name string, put incusapi.StorageVolumePut) error {
	if err := f.failure("UpdateVolume"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.volumes[pool+"/"+name]
	if !ok {
		return errNotFound
	}
	v.StorageVolumePut = put
	return nil
}

func (f *fakeIncus) DeleteVolume(_ context.Context, pool, name string) error {
	if err := f.failure("DeleteVolume"); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[pool+"/"+name]; !ok {
		return errNotFound
	}
	delete(f.volumes, pool+"/"+name)
	return nil
}

func (f *fakeIncus) VolumeFiles(_ context.Context, pool, name string) (*sftp.Client, error) {
	if err := f.failure("VolumeFiles"); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.volumes[pool+"/"+name]; !ok {
		return nil, errNotFound
	}
	fs, ok := f.files[pool+"/"+name]
	if !ok {
		return nil, errNotFound
	}
	return fs.client(), nil
}

func (f *fakeIncus) ImageAlias(_ context.Context, name string) (*incusapi.ImageAliasesEntry, error) {
	if err := f.failure("ImageAlias"); err != nil {
		return nil, err
	}
	if !f.aliases[name] {
		return nil, errNotFound
	}
	return &incusapi.ImageAliasesEntry{Name: name}, nil
}

func (f *fakeIncus) Publish(_ context.Context, _, alias string, _ map[string]string) error {
	if err := f.failure("Publish"); err != nil {
		return err
	}
	f.aliases[alias] = true
	return nil
}

// memFS is an in-memory file system served over SFTP, a new client per
// open, as Incus serves a volume.
type memFS struct {
	t *testing.T
	h sftp.Handlers
}

func newMemFS(t *testing.T) *memFS {
	h := sftp.InMemHandler()
	h.FileCmd = setstatDirs{h.FileCmd}
	return &memFS{t: t, h: h}
}

func (m *memFS) client() *sftp.Client {
	a, b := net.Pipe()
	srv := sftp.NewRequestServer(b, m.h)
	go func() { _ = srv.Serve() }()
	c, err := sftp.NewClientPipe(a, a)
	require.NoError(m.t, err)
	m.t.Cleanup(func() { _ = c.Close(); _ = srv.Close() })
	return c
}

// memorySFTP is an SFTP client over a new in-memory file system.
func memorySFTP(t *testing.T) *sftp.Client { return newMemFS(t).client() }

// setstatDirs accepts a mode change on a directory, which the in-memory
// server refuses and a file system (and Incus's server) does not.
type setstatDirs struct{ sftp.FileCmder }

func (s setstatDirs) Filecmd(r *sftp.Request) error {
	err := s.FileCmder.Filecmd(r)
	if err != nil && r.Method == "Setstat" {
		return nil
	}
	return err
}

// fakeInner is the Docker adapter in a VM.
type fakeInner struct {
	api.RuntimeAdapter
	applied  []api.BundlePlan
	observed api.ObservedBundle
}

func (f *fakeInner) Apply(_ context.Context, p api.BundlePlan) (api.BundleHandle, error) {
	f.applied = append(f.applied, p)
	return api.BundleHandle{BundleID: p.BundleID}, nil
}

func (f *fakeInner) Observe(context.Context, api.BundleRef) (api.ObservedBundle, error) {
	return f.observed, nil
}

func (f *fakeInner) Upstream(_ context.Context, ref api.WorkloadRef, port int) (api.Upstream, error) {
	return api.Upstream{URL: "http://pando-" + ref.BundleID + "-" + ref.Workload + ":" + strconv.Itoa(port)}, nil
}

func (f *fakeInner) Trial(context.Context, api.TrialRequest) (api.TrialResult, error) {
	return api.TrialResult{Started: true, ObservedPorts: []int{80}}, nil
}

func (f *fakeInner) Logs(context.Context, api.WorkloadRef, api.LogOptions) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("log")), nil
}

func (f *fakeInner) Exec(context.Context, api.WorkloadRef, api.ExecRequest) (api.ExecSession, error) {
	return nil, nil
}

func (f *fakeInner) Usage(context.Context, api.BundleRef) (api.BundleUsage, error) {
	return api.BundleUsage{Workloads: []api.WorkloadUsage{{Workload: "web", Running: true}}}, nil
}

type fakeAgent struct{ ensured int }

func (f *fakeAgent) Ensure(context.Context) (bool, error) { f.ensured++; return f.ensured == 1, nil }

// testAdapter is an adapter over the fakes, configured from cfg after its
// defaults are filled.
func testAdapter(t *testing.T, f *fakeIncus, mutate func(*Config)) (*Adapter, map[string]*fakeInner, map[string]*fakeAgent) {
	t.Helper()
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	cfg := Config{
		Network:      "incusbr0",
		ProxySources: "10.1.0.10",
		AgentImage:   "trypando/pando:0.4.0",
		Credentials:  Credentials{AgentAuthority: string(authority)},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	require.NoError(t, cfg.validate())

	inners := map[string]*fakeInner{}
	agents := map[string]*fakeAgent{}
	a := &Adapter{
		connect:   func(Config) (incusAPI, error) { return f, nil },
		newDocker: func(incusAPI, string) (*client.Client, error) { return nil, nil },
		readyWait: time.Second,
		sleep:     func(time.Duration) <-chan time.Time { return time.After(time.Millisecond) },
	}
	var current string
	a.newDocker = func(_ incusAPI, instance string) (*client.Client, error) { current = instance; return nil, nil }
	a.newInner = func(_ *client.Client, _ docker.Config) inner {
		in := &fakeInner{}
		inners[current] = in
		return in
	}
	a.newAgent = func(_ *client.Client, instance string) agentEnsurer {
		ag := &fakeAgent{}
		agents[instance] = ag
		return ag
	}
	a.use(cfg)
	as, err := hostagent.ParseAuthorities(authority)
	require.NoError(t, err)
	cert, err := as.IssueClient()
	require.NoError(t, err)
	a.api, a.authorities, a.clientCert = f, as, cert
	return a, inners, agents
}

func webPlan(bundleID string) api.BundlePlan {
	return api.BundlePlan{
		BundleID: bundleID,
		Workloads: []api.WorkloadPlan{{
			Name: "web", Image: "nginx", Exposed: true,
			Ports:     []api.PortPlan{{Number: 80}},
			Resources: api.ResourcePlan{CPUMillis: 1500, MemoryBytes: 512 << 20},
		}},
		Network: api.NetworkPlan{Private: true},
	}
}

// failure is the error the fake was told to fail a method with, if any.
func (f *fakeIncus) failure(method string) error {
	if f.fail == nil {
		return nil
	}
	return f.fail[method]
}
