package hosts_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/hosts"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// runtimeFakes stands in for the stores the runtime sync writes through.
type runtimeFakes struct {
	adapters []state.AdapterConfig
	hosts    []state.Host
	saves    []state.AdapterConfig
	creds    map[string]string
	restarts int
	events   []audit.Event
	failOn   string
}

func (f *runtimeFakes) Hosts(context.Context) ([]state.Host, error) { return f.hosts, nil }
func (f *runtimeFakes) List(context.Context) ([]state.AdapterConfig, error) {
	return f.adapters, nil
}

func (f *runtimeFakes) Save(_ context.Context, c state.AdapterConfig) (state.Replaced, error) {
	if f.failOn == "save" {
		return state.Replaced{}, errors.New("database down")
	}
	f.saves = append(f.saves, c)
	for i := range f.adapters {
		if f.adapters[i].ID == c.ID {
			f.adapters[i] = c
		}
	}
	return state.Replaced{}, nil
}

func (f *runtimeFakes) Put(_ context.Context, adapterID, field string, v secret.Value) error {
	if f.failOn == "put" {
		return errors.New("database down")
	}
	if f.creds == nil {
		f.creds = map[string]string{}
	}
	f.creds[adapterID+"/"+field] = v.Reveal()
	return nil
}

func (f *runtimeFakes) RequestRestart(context.Context) error { f.restarts++; return nil }

func (f *runtimeFakes) Write(_ context.Context, e audit.Event) error {
	if f.failOn == "audit" {
		return errors.New("audit down")
	}
	f.events = append(f.events, e)
	return nil
}

func newRuntime(t *testing.T, f *runtimeFakes) *hosts.Runtime {
	t.Helper()
	a, err := hosts.LoadOrCreateAuthority(filepath.Join(t.TempDir(), "ca.pem"))
	require.NoError(t, err)
	return &hosts.Runtime{
		Hosts: f, Adapters: f, Credentials: f, Restart: f, Audit: f, Authority: a,
		LocalName: "pando-1", LocalAgentAddress: "10.0.0.5:7443",
	}
}

const pin = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func appHost(name, address string, roles ...string) state.Host {
	if len(roles) == 0 {
		roles = []string{state.HostRoleApp}
	}
	return state.Host{ID: "host_" + name, Name: name, Roles: roles, Address: address, CertSHA256: pin, JoinedWith: "jt_" + name}
}

// installRow is the install's own machine's row: no join token brought it in.
func installRow(name, address string) state.Host {
	return state.Host{ID: "host_" + name, Name: name, Roles: []string{state.HostRoleControl, state.HostRoleApp},
		Address: address, CertSHA256: pin}
}

// TestR421_TheInstallsOwnMachineHasOneNameInTheRuntimeAndTheCluster asserts
// R-421's precondition for failing over the install's own machine's apps:
// the conversion names its control entry as the machine's cluster_hosts row
// does, so the home the runtime reports is the name the liveness pass looks
// the lost host's apps up by; and a joined host named as the control entry
// is refused, never silently left out of the host list.
func TestR421_TheInstallsOwnMachineHasOneNameInTheRuntimeAndTheCluster(t *testing.T) {
	ctx := context.Background()
	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{}`)}}
	r := newRuntime(t, f)
	f.hosts = []state.Host{installRow("3f9c2a1b7d4e", "10.0.0.5:7443"), appHost("app-1", "10.0.0.6:7443")}
	_, err := r.Sync(ctx)
	require.NoError(t, err)
	var cfg struct {
		Hosts []map[string]any `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(f.saves[0].Config, &cfg))
	require.Equal(t, "3f9c2a1b7d4e", cfg.Hosts[0]["name"], "the row's name, not LocalName or control")
	require.Equal(t, "3f9c2a1b7d4e", f.events[0].Detail["control_host"])

	// Without a row the configured name is used.
	f = &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{}`)}, hosts: []state.Host{appHost("app-1", "10.0.0.6:7443")}}
	_, err = newRuntime(t, f).Sync(ctx)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(f.saves[0].Config, &cfg))
	require.Equal(t, "pando-1", cfg.Hosts[0]["name"])

	// A joined host named "control" is listed, not dropped as the control
	// entry; where that name is taken, the sync is refused with why.
	list, err := hosts.RuntimeHosts([]state.Host{installRow("pando-1", "10.0.0.5:7443"), appHost("control", "10.0.0.7:7443")})
	require.NoError(t, err)
	require.Len(t, list, 1)
	require.Equal(t, "control", list[0].Name)
	f = &runtimeFakes{adapters: []state.AdapterConfig{{
		ID: "rt_hosts", Category: "runtime", Kind: "docker-hosts", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"hosts":[{"name":"control","control":true,"agent_address":"10.0.0.5:7443"}]}`),
	}}, hosts: []state.Host{appHost("control", "10.0.0.7:7443")}}
	_, err = newRuntime(t, f).Sync(ctx)
	require.ErrorContains(t, err, "already lists a host of that name")
}

