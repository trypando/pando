// Package imageregistry is the install's image registry: where a build is
// pushed when the runtime pulls rather than imports (issue #72, PR 5,
// docs/design/notes-image-registry-issue-72.md).
//
// The registry is an adapter (R-252, issue #153; adapter/api/imageregistry.go)
// and this package is how core uses it. Like source connections, a stored
// registry adapter is built from its row each time it is used, never cached:
// a password rotated on one replica is what every replica pushes with next,
// with no restart and nothing to watch. One declared in the startup
// configuration — the config file, or PANDO_REGISTRY_* — is built once at
// startup and is read-only (R-271).
package imageregistry

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// Registry is the install's registry adapter, as core uses it. A nil
// *Registry is "none configured", and every method answers accordingly.
type Registry struct {
	id string
	a  api.ImageRegistryAdapter
}

// Of is the Registry for a configured adapter.
func Of(id string, a api.ImageRegistryAdapter) *Registry {
	if a == nil {
		return nil
	}
	return &Registry{id: id, a: a}
}

// ID is the adapter's ID.
func (r *Registry) ID() string {
	if r == nil {
		return ""
	}
	return r.id
}

// Configured reports whether the install has a registry.
func (r *Registry) Configured() bool { return r != nil }

// Always reports whether builds go through the registry even for a runtime
// that can import them.
func (r *Registry) Always() bool {
	return r != nil && r.a.ImageRegistryCapabilities().SendsEveryBuild
}

// Host is the registry's host and port, for messages.
func (r *Registry) Host() string {
	if r == nil {
		return ""
	}
	return r.a.ImageRegistryCapabilities().Host
}

// Owns reports whether an image reference is a build Pando pushed there.
func (r *Registry) Owns(ref string) bool { return r != nil && r.a.Owns(ref) }

// Auth is the credential for one push or pull. Nil for anonymous.
func (r *Registry) Auth(ctx context.Context) (*api.RegistryAuth, error) {
	if r == nil {
		return nil, nil
	}
	return r.a.PullAuth(ctx)
}

