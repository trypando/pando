package proxy

import (
	"context"
	"crypto/sha256"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
)

// Cache keeps, for a few seconds, what a proxied request reads from the state
// store: who a session cookie belongs to, which app a hostname, slug or port
// names, and the facts CheckData decides on (issue #93). Every request to every
// app reads these, a page is dozens of requests, and they rarely change.
//
// Rarely, but at once when they do: a revoked session or grant must stop
// working now, not when an entry ages out (design 06 §3.1). So the cache is
// emptied on every replica whenever anything it may hold changes, by a NOTIFY
// the database sends from triggers on the tables involved (migration 68), and
// it holds nothing at all while it is not listening for those — a copy kept
// only because nothing said it changed is good only while something could
// have. TTL bounds an entry anyway, for a notification lost in flight.
//
// What it does not keep: a verdict. CheckData still runs on every request,
// over cached facts, so every denial is audited as it happens (design 06 §6).
// Nor a bearer token, nor a session that did not resolve: those are read every
// time.
type Cache struct {
	// TTL is the longest an entry lives while the cache is listening. Thirty
	// seconds when zero, a quarter of the revocation window, which is the
	// most a lost notification can add to it.
	TTL time.Duration

	// Limit is how many entries of each kind are kept; past it, that kind is
	// emptied. 4096 when zero. A hostname or cookie is whatever a client sends,
	// and must not be a way to grow this without bound.
	Limit int

	now func() time.Time

	mu        sync.Mutex
	listening bool
	gen       uint64
	sessions  map[[32]byte]cached[authz.Principal]
	facts     map[factsKey]cached[authz.DataFacts]
	apps      map[string]cached[resolved]
}

type cached[T any] struct {
	value   T
	gen     uint64
	expires time.Time
}

type factsKey struct {
	appID, kind, id, userID string
	unlock                  string
}

type resolved struct {
	app   state.App
	spec  *spec.AppSpec
	found bool
}

// CacheChannel is the channel migration 68's triggers notify on.
const CacheChannel = "pando_proxy_cache"

// Listen keeps the cache in step with the database until ctx ends: every
// notification empties it, and while the connection is down it keeps nothing.
func (c *Cache) Listen(ctx context.Context, db *state.DB) {
	db.Listen(ctx, CacheChannel, func(string) { c.Clear() }, c.setListening)
}

func (c *Cache) setListening(up bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.listening = up
	c.gen++ // whatever was kept across a gap is not trusted after it
}

// Clear forgets everything.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
}

func (c *Cache) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *Cache) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return 30 * time.Second
}

func (c *Cache) limit() int {
	if c.Limit > 0 {
		return c.Limit
	}
	return 4096
}

// lookup returns a live entry. Called with c.mu held.
func lookup[K comparable, T any](c *Cache, m map[K]cached[T], key K) (T, bool) {
	var zero T
	if c == nil || !c.listening {
		return zero, false
	}
	e, ok := m[key]
	if !ok || e.gen != c.gen || !c.clock().Before(e.expires) {
		return zero, false
	}
	return e.value, true
}

// store keeps an entry until expires at the latest, and returns the map, made
// or emptied as needed. Called with c.mu held, and only with a generation read
// before the value was: a value read across a Clear is not kept.
func store[K comparable, T any](c *Cache, m map[K]cached[T], key K, value T, gen uint64, expires time.Time) map[K]cached[T] {
	if !c.listening || gen != c.gen {
		return m
	}
	if m == nil || len(m) >= c.limit() {
		m = make(map[K]cached[T])
	}
	if latest := c.clock().Add(c.ttl()); expires.IsZero() || expires.After(latest) {
		expires = latest
	}
	m[key] = cached[T]{value: value, gen: gen, expires: expires}
	return m
}

// SessionResolver is an Authenticator that can resolve a session cookie on its
// own and say when the session ends, which is what lets the cache keep it.
type SessionResolver interface {
	SessionPrincipal(ctx context.Context, sessionID string) (authz.Principal, time.Time, error)
}

