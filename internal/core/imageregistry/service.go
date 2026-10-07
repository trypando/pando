package imageregistry

import (
	"context"
	"sort"
	"time"

	"github.com/trypando/pando/internal/core/planner"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// The install registry as both startup configuration and a stored setting the
// console and the API change (issue #72, PR 5). Startup configuration wins field
// by field and is shown as fixed (R-271), as host policy's startup fields are.
//
// Nothing is cached. Every push, pull and plan reads the stored row and opens
// the stored password, so a credential rotated through one replica is what
// every replica uses next, with no restart and nothing to watch.

// Source is where a startup value was set, as the config package reports it.
type Source struct {
	Kind string `json:"kind"`
	Name string `json:"name,omitempty"`
	Key  string `json:"key,omitempty"`
}

// Fields, as the API and the startup configuration name them.
const (
	FieldURL      = "url"
	FieldUsername = "username"
	FieldPassword = "password"
	FieldKind     = "kind"
	FieldLayout   = "layout"
	FieldInsecure = "insecure"
	FieldAlways   = "always"
)

// Store is the stored registry (state.InstallRegistry).
type Store interface {
	Load(ctx context.Context) (state.StoredRegistry, bool, error)
	Save(ctx context.Context, r state.StoredRegistry, by string) error
	Clear(ctx context.Context) error
	SetPassword(ctx context.Context, v secret.Value) error
	Password(ctx context.Context) (secret.Value, bool, error)
}

// Service resolves the registry in effect and changes the stored one.
type Service struct {
	// Startup is the startup configuration, and Fixed the fields it sets.
	Startup Config
	Fixed   map[string]Source

	// Store is nil on an install that only reads startup configuration.
	Store Store
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

// Current is the registry in effect now: nil when none is configured.
func (s *Service) Current(ctx context.Context) (*Registry, error) {
	if s == nil {
		return nil, nil
	}
	c, _, err := s.effective(ctx, true)
	if err != nil {
		return nil, err
	}
	return New(c)
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

func (s *Service) fixed(field string) bool {
	_, ok := s.Fixed[field]
	return ok
}

// effective lays the startup fields over the stored ones. withPassword opens
// the stored password; without it, passwordSet still says whether one exists.
func (s *Service) effective(ctx context.Context, withPassword bool) (Config, bool, error) {
	c := Config{}
	var stored state.StoredRegistry
	var have bool
	if s.Store != nil {
		var err error
		if stored, have, err = s.Store.Load(ctx); err != nil {
			return Config{}, false, err
		}
	}
	if have {
		c = Config{URL: stored.URL, Username: stored.Username, Kind: stored.Kind, Layout: stored.Layout,
			Insecure: stored.Insecure, Always: stored.Always}
	}
	st := s.Startup
	if s.fixed(FieldURL) {
		c.URL = st.URL
	}
	if s.fixed(FieldUsername) {
		c.Username = st.Username
	}
	if s.fixed(FieldKind) {
		c.Kind = st.Kind
	}
	if s.fixed(FieldLayout) {
		c.Layout = st.Layout
	}
	if s.fixed(FieldInsecure) {
		c.Insecure = st.Insecure
	}
	if s.fixed(FieldAlways) {
		c.Always = st.Always
	}

	passwordSet := false
	if s.fixed(FieldPassword) {
		c.Password = st.Password
		passwordSet = !st.Password.IsZero()
	} else if s.Store != nil && c.URL != "" {
		v, ok, err := s.Store.Password(ctx)
		if err != nil {
			return Config{}, false, err
		}
		passwordSet = ok
		if withPassword {
			c.Password = v
		}
	}
	return c, passwordSet, nil
}

// Fixed is one field the startup configuration sets. Value is omitted for the
// password, which is never shown (R-194).
type Fixed struct {
	Key    string `json:"key"`
	Value  any    `json:"value,omitempty"`
	Source Source `json:"source"`
}

// View is what GET /registry shows: the registry in effect, which fields are
// fixed at startup, and whether a password is set — never the password.
type View struct {
	Configured  bool      `json:"configured"`
	URL         string    `json:"url"`
	Username    string    `json:"username"`
	Kind        string    `json:"kind"`
	Layout      string    `json:"layout"`
	Insecure    bool      `json:"insecure"`
	Always      bool      `json:"always"`
	PasswordSet bool      `json:"password_set"`
	Fixed       []Fixed   `json:"fixed"`
	UpdatedBy   string    `json:"updated_by,omitempty"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
}

// Describe is the registry in effect, without its password.
func (s *Service) Describe(ctx context.Context) (View, error) {
	c, passwordSet, err := s.effective(ctx, false)
	if err != nil {
		return View{}, err
	}
	v := View{
		Configured: c.URL != "", URL: c.URL, Username: c.Username,
		Kind: orDefault(c.Kind, "basic"), Layout: orDefault(c.Layout, string(LayoutPerApp)),
		Insecure: c.Insecure, Always: c.Always, PasswordSet: passwordSet, Fixed: []Fixed{},
	}
	if s.Store != nil {
		if stored, ok, err := s.Store.Load(ctx); err == nil && ok {
			v.UpdatedBy, v.UpdatedAt = stored.UpdatedBy, stored.UpdatedAt
		}
	}
	for key, src := range s.Fixed {
		f := Fixed{Key: key, Source: src}
		switch key {
		case FieldURL:
			f.Value = c.URL
		case FieldUsername:
			f.Value = c.Username
		case FieldKind:
			f.Value = v.Kind
		case FieldLayout:
			f.Value = v.Layout
		case FieldInsecure:
			f.Value = c.Insecure
		case FieldAlways:
			f.Value = c.Always
		}
		v.Fixed = append(v.Fixed, f)
	}
	sort.Slice(v.Fixed, func(i, j int) bool { return v.Fixed[i].Key < v.Fixed[j].Key })
	return v, nil
}

// Change is a PUT /registry body. An absent field is left as it is. A
// password of "" removes the stored one.
type Change struct {
	URL      *string       `json:"url,omitempty"`
	Username *string       `json:"username,omitempty"`
	Password *secret.Value `json:"password,omitempty"`
	Kind     *string       `json:"kind,omitempty"`
	Layout   *string       `json:"layout,omitempty"`
	Insecure *bool         `json:"insecure,omitempty"`
	Always   *bool         `json:"always,omitempty"`
}

// Changed names the fields a change sets, for the audit event. Never values.
func (c Change) Changed() []string {
	var out []string
	add := func(set bool, name string) {
		if set {
			out = append(out, name)
		}
	}
	add(c.URL != nil, FieldURL)
	add(c.Username != nil, FieldUsername)
	add(c.Password != nil, FieldPassword)
	add(c.Kind != nil, FieldKind)
	add(c.Layout != nil, FieldLayout)
	add(c.Insecure != nil, FieldInsecure)
	add(c.Always != nil, FieldAlways)
	return out
}

// Update applies a change to the stored registry, refusing a field fixed at
// startup to anything but its startup value, and a result Pando could not
// use. The password is sealed by the secrets adapter (R-190).
func (s *Service) Update(ctx context.Context, ch Change, by string) (View, error) {
	if s == nil || s.Store == nil {
		return View{}, errs.New(errs.StateInvalid, "Pando cannot store the install registry's settings on this installation.").
			WithRemedy("Set the registry in the startup configuration with PANDO_REGISTRY_URL and its credential instead.")
	}
	current, _, err := s.effective(ctx, true)
	if err != nil {
		return View{}, err
	}
	stored, _, err := s.Store.Load(ctx)
	if err != nil {
		return View{}, err
	}

	refuse := func(field string) error {
		src := s.Fixed[field]
		return errs.Newf(errs.ValidInvalid,
			"The registry's %s is set in the startup configuration (%s), so it cannot be changed here.", field, where(src)).
			WithRemedy("Change it there and restart Pando, or remove it there to manage it here.")
	}
	str := func(field string, in *string, now string, dst *string) error {
		if in == nil {
			return nil
		}
		if s.fixed(field) {
			if *in != now {
				return refuse(field)
			}
			return nil
		}
		*dst = *in
		return nil
	}
	boolean := func(field string, in *bool, now bool, dst *bool) error {
		if in == nil {
			return nil
		}
		if s.fixed(field) {
			if *in != now {
				return refuse(field)
			}
			return nil
		}
		*dst = *in
		return nil
	}
	for _, step := range []error{
		str(FieldURL, ch.URL, current.URL, &stored.URL),
		str(FieldUsername, ch.Username, current.Username, &stored.Username),
		str(FieldKind, ch.Kind, orDefault(current.Kind, "basic"), &stored.Kind),
		str(FieldLayout, ch.Layout, orDefault(current.Layout, string(LayoutPerApp)), &stored.Layout),
		boolean(FieldInsecure, ch.Insecure, current.Insecure, &stored.Insecure),
		boolean(FieldAlways, ch.Always, current.Always, &stored.Always),
	} {
		if step != nil {
			return View{}, step
		}
	}
	if ch.Password != nil && s.fixed(FieldPassword) {
		return View{}, refuse(FieldPassword)
	}

	// What would be in effect, checked before anything is written: a registry
	// saved that Pando cannot use fails every deploy instead of this request.
	next := Config{URL: stored.URL, Username: stored.Username, Kind: stored.Kind, Layout: stored.Layout,
		Insecure: stored.Insecure, Always: stored.Always, Password: current.Password}
	if ch.Password != nil {
		next.Password = *ch.Password
	}
	probe := &Service{Startup: s.Startup, Fixed: s.Fixed, Store: fixedStore{stored: stored, password: next.Password}}
	effective, _, err := probe.effective(ctx, true)
	if err != nil {
		return View{}, err
	}
	if _, err := New(effective); err != nil {
		return View{}, err
	}

	if err := s.Store.Save(ctx, stored, by); err != nil {
		return View{}, err
	}
	if ch.Password != nil {
		if err := s.Store.SetPassword(ctx, *ch.Password); err != nil {
			return View{}, err
		}
	}
	return s.Describe(ctx)
}

// Clear removes the stored registry. Startup configuration is untouched.
func (s *Service) Clear(ctx context.Context) (View, error) {
	if s == nil || s.Store == nil {
		return View{}, errs.New(errs.StateInvalid, "Pando stores no install registry settings on this installation.")
	}
	if err := s.Store.Clear(ctx); err != nil {
		return View{}, err
	}
	return s.Describe(ctx)
}

// fixedStore answers effective for a change not yet written.
type fixedStore struct {
	Store
	stored   state.StoredRegistry
	password secret.Value
}

func (f fixedStore) Load(context.Context) (state.StoredRegistry, bool, error) {
	return f.stored, true, nil
}
func (f fixedStore) Password(context.Context) (secret.Value, bool, error) {
	return f.password, !f.password.IsZero(), nil
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func where(src Source) string {
	switch src.Kind {
	case "env":
		return "environment variable " + src.Name
	case "file":
		return "config file " + src.Name + ", key " + src.Key
	}
	return "startup configuration"
}
