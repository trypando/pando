package docker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/moby/moby/client"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// networkRequest is the part of a network-create request these tests read.
type networkRequest struct {
	Name string
	IPAM *struct {
		Config []struct{ Subnet string }
	}
}

func networkRequestOptions() client.NetworkCreateOptions {
	return client.NetworkCreateOptions{Driver: "bridge", Labels: map[string]string{labelManaged: "true"}}
}

func (n networkRequest) subnet() string {
	if n.IPAM == nil || len(n.IPAM.Config) == 0 {
		return ""
	}
	return n.IPAM.Config[0].Subnet
}

func decodeNetwork(t *testing.T, r *http.Request) networkRequest {
	t.Helper()
	var req networkRequest
	require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
	return req
}

func TestWithThePoolOffDockerChoosesTheAddresses(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"network_pool": "off"})
	var got networkRequest
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		got = decodeNetwork(t, r)
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	created, err := a.createNetwork(context.Background(), "test-pool-off", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Equal(t, "net1", created.ID)
	require.Empty(t, got.subnet(), "no block is asked for")
	require.Zero(t, f.called("GET /networks"), "the pool is off, so nothing is listed")
}

func TestANetworkIsStillCreatedWhenTheTakenOnesCannotBeListed(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	var got networkRequest
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		got = decodeNetwork(t, r)
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	_, err := a.createNetwork(context.Background(), "test-list-fails", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Empty(t, got.subnet(), "with nothing known about what is taken, Docker chooses")
}

// TestR025_ABlockSomethingElseTookIsSkipped asserts R-025. A block taken by
// something the list did not show is refused by Docker as overlapping, and the
// next block is tried rather than failing the deploy.
func TestR025_ABlockSomethingElseTookIsSkipped(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusOK, []any{
		map[string]any{"Id": "other", "IPAM": map[string]any{"Config": []any{map[string]string{"Subnet": "172.17.0.0/16"}}}},
	}))
	var subnets []string
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		req := decodeNetwork(t, r)
		subnets = append(subnets, req.subnet())
		if req.subnet() == "10.213.0.0/28" {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Pool overlaps with other one on this address space"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	_, err := a.createNetwork(context.Background(), "test-overlap", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Equal(t, []string{"10.213.0.0/28", "10.213.0.16/28"}, subnets)
}

// The same, in Podman's words. Podman also refuses a block the host's routes
// already use, which is what another engine's network on the same machine
// looks like to it — and it does not say "overlap".
func TestR025_ABlockPodmanSaysIsTakenIsSkipped(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusOK, []any{}))
	var subnets []string
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		req := decodeNetwork(t, r)
		subnets = append(subnets, req.subnet())
		if req.subnet() == "10.213.0.0/28" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{
				"message": "subnet 10.213.0.0/28 is already used on the host or by another config"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	_, err := a.createNetwork(context.Background(), "test-podman-taken", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Equal(t, []string{"10.213.0.0/28", "10.213.0.16/28"}, subnets)
}

func TestAfterEightRefusedBlocksDockerChoosesTheAddresses(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusOK, []any{}))
	var subnets []string
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		req := decodeNetwork(t, r)
		subnets = append(subnets, req.subnet())
		if req.subnet() != "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Pool overlaps with other one on this address space"})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	_, err := a.createNetwork(context.Background(), "test-overlap-all", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Len(t, subnets, 9)
	require.Empty(t, subnets[8], "the last try leaves the choice to Docker")
}

func TestAFullPoolLeavesTheAddressesToDocker(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"network_pool": "10.214.0.0/24"})
	f.on("GET /networks", respond(http.StatusOK, []any{
		map[string]any{"Id": "taken", "IPAM": map[string]any{"Config": []any{map[string]string{"Subnet": "10.214.0.0/24"}}}},
	}))
	var subnets []string
	f.on("POST /networks/create", func(w http.ResponseWriter, r *http.Request) {
		subnets = append(subnets, decodeNetwork(t, r).subnet())
		writeJSON(w, http.StatusCreated, map[string]string{"Id": "net1"})
	})

	_, err := a.createNetwork(context.Background(), "test-pool-full", defaultBlockBits, networkRequestOptions())
	require.NoError(t, err)
	require.Equal(t, []string{""}, subnets)
}