// sessionCookie is the cookie the cache looks for; httpapi.SessionCookie,
// spelled out because the proxy does not import the API's package.
const sessionCookie = "pando_session"

// principal resolves a request carried by a session cookie alone, from the
// cache when it can. ok is false for any other request, which the caller
// authenticates as before.
func (c *Cache) principal(r *http.Request, auth Authenticator) (p authz.Principal, ok bool, err error) {
	resolver, can := auth.(SessionResolver)
	if c == nil || !can {
		return authz.Principal{}, false, nil
	}
	// A Pando token decides who this is, and is not kept. Any other
	// Authorization header is the app's own, and the cookie decides
	// (credentials.go).
	for _, v := range r.Header.Values("Authorization") {
		if isPandoToken(v) {
			return authz.Principal{}, false, nil
		}
	}
	session := ""
	for _, ck := range r.Cookies() {
		if ck.Name == sessionCookie {
			session = ck.Value
			break
		}
	}
	if session == "" {
		return authz.Principal{}, false, nil // no session: the ordinary path decides
	}
	key := sha256.Sum256([]byte(session))

	c.mu.Lock()
	hit, found := lookup(c, c.sessions, key)
	gen := c.gen
	c.mu.Unlock()
	if found {
		return hit, true, nil
	}

	p, expires, err := resolver.SessionPrincipal(r.Context(), session)
	if err != nil || p.Kind != authz.KindUser {
		return p, true, err
	}
	c.mu.Lock()
	c.sessions = store(c, c.sessions, key, p, gen, expires)
	c.mu.Unlock()
	return p, true, nil
}

// Store wraps an authorization store so the DataFacts the proxy's authorizer
// reads are kept. Everything else goes to the store underneath, and a store
// that cannot read DataFacts in one query is returned as it is.
func (c *Cache) Store(s authz.Store) authz.Store {
	r, ok := s.(authz.DataFactsReader)
	if c == nil || !ok {
		return s
	}
	return &cachedStore{Store: s, reader: r, cache: c}
}

type cachedStore struct {
	authz.Store
	reader authz.DataFactsReader
	cache  *Cache
}

// DataFacts implements authz.DataFactsReader.
func (s *cachedStore) DataFacts(ctx context.Context, appID string, p authz.Principal, passcodeToken string) (authz.DataFacts, error) {
	// The unlock as it is: a random token, held for the entry's few seconds,
	// in the memory of the process the request brought it to.
	key := factsKey{appID: appID, kind: string(p.Kind), id: p.ID, userID: p.UserID, unlock: passcodeToken}
	c := s.cache
	c.mu.Lock()
	hit, found := lookup(c, c.facts, key)
	gen := c.gen
	c.mu.Unlock()
	if found {
		return hit, nil
	}
	f, err := s.reader.DataFacts(ctx, appID, p, passcodeToken)
	if err != nil {
		return f, err
	}
	c.mu.Lock()
	c.facts = store(c, c.facts, key, f, gen, time.Time{})
	c.mu.Unlock()
	return f, nil
}

var _ authz.DataFactsReader = (*cachedStore)(nil)

// resolve answers an app lookup from the cache, or with read, keeping the
// answer — a miss included, so a request for a hostname no app has does not
// cost a query each time.
func (c *Cache) resolve(ctx context.Context, key string, read func(context.Context) (state.App, *spec.AppSpec, bool, error)) (state.App, *spec.AppSpec, bool, error) {
	if c == nil {
		return read(ctx)
	}
	c.mu.Lock()
	hit, found := lookup(c, c.apps, key)
	gen := c.gen
	c.mu.Unlock()
	if found {
		return hit.app, hit.spec, hit.found, nil
	}
	app, s, ok, err := read(ctx)
	if err != nil {
		return app, s, ok, err
	}
	c.mu.Lock()
	c.apps = store(c, c.apps, key, resolved{app: app, spec: s, found: ok}, gen, time.Time{})
	c.mu.Unlock()
	return app, s, ok, nil
}

func appKey(by, value string) string { return by + "\x00" + strings.ToLower(value) }
