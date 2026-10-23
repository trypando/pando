// Package address changes where a configured app is reached (R-162, R-163,
// R-165).
//
// An app's address is its spec's routing block, and changing it is a spec
// revision like any other edit (R-152). What this package adds is the part
// that is not a plain field edit: which modes the chosen routing adapter
// serves, what the hostname, path or port defaults to under it, and whether
// the result deviates from the adapter's default mode — which R-163 gates
// behind app.routing.override. The HTTP handler asks the authorizer; the
// decision of what needs asking is made here, once, for every surface.
package address

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Ports allocates a host port for port-mode routing (design 03 §4.2).
// Idempotent: an app that holds a port on an adapter gets the same one back.
type Ports interface {
	Allocate(ctx context.Context, adapterRef, appID string, from, to int) (int, error)
}

// Taken reports whether another app already holds an address.
type Taken interface {
	CheckAddress(ctx context.Context, appID string, r spec.Routing) error
}

// Service resolves routing changes against the configured routing adapters.
type Service struct {
	Registry *api.Registry
	Ports    Ports
	Taken    Taken

	// PortRangeStart and PortRangeEnd are the install's port range.
	PortRangeStart, PortRangeEnd int

	// BaseDomain is the install's server.base_domain, used when the routing
	// adapter has no domain of its own.
	BaseDomain string
}

// Request is the address somebody asked for. Empty fields take the adapter's
// default: its default mode, and the hostname, path or port that mode implies.
// A port is always allocated, never named.
type Request struct {
	AdapterRef string           `json:"adapter_ref,omitempty"`
	Mode       spec.RoutingMode `json:"mode,omitempty"`
	Hostname   string           `json:"hostname,omitempty"`
	PathPrefix string           `json:"path_prefix,omitempty"`

	// Domain is the domain a subdomain address is made in, when the adapter
	// serves more than one: one of its Domains, such as a second Cloudflare
	// zone. With no hostname the app is <slug>.<domain>; with a single label
	// such as "docs" it is docs.<domain>. Empty is the adapter's base domain.
	Domain string `json:"domain,omitempty"`
}

// Decision is a resolved routing change.
type Decision struct {
	Routing spec.Routing

	// Changed is whether it differs from the current routing. A change moves
	// the app: the old address stops working, and bookmarks to it break
	// (R-165), which is why the caller asks for confirmation.
	Changed bool

	// Override is whether the mode deviates from the adapter's default, which
	// needs app.routing.override (R-163). Keeping a deviation an app already
	// has is not a new one and is not asked about again.
	Override bool
}

