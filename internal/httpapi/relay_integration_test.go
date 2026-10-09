//go:build integration

package httpapi_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/httpapi"
)

// deploymentFor records a deploy of appID's pinned revision, as a replica
// claiming it would, and finishes it when finished is set.
func (i *install) deploymentFor(appID string, finished bool) string {
	i.t.Helper()
	ctx := context.Background()
	depID := i.queuedDeployment(appID)
	claimed, err := i.Server.Deployments.Claim(ctx, 100)
	require.NoError(i.t, err)
	require.NotEmpty(i.t, claimed)
	dep := state.Deployment{ID: depID}
	if finished {
		require.NoError(i.t, i.Server.Deployments.Finish(ctx, dep.ID, state.DeploySucceeded, "", ""))
	}
	return dep.ID
}

// queuedDeployment records a deploy of appID's pinned revision and leaves it
// in the queue, unclaimed.
func (i *install) queuedDeployment(appID string) string {
	i.t.Helper()
	ctx := context.Background()
	app, _, err := i.Apps.ByID(ctx, appID)
	require.NoError(i.t, err)
	dep, err := i.Server.Deployments.Create(ctx, appID, app.PinnedSpecID, state.TriggerManual, i.AdminID)
	require.NoError(i.t, err)
	return dep.ID
}

// logsRequest asks for a deploy's live log as s, with extra headers.
func (i *install) logsRequest(s *session, appID, depID string, hdr map[string]string) *httptest.ResponseRecorder {
	i.t.Helper()
	req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", appID, depID), nil)
	req.Header.Set("Cookie", httpapi.SessionCookie+"="+s.cookie)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	i.handler.ServeHTTP(rec, req)
	return rec
}

// TestR256_ADeployLogIsRelayedToTheReplicaRunningTheDeploy asserts that a
// deploy's live log, asked of a replica that is not running the deploy, is
// served by the one that is (R-256, R-170): passed on with the caller's
// credentials, marked as a relay, and streamed back.
func TestR256_ADeployLogIsRelayedToTheReplicaRunningTheDeploy(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.deploymentFor(appID, false)

	var gotPath, gotRelay, gotCookie atomic.Value
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath.Store(r.URL.Path)
		gotRelay.Store(r.Header.Get("Pando-Replica-Relay"))
		gotCookie.Store(r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: building on the other replica\n\nevent: end\ndata: \n\n")
	}))
	t.Cleanup(owner.Close)

	var asked atomic.Value
	i.Server.LogOwner = func(_ context.Context, id string) (string, error) {
		asked.Store(id)
		return owner.URL, nil
	}

	got := i.logsRequest(admin, appID, depID, nil)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.Equal(t, depID, asked.Load())
	require.Contains(t, got.Body.String(), "building on the other replica", "the owner's stream comes back")
	require.Equal(t, fmt.Sprintf("/api/v1/apps/%s/deployments/%s/logs", appID, depID), gotPath.Load())
	require.Equal(t, "1", gotRelay.Load(), "marked, so the owner serves it rather than relaying again")
	require.Contains(t, gotCookie.Load(), httpapi.SessionCookie+"=",
		"passed on with the caller's credentials, for the owner to authorize again")
}

// TestR256_AQueuedDeploysLogFollowsItToTheReplicaThatTakesIt asserts issue
// #93 with more than one replica: the stream that showed the deploy's place in
// the queue carries on with the log of whichever replica takes it, in the same
// response, relayed with the caller's credentials as any deploy log is.
func TestR256_AQueuedDeploysLogFollowsItToTheReplicaThatTakesIt(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	i.Server.QueuePollEvery = 10 * time.Millisecond
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.queuedDeployment(appID)

	var gotRelay, gotCookie atomic.Value
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRelay.Store(r.Header.Get("Pando-Replica-Relay"))
		gotCookie.Store(r.Header.Get("Cookie"))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: => Starting the deploy\n\nevent: end\ndata: \n\n")
	}))
	t.Cleanup(owner.Close)
	i.Server.LogOwner = func(context.Context, string) (string, error) { return owner.URL, nil }

	srv := httptest.NewServer(i.handler)
	t.Cleanup(srv.Close)
	stream := openSSE(t, srv.URL, admin, "/apps/"+appID+"/deployments/"+depID+"/logs")
	require.Equal(t, sseEvent{data: "=> Waiting for a build slot: this deploy is next."}, stream.next(t))

	_, err := i.Server.Deployments.Claim(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, sseEvent{data: "=> Starting the deploy"}, stream.next(t), "the owner's log, in the same stream")
	require.Equal(t, "end", stream.next(t).name)
	require.Equal(t, "1", gotRelay.Load())
	require.Contains(t, gotCookie.Load(), httpapi.SessionCookie+"=")
}

