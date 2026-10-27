package multidocker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/hostagent"
)

// localDaemon serves f on a unix socket, as a machine's own Docker is
// reached, and returns its endpoint.
func localDaemon(t *testing.T, f *fakeDaemon) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "mdl")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "d.sock")
	ln, err := net.Listen("unix", sock)
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(f.serve)}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "unix://" + sock
}

// pandoContainer makes f answer for a container named pando-agent that the
// runtime did not make: a joined host's own Pando, which runs its agent
// (notes-cluster.md, "finishing the join").
func pandoContainer(f *fakeDaemon) {
	f.on("GET /containers/pando-agent/json", respond(http.StatusOK, map[string]any{
		"Id": "c-pando", "Config": map[string]any{"Labels": map[string]string{}}, "State": map[string]any{"Running": true},
		"NetworkSettings": map[string]any{"Networks": map[string]any{}},
	}))
	f.on("DELETE /containers/c-pando", noContent)
	f.on("GET /networks", respond(http.StatusOK, []any{}))
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	f.on("GET /volumes", respond(http.StatusOK, map[string]any{"Volumes": []any{}}))
}

// cluster is three machines' Docker, as each control host's runtime sees
// them: h1, the install's own machine; h2, joined with the control and app
// roles; h3, joined with the app role. Each machine's Docker is served on
// its own socket and through its agent, by separate fakes, so a test tells
// which way a call came.
type cluster struct {
	authority []byte
	// local is each machine's Docker reached on the machine itself, and
	// remote the same machine's reached through its agent.
	local, remote map[string]*fakeDaemon
	socket        map[string]string
	hosts         []map[string]any
}

func newCluster(t *testing.T) *cluster {
	t.Helper()
	authority, err := hostagent.NewAuthority()
	require.NoError(t, err)
	c := &cluster{authority: authority, local: map[string]*fakeDaemon{}, remote: map[string]*fakeDaemon{}, socket: map[string]string{}}
	addr := map[string]string{}
	pin := map[string]string{}
	for _, name := range []string{"h1", "h2", "h3"} {
		c.local[name], c.remote[name] = newFakeDaemon(t), newFakeDaemon(t)
		for _, f := range []*fakeDaemon{c.local[name], c.remote[name]} {
			pandoContainer(f)
		}
		c.socket[name] = localDaemon(t, c.local[name])
		addr[name], pin[name] = joinedHost(t, authority, name, c.remote[name])
	}
	// What hosts.Runtime.Sync writes after the install's machine became the
	// control entry (R-418): h1 by its own socket and its agent's address,
	// which on every other machine names that machine's own Docker; the
	// joined hosts by their agents and pins.
	c.hosts = []map[string]any{
		{"name": "h1", "control": true, "endpoint": c.socket["h1"], "agent_address": addr["h1"]},
		{"name": "h2", "endpoint": "agent://" + addr["h2"], "cert_sha256": pin["h2"]},
		{"name": "h3", "endpoint": "agent://" + addr["h3"], "cert_sha256": pin["h3"]},
	}
	return c
}

// on configures the runtime as the process on machine runs it, with list as
// the stored host list.
func (c *cluster) on(t *testing.T, machine Machine, list []map[string]any) *Adapter {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"hosts":       list,
		"agent_image": testAgentImage,
		"credentials": map[string]string{"agent_authority": string(c.authority)},
	})
	require.NoError(t, err)
	a := NewOn(machine)
	require.NoError(t, a.Configure(context.Background(), raw))
	return a
}

// TestR416_ASecondControlHostNeverTouchesItsOwnPandoContainer asserts the
// bug the cluster's end-to-end test found: a host joined with the control and
// app roles read the install machine's entry — its own socket — as its own
// Docker, kept "h1's agent" there, found its own Pando by the agent's name
// and force-removed it. Now it reaches h1 only through h1's agent, keeps no
// agent anywhere a joined host runs its own, and its own Pando container is
// never removed, replaced, created or started.
func TestR416_ASecondControlHostNeverTouchesItsOwnPandoContainer(t *testing.T) {
	c := newCluster(t)
	// h1's entry names the socket path h2 also has: on h2 it is h2's Docker.
	c.hosts[0]["endpoint"] = c.socket["h2"]
	a := c.on(t, Machine{Name: "h2", Endpoint: c.socket["h2"]}, c.hosts)
	ctx := context.Background()

	_, _ = a.RejoinNetworks(ctx, func(string) bool { return true })

	for name, f := range map[string]*fakeDaemon{"h2 locally": c.local["h2"], "h2 through its agent": c.remote["h2"]} {
		require.Zero(t, f.called("DELETE"), "%s: Pando's own container is never removed", name)
		require.Zero(t, f.called("POST /containers/create"), name)
		require.Zero(t, f.called("POST /containers/c-pando"), name)
		require.Zero(t, f.called("POST /images/create"), name)
	}
	require.Positive(t, c.remote["h1"].called("GET /networks"), "h1 is reached through its agent")
	require.Zero(t, c.remote["h1"].called("DELETE"))
	require.Zero(t, c.local["h1"].called("GET"), "h1's own socket is not reached from h2")
	for _, h := range a.hosts {
		require.Nil(t, h.agent, "h2 keeps no agent: %s runs its own, or is h2", h.cfg.Name)
	}
}

