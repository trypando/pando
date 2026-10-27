package hosts_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/runtime/multidocker"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// TestR416_ASecondControlHostTurnsDockerIntoDockerHosts asserts that a
// second control host, even one with no app role, turns a single-host Docker
// runtime into docker-hosts in place (R-418): it must reach the install's own
// machine through that machine's agent, never as its own Docker, and only
// the Docker hosts runtime does that (R-416). The list it writes describes
// the install's machine by its agent's address, which each control host
// reads as its own entry or another's when it loads the runtime.
func TestR416_ASecondControlHostTurnsDockerIntoDockerHosts(t *testing.T) {
	ctx := context.Background()
	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{"network_pool":"10.208.0.0/12"}`)}}
	r := newRuntime(t, f)
	require.NoError(t, r.CheckJoin(ctx, "ctl-2", []string{state.HostRoleControl}))

	f.hosts = []state.Host{installRow("pando-1", "10.0.0.5:7443"), appHost("ctl-2", "10.0.0.7:7443", state.HostRoleControl)}
	res, err := r.Sync(ctx)
	require.NoError(t, err)
	require.True(t, res.Converted)
	require.Len(t, f.saves, 1)
	require.Equal(t, "docker-hosts", f.saves[0].Kind)
	var cfg struct {
		Hosts []map[string]any `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(f.saves[0].Config, &cfg))
	require.Equal(t, []map[string]any{{"name": "pando-1", "control": true, "agent_address": "10.0.0.5:7443"}}, cfg.Hosts,
		"a control host with no app role runs no apps, so it has no entry")

	// The written list loads on the install's machine and on the second
	// control host alike.
	raw := withAuthority(t, f.saves[0].Config, f.creds["rt_docker/agent_authority"])
	require.NoError(t, multidocker.New().Configure(ctx, raw))
	require.NoError(t, multidocker.NewOn(multidocker.Machine{Name: "ctl-2"}).Configure(ctx, raw))

	// Again: nothing.
	res, err = r.Sync(ctx)
	require.NoError(t, err)
	require.False(t, res.Changed)
	require.Len(t, f.saves, 1)

	// Refusals the conversion would make are made at the join.
	off := newRuntime(t, &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{"network_pool":"off"}`)}})
	require.ErrorContains(t, off.CheckJoin(ctx, "ctl-2", []string{state.HostRoleControl}), "network_pool")
	// No Docker runtime: nothing to convert.
	require.NoError(t, newRuntime(t, &runtimeFakes{}).CheckJoin(ctx, "ctl-2", []string{state.HostRoleControl}))
	// A docker-hosts runtime is written already.
	hostsRT := newRuntime(t, &runtimeFakes{adapters: []state.AdapterConfig{{
		ID: "rt_hosts", Category: "runtime", Kind: "docker-hosts", IsDefault: true, Enabled: true,
		Config: json.RawMessage(`{"hosts":[{"name":"pando-1","control":true,"agent_address":"10.0.0.5:7443"}]}`),
	}}})
	require.NoError(t, hostsRT.CheckJoin(ctx, "ctl-2", []string{state.HostRoleControl}))
	require.NoError(t, r.CheckJoin(ctx, "ctl-2", nil), "no roles: not this check's")
}

// TestR416_AConversionOnAJoinedControlHostNamesTheInstallsMachine asserts
// that a conversion made on a control host that joined writes the same list
// one made on the install's machine would: the install's machine at its own
// row's agent address, never the joined host's.
func TestR416_AConversionOnAJoinedControlHostNamesTheInstallsMachine(t *testing.T) {
	ctx := context.Background()
	f := &runtimeFakes{adapters: []state.AdapterConfig{singleDocker(`{}`)}}
	r := newRuntime(t, f)
	r.Joined, r.LocalAgentAddress = true, "10.0.0.7:7443"

	// Without the install's row, its address is not known.
	f.hosts = []state.Host{appHost("ctl-2", "10.0.0.7:7443", state.HostRoleControl, state.HostRoleApp)}
	_, err := r.Sync(ctx)
	require.Contains(t, errs.As(err).Remedy, "advertise_url")

	f.hosts = append(f.hosts, installRow("pando-1", "10.0.0.5:7443"))
	_, err = r.Sync(ctx)
	require.NoError(t, err)
	var cfg struct {
		Hosts []map[string]any `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(f.saves[0].Config, &cfg))
	require.Equal(t, "pando-1", cfg.Hosts[0]["name"])
	require.Equal(t, "10.0.0.5:7443", cfg.Hosts[0]["agent_address"])
	require.Equal(t, "agent://10.0.0.7:7443", cfg.Hosts[1]["endpoint"])
}
