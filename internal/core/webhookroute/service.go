// Package webhookroute manages an app's webhook routes (R-425, R-432): the
// routes themselves, which are in the spec, and their secrets, which are not.
// The proxy decides on a delivery; this package only changes what it decides by.
package webhookroute

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"slices"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// minSecretLength is the shortest secret a person may supply (R-427) [P].
const minSecretLength = 16

// secretPrefix marks a generated secret, so one pasted into the wrong place is
// recognizable.
const secretPrefix = "pdwr_"

// Authorizer is the question this package asks beyond the handler's verb.
// *authz.Authorizer answers it.
type Authorizer interface {
	CheckControl(ctx context.Context, p authz.Principal, appID string, verb authz.Verb) error
}

// Policy says whether a spec's routes are allowed. *policy.Evaluator is one.
type Policy interface {
	AllowsWebhookRoutes(ctx context.Context, s *spec.AppSpec) error
}

// Secrets keeps routes' secrets. *state.WebhookRouteSecrets is one.
type Secrets interface {
	Put(ctx context.Context, appID, route string, v secret.Value) error
	Names(ctx context.Context, appID string) (map[string]bool, error)
	Delete(ctx context.Context, appID, route string) error
}

// Revisions reads and writes an app's specs. *state.Apps is one.
type Revisions interface {
	PinnedAndNewest(ctx context.Context, app state.App) (pinned, newest state.Revision, err error)
	CreateRevision(ctx context.Context, appID string, s *spec.AppSpec, origin spec.Origin, createdBy string) (state.Revision, error)
}

// Service reads and changes an app's webhook routes.
type Service struct {
	Apps    Revisions
	Secrets Secrets
	Policy  Policy
	Authz   Authorizer
	Audit   func(ctx context.Context, e audit.Event)
}

// Route is a route as the API shows it.
type Route struct {
	spec.WebhookRoute

	// SecretSet is whether the route has a secret. Without one it refuses
	// every delivery (R-430).
	SecretSet bool `json:"secret_set"`

	// Deployed is whether the app is running this route as it stands. A
	// change takes effect at the app's next deploy.
	Deployed bool `json:"deployed"`
}

// View is an app's routes as the API shows them.
type View struct {
	Routes []Route `json:"routes"`
}

// Saved is a change to an app's routes.
type Saved struct {
	Route    Route `json:"route"`
	Revision int   `json:"revision"`

	// Secret is set when Pando generated one: the only time it is shown.
	Secret *Secret `json:"secret,omitempty"`
}

// Secret is a generated secret, shown once. R-194 holds everywhere else: the
// one response that carries it marshals it in the clear, on purpose (R-427).
type Secret struct{ v secret.Value }

// MarshalJSON writes the secret itself.
func (k Secret) MarshalJSON() ([]byte, error) { return json.Marshal(k.v.Reveal()) }

// List is the routes on the app's newest revision: what its next deploy ships.
func (s *Service) List(ctx context.Context, app state.App) (View, error) {
	pinned, newest, err := s.Apps.PinnedAndNewest(ctx, app)
	if err != nil {
		return View{}, err
	}
	view := View{Routes: []Route{}}
	if newest.Body == nil {
		return view, nil
	}
	set, err := s.Secrets.Names(ctx, app.ID)
	if err != nil {
		return View{}, err
	}
	for _, r := range newest.Body.WebhookRoutes {
		view.Routes = append(view.Routes, Route{
			WebhookRoute: r, SecretSet: set[r.Name], Deployed: deployed(pinned, r),
		})
	}
	return view, nil
}

func deployed(pinned state.Revision, r spec.WebhookRoute) bool {
	return pinned.Body != nil && slices.Contains(pinned.Body.WebhookRoutes, r)
}

