package multidocker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hostagent"
)

const testAgentImage = "registry.example/pando:1.2.3"

func newKeeper(t *testing.T, f *fakeDaemon, image string) *agentKeeper {
	t.Helper()
	caPEM, err := hostagent.NewAuthority()
	require.NoError(t, err)
	as, err := hostagent.ParseAuthorities(caPEM)
	require.NoError(t, err)
	return &agentKeeper{
		cli: f.cli, host: "app-1", port: 7443, pool: netip.MustParsePrefix(defaultNetworkPool),
		authorities: as, image: func(context.Context) string { return image },
	}
}

// createdAgent is what the adapter asked Docker to create, decoded.
type createdAgent struct {
	Image      string
	Entrypoint []string
	Cmd        []string
	User       string
	Env        []string
	Labels     map[string]string
	HostConfig struct {
		ReadonlyRootfs bool
		CapDrop        []string
		SecurityOpt    []string
		PortBindings   map[string][]struct{ HostPort string }
		Memory         int64
	}
	NetworkingConfig struct {
		EndpointsConfig map[string]struct{ NetworkID string }
	}
}

// TestR023_AHostsAgentIsCreatedLockedDownAndOutsideTheAppRange asserts what
// the forwarding agent each app host runs is made with (O-45, design 06 §4):
// Pando's image as `pando host-agent serve`, the agent's own server key and
// the authorities' certificates only, the app range it may forward into, a
// read-only root and no capabilities, and one published port on a network
// of its own that is not an app network.
func TestR023_AHostsAgentIsCreatedLockedDownAndOutsideTheAppRange(t *testing.T) {
	f := newFakeDaemon(t)
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{}))
	f.on("POST /networks/create", respond(http.StatusCreated, map[string]string{"Id": "net-agent"}))
	f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"Pulling"}` + "\n" + `{"status":"Downloaded"}` + "\n"))
	})
	f.on("POST /containers/create", respond(http.StatusCreated, map[string]any{"Id": "c-agent"}))
	f.on("POST /containers/c-agent/start", noContent)
	k := newKeeper(t, f, testAgentImage)

	created, err := k.ensure(context.Background())
	require.NoError(t, err)
	require.True(t, created)
	require.Equal(t, 1, f.called("POST /images/create"), "the host did not have the image, so it was pulled")
	require.Equal(t, 1, f.called("POST /containers/c-agent/start"))

	var got createdAgent
	require.NoError(t, json.Unmarshal(f.body("POST /containers/create"), &got))
	require.Equal(t, testAgentImage, got.Image)
	require.Equal(t, []string{agentBinary}, got.Entrypoint)
	require.Equal(t, []string{"host-agent", "serve"}, got.Cmd)
	require.Equal(t, agentUser, got.User)
	require.True(t, got.HostConfig.ReadonlyRootfs)
	require.Equal(t, []string{"ALL"}, got.HostConfig.CapDrop)
	require.Contains(t, got.HostConfig.SecurityOpt, "no-new-privileges")
	require.Equal(t, "7443", got.HostConfig.PortBindings["7443/tcp"][0].HostPort)
	require.Equal(t, "net-agent", got.NetworkingConfig.EndpointsConfig[agentNetwork].NetworkID)
	require.Equal(t, k.digest(testAgentImage), got.Labels[labelAgentDigest])
	require.Equal(t, roleAgent, got.Labels[labelRole])

	env := map[string]string{}
	for _, kv := range got.Env {
		name, value, _ := strings.Cut(kv, "=")
		env[name] = value
	}
	require.Contains(t, env[EnvAgentCert], "BEGIN CERTIFICATE")
	require.Contains(t, env[EnvAgentKey], "PRIVATE KEY")
	require.Equal(t, string(k.authorities.PoolPEM()), env[EnvAgentAuthorities])
	require.NotContains(t, env[EnvAgentAuthorities], "PRIVATE KEY", "the authority's key never leaves Pando")
	require.Equal(t, defaultNetworkPool, env[EnvAgentPool])
	require.Equal(t, ":7443", env[EnvAgentListen])

	// The certificate the agent was given verifies as the host's own.
	tlsConfig, err := hostagent.ServerTLS([]byte(env[EnvAgentCert]), []byte(env[EnvAgentKey]), []byte(env[EnvAgentAuthorities]))
	require.NoError(t, err)
	require.NotNil(t, tlsConfig)

	// Checked again within agentCheckEvery: Docker is not asked.
	before := len(f.calls)
	created, err = k.ensure(context.Background())
	require.NoError(t, err)
	require.False(t, created)
	require.Len(t, f.calls, before)
}

func TestACurrentAgentIsStartedIfStoppedAndKeptOtherwise(t *testing.T) {
	f := newFakeDaemon(t)
	k := newKeeper(t, f, testAgentImage)
	running := false
	f.on("GET /containers/pando-agent/json", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"Id":     "c-old",
			"Config": map[string]any{"Labels": map[string]string{labelAgentDigest: k.digest(testAgentImage), labelAgentIssued: time.Now().UTC().Format(time.RFC3339)}},
			"State":  map[string]any{"Running": running},
		})
	})
	f.on("POST /containers/c-old/start", noContent)

	created, err := k.ensure(context.Background())
	require.NoError(t, err)
	require.False(t, created, "the agent was there; it was started, not made again")
	require.Equal(t, 1, f.called("POST /containers/c-old/start"))
	require.Zero(t, f.called("POST /containers/create"))
	require.Zero(t, f.called("DELETE"))

	running = true
	k.checked = time.Time{}
	created, err = k.ensure(context.Background())
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, 1, f.called("POST /containers/c-old/start"), "a running agent is left alone")

	// It cannot be started: the error names the host.
	running = false
	k.checked = time.Time{}
	f.on("POST /containers/c-old/start", respond(http.StatusInternalServerError, map[string]string{"message": "port is already allocated"}))
	_, err = k.ensure(context.Background())
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app-1")
}

// TestAnAgentOnAnotherConfigurationOrAnOldCertificateIsReplaced asserts that
// an agent made with another image, port, range or authority, or whose
// certificate is due for renewal, is removed and made again.
func TestAnAgentOnAnotherConfigurationOrAnOldCertificateIsReplaced(t *testing.T) {
	for name, labels := range map[string]func(k *agentKeeper) map[string]string{
		"another image": func(k *agentKeeper) map[string]string {
			return map[string]string{labelAgentDigest: k.digest("registry.example/pando:1.0.0"), labelAgentIssued: time.Now().UTC().Format(time.RFC3339)}
		},
		"a certificate due for renewal": func(k *agentKeeper) map[string]string {
			return map[string]string{labelAgentDigest: k.digest(testAgentImage),
				labelAgentIssued: time.Now().Add(-agentReissueAfter - time.Hour).UTC().Format(time.RFC3339)}
		},
		"no record of when it was issued": func(k *agentKeeper) map[string]string {
			return map[string]string{labelAgentDigest: k.digest(testAgentImage)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeDaemon(t)
			k := newKeeper(t, f, testAgentImage)
			f.on("GET /containers/pando-agent/json", respond(http.StatusOK, map[string]any{
				"Id": "c-old", "Config": map[string]any{"Labels": labels(k)}, "State": map[string]any{"Running": true},
			}))
			f.on("DELETE /containers/c-old", noContent)
			f.on("GET /networks", respond(http.StatusOK, []map[string]any{
				{"Id": "net-agent", "Name": agentNetwork, "IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "172.30.0.0/16"}}}},
			}))
			f.on("GET /images/"+testAgentImage+"/json", respond(http.StatusOK, map[string]any{"Id": "sha256:abc"}))
			f.on("POST /containers/create", respond(http.StatusCreated, map[string]any{"Id": "c-new"}))
			f.on("POST /containers/c-new/start", noContent)

			created, err := k.ensure(context.Background())
			require.NoError(t, err)
			require.True(t, created)
			require.Equal(t, 1, f.called("DELETE /containers/c-old"))
			require.Zero(t, f.called("POST /networks/create"), "the agent's network was there already")
			require.Zero(t, f.called("POST /images/create"), "the host had the image")
		})
	}
}

// TestR023_AnAgentNetworkInsideTheAppRangeIsRefused asserts the guard that
// keeps the agent's published network out of what it forwards into: a
// network by the agent's name with addresses in the app range is not used.
func TestR023_AnAgentNetworkInsideTheAppRangeIsRefused(t *testing.T) {
	f := newFakeDaemon(t)
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{
		{"Id": "other", "Name": "pando-agent-2", "IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "10.213.0.0/24"}}}},
		{"Id": "net-agent", "Name": agentNetwork, "IPAM": map[string]any{"Config": []map[string]any{{"Subnet": "10.213.9.0/24"}}}},
	}))
	k := newKeeper(t, f, testAgentImage)
	created, err := k.ensure(context.Background())
	require.False(t, created)
	require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "app network range")
	require.Contains(t, errs.As(err).Remedy, "docker network rm "+agentNetwork)
	require.Zero(t, f.called("POST /containers/create"))
}

func TestAnAgentThatCannotBeMadeSaysWhy(t *testing.T) {
	ctx := context.Background()

	t.Run("no image to run", func(t *testing.T) {
		f := newFakeDaemon(t)
		_, err := newKeeper(t, f, "").ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Remedy, "agent_image")
		require.Empty(t, f.calls, "nothing is asked of Docker without an image")
	})

	t.Run("Docker does not answer", func(t *testing.T) {
		f := newFakeDaemon(t)
		f.on("GET /containers/pando-agent/json", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "app-1")
	})

	t.Run("the old agent cannot be removed", func(t *testing.T) {
		f := newFakeDaemon(t)
		f.on("GET /containers/pando-agent/json", respond(http.StatusOK, map[string]any{"Id": "c-old", "Config": map[string]any{}}))
		f.on("DELETE /containers/c-old", respond(http.StatusInternalServerError, map[string]string{"message": "busy"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "replace the host agent")
	})

	t.Run("the network list fails", func(t *testing.T) {
		f := newFakeDaemon(t)
		f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "boom"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	})

	t.Run("the network cannot be made", func(t *testing.T) {
		f := newFakeDaemon(t)
		f.on("GET /networks", respond(http.StatusOK, []map[string]any{}))
		f.on("POST /networks/create", respond(http.StatusInternalServerError, map[string]string{"message": "no addresses"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "network")
	})

	withNetwork := func(f *fakeDaemon) {
		f.on("GET /networks", respond(http.StatusOK, []map[string]any{{"Id": "net-agent", "Name": agentNetwork}}))
	}

	t.Run("the image cannot be pulled", func(t *testing.T) {
		f := newFakeDaemon(t)
		withNetwork(f)
		f.on("POST /images/create", respond(http.StatusInternalServerError, map[string]string{"message": "unauthorized"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, testAgentImage)
	})

	t.Run("the pull fails part way", func(t *testing.T) {
		f := newFakeDaemon(t)
		withNetwork(f)
		f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}` + "\n"))
		})
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Zero(t, f.called("POST /containers/create"))
	})

	withImage := func(f *fakeDaemon) {
		withNetwork(f)
		f.on("GET /images/"+testAgentImage+"/json", respond(http.StatusOK, map[string]any{"Id": "sha256:abc"}))
	}

	t.Run("the container cannot be made", func(t *testing.T) {
		f := newFakeDaemon(t)
		withImage(f)
		f.on("POST /containers/create", respond(http.StatusConflict, map[string]string{"message": "name in use"}))
		_, err := newKeeper(t, f, testAgentImage).ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "create the host agent on app-1")
	})

	t.Run("the port is taken", func(t *testing.T) {
		f := newFakeDaemon(t)
		withImage(f)
		f.on("POST /containers/create", respond(http.StatusCreated, map[string]any{"Id": "c-new"}))
		f.on("POST /containers/c-new/start", respond(http.StatusInternalServerError, map[string]string{"message": "port is already allocated"}))
		k := newKeeper(t, f, testAgentImage)
		_, err := k.ensure(ctx)
		require.Equal(t, errs.AdapterFailed, errs.CodeOf(err))
		require.Contains(t, errs.As(err).Message, "port 7443")
		require.Contains(t, errs.As(err).Remedy, "agent_port")
		require.True(t, k.checked.IsZero(), "a failed start is tried again on the next call")
	})
}

