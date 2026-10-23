package address

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// TestR162_AnAddressCanBeInAnyOfTheAdaptersDomains: an adapter serving several
// domains — a Cloudflare adapter with more than one zone — keeps the first as
// the default, and an app moves into another by naming it, by a single label
// in it, or by a whole hostname. A hostname in none of them is refused when it
// is chosen, not when the route is written.
func TestR162_AnAddressCanBeInAnyOfTheAdaptersDomains(t *testing.T) {
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("rte_cf", routingStub{caps: api.RoutingCapabilities{
		Modes: []spec.RoutingMode{spec.RoutingSubdomain}, DefaultMode: spec.RoutingSubdomain,
		BaseDomain: "bemeek.io", Domains: []string{"bemeek.io", "trypando.ai"}}}))
	s := &Service{Registry: reg}
	ctx := context.Background()
	current := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain,
		ModeSource: spec.ModeFromAdapterDefault, Hostname: "notes.bemeek.io"}

	for req, want := range map[Request]string{
		{Domain: "trypando.ai"}:                               "notes.trypando.ai",
		{Domain: "TryPando.ai."}:                              "notes.trypando.ai",
		{Domain: "trypando.ai", Hostname: "docs"}:             "docs.trypando.ai",
		{Domain: "trypando.ai", Hostname: "Docs.trypando.ai"}: "docs.trypando.ai",
		{Hostname: "docs.trypando.ai"}:                        "docs.trypando.ai",
		{Domain: "bemeek.io"}:                                 "notes.bemeek.io",
	} {
		d, err := s.Resolve(ctx, "app_1", "notes", current, req)
		require.NoError(t, err, req)
		require.Equal(t, want, d.Routing.Hostname, req)
	}

	var e *errs.Error
	_, err := s.Resolve(ctx, "app_1", "notes", current, Request{Domain: "example.org"})
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "bemeek.io, trypando.ai")

	_, err = s.Resolve(ctx, "app_1", "notes", current, Request{Domain: "trypando.ai", Hostname: "docs.bemeek.io"})
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "isn't in the domain trypando.ai")

	_, err = s.Resolve(ctx, "app_1", "notes", current, Request{Hostname: "docs.example.org"})
	require.ErrorAs(t, err, &e)
	require.Contains(t, e.Message, "It serves bemeek.io, trypando.ai")

	opts := s.Options(ctx)
	require.Len(t, opts, 1)
	require.Equal(t, []string{"bemeek.io", "trypando.ai"}, opts[0].Domains)
}

// An adapter that names no domains takes its base domain as the only one.
func TestADomainIsCheckedAgainstTheBaseDomainAlone(t *testing.T) {
	s, _ := service(t)
	d, err := s.Resolve(context.Background(), "app_1", "notes", onLoopback, Request{AdapterRef: "rte_cf", Domain: "bemeek.io"})
	require.NoError(t, err)
	require.Equal(t, "notes.bemeek.io", d.Routing.Hostname)
	_, err = s.Resolve(context.Background(), "app_1", "notes", onLoopback, Request{AdapterRef: "rte_cf", Domain: "trypando.ai"})
	require.ErrorContains(t, err, "doesn't serve the domain trypando.ai")
}

// TestR162_ASpecWrittenWholeIsHeldToTheAdaptersDomains: a hostname that
// arrives in a whole spec rather than through Resolve is held to the domains
// the adapter serves too. One the app already has is not asked about again.
func TestR162_ASpecWrittenWholeIsHeldToTheAdaptersDomains(t *testing.T) {
	reg := api.NewRegistry()
	require.NoError(t, reg.Register("rte_cf", routingStub{caps: api.RoutingCapabilities{
		Modes: []spec.RoutingMode{spec.RoutingSubdomain}, DefaultMode: spec.RoutingSubdomain,
		BaseDomain: "bemeek.io", Domains: []string{"bemeek.io", "trypando.ai"}}}))
	s := &Service{Registry: reg}
	ctx := context.Background()
	current := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.bemeek.io"}

	in := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "Docs.TryPando.ai."}
	_, err := s.Overrides(ctx, &current, &in)
	require.NoError(t, err)

	out := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.example.org"}
	_, err = s.Overrides(ctx, &current, &out)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err))
	require.Contains(t, errs.As(err).Message, "It serves bemeek.io, trypando.ai")
	_, err = s.Overrides(ctx, nil, &out)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(err), "a first spec is held to them too")

	kept := spec.Routing{AdapterRef: "rte_cf", Mode: spec.RoutingSubdomain, Hostname: "notes.example.org"}
	_, err = s.Overrides(ctx, &kept, &out)
	require.NoError(t, err, "a hostname the app already has is left alone")
}