// TestR416_AHostWithBothRolesIsReachedThroughItsAgent asserts R-416 for a
// host that joined with the control and app roles: its runtime entry is its
// agent's address and pin, never the cluster listener it answers pings on.
func TestR416_AHostWithBothRolesIsReachedThroughItsAgent(t *testing.T) {
	both := appHost("core-2", "10.0.0.8:7443", state.HostRoleControl, state.HostRoleApp)
	both.PingAddress, both.PingCertSHA256 = "10.0.0.8:7444", strings.Repeat("e", 64)
	list, err := hosts.RuntimeHosts([]state.Host{both})
	require.NoError(t, err)
	require.Equal(t, []hosts.RuntimeHost{{Name: "core-2", Endpoint: "agent://10.0.0.8:7443", CertSHA256: pin}}, list)
	require.Equal(t, []hosts.Peer{{ID: both.ID, Name: "core-2", Address: "10.0.0.8:7444", Control: true}}, hosts.LivenessPeers([]state.Host{both}),
		"pinged on its cluster listener (R-421)")
}

func singleDocker(config string) state.AdapterConfig {
	return state.AdapterConfig{ID: "rt_docker", Category: "runtime", Kind: "docker", Name: "Docker",
		IsDefault: true, Enabled: true, Config: json.RawMessage(config)}
}

// TestR418_TheFirstAppHostTurnsDockerIntoDockerHostsInPlace asserts R-418:
// the first app host to join turns the single-host docker runtime into
// docker-hosts in place — the same adapter, so every app stays pinned to it,
// with this machine as the control host carrying its endpoint and settings,
// so its apps are found where they are and nothing is re-created — audited,
// and a second run with the same hosts changes nothing.
func TestR418_TheFirstAppHostTurnsDockerIntoDockerHostsInPlace(t *testing.T) {
	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(
		`{"total_memory_bytes":8589934592,"network_pool":"10.208.0.0/12","pids_limit":2048,"egress_gateway_image":"registry.example/pando:1"}`)}}
	r := newRuntime(t, f)
	ctx := context.Background()

	// A cluster of one is left as it is.
	f.hosts = []state.Host{installRow("pando-1", "10.0.0.5:7443")}
	res, err := r.Sync(ctx)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.Empty(t, f.saves)

	f.hosts = append(f.hosts, appHost("app-1", "10.0.0.6:7443"))
	res, err = r.Sync(ctx)
	require.NoError(t, err)
	require.True(t, res.Converted)
	require.Len(t, f.saves, 1)
	saved := f.saves[0]
	require.Equal(t, "rt_docker", saved.ID, "the same adapter: apps pinned to it stay pinned (R-257)")
	require.Equal(t, "docker-hosts", saved.Kind)
	require.True(t, saved.IsDefault)

	var cfg struct {
		Hosts       []map[string]any `json:"hosts"`
		NetworkPool string           `json:"network_pool"`
		PidsLimit   int64            `json:"pids_limit"`
		AgentImage  string           `json:"agent_image"`
	}
	require.NoError(t, json.Unmarshal(saved.Config, &cfg))
	require.Equal(t, "10.208.0.0/12", cfg.NetworkPool)
	require.EqualValues(t, 2048, cfg.PidsLimit)
	require.Equal(t, "registry.example/pando:1", cfg.AgentImage)
	require.Len(t, cfg.Hosts, 2)
	require.Equal(t, map[string]any{
		"name": "pando-1", "control": true, "agent_address": "10.0.0.5:7443", "total_memory_bytes": float64(8589934592),
	}, cfg.Hosts[0], "this machine is the control host and the first app host, reached as before")
	require.Equal(t, map[string]any{"name": "app-1", "endpoint": "agent://10.0.0.6:7443", "cert_sha256": pin}, cfg.Hosts[1])
	require.NotContains(t, string(saved.Config), "credentials")

	require.Contains(t, f.creds["rt_docker/agent_authority"], "BEGIN CERTIFICATE")
	require.Equal(t, 1, f.restarts)
	require.Len(t, f.events, 1)
	require.Equal(t, hosts.ConvertAction, f.events[0].Action)
	require.Equal(t, "rt_docker", f.events[0].TargetID)

	// The runtime takes what was written.
	raw := withAuthority(t, saved.Config, f.creds["rt_docker/agent_authority"])
	require.NoError(t, multidocker.New().Configure(ctx, raw))

	// Again, with the same hosts: nothing.
	res, err = r.Sync(ctx)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.False(t, res.Converted)
	require.Len(t, f.saves, 1)
	require.Equal(t, 1, f.restarts)
	require.Len(t, f.events, 1)
}

