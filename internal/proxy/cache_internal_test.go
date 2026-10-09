package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/id"
)

// countingSessions resolves every cookie to alice and counts how often it is
// asked: each call stands for a query the cache exists to save.
type countingSessions struct {
	calls   atomic.Int32
	expires time.Time
}

func (s *countingSessions) Authenticate(*http.Request) (authz.Principal, error) {
	return authz.Anonymous(), nil
}

func (s *countingSessions) SessionPrincipal(context.Context, string) (authz.Principal, time.Time, error) {
	s.calls.Add(1)
	return authz.Principal{Kind: authz.KindUser, ID: "usr_alice", UserID: "usr_alice", Status: "active"}, s.expires, nil
}

type countingFacts struct {
	authz.Store
	calls atomic.Int32
}

func (f *countingFacts) DataFacts(context.Context, string, authz.Principal, string) (authz.DataFacts, error) {
	f.calls.Add(1)
	return authz.DataFacts{Grant: true}, nil
}

func cookieRequest(header ...string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookie, Value: "ses_alice"})
	for _, h := range header {
		r.Header.Add("Authorization", h)
	}
	return r
}

func listeningCache(now *time.Time) *Cache {
	c := &Cache{now: func() time.Time { return *now }}
	c.setListening(true)
	return c
}

// TestR048_TheProxyCacheIsEmptiedByEveryChange asserts what keeping a
// session's reads (issue #93) must not cost: R-048's revocation. A second
// request is answered without the store, and a change anywhere — the
// database's notification is Clear — sends the next one back to it. While
// the cache is not listening for changes it keeps nothing.
func TestR048_TheProxyCacheIsEmptiedByEveryChange(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	sessions := &countingSessions{}

	for range 3 {
		p, ok, err := c.principal(cookieRequest(), sessions)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "usr_alice", p.ID)
	}
	require.Equal(t, int32(1), sessions.calls.Load(), "one read for three requests")

	c.Clear()
	_, _, _ = c.principal(cookieRequest(), sessions)
	require.Equal(t, int32(2), sessions.calls.Load(), "a change sends the next request to the store")

	c.setListening(false)
	_, _, _ = c.principal(cookieRequest(), sessions)
	_, _, _ = c.principal(cookieRequest(), sessions)
	require.Equal(t, int32(4), sessions.calls.Load(), "not listening, nothing is kept")
}

// An entry lives no longer than the TTL, nor than the session it describes.
func TestR048_ACachedSessionLivesNoLongerThanItself(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	sessions := &countingSessions{expires: now.Add(5 * time.Second)}

	_, _, _ = c.principal(cookieRequest(), sessions)
	now = now.Add(4 * time.Second)
	_, _, _ = c.principal(cookieRequest(), sessions)
	require.Equal(t, int32(1), sessions.calls.Load())

	now = now.Add(2 * time.Second) // past the session's own expiry
	_, _, _ = c.principal(cookieRequest(), sessions)
	require.Equal(t, int32(2), sessions.calls.Load(), "the session ended, so its entry did")

	sessions.expires = now.Add(time.Hour)
	_, _, _ = c.principal(cookieRequest(), sessions)
	now = now.Add(c.ttl() + time.Second)
	_, _, _ = c.principal(cookieRequest(), sessions)
	require.Equal(t, int32(4), sessions.calls.Load(), "and none outlives the TTL")
}

// A Pando token is never kept, and is left to the ordinary path; an app's own
// Authorization header is not Pando's, so the cookie decides and is kept.
func TestR173_OnlyASessionIsKept(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	sessions := &countingSessions{}

	_, ok, _ := c.principal(cookieRequest("Bearer "+id.New(id.Token)+".c2VjcmV0"), sessions)
	require.False(t, ok, "a Pando token goes the ordinary way")
	require.Zero(t, sessions.calls.Load())

	_, ok, _ = c.principal(cookieRequest("Bearer the-apps-own-jwt"), sessions)
	require.True(t, ok)
	_, _, _ = c.principal(cookieRequest("Basic YXBwOnB3"), sessions)
	require.Equal(t, int32(1), sessions.calls.Load())

	_, ok, _ = c.principal(httptest.NewRequest(http.MethodGet, "/", nil), sessions)
	require.False(t, ok, "no cookie, nothing to keep")
}

