package incus

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/trypando/pando/internal/adapter/runtime/incus/incusapi"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

var ctx = context.Background()

// TestR115_TheIncusRuntimeReportsVMIsolation asserts R-115: the class policy
// floors compare against is vm, and built images arrive from a registry.
func TestR115_TheIncusRuntimeReportsVMIsolation(t *testing.T) {
	a, _, _ := testAdapter(t, newFakeIncus(), nil)
	caps, err := a.Capabilities(ctx)
	require.NoError(t, err)
	require.Equal(t, spec.IsolationVM, caps.IsolationClass)
	require.True(t, caps.SupportsPrivateNetwork)
	require.Equal(t, []api.ImageDelivery{api.ImageDeliveryRegistry}, caps.ImageDelivery)
	require.False(t, caps.SupportsEdge, "the edge runs on the Docker runtime")
	require.Equal(t, "linux/amd64", caps.Platform)

	_, err = a.ImportImage(ctx, bytes.NewReader(nil))
	require.Equal(t, errs.PlanCapabilityUnsupported, errs.As(err).Code)
	require.Contains(t, errs.As(err).Remedy, "image registry adapter")
}

// TestR026_AVMHasOnlyTheAdaptersNICAndItsACL asserts R-026: a VM gets no
// profile (a default profile commonly puts it on the LAN), and exactly one
// NIC, behind the adapter's ACL, dropping what the ACL does not admit.
func TestR026_AVMHasOnlyTheAdaptersNICAndItsACL(t *testing.T) {
	f := newFakeIncus()
	a, inners, agents := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_01ABC"))
	require.NoError(t, err)

	require.Len(t, f.created, 1)
	post := f.created[0]
	require.Equal(t, "pando-app-01abc", post.Name)
	require.Equal(t, incusapi.InstanceTypeVM, post.Type)
	require.NotNil(t, post.Profiles)
	require.Empty(t, post.Profiles)

	var nics []map[string]string
	for _, d := range post.Devices {
		if d["type"] == "nic" {
			nics = append(nics, d)
		}
	}
	require.Len(t, nics, 1)
	require.Equal(t, "incusbr0", nics[0]["network"])
	require.Equal(t, "pando-ingress", nics[0]["security.acls"])
	require.Equal(t, "drop", nics[0]["security.acls.default.ingress.action"])
	require.Equal(t, "true", nics[0]["security.port_isolation"], "R-025 between VMs on one bridge")

	// Sized from the workloads' limits and the guest's overhead.
	require.Equal(t, "2", post.Config["limits.cpu"])
	require.Equal(t, "1024MiB", post.Config["limits.memory"])
	require.Equal(t, "app_01ABC", post.Config[keyBundle])

	// The agent before the app, then the Docker adapter inside the VM.
	require.Equal(t, 1, agents["pando-app-01abc"].ensured)
	require.Len(t, inners["pando-app-01abc"].applied, 1)
}

// TestR026_TheACLAdmitsOnlyTheProxyOnTheAgentPort asserts R-025 and R-026:
// the ACL's only rule admits Pando's addresses to the agent's port.
func TestR026_TheACLAdmitsOnlyTheProxyOnTheAgentPort(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, func(c *Config) { c.ProxySources = "10.1.0.10, 10.1.0.11/32"; c.Project = "pando" })
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)

	acl := f.acls["pando-ingress-pando"]
	require.NotNil(t, acl)
	require.Equal(t, []incusapi.NetworkACLRule{{
		Action: aclAllow, Source: "10.1.0.10/32,10.1.0.11/32", Protocol: "tcp",
		DestinationPort: "7443", State: "enabled", Description: "Pando's proxy to the forwarding agent",
	}}, acl.Ingress)

	// Changed settings rewrite it.
	a.sources = a.sources[:1]
	require.NoError(t, a.ensureACL(ctx))
	require.Equal(t, "10.1.0.10/32", f.acls["pando-ingress-pando"].Ingress[0].Source)
}