// Resolve works out the routing an app would have under req.
func (s *Service) Resolve(ctx context.Context, appID, slug string, current spec.Routing, req Request) (Decision, error) {
	ref := strings.TrimSpace(req.AdapterRef)
	if ref == "" {
		ref = current.AdapterRef
	}
	if ref == "" && s.Registry != nil {
		ref, _ = s.Registry.Default(api.CategoryRouting)
	}
	routing, ok := s.routing(ref)
	if !ok {
		return Decision{}, errs.Newf(errs.ValidInvalid, "There is no routing adapter %q on this installation.", ref).
			WithRemedy("Choose one of the routing adapters listed under Installation, Adapters.")
	}
	caps, err := routing.Capabilities(ctx)
	if err != nil {
		return Decision{}, errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not ask the %s routing adapter what it can do.", ref), err)
	}

	mode := req.Mode
	if mode == "" {
		// Staying on the same adapter keeps the mode; moving to another takes
		// its default, since the old mode may be one it does not serve.
		if ref == current.AdapterRef && current.Mode != "" {
			mode = current.Mode
		} else {
			mode = caps.DefaultMode
		}
	}
	if !caps.Supports(mode) {
		return Decision{}, errs.Newf(errs.PlanCapabilityUnsupported,
			"The %s routing adapter does not serve apps %s.", ref, describeMode(mode)).
			WithRemedy("Choose " + describeModes(caps.Modes) + ", or another routing adapter.")
	}

	next := spec.Routing{AdapterRef: ref, Mode: mode, ModeSource: spec.ModeFromAdapterDefault}
	if mode != caps.DefaultMode {
		next.ModeSource = spec.ModeFromUserOverride
	}

	// The address the mode implies: what was asked for, or what the app
	// already has in that mode, or the default for its name.
	same := ref == current.AdapterRef && mode == current.Mode
	switch mode {
	case spec.RoutingSubdomain:
		host, err := s.hostname(ref, slug, caps, req, current, same)
		if err != nil {
			return Decision{}, err
		}
		probe := spec.AppSpec{Routing: next}
		if err := spec.SetHostname(&probe, host); err != nil {
			return Decision{}, err
		}
		next.Hostname = probe.Routing.Hostname
		if err := checkServed(next.Hostname, ref, caps); err != nil {
			return Decision{}, err
		}

	case spec.RoutingPath:
		// What was asked for, or the path the app already has, or its slug.
		// The proxy finds a path-mode app by its path (proxy.resolve), so any
		// path that passes CheckPathPrefix and is free is served.
		prefix := ""
		if strings.TrimSpace(req.PathPrefix) != "" {
			prefix = spec.NormalizePathPrefix(req.PathPrefix)
		}
		if prefix == "" && same {
			prefix = current.PathPrefix
		}
		if prefix == "" {
			prefix = "/" + slug
		}
		if err := spec.CheckPathPrefix(prefix); err != nil {
			return Decision{}, err
		}
		next.PathPrefix = prefix

	case spec.RoutingPort:
		// Allocated, never typed: two apps must not be handed one port, and
		// the allocation is what guarantees it (design 03 §4.2).
		if same && current.Port > 0 {
			next.Port = current.Port
			break
		}
		if s.Ports == nil {
			return Decision{}, errs.New(errs.Internal, "Pando cannot assign ports here.")
		}
		port, err := s.Ports.Allocate(ctx, ref, appID, s.PortRangeStart, s.PortRangeEnd)
		if err != nil {
			return Decision{}, err
		}
		next.Port = port
	}

	changed := !equal(current, next)
	if changed && s.Taken != nil {
		// Said now, to the person choosing, rather than at the pin a deploy
		// makes later — where it is also checked, and decided (state.Pin).
		if err := s.Taken.CheckAddress(ctx, appID, next); err != nil {
			return Decision{}, err
		}
	}

	return Decision{
		Routing: next,
		Changed: changed,
		// Asked only when the change introduces the deviation: an app already
		// on a non-default mode that only changes its path is not deviating
		// anew.
		Override: next.ModeSource == spec.ModeFromUserOverride &&
			(current.AdapterRef != next.AdapterRef || current.Mode != next.Mode),
	}, nil
}

// checkServed refuses a hostname outside every domain the adapter names, when
// it names any.
func checkServed(host, ref string, caps api.RoutingCapabilities) error {
	if len(caps.Domains) == 0 || inDomain(strings.TrimSuffix(strings.ToLower(host), "."), caps.Domains) != "" {
		return nil
	}
	return errs.Newf(errs.ValidInvalid,
		"%s isn't in a domain the %s routing adapter serves. It serves %s.",
		host, ref, strings.Join(caps.Domains, ", ")).
		WithRemedy("Choose a hostname ending in one of those domains, or add the domain to the routing adapter's settings.")
}

// Overrides reports whether next deviates from its adapter's default mode
// where current did not — for a spec written whole, through POST /specs,
// rather than through Resolve. It also corrects next's ModeSource, which a
// hand-written spec may have left saying anything.
//
// A hostname new to the app is held to the domains the adapter serves, as
// Resolve holds it: a spec written whole is not a way around that. One the
// app already has is left alone, so an adapter's domains changing later does
// not stop the app's next edit.
func (s *Service) Overrides(ctx context.Context, current *spec.Routing, next *spec.Routing) (bool, error) {
	routing, ok := s.routing(next.AdapterRef)
	if !ok {
		// Not ours to refuse: validation and the planner name an unknown
		// adapter in their own words.
		return false, nil
	}
	caps, err := routing.Capabilities(ctx)
	if err != nil {
		return false, errs.Wrap(errs.AdapterUnavailable,
			fmt.Sprintf("Pando could not ask the %s routing adapter what it can do.", next.AdapterRef), err)
	}
	newHost := current == nil || current.AdapterRef != next.AdapterRef || current.Hostname != next.Hostname
	if next.Mode == spec.RoutingSubdomain && newHost {
		if err := checkServed(next.Hostname, next.AdapterRef, caps); err != nil {
			return false, err
		}
	}
	if next.Mode == caps.DefaultMode || next.Mode == "" {
		next.ModeSource = spec.ModeFromAdapterDefault
		return false, nil
	}
	next.ModeSource = spec.ModeFromUserOverride
	if current != nil && current.AdapterRef == next.AdapterRef && current.Mode == next.Mode {
		return false, nil
	}
	return true, nil
}