// Add declares a route and gives it a secret: the one supplied, or a generated
// one, which comes back in Saved. Declaring is the handler's app.spec.edit; the
// secret needs app.secrets.write as well (R-427).
func (s *Service) Add(ctx context.Context, p authz.Principal, app state.App, r spec.WebhookRoute, supplied string) (Saved, error) {
	if err := s.Authz.CheckControl(ctx, p, app.ID, authz.AppSecretsWrite); err != nil {
		return Saved{}, err
	}
	pinned, newest, err := s.Apps.PinnedAndNewest(ctx, app)
	if err != nil {
		return Saved{}, err
	}
	if newest.Body == nil {
		return Saved{}, errs.New(errs.ValidInvalid, "This app isn't configured yet, so it can't have webhook routes.").
			WithRemedy("Accept its setup first, then add the route.")
	}
	key, generated, err := secretFrom(supplied)
	if err != nil {
		return Saved{}, err
	}

	next := *newest.Body
	next.WebhookRoutes = append(slices.Clone(next.WebhookRoutes), r)
	if err := spec.Validate(&next); err != nil {
		return Saved{}, err
	}
	if err := s.Policy.AllowsWebhookRoutes(ctx, &next); err != nil {
		return Saved{}, err
	}

	if err := s.Secrets.Put(ctx, app.ID, r.Name, key); err != nil {
		return Saved{}, err
	}
	rev, err := s.Apps.CreateRevision(ctx, app.ID, &next, spec.OriginEdited, p.ID)
	if err != nil {
		return Saved{}, err
	}
	s.audit(ctx, p, "app.webhook_route.add", app.ID, map[string]any{
		"route": r.Name, "method": r.Method, "path": r.Path,
		"verification": string(r.Verification.Kind), "revision": rev.Revision,
	})
	saved := Saved{Route: Route{WebhookRoute: r, SecretSet: true, Deployed: deployed(pinned, r)}, Revision: rev.Revision}
	if generated {
		saved.Secret = &Secret{key}
	}
	return saved, nil
}

// Remove takes a route out of the next revision and removes its secret now, so
// the running route refuses every delivery from here on rather than at the next
// deploy (R-430).
func (s *Service) Remove(ctx context.Context, p authz.Principal, app state.App, name string) (int, error) {
	_, newest, err := s.Apps.PinnedAndNewest(ctx, app)
	if err != nil {
		return 0, err
	}
	if _, err := routeNamed(newest, name); err != nil {
		return 0, err
	}
	next := *newest.Body
	next.WebhookRoutes = slices.DeleteFunc(slices.Clone(next.WebhookRoutes),
		func(r spec.WebhookRoute) bool { return r.Name == name })
	rev, err := s.Apps.CreateRevision(ctx, app.ID, &next, spec.OriginEdited, p.ID)
	if err != nil {
		return 0, err
	}
	if err := s.Secrets.Delete(ctx, app.ID, name); err != nil {
		return 0, err
	}
	s.audit(ctx, p, "app.webhook_route.remove", app.ID, map[string]any{"route": name, "revision": rev.Revision})
	return rev.Revision, nil
}

// SetSecret replaces a route's secret at once, with no overlap (R-427): the one
// supplied, or a generated one, which it returns. The handler checks
// app.secrets.write.
func (s *Service) SetSecret(ctx context.Context, p authz.Principal, app state.App, name, supplied string) (*Secret, error) {
	_, newest, err := s.Apps.PinnedAndNewest(ctx, app)
	if err != nil {
		return nil, err
	}
	if _, err := routeNamed(newest, name); err != nil {
		return nil, err
	}
	key, generated, err := secretFrom(supplied)
	if err != nil {
		return nil, err
	}
	if err := s.Secrets.Put(ctx, app.ID, name, key); err != nil {
		return nil, err
	}
	s.audit(ctx, p, "app.webhook_route.secret", app.ID, map[string]any{"route": name, "generated": generated})
	if !generated {
		return nil, nil
	}
	return &Secret{key}, nil
}

func routeNamed(rev state.Revision, name string) (spec.WebhookRoute, error) {
	if rev.Body != nil {
		for _, r := range rev.Body.WebhookRoutes {
			if r.Name == name {
				return r, nil
			}
		}
	}
	return spec.WebhookRoute{}, errs.Newf(errs.NotFound, "This app has no webhook route named %q.", name)
}

// secretFrom is the secret supplied, or a new one when none was.
func secretFrom(supplied string) (secret.Value, bool, error) {
	if supplied != "" {
		if len(supplied) < minSecretLength {
			return secret.Value{}, false, errs.Newf(errs.ValidInvalid,
				"A webhook route's secret has at least %d characters.", minSecretLength).
				WithRemedy("Paste the whole secret the provider issued, or leave it out for Pando to generate one.")
		}
		return secret.New(supplied), false, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return secret.Value{}, false, errs.Wrap(errs.Internal, "Could not make a webhook secret.", err)
	}
	return secret.New(secretPrefix + base64.RawURLEncoding.EncodeToString(b)), true, nil
}

func (s *Service) audit(ctx context.Context, p authz.Principal, action, appID string, detail map[string]any) {
	if s.Audit == nil {
		return
	}
	s.Audit(ctx, audit.Event{
		PrincipalKind: audit.PrincipalKind(p.Kind),
		PrincipalID:   p.ID,
		OnBehalfOf:    p.UserID,
		Action:        action,
		AppID:         appID,
		TargetKind:    "app",
		TargetID:      appID,
		Detail:        detail,
	})
}