// TestR254_ConfigurationThatCannotWorkIsRefused asserts R-254: a setting
// that cannot work is refused when it is saved, saying what to set.
func TestR254_ConfigurationThatCannotWorkIsRefused(t *testing.T) {
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	base := func() map[string]any {
		return map[string]any{
			"network": "incusbr0", "proxy_sources": "10.1.0.10", "agent_image": "trypando/pando:0.4.0",
			"credentials": map[string]any{"agent_authority": string(authority)},
		}
	}
	for name, tc := range map[string]struct {
		change func(map[string]any)
		says   string
	}{
		"no network":            {func(m map[string]any) { delete(m, "network") }, "needs the network"},
		"no proxy sources":      {func(m map[string]any) { delete(m, "proxy_sources") }, "proxy sources"},
		"every address":         {func(m map[string]any) { m["proxy_sources"] = "0.0.0.0/0" }, "admits every address"},
		"no agent image":        {func(m map[string]any) { delete(m, "agent_image") }, "forwarding agent"},
		"https without a cert":  {func(m map[string]any) { m["address"] = "https://incus:8443" }, "certificate is not set"},
		"tcp":                   {func(m map[string]any) { m["address"] = "tcp://incus:8443" }, "not usable"},
		"a tiny disk":           {func(m map[string]any) { m["root_size_bytes"] = 1 << 30 }, "4 GiB"},
		"no authority":          {func(m map[string]any) { m["credentials"] = map[string]any{} }, "certificate authority"},
		"a bad target":          {func(m map[string]any) { m["target"] = "@" }, "member or group"},
		"an overlapping range?": {func(m map[string]any) { m["network_pool"] = "nowhere" }, "address range"},
	} {
		t.Run(name, func(t *testing.T) {
			m := base()
			tc.change(m)
			raw, _ := json.Marshal(m)
			err := New().Configure(ctx, raw)
			require.Error(t, err)
			require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
			require.Contains(t, errs.As(err).Message+errs.As(err).Remedy, tc.says)
		})
	}
}

// TestR254_ABridgeOnAClusterWithoutATargetIsRefused asserts R-254: on a
// cluster, a bridge exists separately on each member, so a VM Incus placed
// elsewhere would be unreachable; HealthCheck says so before a deploy fails.
func TestR254_ABridgeOnAClusterWithoutATargetIsRefused(t *testing.T) {
	f := newFakeIncus()
	f.members = []incusapi.ClusterMember{
		{ServerName: "one", Architecture: "x86_64", Status: "Online"},
		{ServerName: "two", Architecture: "x86_64", Status: "Online"},
	}
	a, _, _ := testAdapter(t, f, nil)
	err := a.HealthCheck(ctx)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "bridge")
	require.Contains(t, errs.As(err).Remedy, "OVN")

	// A local pool is refused for the same reason once the network spans.
	f.networks["ovn0"] = &incusapi.Network{Name: "ovn0", Type: "ovn"}
	a, _, _ = testAdapter(t, f, func(c *Config) { c.Network = "ovn0"; c.StoragePool = "local" })
	err = a.HealthCheck(ctx)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "storage pool local")

	// Pinned to one member, both are fine.
	a, _, _ = testAdapter(t, f, func(c *Config) { c.Target = "one"; c.StoragePool = "local" })
	require.NoError(t, a.HealthCheck(ctx))

	// Members of two architectures cannot share images.
	f.members[1].Architecture = "aarch64"
	f.drivers["ceph"] = "ceph"
	a, _, _ = testAdapter(t, f, func(c *Config) { c.Network = "ovn0"; c.StoragePool = "ceph" })
	err = a.HealthCheck(ctx)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "architectures")
}

// TestR254_AnIncusWithoutWhatTheAdapterNeedsIsUnavailable asserts R-254.
func TestR254_AnIncusWithoutWhatTheAdapterNeedsIsUnavailable(t *testing.T) {
	f := newFakeIncus()
	f.extensions = f.extensions[1:]
	a, _, _ := testAdapter(t, f, nil)
	err := a.HealthCheck(ctx)
	require.Equal(t, errs.AdapterUnavailable, errs.As(err).Code)
	require.Contains(t, errs.As(err).Remedy, "6.3")

	f = newFakeIncus()
	f.project = "elsewhere"
	a, _, _ = testAdapter(t, f, nil)
	require.Contains(t, errs.As(a.HealthCheck(ctx)).Message, "restricted")
}