func withAuthority(t *testing.T, config json.RawMessage, authority string) json.RawMessage {
	t.Helper()
	m := map[string]any{}
	require.NoError(t, json.Unmarshal(config, &m))
	m["credentials"] = map[string]string{"agent_authority": authority}
	out, err := json.Marshal(m)
	require.NoError(t, err)
	return out
}

func TestR418_KubernetesAndIncusTakeControlRoleJoinsOnly(t *testing.T) {
	for _, kind := range []string{"kubernetes", "incus"} {
		err := hosts.CheckAppRoleJoin(kind, []string{state.HostRoleApp})
		require.Error(t, err, kind)
		require.Equal(t, errs.ValidInvalid, errs.As(err).Code)
		require.Contains(t, errs.As(err).Message, "control role only")
		require.NoError(t, hosts.CheckAppRoleJoin(kind, []string{state.HostRoleControl}))
	}
	require.NoError(t, hosts.CheckAppRoleJoin("docker", []string{state.HostRoleApp, state.HostRoleControl}))
	require.NoError(t, hosts.CheckAppRoleJoin("docker-hosts", []string{state.HostRoleApp}))
	require.ErrorContains(t, hosts.CheckAppRoleJoin("other", []string{state.HostRoleApp}), "Only Docker can")

	// Through the install's runtime.
	f := &runtimeFakes{adapters: []state.AdapterConfig{{ID: "rt_k8s", Category: "runtime", Kind: "kubernetes", IsDefault: true, Enabled: true}}}
	r := newRuntime(t, f)
	ctx := context.Background()
	require.ErrorContains(t, r.CheckJoin(ctx, "edge-2", []string{state.HostRoleApp}), "Kubernetes")
	require.NoError(t, r.CheckJoin(ctx, "edge-2", []string{state.HostRoleControl}))

	// And a Sync with an app host there writes nothing.
	f.hosts = []state.Host{appHost("app-1", "10.0.0.6:7443")}
	_, err := r.Sync(ctx)
	require.Error(t, err)
	require.Empty(t, f.saves)
	f.hosts = nil
	_, err = r.Sync(ctx)
	require.NoError(t, err)
}

