package api

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/trypando/pando/internal/core/spec"
)

// Registry holds configured adapter instances.
//
// Registration happens in main at startup, from compiled-in packages (R-253).
// There is no plugin protocol and none is planned — these are ordinary Go
// values, and a "registry" here is a map, not a discovery mechanism.
type Registry struct {
	mu       sync.RWMutex
	byRef    map[string]Adapter
	byCat    map[Category][]string
	defaults map[Category]string

	// assignments is which AI adapter performs each function (R-259).
	assignments map[AIFunction]AIAssignment
}

func NewRegistry() *Registry {
	return &Registry{
		byRef:    map[string]Adapter{},
		byCat:    map[Category][]string{},
		defaults: map[Category]string{},

		assignments: map[AIFunction]AIAssignment{},
	}
}

// Register adds a configured adapter instance under a reference.
//
// The reference is the string the spec names (R-251): rt_docker_local, never
// Docker arguments.
func (r *Registry) Register(ref string, a Adapter) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.byRef[ref]; exists {
		return fmt.Errorf("adapter %q is already registered", ref)
	}

	// An adapter that does not satisfy its category's interface is rejected
	// here, loudly, rather than at the first lookup.
	//
	// Without this the typed accessors below return (nil, false) on a type
	// mismatch, which is indistinguishable from "not configured" — so adding a
	// method to an interface would surface, much later, as a planner error
	// telling an operator their runtime is not configured when in fact it is.
	if err := satisfiesCategory(a); err != nil {
		return fmt.Errorf("adapter %q: %w", ref, err)
	}
	r.byRef[ref] = a
	r.byCat[a.Category()] = append(r.byCat[a.Category()], ref)
	sort.Strings(r.byCat[a.Category()])
	return nil
}

// SetDefault marks a reference as its category's default.
//
// One default per category, matching the unique index on adapter_configs.
func (r *Registry) SetDefault(c Category, ref string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	a, ok := r.byRef[ref]
	if !ok {
		return fmt.Errorf("cannot default to unregistered adapter %q", ref)
	}
	if a.Category() != c {
		return fmt.Errorf("adapter %q is a %s adapter, not %s", ref, a.Category(), c)
	}
	r.defaults[c] = ref
	return nil
}

// satisfiesCategory checks an adapter against the interface its category
// requires.
func satisfiesCategory(a Adapter) error {
	var ok bool
	switch a.Category() {
	case CategoryRuntime:
		_, ok = a.(RuntimeAdapter)
	case CategoryRouting:
		_, ok = a.(RoutingAdapter)
	case CategoryBuilder:
		_, ok = a.(BuilderAdapter)
	case CategorySecrets:
		_, ok = a.(SecretsAdapter)
	case CategoryServices:
		_, ok = a.(ServicesAdapter)
	case CategoryIdentity:
		_, ok = a.(IdentityAdapter)
	case CategoryNotify:
		_, ok = a.(NotifyAdapter)
	case CategoryBackup:
		_, ok = a.(BackupAdapter)
	case CategoryScanner:
		_, ok = a.(ScannerAdapter)
	case CategoryAI:
		_, ok = a.(AIAdapter)
	case CategorySource:
		_, ok = a.(SourceAdapter)
	default:
		return fmt.Errorf("unknown category %q", a.Category())
	}
	if !ok {
		return fmt.Errorf("does not implement the %s adapter interface", a.Category())
	}
	return nil
}

// Get returns an adapter by reference.
func (r *Registry) Get(ref string) (Adapter, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	a, ok := r.byRef[ref]
	return a, ok
}

// ByCategory returns the references registered in a category.
func (r *Registry) ByCategory(c Category) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return append([]string(nil), r.byCat[c]...)
}

// Default returns a category's default reference.
func (r *Registry) Default(c Category) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	ref, ok := r.defaults[c]
	return ref, ok
}

// Refs returns every registered reference.
func (r *Registry) Refs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.byRef))
	for ref := range r.byRef {
		out = append(out, ref)
	}
	sort.Strings(out)
	return out
}

// Runtime returns a runtime adapter by reference.
//
// The typed accessors exist so callers do not type-assert at every call site.
// This is not the capability discovery R-254 forbids — which adapter you have is
// a different question from what it can do, and the latter is always a
// Capabilities() call.
func (r *Registry) Runtime(ref string) (RuntimeAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	rt, ok := a.(RuntimeAdapter)
	return rt, ok
}

// Routing returns a routing adapter by reference.
func (r *Registry) Routing(ref string) (RoutingAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	rt, ok := a.(RoutingAdapter)
	return rt, ok
}

// Builder returns a builder adapter by reference.
func (r *Registry) Builder(ref string) (BuilderAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	b, ok := a.(BuilderAdapter)
	return b, ok
}

// Scanner returns a scanner adapter by reference (R-317).
func (r *Registry) Scanner(ref string) (ScannerAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	s, ok := a.(ScannerAdapter)
	return s, ok
}