// TestR190_TheClientCertificateIsACredential asserts R-190: the secrets the
// adapter holds are credentials, stored sealed, never settings.
func TestR190_TheClientCertificateIsACredential(t *testing.T) {
	creds := map[string]bool{}
	for _, f := range Info().Fields {
		creds[f.Key] = f.Credential
	}
	require.True(t, creds["tls_client"])
	require.True(t, creds["agent_authority"])
	require.False(t, creds["server_certificate"], "the server's certificate is public")
}

// TestR204_DestroyKeepsTheAppsVolumes asserts R-204: an app's volumes are
// Incus volumes attached to its VM, and outlive it unless asked otherwise.
func TestR204_DestroyKeepsTheAppsVolumes(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	p := webPlan("app_1")
	p.Volumes = []api.VolumePlan{{VolumeID: "vol_1", Name: "data"}}
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)

	dev := f.instances["pando-app-1"].Devices["vol-vol-1"]
	require.Equal(t, "pando-vol-1", dev["source"])
	require.Equal(t, volumeRoot+"/vol_1", dev["path"])
	require.Contains(t, f.volumes, "default/pando-vol-1")
	require.Equal(t, "10737418240B", f.volumes["default/pando-vol-1"].Config["size"])

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_1"}, api.DestroyOptions{KeepVolumes: true}))
	require.NotContains(t, f.instances, "pando-app-1")
	require.Contains(t, f.volumes, "default/pando-vol-1")

	// The next VM for the app gets them back.
	_, err = a.Apply(ctx, p)
	require.NoError(t, err)
	require.Equal(t, "pando-vol-1", f.instances["pando-app-1"].Devices["vol-vol-1"]["source"])

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_1"}, api.DestroyOptions{KeepVolumes: false}))
	require.NotContains(t, f.volumes, "default/pando-vol-1")
}

// TestR204_DestroyingAnAttachedVolumeDetachesItFirst asserts R-204's
// explicit removal path.
func TestR204_DestroyingAnAttachedVolumeDetachesItFirst(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	p := webPlan("app_1")
	p.Volumes = []api.VolumePlan{{VolumeID: "vol_1"}}
	_, err := a.Apply(ctx, p)
	require.NoError(t, err)

	require.NoError(t, a.DestroyVolume(ctx, api.VolumeHandle{VolumeID: "vol_1", Handle: "pando-vol-1"}))
	require.NotContains(t, f.instances["pando-app-1"].Devices, "vol-vol-1")
	require.NotContains(t, f.volumes, "default/pando-vol-1")
}

// TestR148_ObserveNeverStartsAStoppedVM asserts R-148: a stopped app's
// workloads are reported present and not running, from the VM's own record,
// and nothing is started to find out.
func TestR148_ObserveNeverStartsAStoppedVM(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)
	require.NoError(t, a.Stop(ctx, api.BundleRef{BundleID: "app_1"}))
	f.states = nil

	obs, err := a.Observe(ctx, api.BundleRef{BundleID: "app_1"})
	require.NoError(t, err)
	require.True(t, obs.Exists)
	require.Equal(t, []api.ObservedWorkload{{Name: "web", Present: true}}, obs.Workloads)
	require.Empty(t, f.states)

	obs, err = a.Observe(ctx, api.BundleRef{BundleID: "app_none"})
	require.NoError(t, err)
	require.False(t, obs.Exists)
}

// TestR151_AnInstanceThatIsNotTheAppsIsLeftAlone asserts that an instance
// someone else named like Pando's is never taken over or deleted.
func TestR151_AnInstanceThatIsNotTheAppsIsLeftAlone(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	require.NoError(t, f.CreateInstance(ctx, "", incusapi.InstancesPost{Name: "pando-app-1"}))

	_, err := a.Apply(ctx, webPlan("app_1"))
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "not this app's VM")

	require.NoError(t, a.Destroy(ctx, api.BundleRef{BundleID: "app_1"}, api.DestroyOptions{KeepVolumes: true}))
	require.Contains(t, f.instances, "pando-app-1")
	obs, err := a.Observe(ctx, api.BundleRef{BundleID: "app_1"})
	require.NoError(t, err)
	require.False(t, obs.Exists)
}

