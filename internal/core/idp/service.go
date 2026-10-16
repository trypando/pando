// Package idp is external identity: the providers an administrator connects,
// redirect sign-in through them, linking their identities to accounts, the
// groups they sync, and SCIM provisioning (R-043, R-045, R-047, R-048, R-050).
//
// Every surface calls this — the HTTP API, and through it the console and CLI
// — so none of them holds a rule the others lack (R-261). Adapters
// authenticate; everything that decides anything is here (R-044).
package idp

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Kind is a compiled-in identity adapter kind (R-253): how to make one, and
// how to describe it to a form.
type Kind struct {
	New  func() api.IdentityAdapter
	Info api.KindInfo
}

// Service is external identity.
type Service struct {
	Providers   *state.IdentityProviders
	Credentials *state.AdapterCredentials
	Identities  *state.Identities
	Users       *state.Users
	Sessions    *state.Sessions
	Groups      *state.Groups
	Flows       *state.SSOFlows
	SCIMUsers   *state.SCIMUsers
	SCIMGroups  *state.SCIMGroups

	// Grants says whether an account holds an installation role, which an
	// email alone may not link a provider's identity to (R-080a). Nil links
	// no account by email.
	Grants interface {
		InstallRolesForUsers(ctx context.Context, userIDs []string) (map[string][]state.HeldInstallRole, error)
	}

	// Policy is host policy as in force, startup overlay included.
	Policy interface {
		Load(ctx context.Context) (policy.Document, error)
	}

	// Local is the local username-and-password adapter (R-041).
	Local api.IdentityAdapter

	// Kinds are the external kinds this build can run, by kind string.
	Kinds map[string]Kind

	// ExternalURL is PANDO_SERVER_EXTERNAL_URL. When set it is the only
	// address callbacks go to; when not, the address a request arrived at.
	ExternalURL *url.URL

	// Audit records an event. The HTTP layer supplies one that adds the
	// request ID.
	Audit func(ctx context.Context, e audit.Event)

	Clock clock.Clock

	mu    sync.Mutex
	built map[string]builtAdapter
}

type builtAdapter struct {
	stamp   time.Time
	adapter api.IdentityAdapter
}

func (s *Service) now() time.Time {
	if s.Clock == nil {
		return time.Now().UTC()
	}
	return s.Clock.Now()
}

func (s *Service) audit(ctx context.Context, e audit.Event) {
	if s.Audit != nil {
		s.Audit(ctx, e)
	}
}

func principalEvent(p authz.Principal, action, targetKind, targetID string, detail map[string]any) audit.Event {
	return audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind), PrincipalID: p.ID, OnBehalfOf: p.UserID,
		Action: action, TargetKind: targetKind, TargetID: targetID, Detail: detail,
	}
}

// Adapter returns a provider's adapter, configured from its stored settings
// and decrypted credentials.
//
// Built on first use and rebuilt when the provider changes, so a provider an
// administrator adds or edits works on the next request — unlike other adapter
// categories, whose instances are fixed at startup (R-253). A sign-in page
// that needed a restart to show a new provider would be a restart in front of
// everyone who uses the install.
func (s *Service) Adapter(ctx context.Context, providerID string) (api.IdentityAdapter, state.IdentityProvider, error) {
	p, found, err := s.Providers.ByID(ctx, providerID)
	if err != nil {
		return nil, state.IdentityProvider{}, err
	}
	if !found {
		return nil, state.IdentityProvider{}, errs.New(errs.NotFound, "There is no identity provider with that ID.")
	}
	if p.Kind == "local" {
		return s.Local, p, nil
	}
	s.mu.Lock()
	b, ok := s.built[p.ID]
	s.mu.Unlock()
	if ok && b.stamp.Equal(p.UpdatedAt) {
		return b.adapter, p, nil
	}
	a, err := s.build(ctx, p)
	if err != nil {
		return nil, p, err
	}
	s.mu.Lock()
	if s.built == nil {
		s.built = map[string]builtAdapter{}
	}
	s.built[p.ID] = builtAdapter{stamp: p.UpdatedAt, adapter: a}
	s.mu.Unlock()
	return a, p, nil
}