func TestR418_AJoinIsRefusedWhenDockerCannotBecomeSeveralHosts(t *testing.T) {
	ctx := context.Background()
	app := []string{state.HostRoleApp}

	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{"network_pool":"off"}`)}}
	r := newRuntime(t, f)
	err := r.CheckJoin(ctx, "edge-2", app)
	require.ErrorContains(t, err, "network_pool")
	f.hosts = []state.Host{appHost("app-1", "10.0.0.6:7443")}
	_, err = r.Sync(ctx)
	require.ErrorContains(t, err, "network_pool")
	require.Empty(t, f.saves)

	f = &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{}`)}}
	r = newRuntime(t, f)
	r.LocalAgentAddress = ""
	require.Contains(t, errs.As(r.CheckJoin(ctx, "edge-2", app)).Remedy, "advertise_url")

	r = newRuntime(t, f)
	r.Declared = func(id string) bool { return id == "rt_docker" }
	require.ErrorContains(t, r.CheckJoin(ctx, "edge-2", app), "configuration file")
	// A second control host is refused the same way: it reaches this
	// machine through its agent, so the runtime is converted (R-416).
	require.ErrorContains(t, r.CheckJoin(ctx, "edge-2", []string{state.HostRoleControl}), "configuration file")

	require.Error(t, newRuntime(t, &runtimeFakes{}).CheckJoin(ctx, "edge-2", app), "no runtime stored")
	r = newRuntime(t, &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`not json`)}})
	require.ErrorContains(t, r.CheckJoin(ctx, "edge-2", app), "could not be read")
	require.NoError(t, newRuntime(t, f).CheckJoin(ctx, "edge-2", app))
}

func TestTheHostListFollowsTheClusterAndKeepsWhatTheOperatorWrote(t *testing.T) {
	ctx := context.Background()
	manual := `[{"name":"control","control":true,"agent_address":"10.0.0.5:7443"},{"name":"old-1","endpoint":"tcp://10.0.0.9:2376"},{"name":"gone","endpoint":"agent://10.0.0.8:7443"}]`
	quoted, _ := json.Marshal(manual)
	f := &runtimeFakes{adapters: []state.AdapterConfig{{
		ID: "rt_hosts", Category: "runtime", Kind: "docker-hosts", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"hosts":` + string(quoted) + `,"agent_port":7443}`),
	}}}
	r := newRuntime(t, f)
	r.LocalName = "control"
	f.hosts = []state.Host{installRow("control", "10.0.0.5:7443"), appHost("app-2", "10.0.0.6:7443")}

	res, err := r.Sync(ctx)
	require.NoError(t, err)
	require.True(t, res.Changed)
	require.False(t, res.Converted)
	require.Empty(t, f.events, "only the conversion is audited; the join was")
	var cfg map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(f.saves[0].Config, &cfg))
	require.JSONEq(t, `7443`, string(cfg["agent_port"]))
	hostsJSON := string(cfg["hosts"])
	require.Contains(t, hostsJSON, `"old-1"`)
	require.Contains(t, hostsJSON, `"agent://10.0.0.6:7443"`)
	require.NotContains(t, hostsJSON, `"gone"`, "a host that left the cluster leaves the list")
	require.Equal(t, 1, strings.Count(hostsJSON, `"control"`)-1, "the install's own machine is not listed twice")

	res, err = r.Sync(ctx)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.Len(t, f.saves, 1)

	// A joined host under a name the operator already used.
	f.hosts = append(f.hosts, appHost("old-1", "10.0.0.10:7443"))
	_, err = r.Sync(ctx)
	require.ErrorContains(t, err, "already lists a host of that name")

	// A name the runtime cannot take.
	f.hosts = []state.Host{appHost("App_Host", "10.0.0.10:7443")}
	_, err = r.Sync(ctx)
	require.ErrorContains(t, err, "lower-case letters")
}

func TestASyncThatCannotWriteSaysSo(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"audit", "put", "save"} {
		f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{}`)}, hosts: []state.Host{appHost("app-1", "10.0.0.6:7443")}, failOn: step}
		_, err := newRuntime(t, f).Sync(ctx)
		require.Error(t, err, step)
		require.Zero(t, f.restarts, step)
	}

	f := &runtimeFakes{adapters: []state.AdapterConfig{{ID: "rt_hosts", Category: "runtime", Kind: "docker-hosts", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"hosts":{"not":"a list"}}`)}}}
	_, err := newRuntime(t, f).Sync(ctx)
	require.ErrorContains(t, err, "not a JSON list")
	f.adapters[0].Config = json.RawMessage(`[]`)
	_, err = newRuntime(t, f).Sync(ctx)
	require.ErrorContains(t, err, "could not be read")

	var none *hosts.Authority
	_, err = none.PEM()
	require.Error(t, err)
}