// TestR023_UpstreamDialsTheAgentInTheAppsVM asserts R-023: the proxy reaches
// a workload through its VM's agent, at the VM's address on the network.
func TestR023_UpstreamDialsTheAgentInTheAppsVM(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)

	up, err := a.Upstream(ctx, api.WorkloadRef{BundleID: "app_1", Workload: "web"}, 80)
	require.NoError(t, err)
	require.Equal(t, "http://pando-app_1-web:80", up.URL)
	require.NotNil(t, up.Dial)
	require.Equal(t, "pando-app-1/pando-app_1-web:80", up.PoolKey)
	require.Equal(t, "10.1.0.42", a.vms["app_1"].addr.String())
}

// TestR242_LargestFitIsTheRoomiestMember asserts R-242: a bundle must fit on
// one member, counting what every running instance there is limited to, and
// a redeploy counts its own VM as room.
func TestR242_LargestFitIsTheRoomiestMember(t *testing.T) {
	f := newFakeIncus()
	f.networks["ovn0"] = &incusapi.Network{Name: "ovn0", Type: "ovn"}
	f.drivers["ceph"] = "ceph"
	f.members = []incusapi.ClusterMember{
		{ServerName: "one", Architecture: "x86_64", Status: "Online"},
		{ServerName: "two", Architecture: "x86_64", Status: "Online"},
	}
	f.resources = map[string]*incusapi.Resources{"one": res(8, 32<<30, 4<<30), "two": res(8, 32<<30, 4<<30)}
	// Somebody else's VM on two.
	f.instances["theirs"] = &incusapi.InstanceFull{Instance: incusapi.Instance{
		Name: "theirs", Status: "Running", Location: "two",
		InstancePut: incusapi.InstancePut{Config: map[string]string{"limits.cpu": "4", "limits.memory": "20GiB"}},
	}}
	a, _, _ := testAdapter(t, f, func(c *Config) { c.Network = "ovn0"; c.StoragePool = "ceph" })

	c, err := a.Capacity(ctx)
	require.NoError(t, err)
	require.Equal(t, 16000, c.TotalCPUMillis)
	require.Equal(t, int64(64<<30), c.TotalMemoryBytes)
	require.NotNil(t, c.LargestFit)
	require.Equal(t, int64(28<<30), c.LargestFit.MemoryBytes, "one: 32 GiB, 4 GiB used")

	f.resources["one"] = res(8, 32<<30, 30<<30)
	fit, err := a.LargestFitFor(ctx, "app_1")
	require.NoError(t, err)
	require.Equal(t, int64(12<<30)-a.config.GuestOverheadBytes, fit.MemoryBytes, "two: 32 GiB less theirs' 20")
}

// TestR243_CapacityIsSummedFromTheMembers asserts R-243: capacity comes from
// Incus, and an operator's cap holds.
func TestR243_CapacityIsSummedFromTheMembers(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, func(c *Config) { c.TotalMemoryBytes = 8 << 30 })
	c, err := a.Capacity(ctx)
	require.NoError(t, err)
	require.Equal(t, 16000, c.TotalCPUMillis)
	require.Equal(t, int64(8<<30), c.TotalMemoryBytes)
	require.Nil(t, c.LargestFit, "one place: the totals say it")
}

// TestR097_ATrialRunsInAThrowawayVMThatIsRemoved asserts R-097: the trial's
// VM is deleted on every path.
func TestR097_ATrialRunsInAThrowawayVMThatIsRemoved(t *testing.T) {
	f := newFakeIncus()
	a, inners, _ := testAdapter(t, f, nil)
	res, err := a.Trial(ctx, api.TrialRequest{TrialID: "tr_1", Image: "nginx"})
	require.NoError(t, err)
	require.Equal(t, []int{80}, res.ObservedPorts)
	require.Len(t, f.created, 1)
	require.Equal(t, "tr_1", f.created[0].Config[keyTrial])
	require.Empty(t, f.created[0].Config[keyBundle])
	require.NotContains(t, f.instances, "pando-trial-tr-1")
	require.Contains(t, inners, "pando-trial-tr-1")
}

// TestR212_ARestoreCannotWriteThroughALinkItMade asserts R-212's restore
// handles an archive from an app Pando does not trust.
func TestR212_ARestoreCannotWriteThroughALinkItMade(t *testing.T) {
	c := memorySFTP(t)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./out", Typeflag: tar.TypeSymlink, Linkname: "/etc"}))
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "./out/passwd", Typeflag: tar.TypeReg, Mode: 0o644, Size: 1}))
	_, _ = tw.Write([]byte("x"))
	require.NoError(t, tw.Close())

	err := extract(ctx, c, tar.NewReader(&buf))
	require.Error(t, err)
	require.Contains(t, err.Error(), "through a link")
}