// Option is a routing adapter an app can be moved to, and what it offers.
type Option struct {
	AdapterRef  string             `json:"adapter_ref"`
	Modes       []spec.RoutingMode `json:"modes"`
	DefaultMode spec.RoutingMode   `json:"default_mode"`
	BaseDomain  string             `json:"base_domain,omitempty"`
	Domains     []string           `json:"domains,omitempty"`
	IsDefault   bool               `json:"is_default"`
}

// Options lists the routing adapters an app's address can use. One that
// cannot say what it serves is left out rather than offered and refused.
func (s *Service) Options(ctx context.Context) []Option {
	if s.Registry == nil {
		return nil
	}
	def, _ := s.Registry.Default(api.CategoryRouting)
	var out []Option
	for _, ref := range s.Registry.ByCategory(api.CategoryRouting) {
		routing, ok := s.Registry.Routing(ref)
		if !ok {
			continue
		}
		caps, err := routing.Capabilities(ctx)
		if err != nil {
			continue
		}
		base := caps.BaseDomain
		if base == "" {
			base = s.BaseDomain
		}
		out = append(out, Option{AdapterRef: ref, Modes: caps.Modes, DefaultMode: caps.DefaultMode,
			BaseDomain: base, Domains: caps.Domains, IsDefault: ref == def})
	}
	return out
}

// hostname is the hostname a subdomain address asks for: the one named, made
// in the domain named, or the app's current one, or <slug>.<base domain>.
func (s *Service) hostname(ref, slug string, caps api.RoutingCapabilities, req Request, current spec.Routing, same bool) (string, error) {
	host := strings.TrimSpace(req.Hostname)
	domain := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(req.Domain), "."))
	if domain == "" {
		if host == "" && same {
			return current.Hostname, nil
		}
		if host != "" {
			return host, nil
		}
		domain = caps.BaseDomain
		if domain == "" {
			domain = s.BaseDomain
		}
		if domain == "" {
			return "", errs.New(errs.ValidInvalid, "This app needs a hostname, and there is no base domain to make one from.").
				WithRemedy("Enter the hostname, such as notes.example.com.")
		}
		return slug + "." + domain, nil
	}

	served := caps.Domains
	if len(served) == 0 && caps.BaseDomain != "" {
		served = []string{caps.BaseDomain}
	}
	if len(served) > 0 && !slices.Contains(served, domain) {
		return "", errs.Newf(errs.ValidInvalid, "The %s routing adapter doesn't serve the domain %s. It serves %s.",
			ref, domain, strings.Join(served, ", ")).
			WithRemedy("Choose one of those domains, or add " + domain + " to the routing adapter's settings.")
	}
	name := strings.ToLower(strings.TrimSuffix(host, "."))
	switch {
	case name == "":
		return slug + "." + domain, nil
	case !strings.Contains(name, "."):
		// A single label, named in the domain.
		return name + "." + domain, nil
	case inDomain(name, []string{domain}) == "":
		return "", errs.Newf(errs.ValidInvalid, "%s isn't in the domain %s.", host, domain).
			WithRemedy("Give a hostname ending in " + domain + ", or only the name before it, such as " + slug + ".")
	}
	return host, nil
}

// inDomain is the longest of domains that host is, or is a name under; empty
// when it is in none.
func inDomain(host string, domains []string) string {
	best := ""
	for _, d := range domains {
		if (host == d || strings.HasSuffix(host, "."+d)) && len(d) > len(best) {
			best = d
		}
	}
	return best
}

func (s *Service) routing(ref string) (api.RoutingAdapter, bool) {
	if s.Registry == nil || ref == "" {
		return nil, false
	}
	return s.Registry.Routing(ref)
}

func equal(a, b spec.Routing) bool {
	return a.AdapterRef == b.AdapterRef && a.Mode == b.Mode &&
		a.Hostname == b.Hostname && a.PathPrefix == b.PathPrefix && a.Port == b.Port
}

func describeMode(m spec.RoutingMode) string {
	switch m {
	case spec.RoutingSubdomain:
		return "at their own hostname"
	case spec.RoutingPath:
		return "under a path"
	case spec.RoutingPort:
		return "on their own port"
	}
	return fmt.Sprintf("in %q mode", m)
}

func describeModes(modes []spec.RoutingMode) string {
	parts := make([]string, 0, len(modes))
	for _, m := range modes {
		parts = append(parts, describeMode(m))
	}
	return strings.Join(parts, " or ")
}