func (s *Service) build(ctx context.Context, p state.IdentityProvider) (api.IdentityAdapter, error) {
	kind, ok := s.Kinds[p.Kind]
	if !ok {
		return nil, errs.Newf(errs.AdapterUnavailable, "This build of Pando cannot run %s identity providers.", p.Kind)
	}
	creds, err := s.Credentials.Resolve(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	raw, err := withCredentials(p.Config, creds)
	if err != nil {
		return nil, errs.Wrap(errs.Internal, "Could not read the identity provider's settings.", err)
	}
	a := kind.New()
	if err := a.Configure(ctx, raw); err != nil {
		return nil, err
	}
	return a, nil
}

// withCredentials adds decrypted credentials as a `credentials` object — the
// key the database refuses in stored config, so the adapter knows where they
// came from. The same convention as every other adapter category.
func withCredentials(raw json.RawMessage, creds map[string]secret.Value) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	if len(creds) > 0 {
		plain := make(map[string]string, len(creds))
		for field, v := range creds {
			plain[field] = v.Reveal()
		}
		body, err := json.Marshal(plain)
		if err != nil {
			return nil, err
		}
		cfg["credentials"] = body
	}
	return json.Marshal(cfg)
}

// Origin is where a request came from, as scheme and host — the address a
// browser is sent back to when a flow finishes.
type Origin struct {
	Scheme string
	Host   string
}

func (o Origin) String() string { return o.Scheme + "://" + o.Host }

// base is where the provider sends responses: the external URL when the
// operator stated one, the request's origin otherwise.
func (s *Service) base(o Origin) string {
	if s.ExternalURL != nil && s.ExternalURL.Host != "" {
		return strings.TrimRight(s.ExternalURL.String(), "/")
	}
	return o.String()
}

// Endpoints are a provider's callback URL and entity ID. What an
// administrator registers with the provider, so they are stable — computed
// from the external URL and the provider's ID, never from anything a person
// types.
func (s *Service) Endpoints(o Origin, providerID string) api.Endpoints {
	b := s.base(o) + "/api/v1/auth/providers/" + providerID
	return api.Endpoints{CallbackURL: b + "/callback", EntityID: b + "/metadata"}
}

// SCIMBaseURL is what an administrator gives a SCIM client.
func (s *Service) SCIMBaseURL(o Origin) string { return s.base(o) + "/api/v1/scim/v2" }

// Revocation is a provider's session behavior as it actually is on this
// install (R-047, R-050): the adapter's declaration, upgraded to push when a
// SCIM client can push, and the window that results.
type Revocation struct {
	MaxLifetimeSeconds int64  `json:"max_lifetime_seconds"`
	Mode               string `json:"mode"`

	// WindowSeconds is the longest access can outlive its removal at the
	// provider: the session lifetime without push; with push, the two-minute
	// window every revocation has anyway (design 06 §3.1).
	WindowSeconds int64 `json:"window_seconds"`
}

// RevocationWindow is design 06 §3.1's number: the longest any revocation
// takes to take effect, once Pando knows about it.
const RevocationWindow = 120 * time.Second

func revocation(a api.IdentityAdapter, p state.IdentityProvider) Revocation {
	pol := a.SessionPolicy()
	mode := pol.RevocationMode
	if p.Kind != "local" && a.SupportsPush() && p.SCIMEnabled {
		mode = api.RevocationPush
	}
	r := Revocation{MaxLifetimeSeconds: int64(pol.MaxLifetime / time.Second), Mode: string(mode)}
	if mode == api.RevocationPush {
		r.WindowSeconds = int64(RevocationWindow / time.Second)
	} else {
		r.WindowSeconds = r.MaxLifetimeSeconds
	}
	return r
}

// SessionLifetime is how long a session made through a provider lasts.
func (s *Service) SessionLifetime(ctx context.Context, providerID string) (time.Duration, error) {
	a, _, err := s.Adapter(ctx, providerID)
	if err != nil {
		return 0, err
	}
	return a.SessionPolicy().MaxLifetime, nil
}

func randomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand failed: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Digest is the SHA-256 of a high-entropy random value, hex-encoded. Used for
// the browser binding, the handoff code and the SCIM token, each of which is
// 256 random bits: nothing to brute-force, so the slow hash that protects
// passwords would only cost time on every request.
func Digest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}