// rejoinCounter is a fakeHost that counts rejoins.
type rejoinCounter struct {
	*fakeHost
	rejoined int
}

func (r *rejoinCounter) RejoinNetworks(context.Context, func(string) bool) (int, error) {
	r.rejoined++
	return 2, nil
}

// TestAReCreatedAgentIsJoinedToItsHostsAppNetworksAtOnce asserts that an
// agent made again is joined to the app networks right away, once the
// adapter knows what the install owns, rather than at the next rejoin.
func TestAReCreatedAgentIsJoinedToItsHostsAppNetworksAtOnce(t *testing.T) {
	f := newFakeDaemon(t)
	f.on("GET /networks", respond(http.StatusOK, []map[string]any{{"Id": "net-agent", "Name": agentNetwork}}))
	f.on("GET /images/"+testAgentImage+"/json", respond(http.StatusOK, map[string]any{"Id": "sha256:abc"}))
	f.on("POST /containers/create", respond(http.StatusCreated, map[string]any{"Id": "c-new"}))
	f.on("POST /containers/c-new/start", noContent)

	rt := &rejoinCounter{fakeHost: &fakeHost{}}
	h := &host{cfg: HostConfig{Name: "app-1"}, rt: rt, agent: newKeeper(t, f, testAgentImage)}
	a := &Adapter{}

	// Not yet told what the install owns: nothing to join.
	require.NoError(t, a.ensureAgent(context.Background(), h))
	require.Zero(t, rt.rejoined)

	a.setOwns(func(string) bool { return true })
	h.agent.checked = time.Time{}
	require.NoError(t, a.ensureAgent(context.Background(), h))
	require.Equal(t, 1, rt.rejoined)

	// No agent kept for the host (a test double): nothing to do.
	require.NoError(t, a.ensureAgent(context.Background(), &host{rt: rt}))
	require.Equal(t, 1, rt.rejoined)
}

func TestStaleReadsTheIssueTime(t *testing.T) {
	require.True(t, stale(""))
	require.True(t, stale("yesterday"))
	require.True(t, stale(time.Now().Add(-31*24*time.Hour).UTC().Format(time.RFC3339)))
	require.False(t, stale(time.Now().UTC().Format(time.RFC3339)))
}

var _ api.RuntimeAdapter = (*rejoinCounter)(nil)
