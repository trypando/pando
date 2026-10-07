package docker

import (
	"context"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// rejoinDaemon is a fake daemon holding the given networks and containers,
// with Pando's own container (named self) on the networks in selfOn, by name
// and endpoint ID. It records which networks Pando was connected to.
func rejoinDaemon(t *testing.T, config map[string]any, networks, containers []any, self string, selfOn map[string]any) (*fakeDaemon, *Adapter, func() []string) {
	t.Helper()
	f, a := newFakeDaemon(t, config)
	f.on("GET /networks", respond(http.StatusOK, networks))
	f.on("GET /containers/json", respond(http.StatusOK, containers))
	if self != "" {
		f.on("GET /containers/"+self+"/json", respond(http.StatusOK, map[string]any{
			"Id": self, "Name": "/" + self,
			"NetworkSettings": map[string]any{"Networks": selfOn},
		}))
	}
	var mu sync.Mutex
	var connected []string
	f.on("POST /networks/*", func(w http.ResponseWriter, r *http.Request) {
		path := apiVersionPrefix.ReplaceAllString(r.URL.Path, "")
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/networks/"), "/connect")
		mu.Lock()
		connected = append(connected, id)
		mu.Unlock()
		if id == "n-refuses" {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": "daemon busy"})
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return f, a, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), connected...)
	}
}

func appNetwork(id, name, bundle string, extra ...string) map[string]any {
	labels := map[string]string{labelManaged: "true"}
	if bundle != "" {
		labels[labelBundle] = bundle
	}
	for i := 0; i+1 < len(extra); i += 2 {
		labels[extra[i]] = extra[i+1]
	}
	return map[string]any{"Id": id, "Name": name, "Labels": labels}
}

func appContainer(bundle string, networks ...string) map[string]any {
	labels := map[string]string{}
	if bundle != "" {
		labels[labelBundle] = bundle
	}
	c := map[string]any{"Id": "c-" + bundle, "Labels": labels, "State": "running"}
	if networks != nil {
		on := map[string]any{}
		for _, n := range networks {
			on[n] = map[string]any{}
		}
		c["NetworkSettings"] = map[string]any{"Networks": on}
	}
	return c
}

// TestR023_RejoiningJoinsOnlyTheNetworksWorthJoining asserts which app
// networks a restarted Pando rejoins, from one list of networks and one of
// containers: one of its app's own containers is on it, it is not the egress
// gateway's way out, this install owns it, and Pando is not on it already —
// by name, or by the endpoint's network ID.
func TestR023_RejoiningJoinsOnlyTheNetworksWorthJoining(t *testing.T) {
	networks := []any{
		appNetwork("n-joined-by-name", "pando-a", "app_a"),
		appNetwork("n-joined-by-id", "pando-b-renamed", "app_b"),
		appNetwork("n-empty", "pando-c", "app_c"),
		appNetwork("n-outbound", "pando-d-outbound", "app_d", labelEgressNetwork, egressNetworkOutbound),
		appNetwork("n-unlabeled", "pando-orphan", ""),
		appNetwork("n-unreported", "pando-e", "app_e"),
		appNetwork("n-other-install", "pando-f", "app_f"),
		appNetwork("n-wanted", "pando-g", "app_g"),
		appNetwork("n-refuses", "pando-h", "app_h"),
	}
	containers := []any{
		appContainer("app_a", "pando-a"),
		appContainer("app_b", "pando-b-renamed"),
		appContainer("app_c", "bridge"), // on some other network, not its own
		appContainer("app_d", "pando-d-outbound"),
		appContainer("app_e"), // its networks were not reported: counts as on all of them
		appContainer("app_f", "pando-f"),
		appContainer("app_g", "pando-g"),
		appContainer("app_h", "pando-h"),
		appContainer("", "pando-orphan"), // not an app's container
	}
	selfOn := map[string]any{
		"pando-a":         map[string]any{"NetworkID": "n-joined-by-name"},
		"an-older-name":   map[string]any{"NetworkID": "n-joined-by-id"},
		"compose_default": nil,
	}
	_, a, connected := rejoinDaemon(t, map[string]any{"proxy_container": "pando-self", "network_pool": "off"},
		networks, containers, "pando-self", selfOn)

	var asked []string
	owns := func(bundle string) bool { asked = append(asked, bundle); return bundle != "app_f" }
	joined, err := a.RejoinNetworks(context.Background(), owns)
	require.NoError(t, err)
	require.Equal(t, 2, joined, "a network that refused the connect is not counted")
	require.ElementsMatch(t, []string{"n-unreported", "n-wanted", "n-refuses"}, connected())
	require.ElementsMatch(t, []string{"app_e", "app_f", "app_g", "app_h"}, asked,
		"only a network about to be joined costs a question about who owns it")
}

func TestRejoiningWithoutKnowingItsOwnNetworksStillJoins(t *testing.T) {
	// Pando not in a container, or its inspect failing: nothing is assumed
	// joined, and every occupied network goes on to the other checks.
	_, a, connected := rejoinDaemon(t, map[string]any{"network_pool": "off"},
		[]any{appNetwork("n1", "pando-a", "app_a")}, []any{appContainer("app_a", "pando-a")}, "", nil)
	host, err := os.Hostname()
	require.NoError(t, err)
	require.Nil(t, a.proxyNetworks(context.Background()), "the container named %q does not exist", host)

	joined, err := a.RejoinNetworks(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, 1, joined)
	require.Equal(t, []string{"n1"}, connected())
}

func TestRejoiningFailsWhenTheAppContainersCannotBeListed(t *testing.T) {
	f, a := newFakeDaemon(t, map[string]any{"proxy_container": "pando-self"})
	f.on("GET /networks", respond(http.StatusOK, []any{appNetwork("n1", "pando-a", "app_a")}))
	f.on("GET /containers/json", respond(http.StatusInternalServerError, map[string]string{"message": "daemon busy"}))
	_, err := a.RejoinNetworks(context.Background(), nil)
	require.Equal(t, errs.AdapterUnavailable, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "Could not list the app containers.")
}

func TestOccupancyOfANetworkWithNoApp(t *testing.T) {
	o := occupancy{onNetwork: map[string]map[string]bool{"app_a": {"pando-a": true}}, unknown: map[string]bool{"": true}}
	require.False(t, o.has("", "pando-a"), "a network that names no app is no app's")
	require.True(t, o.has("app_a", "pando-a"))
	require.False(t, o.has("app_a", "pando-b"))
}

// TestR245_AUsageReadingCanceledWhileWaitingReportsUnknown asserts that a
// reading whose caller gave up while every sampling slot was taken returns
// what it knows — each workload, its usage unknown — rather than waiting on
// the daemon for samples nobody will read.
func TestR245_AUsageReadingCanceledWhileWaitingReportsUnknown(t *testing.T) {
	f, a := newFakeDaemon(t, nil)
	f.on("GET /containers/json", respond(http.StatusOK, []any{
		map[string]any{"Id": "c1", "State": "running", "Labels": map[string]string{labelBundle: "app_x", labelWorkload: "web"}},
		map[string]any{"Id": "c2", "State": "running", "Labels": map[string]string{labelBundle: "app_x", labelWorkload: "worker"}},
	}))
	f.on("GET /volumes", respond(http.StatusOK, map[string]any{"Volumes": []any{}}))

	// Every slot held by readings already in flight.
	slots := a.samplingSlots()
	for range samplingConcurrency {
		slots <- struct{}{}
	}
	require.Equal(t, cap(slots), cap(a.samplingSlots()), "one limit for the whole adapter")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	usage, err := a.Usage(ctx, api.BundleRef{BundleID: "app_x"})
	require.NoError(t, err)
	require.Len(t, usage.Workloads, 2)
	for _, w := range usage.Workloads {
		require.Contains(t, []string{"web", "worker"}, w.Workload)
		require.Equal(t, int64(-1), w.DiskBytes, "unknown, not zero")
	}
	require.Zero(t, f.called("GET /containers/c1"), "no sample was taken")

	ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	inUse, err := a.InUse(ctx)
	require.NoError(t, err, "a reading cut short is what was read, not an error")
	require.Zero(t, inUse.CPUMillis)
	require.Zero(t, inUse.MemoryBytes)
}

func TestFreeBlocksSmallerThanThePoolOnly(t *testing.T) {
	var got []netip.Prefix
	for b := range freeBlocks(netip.MustParsePrefix("10.213.0.0/24"), 16, nil) {
		got = append(got, b)
	}
	require.Empty(t, got, "a block larger than the pool does not fit in it")
}