// DefaultScanner returns the configured scanner, if there is one.
//
// An installation with none has no scores, and a security threshold set on it
// is inert (R-317). That is a state the console has to be able to report, so
// "is there a scanner" is a question with an answer rather than a nil check at
// every call site.
func (r *Registry) DefaultScanner() (ScannerAdapter, string, bool) {
	ref, ok := r.Default(CategoryScanner)
	if !ok {
		refs := r.ByCategory(CategoryScanner)
		if len(refs) == 0 {
			return nil, "", false
		}
		ref = refs[0]
	}
	s, ok := r.Scanner(ref)
	if !ok {
		return nil, "", false
	}
	return s, ref, true
}

// Secrets returns a secrets adapter by reference.
func (r *Registry) Secrets(ref string) (SecretsAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	s, ok := a.(SecretsAdapter)
	return s, ok
}

// Services returns a services adapter by reference (R-131).
func (r *Registry) Services(ref string) (ServicesAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	s, ok := a.(ServicesAdapter)
	return s, ok
}

// ServicesFor returns the configured services adapter that can fill a slot of
// this type, preferring the category default.
//
// Type-directed rather than ref-directed because a slot says what it needs, not
// who should supply it: an app declaring REDIS_URL has no opinion about which
// adapter stands a Redis up, and asking the user to pick one would be asking
// them to answer a question they cannot have information about.
func (r *Registry) ServicesFor(t spec.SlotType) (ServicesAdapter, string, bool) {
	refs := r.ByCategory(CategoryServices)
	if def, ok := r.Default(CategoryServices); ok {
		refs = append([]string{def}, refs...)
	}
	for _, ref := range refs {
		sa, ok := r.Services(ref)
		if !ok {
			continue
		}
		for _, supported := range sa.Supports() {
			if supported == t {
				return sa, ref, true
			}
		}
	}
	return nil, "", false
}

// Notify returns a notification adapter by reference (R-231).
func (r *Registry) Notify(ref string) (NotifyAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	n, ok := a.(NotifyAdapter)
	return n, ok
}

// Backup returns a backup adapter by reference (R-217).
func (r *Registry) Backup(ref string) (BackupAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	b, ok := a.(BackupAdapter)
	return b, ok
}

// AI returns an AI adapter by reference (R-258).
func (r *Registry) AI(ref string) (AIAdapter, bool) {
	a, ok := r.Get(ref)
	if !ok {
		return nil, false
	}
	ai, ok := a.(AIAdapter)
	return ai, ok
}

// AIAssignment is which adapter performs one AI function, and on which model
// (R-259). Model is empty to use the adapter's own.
type AIAssignment struct {
	Function   AIFunction `json:"function"`
	AdapterRef string     `json:"adapter_id"`
	Model      string     `json:"model,omitempty"`
}

// SetAIAssignments replaces every assignment. Called at startup, and after
// each change, with what the database and the startup configuration say
// together; checking an assignment is core's job, not the registry's.
func (r *Registry) SetAIAssignments(assignments []AIAssignment) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.assignments = make(map[AIFunction]AIAssignment, len(assignments))
	for _, a := range assignments {
		r.assignments[a.Function] = a
	}
}

// AIAssignments returns every assignment, in AIFunctions order.
func (r *Registry) AIAssignments() []AIAssignment {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]AIAssignment, 0, len(r.assignments))
	for _, fn := range AIFunctions() {
		if a, ok := r.assignments[fn]; ok {
			out = append(out, a)
		}
	}
	return out
}

// AIFor returns the adapter assigned a function, and the assignment.
//
// Returning found=false is an ordinary outcome, not a failure. R-335: a
// function nobody is assigned is off, and every caller of this has to be
// written that way. An assignment naming an adapter that is not running — it
// failed to configure, or was removed — is off in the same way.
func (r *Registry) AIFor(fn AIFunction) (AIAdapter, AIAssignment, bool) {
	r.mu.RLock()
	a, ok := r.assignments[fn]
	r.mu.RUnlock()
	if !ok {
		return nil, AIAssignment{}, false
	}
	ai, ok := r.AI(a.AdapterRef)
	if !ok {
		return nil, a, false
	}
	return ai, a, true
}

// HealthCheckAll reports which adapters are unhealthy, by reference.
//
// The planner uses this to refuse to plan against an unhealthy adapter and
// return ADAPTER_UNAVAILABLE, rather than failing mid-deploy (R-254).
func (r *Registry) HealthCheckAll(ctx context.Context) map[string]error {
	r.mu.RLock()
	snapshot := make(map[string]Adapter, len(r.byRef))
	for ref, a := range r.byRef {
		snapshot[ref] = a
	}
	r.mu.RUnlock()

	out := map[string]error{}
	for ref, a := range snapshot {
		if err := a.HealthCheck(ctx); err != nil {
			out[ref] = err
		}
	}
	return out
}