// TestR212_AVolumeRoundTripsThroughABackup asserts R-212 over SFTP.
func TestR212_AVolumeRoundTripsThroughABackup(t *testing.T) {
	f := newFakeIncus()
	a, _, _ := testAdapter(t, f, nil)
	_, err := a.CreateVolume(ctx, api.VolumeRequest{VolumeID: "vol_1", BundleID: "app_1"})
	require.NoError(t, err)
	_, err = a.CreateVolume(ctx, api.VolumeRequest{VolumeID: "vol_2", BundleID: "app_1"})
	require.NoError(t, err)
	srcFS, dstFS := newMemFS(t), newMemFS(t)
	f.files["default/pando-vol-1"], f.files["default/pando-vol-2"] = srcFS, dstFS
	src, dst := srcFS.client(), dstFS.client()

	require.NoError(t, src.MkdirAll("/db/sub"))
	w, err := src.Create("/db/sub/data")
	require.NoError(t, err)
	_, _ = w.Write([]byte("kept"))
	_ = w.Close()
	stale, err := dst.Create("/stale")
	require.NoError(t, err)
	_ = stale.Close()

	var backup bytes.Buffer
	require.NoError(t, a.SnapshotVolume(ctx, api.VolumeHandle{VolumeID: "vol_1", Handle: "pando-vol-1"}, &backup))
	require.NoError(t, a.RestoreVolume(ctx, api.VolumeHandle{VolumeID: "vol_2", Handle: "pando-vol-2"}, bytes.NewReader(backup.Bytes())))

	r, err := dst.Open("/db/sub/data")
	require.NoError(t, err)
	got, _ := io.ReadAll(r)
	require.Equal(t, "kept", string(got))
	_, err = dst.Stat("/stale")
	require.Error(t, err, "a restore replaces, never merges")
}

// TestR026_TheACLGoesOnTheNetworkWhenTheVMsAreInAnotherProject asserts R-026
// where Incus cannot apply a NIC's ACL across projects: the ACL goes on the
// network, which then drops every inbound connection it does not admit.
func TestR026_TheACLGoesOnTheNetworkWhenTheVMsAreInAnotherProject(t *testing.T) {
	f := newFakeIncus()
	f.networks["pando0"] = &incusapi.Network{Name: "pando0", Type: "bridge", Project: "default"}
	a, _, _ := testAdapter(t, f, func(c *Config) { c.Network = "pando0"; c.Project = "pando" })
	_, err := a.Apply(ctx, webPlan("app_1"))
	require.NoError(t, err)

	cfg := f.networks["pando0"].Config
	require.Equal(t, "pando-ingress-pando", cfg["security.acls"])
	require.Equal(t, "drop", cfg["security.acls.default.ingress.action"])
	nic := f.created[0].Devices[nicName]
	require.Empty(t, nic["security.acls"], "the NIC carries none of it")
}

// TestR026_ASharedNetworkIsRefusedWhenTheACLMustGoOnIt asserts R-026 and
// R-254: an ACL on a network others use would close it to them too.
func TestR026_ASharedNetworkIsRefusedWhenTheACLMustGoOnIt(t *testing.T) {
	f := newFakeIncus()
	f.project = "pando"
	f.networks["incusbr0"].Project = "default"
	f.networks["incusbr0"].UsedBy = []string{"/1.0/instances/pando-app-1?project=pando", "/1.0/instances/homeassistant"}
	a, _, _ := testAdapter(t, f, func(c *Config) { c.Project = "pando"; c.StoragePool = "default" })
	err := a.HealthCheck(ctx)
	require.Error(t, err)
	require.Contains(t, errs.As(err).Message, "/1.0/instances/homeassistant")
	require.Contains(t, errs.As(err).Remedy, "network for Pando's VMs alone")

	f.networks["incusbr0"].UsedBy = f.networks["incusbr0"].UsedBy[:1]
	require.NoError(t, a.HealthCheck(ctx))
}