func TestARefusalOtherThanAnOverlapIsReported(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusOK, []any{}))
	f.on("POST /networks/create", respond(http.StatusInternalServerError, map[string]string{"message": "driver failed"}))

	_, err := a.createNetwork(context.Background(), "test-refused", defaultBlockBits, networkRequestOptions())
	require.ErrorContains(t, err, "driver failed")
	require.Equal(t, 1, f.called("POST /networks/create"), "only an overlap is worth another block")
}

// TestR025_OnlyThisInstallsEmptyNetworksAreReclaimed asserts R-025 on a host
// Pando shares. A network is removed at startup only when it belongs to an app
// this install knows and no container, running or stopped, belongs to that
// app (issue #55).
func TestR025_OnlyThisInstallsEmptyNetworksAreReclaimed(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	networks := map[string]map[string]any{
		"n-empty":   {"Labels": map[string]string{labelBundle: "app_empty"}},
		"n-stopped": {"Labels": map[string]string{labelBundle: "app_stopped"}},
		"n-err":     {"Labels": map[string]string{labelBundle: "app_err"}},
		"n-theirs":  {"Labels": map[string]string{labelBundle: "app_theirs"}},
		"n-trial":   {"Labels": map[string]string{labelTrial: "t1"}},
		"n-busy":    {"Labels": map[string]string{labelBundle: "app_busy"}, "Containers": map[string]any{"c1": map[string]string{"Name": "x"}}},
	}
	list := make([]any, 0, len(networks))
	for id := range networks {
		list = append(list, map[string]string{"Id": id})
	}
	f.on("GET /networks", respond(http.StatusOK, list))
	f.on("GET /networks/*", func(w http.ResponseWriter, r *http.Request) {
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		n, ok := networks[id]
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "no such network"})
			return
		}
		body := map[string]any{"Id": id}
		for k, v := range n {
			body[k] = v
		}
		writeJSON(w, http.StatusOK, body)
	})
	f.on("GET /containers/json", func(w http.ResponseWriter, r *http.Request) {
		filter := r.URL.Query().Get("filters")
		switch {
		case strings.Contains(filter, "app_stopped"):
			writeJSON(w, http.StatusOK, []any{map[string]string{"Id": "stopped", "State": "exited"}})
		case strings.Contains(filter, "app_err"):
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "daemon busy"})
		default:
			writeJSON(w, http.StatusOK, []any{})
		}
	})
	var removed []string
	f.on("DELETE /networks/*", func(w http.ResponseWriter, r *http.Request) {
		removed = append(removed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
		w.WriteHeader(http.StatusNoContent)
	})

	mine := func(bundle string) bool { return bundle != "app_theirs" }
	n, err := a.ReclaimNetworks(context.Background(), mine)
	require.NoError(t, err)
	require.Equal(t, 1, n)
	require.Equal(t, []string{"n-empty"}, removed,
		"kept: a stopped app's, one whose containers could not be read, another install's, a trial's, and a busy one")
}

func TestReclaimingReportsANetworkListItCannotRead(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /networks", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	_, err := a.ReclaimNetworks(context.Background(), nil)
	require.Equal(t, errs.AdapterUnavailable, errs.As(err).Code)
}

// dependencyDaemon answers for one dependency container whose inspect body is
// state.
func dependencyDaemon(t *testing.T, state map[string]any) *Adapter {
	t.Helper()
	f, a := newFakeDaemon(t, nil)
	f.on("GET /containers/json", respond(http.StatusOK, []any{map[string]string{"Id": "dep1", "State": "running"}}))
	f.on("GET /containers/dep1/json", respond(http.StatusOK, map[string]any{"Id": "dep1", "State": state}))
	return a
}