// Target is where one build is pushed, with the credential for it.
func (r *Registry) Target(ctx context.Context, appID, workload, deploymentID string) (*api.PushTarget, error) {
	if r == nil {
		return nil, errs.New(errs.Internal, "No image registry is configured.")
	}
	t, err := r.a.Target(ctx, appID, workload, deploymentID)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteApp deletes every manifest of a deleted app's builds (R-224).
func (r *Registry) DeleteApp(ctx context.Context, appID string, workloads []string) (int, error) {
	if r == nil {
		return 0, nil
	}
	return r.a.DeleteApp(ctx, appID, workloads)
}

// Provider answers the registry in effect now. *Service is one; Static is one
// that never changes.
type Provider interface {
	Current(ctx context.Context) (*Registry, error)
}

type static struct{ r *Registry }

func (s static) Current(context.Context) (*Registry, error) { return s.r, nil }

// Static is a Provider that always answers r.
func Static(r *Registry) Provider { return static{r} }

// ConfigStore lists stored adapters (state.Adapters).
type ConfigStore interface {
	List(ctx context.Context) ([]state.AdapterConfig, error)
}

// CredentialStore opens an adapter's sealed credentials
// (state.AdapterCredentials).
type CredentialStore interface {
	Resolve(ctx context.Context, adapterID string) (map[string]secret.Value, error)
}

// Declared is a registry adapter the startup configuration declares, already
// configured.
type Declared struct {
	ID        string
	Adapter   api.ImageRegistryAdapter
	IsDefault bool
}

// Service builds the install's image registry adapter.
type Service struct {
	Configs     ConfigStore
	Credentials CredentialStore

	// New returns an unconfigured adapter of a kind, or nil.
	New func(kind string) api.ImageRegistryAdapter

	// Declared are the registries the startup configuration declares. A row
	// in adapter_configs with the same ID is overridden by it.
	Declared []Declared
}

// candidate is one registry the install could push to.
type candidate struct {
	id        string
	isDefault bool
	row       state.AdapterConfig
	adapter   api.ImageRegistryAdapter
}

func (s *Service) candidates(ctx context.Context) ([]candidate, error) {
	var out []candidate
	declared := map[string]bool{}
	declaredDefault := false
	for _, d := range s.Declared {
		declared[d.ID] = true
		declaredDefault = declaredDefault || d.IsDefault
		out = append(out, candidate{id: d.ID, isDefault: d.IsDefault, adapter: d.Adapter})
	}
	if s.Configs == nil {
		return out, nil
	}
	rows, err := s.Configs.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Category != string(api.CategoryImageRegistry) || !row.Enabled || declared[row.ID] {
			continue
		}
		// A declared default overrides a stored one (R-271), as it does in
		// every other category.
		out = append(out, candidate{id: row.ID, isDefault: row.IsDefault && !declaredDefault, row: row})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// Current is the registry in effect now: nil when none is configured. One
// that cannot be built is an error that says why, never "none": a registry
// the operator configured and Pando silently ignored would send builds
// nowhere.
func (s *Service) Current(ctx context.Context) (*Registry, error) {
	if s == nil {
		return nil, nil
	}
	all, err := s.candidates(ctx)
	if err != nil {
		return nil, err
	}
	var chosen *candidate
	for i := range all {
		if all[i].isDefault {
			chosen = &all[i]
			break
		}
	}
	if chosen == nil {
		switch len(all) {
		case 0:
			return nil, nil
		case 1:
			chosen = &all[0]
		default:
			return nil, errs.Newf(errs.ValidInvalid,
				"This install has %d image registry adapters and none is the default, so Pando cannot tell which one to push builds to.", len(all)).
				WithRemedy("Make one of them the default under System → Adapters, or turn off the others.")
		}
	}
	if chosen.adapter != nil {
		return Of(chosen.id, chosen.adapter), nil
	}
	a, err := s.build(ctx, chosen.row)
	if err != nil {
		return nil, err
	}
	return Of(chosen.id, a), nil
}

// CurrentRegistry is Current as the planner asks it (planner.InstallRegistries).
// The planner cannot name this package's type: state imports the planner.
func (s *Service) CurrentRegistry(ctx context.Context) (planner.InstallRegistry, error) {
	r, err := s.Current(ctx)
	if err != nil {
		return nil, err
	}
	return r, nil
}

// Adapter builds one registry adapter by ID, declared or stored, for the
// adapters screen's health and capabilities.
func (s *Service) Adapter(ctx context.Context, id string) (api.ImageRegistryAdapter, error) {
	for _, d := range s.Declared {
		if d.ID == id {
			return d.Adapter, nil
		}
	}
	if s.Configs != nil {
		rows, err := s.Configs.List(ctx)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if row.ID == id && row.Category == string(api.CategoryImageRegistry) {
				return s.build(ctx, row)
			}
		}
	}
	return nil, errs.Newf(errs.NotFound, "There is no image registry adapter %q on this installation.", id)
}

// build configures a registry from its row and its sealed credentials. The
// adapter's own refusal is kept, never the credential: an adapter's Configure
// error is held to R-194 like every other.
func (s *Service) build(ctx context.Context, row state.AdapterConfig) (api.ImageRegistryAdapter, error) {
	unusable := func(why string) error {
		return errs.Newf(errs.AdapterUnavailable, "The image registry adapter %q cannot be used. %s", row.ID, why).
			WithDetail("adapter", row.ID).
			WithRemedy("Change its settings under System → Adapters, or with pando adapter add.")
	}
	var a api.ImageRegistryAdapter
	if s.New != nil {
		a = s.New(row.Kind)
	}
	if a == nil {
		return nil, unusable("This build of Pando has no image registry adapter of kind " + row.Kind + ".")
	}
	var creds map[string]secret.Value
	if s.Credentials != nil {
		var err error
		if creds, err = s.Credentials.Resolve(ctx, row.ID); err != nil {
			return nil, unusable("Its credentials could not be opened. Enter them again in its settings.")
		}
	}
	raw, err := withCredentials(row.Config, creds)
	if err != nil {
		return nil, unusable("Its settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		if e := errs.As(err); e != nil {
			return nil, unusable(e.Message)
		}
		return nil, unusable("Its settings were refused.")
	}
	return a, nil
}

// Validate configures a registry as it would be saved — config and the
// credentials given, beside the ones already stored for id that are not
// replaced — and returns the adapter's refusal, if it has one, so a registry
// Pando cannot use is refused when it is saved rather than by every deploy
// after. A credential sent empty removes the stored one, as saving does.
func (s *Service) Validate(ctx context.Context, id, kind string, config json.RawMessage, given map[string]secret.Value) error {
	if s == nil || s.New == nil {
		return nil
	}
	a := s.New(kind)
	if a == nil {
		return errs.Newf(errs.ValidInvalid, "This build of Pando has no image registry adapter of kind %q.", kind)
	}
	creds := map[string]secret.Value{}
	if s.Credentials != nil && id != "" {
		if stored, err := s.Credentials.Resolve(ctx, id); err == nil {
			for k, v := range stored {
				creds[k] = v
			}
		}
	}
	for k, v := range given {
		if v.IsZero() {
			delete(creds, k)
			continue
		}
		creds[k] = v
	}
	raw, err := withCredentials(config, creds)
	if err != nil {
		return errs.New(errs.ValidInvalid, "The image registry's settings could not be read.")
	}
	if err := a.Configure(ctx, raw); err != nil {
		if errs.As(err) != nil {
			return err
		}
		return errs.New(errs.ValidInvalid, "The image registry's settings were refused.")
	}
	return nil
}

// withCredentials puts credentials where an adapter's Configure reads them,
// as cmd/pando does for adapters built at startup (O-20).
func withCredentials(raw json.RawMessage, creds map[string]secret.Value) (json.RawMessage, error) {
	cfg := map[string]json.RawMessage{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &cfg); err != nil {
			return nil, err
		}
	}
	plain := make(map[string]string, len(creds))
	for field, v := range creds {
		plain[field] = v.Reveal()
	}
	body, err := json.Marshal(plain)
	if err != nil {
		return nil, err
	}
	cfg["credentials"] = body
	return json.Marshal(cfg)
}
