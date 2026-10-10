package proxy_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/assertion"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/proxy"
)

type touches struct {
	mu   sync.Mutex
	apps []string
}

func (t *touches) Touch(appID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.apps = append(t.apps, appID)
}

func (t *touches) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.apps)
}

func idleProxy(t *testing.T, app state.App, principal authz.Principal, activity proxy.ActivityRecorder) *httptest.Server {
	t.Helper()
	s := newStore()
	s.owner[appID] = "usr_alice"

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "upstream ok")
	}))
	t.Cleanup(upstream.Close)

	minter, err := assertion.NewMinter("https://pando.test", nil)
	require.NoError(t, err)
	front := httptest.NewServer(&proxy.Proxy{
		Resolver: &resolver{
			app: app,
			spec: &spec.AppSpec{
				Routing: spec.Routing{Mode: spec.RoutingSubdomain, Hostname: "127.0.0.1"},
				Workloads: []spec.Workload{{Name: "web", Primary: true,
					Ports: []spec.Port{{Number: 80, Protocol: "http"}}}},
			},
		},
		Authenticator: staticAuth{principal: principal},
		Authz:         authz.New(s, nil, nil),
		Minter:        minter,
		Upstreams:     fixedUpstream{addr: upstream.URL},
		Activity:      activity,
		Logger:        zap.NewNop(),
		Mode:          spec.RoutingSubdomain,
	})
	t.Cleanup(front.Close)
	return front
}

func get(t *testing.T, url string) (int, string) {
	t.Helper()
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Get(url)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

// TestR394_OnlyARequestLetThroughIsActivity asserts R-394: a request the
// proxy forwards keeps the app from being idle, and a refused one does not —
// or a crawler at a private app's door would keep it forever.
func TestR394_OnlyARequestLetThroughIsActivity(t *testing.T) {
	running := state.App{ID: appID, Slug: "notes", State: state.StateRunning}

	seen := &touches{}
	status, _ := get(t, idleProxy(t, running, activeUser("usr_alice"), seen).URL+"/")
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, 1, seen.count())
	require.Equal(t, appID, seen.apps[0])

	refused := &touches{}
	status, _ = get(t, idleProxy(t, running, activeUser("usr_bob"), refused).URL+"/")
	require.Equal(t, http.StatusForbidden, status)
	status, _ = get(t, idleProxy(t, running, authz.Anonymous(), refused).URL+"/")
	require.Equal(t, http.StatusFound, status)
	require.Zero(t, refused.count(), "a refused request is not use")
}

// TestR396_AnAppStoppedForInactivitySaysSo asserts R-396 and R-151: a
// visitor to an app Pando stopped for being idle is told why and who can
// start it, not "try again in a moment", which promises it comes back.
func TestR396_AnAppStoppedForInactivitySaysSo(t *testing.T) {
	stopped := state.App{ID: appID, Slug: "notes", State: state.StateStopped, StoppedForIdle: true}

	seen := &touches{}
	status, body := get(t, idleProxy(t, stopped, activeUser("usr_alice"), seen).URL+"/")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, "stopped because nobody had used it")
	require.Contains(t, body, "start it again")
	require.NotContains(t, body, "Try again in a moment")
	require.Zero(t, seen.count(), "a request to a stopped app does not start its clock again (R-396)")
}

// TestR403_AnAppStoppedForDiskSaysSo asserts a visitor to an app Pando
// stopped for staying over its disk limit is told why and who can fix it.
func TestR403_AnAppStoppedForDiskSaysSo(t *testing.T) {
	stopped := state.App{ID: appID, Slug: "notes", State: state.StateStopped, StoppedForDisk: true}

	status, body := get(t, idleProxy(t, stopped, activeUser("usr_alice"), &touches{}).URL+"/")
	require.Equal(t, http.StatusServiceUnavailable, status)
	require.Contains(t, body, "more disk space than it is allowed")
	require.Contains(t, body, "start it again")
	require.NotContains(t, body, "Try again in a moment")
}