// TestR029_TheCacheKeepsFactsNotVerdicts asserts that the proxy's authorizer
// still decides every request (issue #93): the facts are kept, per app, per
// caller and per unlock, and CheckData runs over them each time, so a denial
// is audited on every request as before.
func TestR029_TheCacheKeepsFactsNotVerdicts(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	under := &countingFacts{}
	store := c.Store(under).(authz.DataFactsReader)
	alice := authz.Principal{Kind: authz.KindUser, ID: "usr_alice", UserID: "usr_alice"}
	bob := authz.Principal{Kind: authz.KindUser, ID: "usr_bob", UserID: "usr_bob"}

	for range 3 {
		f, err := store.DataFacts(context.Background(), "app_1", alice, "")
		require.NoError(t, err)
		require.True(t, f.Grant)
	}
	require.Equal(t, int32(1), under.calls.Load())

	_, _ = store.DataFacts(context.Background(), "app_2", alice, "")
	_, _ = store.DataFacts(context.Background(), "app_1", bob, "")
	_, _ = store.DataFacts(context.Background(), "app_1", alice, "an-unlock")
	require.Equal(t, int32(4), under.calls.Load(), "another app, caller or unlock is another entry")

	c.Clear()
	_, _ = store.DataFacts(context.Background(), "app_1", alice, "")
	require.Equal(t, int32(5), under.calls.Load())

	require.Same(t, under, c.Store(under).(*cachedStore).reader, "the reads go to the store underneath")
	var notAReader authz.Store = struct{ authz.Store }{}
	require.Equal(t, notAReader, c.Store(notAReader), "a store that cannot read facts in one go is not wrapped")
}

// App lookups are kept, misses included, so a stream of requests for a
// hostname no app has costs one query; the router's hostname check and the
// proxy's lookup of the same request are one.
func TestR023_AnAppLookupIsKeptMissesIncluded(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	var reads atomic.Int32
	read := func(found bool) func(context.Context) (state.App, *spec.AppSpec, bool, error) {
		return func(context.Context) (state.App, *spec.AppSpec, bool, error) {
			reads.Add(1)
			return state.App{ID: "app_1"}, &spec.AppSpec{}, found, nil
		}
	}

	for range 3 {
		_, _, found, err := c.resolve(context.Background(), appKey("hostname", "Notes.Example.com"), read(true))
		require.NoError(t, err)
		require.True(t, found)
	}
	_, _, _, _ = c.resolve(context.Background(), appKey("hostname", "notes.example.com"), read(true))
	require.Equal(t, int32(1), reads.Load(), "a hostname is matched without regard to case")

	for range 3 {
		_, _, found, _ := c.resolve(context.Background(), appKey("hostname", "nobody.example.com"), read(false))
		require.False(t, found)
	}
	require.Equal(t, int32(2), reads.Load(), "a miss is kept too")

	var nilCache *Cache
	_, _, _, _ = nilCache.resolve(context.Background(), appKey("slug", "notes"), read(true))
	require.Equal(t, int32(3), reads.Load(), "no cache reads every time")
}

// A client choosing hostnames or cookies cannot grow the cache without bound.
func TestTheCacheIsBounded(t *testing.T) {
	now := time.Now()
	c := listeningCache(&now)
	c.Limit = 10
	read := func(context.Context) (state.App, *spec.AppSpec, bool, error) { return state.App{}, nil, false, nil }
	for i := range 100 {
		_, _, _, _ = c.resolve(context.Background(), appKey("hostname", id.New(id.App)), read)
		require.LessOrEqual(t, len(c.apps), 10, "after %d", i)
	}
}