// TestR414_EachControlHostTreatsOnlyItsOwnEntryAsLocal asserts that the same
// stored host list means the right thing on every control host: each reaches
// its own entry's Docker on its own machine and every other machine,
// the install's own included, through that machine's agent (R-416). The
// edge, trial runs and self-upgrade are each control host's own; only the
// install's machine keeps an agent, and that agent carries the other control
// hosts' runtime calls.
func TestR414_EachControlHostTreatsOnlyItsOwnEntryAsLocal(t *testing.T) {
	for _, tc := range []struct {
		machine Machine
		self    string
	}{
		{Machine{}, "h1"},
		{Machine{Name: "h2"}, "h2"},
		{Machine{Name: "h3"}, "h3"},
	} {
		t.Run(tc.self, func(t *testing.T) {
			c := newCluster(t)
			tc.machine.Endpoint = c.socket[tc.self]
			a := c.on(t, tc.machine, c.hosts)
			ctx := context.Background()
			require.Equal(t, tc.self, a.self.cfg.Name)
			require.Equal(t, "h1", a.control.cfg.Name, "the install's machine stays the control entry")

			for _, h := range a.hosts {
				_, err := h.rt.Bundles(ctx)
				require.NoError(t, err, h.cfg.Name)
				if h.cfg.Name == tc.self {
					require.Positive(t, c.local[h.cfg.Name].called("GET /networks"), "%s reaches itself on its own Docker", tc.self)
					require.Zero(t, c.remote[h.cfg.Name].called("GET /networks"))
				} else {
					require.Positive(t, c.remote[h.cfg.Name].called("GET /networks"), "%s reaches %s through its agent", tc.self, h.cfg.Name)
					require.Zero(t, c.local[h.cfg.Name].called("GET /networks"))
				}
				kept := h.cfg.Name == "h1" && tc.self == "h1"
				require.Equal(t, kept, h.agent != nil, "%s keeps an agent on %s", tc.self, h.cfg.Name)
				if kept {
					require.NotNil(t, h.agent.socket, "the install's machine's agent carries runtime calls")
				}
			}

			// The edge is this machine's.
			_, _ = a.Edges(ctx)
			require.Positive(t, c.local[tc.self].called("GET /containers/json"), "the edge is looked for on %s", tc.self)
			require.NoError(t, a.HealthCheck(ctx))
		})
	}
}

// TestR414_AControlHostWithNoAppRoleRunsItsEdgeOnItsOwnMachine asserts that a
// control host the host list has no entry for reaches every machine through
// its agent, keeps no agent anywhere, and runs its edge, trial runs and
// self-upgrade on its own Docker, where its edge's storage is found.
func TestR414_AControlHostWithNoAppRoleRunsItsEdgeOnItsOwnMachine(t *testing.T) {
	c := newCluster(t)
	list := []map[string]any{c.hosts[0], c.hosts[2]}
	a := c.on(t, Machine{Name: "h2", Endpoint: c.socket["h2"]}, list)
	ctx := context.Background()

	require.Nil(t, a.self)
	for _, h := range a.hosts {
		require.Nil(t, h.agent, h.cfg.Name)
		_, err := h.rt.Bundles(ctx)
		require.NoError(t, err)
		require.Zero(t, c.local[h.cfg.Name].called("GET /networks"), "%s is reached through its agent", h.cfg.Name)
	}
	_, _ = a.Edges(ctx)
	require.Positive(t, c.local["h2"].called("GET /containers/json"), "the edge is this machine's")
	require.NoError(t, a.HealthCheck(ctx))

	h, vol, err := a.volumeHost("h2/pando-edge-traefik-certs")
	require.NoError(t, err)
	require.Equal(t, "pando-edge-traefik-certs", vol.Handle)
	require.Same(t, a.unlisted, h, "an edge volume named for this machine is on its own Docker")
	_, _, err = a.volumeHost("h9/x")
	require.Error(t, err)
}