// TestR096_ADependencyIsWaitedOnOnlyWhileItCanStillBecomeHealthy asserts R-096.
// A dependent waits for a dependency that is running and reporting a health
// check; one that is gone, stopped, or has no health check is not worth
// waiting on.
func TestR096_ADependencyIsWaitedOnOnlyWhileItCanStillBecomeHealthy(t *testing.T) {
	ctx := context.Background()

	require.False(t, dependencyDaemon(t, map[string]any{"Running": true, "Health": map[string]string{"Status": "starting"}}).
		settled(ctx, "app_x", "db"), "still starting: wait")
	require.True(t, dependencyDaemon(t, map[string]any{"Running": true, "Health": map[string]string{"Status": "healthy"}}).
		settled(ctx, "app_x", "db"))
	require.True(t, dependencyDaemon(t, map[string]any{"Running": true}).settled(ctx, "app_x", "db"),
		"no health check: nothing to wait for")
	require.True(t, dependencyDaemon(t, map[string]any{"Running": false, "Health": map[string]string{"Status": "unhealthy"}}).
		settled(ctx, "app_x", "db"), "stopped: it will not become healthy by waiting")

	f, gone := newFakeDaemon(t, nil)
	f.on("GET /containers/json", respond(http.StatusOK, []any{}))
	require.True(t, gone.settled(ctx, "app_x", "db"), "no container: nothing to wait for")

	f, unreadable := newFakeDaemon(t, nil)
	f.on("GET /containers/json", respond(http.StatusOK, []any{map[string]string{"Id": "dep1"}}))
	f.on("GET /containers/dep1/json", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	require.True(t, unreadable.settled(ctx, "app_x", "db"))

	f, unlisted := newFakeDaemon(t, nil)
	f.on("GET /containers/json", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	require.True(t, unlisted.settled(ctx, "app_x", "db"))
}

func TestWaitingForADependencyEndsWithTheContext(t *testing.T) {
	a := dependencyDaemon(t, map[string]any{"Running": true, "Health": map[string]string{"Status": "starting"}})
	planned := map[string]api.WorkloadPlan{
		"db":    {Name: "db", Health: &api.HealthPlan{Command: []string{"true"}}},
		"cache": {Name: "cache"},
	}
	w := api.WorkloadPlan{Name: "web", DependsOn: []string{"cache", "missing", "db"}}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	a.waitForDependencies(ctx, "app_x", w, planned)
	require.Less(t, time.Since(started), 2*time.Second,
		"a canceled deploy stops waiting instead of sitting out the whole wait")
}

func TestAHealthyDependencyIsNotWaitedOn(t *testing.T) {
	a := dependencyDaemon(t, map[string]any{"Running": true, "Health": map[string]string{"Status": "healthy"}})
	planned := map[string]api.WorkloadPlan{"db": {Name: "db", Health: &api.HealthPlan{Command: []string{"true"}}}}
	started := time.Now()
	a.waitForDependencies(context.Background(), "app_x", api.WorkloadPlan{Name: "web", DependsOn: []string{"db"}}, planned)
	require.Less(t, time.Since(started), time.Second)
}

func TestPullErrorReadsTheFailureOutOfTheStream(t *testing.T) {
	cases := []struct {
		stream string
		want   string
	}{
		{`{"status":"Pulling from library/x"}` + "\n" + `{"status":"Download complete"}`, ""},
		{`{"status":"Pulling"}` + "\n" + `{"errorDetail":{"message":"manifest unknown"},"error":"manifest unknown"}`, "manifest unknown"},
		{`{"error":"pull access denied"}`, "pull access denied"},
		{``, ""},
	}
	for _, c := range cases {
		err := pullError(strings.NewReader(c.stream))
		if c.want == "" {
			require.NoError(t, err, c.stream)
			continue
		}
		require.EqualError(t, err, c.want)
	}
	require.Error(t, pullError(strings.NewReader(`{"status":`)), "a stream cut short is not a success")
}

func TestOnlyADaemonSideFailureMakesAPullWorthRetrying(t *testing.T) {
	for _, msg := range []string{
		"failed to prepare extraction snapshot: lease does not exist",
		"failed commit on ref \"layer-sha256:abc\"",
		"failed to extract layer",
		"Unexpected EOF",
		"read tcp: connection reset by peer",
		"dial tcp: i/o timeout",
		"net/http: TLS handshake timeout",
	} {
		require.True(t, pullTransient(errString(msg)), msg)
	}
	for _, msg := range []string{"manifest unknown", "pull access denied", "no matching manifest for linux/arm64"} {
		require.False(t, pullTransient(errString(msg)), msg)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// pullDaemon answers an image as absent and each pull with the next stream in
// streams, repeating the last.
func pullDaemon(t *testing.T, streams ...string) (*fakeDaemon, *Adapter) {
	t.Helper()
	f, a := newFakeDaemon(t, nil)
	f.on("GET /images/*", respond(http.StatusNotFound, map[string]string{"message": "No such image"}))
	var n atomic.Int32
	f.on("POST /images/create", func(w http.ResponseWriter, _ *http.Request) {
		i := int(n.Add(1)) - 1
		if i >= len(streams) {
			i = len(streams) - 1
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, streams[i])
	})
	f.on("POST /images/*", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) })
	return f, a
}

const leaseGone = `{"errorDetail":{"message":"failed to prepare extraction snapshot: lease does not exist"}}`

// A pull that trips over the daemon's own content store succeeds a moment
// later, so it is tried again (issue #55).
func TestAPullThatFailsInsideTheDaemonIsTriedAgain(t *testing.T) {
	f, a := pullDaemon(t, leaseGone, `{"status":"Downloaded newer image"}`)

	require.NoError(t, a.ensureImage(context.Background(), "test-image:1", forBundle("app_x")))
	require.Equal(t, 2, f.called("POST /images/create"))
	require.Equal(t, 2, f.called("POST /images/test-image:1/tag"), "fetched by Pando, and claimed by the app")
}

func TestAPullTheRegistryRefusesIsReportedAtOnce(t *testing.T) {
	f, a := pullDaemon(t, `{"errorDetail":{"message":"manifest for test-image:1 not found: manifest unknown"}}`)

	err := a.ensureImage(context.Background(), "test-image:1", forBundle("app_x"))
	require.Error(t, err)
	e := errs.As(err)
	require.Equal(t, errs.AdapterFailed, e.Code)
	require.Contains(t, e.Message, `"test-image:1"`)
	require.Contains(t, err.Error(), "manifest unknown", "the registry's reason is kept")
	require.NotEmpty(t, e.Remedy)
	require.Equal(t, 1, f.called("POST /images/create"))
}

func TestRetryingAPullStopsWithTheContext(t *testing.T) {
	f, a := pullDaemon(t, leaseGone)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := a.ensureImage(ctx, "test-image:1", forBundle("app_x"))
	require.Error(t, err)
	require.Less(t, f.called("POST /images/create"), 3, "a canceled deploy does not sit out every retry")
}

func TestAPullTheDaemonRefusesOutrightIsReported(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /images/*", respond(http.StatusNotFound, map[string]string{"message": "No such image"}))
	f.on("POST /images/create", respond(http.StatusInternalServerError, map[string]string{"message": "registry mirror down"}))

	err := a.ensureImage(context.Background(), "test-image:1", forTrial)
	require.ErrorContains(t, err, "registry mirror down")

	require.Equal(t, errs.ValidInvalid, errs.As(a.ensureImage(context.Background(), "", forTrial)).Code,
		"a workload with no image is refused before asking the daemon")
}

// Vaultwarden declares VOLUME /data and refuses to start without storage
// there, so a trial gives every declared path throwaway storage (issue #55).
func TestATrialGivesEveryDeclaredVolumeThrowawayStorage(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /images/test-vault:1/json", respond(http.StatusOK, map[string]any{
		"Id":     "sha256:abc",
		"Config": map[string]any{"Volumes": map[string]any{"/data": map[string]any{}, "/config": map[string]any{}}},
	}))
	f.on("GET /images/test-plain:1/json", respond(http.StatusOK, map[string]any{"Id": "sha256:def", "Config": map[string]any{}}))

	paths := a.imageVolumes(context.Background(), "test-vault:1")
	require.Equal(t, []string{"/config", "/data"}, paths)
	require.Equal(t, map[string]string{"/config": "", "/data": ""}, tmpfsFor(paths))

	require.Empty(t, a.imageVolumes(context.Background(), "test-plain:1"))
	require.Nil(t, a.imageVolumes(context.Background(), "test-missing:1"), "an image that cannot be read declares nothing")
	require.Nil(t, tmpfsFor(nil))
}

func TestTheNetworkPoolIsOfferedWhereTheRuntimeIsConfigured(t *testing.T) {
	var found bool
	for _, field := range Info().Fields {
		if field.Key == "network_pool" {
			found = true
			require.Equal(t, defaultNetworkPool, field.Default)
			require.Contains(t, field.Help, `"off"`)
		}
	}
	require.True(t, found)
}