// TestR256_OnlyADeploymentIDIsRelayed asserts that a deploy-log request naming
// something that is not a deployment ID is answered where it lands: never
// passed to another replica, and never written into a log line (CodeQL
// go/log-injection).
func TestR256_OnlyADeploymentIDIsRelayed(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")

	var asked, relayed atomic.Bool
	owner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { relayed.Store(true) }))
	t.Cleanup(owner.Close)
	i.Server.LogOwner = func(context.Context, string) (string, error) {
		asked.Store(true)
		return owner.URL, nil
	}

	for _, bad := range []string{"not-a-deployment", "dep_x%0Aforged=1", appID} {
		got := i.logsRequest(admin, appID, bad, nil)
		require.NotEqual(t, http.StatusOK, got.Code, bad)
	}
	require.False(t, asked.Load(), "nothing that is not a deployment ID is looked up")
	require.False(t, relayed.Load(), "or relayed")
}

// TestR256_ARelayedDeployLogIsNeverRelayedAgain asserts that a request one
// replica relayed is served where it lands, so a relay is one hop and never a
// loop — whatever the receiving replica thinks of who holds the log.
func TestR256_ARelayedDeployLogIsNeverRelayedAgain(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.deploymentFor(appID, true)

	var relayed atomic.Bool
	owner := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { relayed.Store(true) }))
	t.Cleanup(owner.Close)
	i.Server.LogOwner = func(context.Context, string) (string, error) { return owner.URL, nil }

	got := i.logsRequest(admin, appID, depID, map[string]string{"Pando-Replica-Relay": "1"})
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.False(t, relayed.Load(), "a relayed request is served where it lands")
	require.Contains(t, got.Body.String(), "the Pando process that ran")
}

// TestR256_AFinishedDeploysLogFromAStoppedReplicaEndsAtOnce asserts that a
// deploy finished by a replica that has since stopped answers with what
// happened to its log and ends, rather than waiting for lines that cannot
// come (R-170: the stream never hangs on nothing).
func TestR256_AFinishedDeploysLogFromAStoppedReplicaEndsAtOnce(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.deploymentFor(appID, true)

	for name, owner := range map[string]httpapi.DeployLogOwner{
		"one replica":              nil,
		"no live replica holds it": func(context.Context, string) (string, error) { return "", nil },
		"the owner is unreadable":  func(context.Context, string) (string, error) { return "", fmt.Errorf("database away") },
		"the owner's URL is bad":   func(context.Context, string) (string, error) { return "not a url", nil },
	} {
		i.Server.LogOwner = owner
		got := i.logsRequest(admin, appID, depID, nil)
		require.Equal(t, http.StatusOK, got.Code, name)
		require.Equal(t, "text/event-stream", got.Header().Get("Content-Type"), name)
		require.Contains(t, got.Body.String(), "the Pando process that ran", name)
		require.Contains(t, got.Body.String(), "event: end", name)
	}
}

// TestR256_AnUnreachableOwnerReplicaIsABadGateway asserts that a relay to a
// replica that cannot be reached answers 502 rather than hanging or
// pretending the log is empty.
func TestR256_AnUnreachableOwnerReplicaIsABadGateway(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()
	appID := i.appWithSpec(admin, "notes")
	depID := i.deploymentFor(appID, false)

	gone := httptest.NewServer(http.NotFoundHandler())
	url := gone.URL
	gone.Close()
	i.Server.LogOwner = func(context.Context, string) (string, error) { return url, nil }

	got := i.logsRequest(admin, appID, depID, nil)
	require.Equal(t, http.StatusBadGateway, got.Code, got.Body.String())
}
