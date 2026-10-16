package main

import (
	"context"
	"net/url"
	"strings"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/source"
	"github.com/trypando/pando/internal/core/state"
)

// guardOwnAddresses keeps Pando's own addresses out of reach of what the API
// lets a person choose: no repository fetch may connect to Pando's port or its
// database (R-092), and no app may be reached at Pando's hostname (R-165).
func guardOwnAddresses(cfg *config.Config, registry *adapterapi.Registry, apps *state.Apps) {
	source.SetGuard(sourceGuard(cfg.Server.Addr, cfg.Database.URL))
	// Validated in config.Load, so an error was already reported there.
	if external, err := cfg.Server.External(); err == nil {
		apps.ReservedHostnames = reservedHostnames(registry, external)
	}
}

// reservedHostnames lists the hostnames Pando answers on itself, which no app
// may be reached at (2026-10-16 assessment, finding 2): the external URL's
// host, and each routing adapter's reserved hostnames — the console hostname
// a Traefik or Cloudflare edge serves Pando on, which on Cloudflare is the
// zone itself unless set. A base domain is not reserved for its own sake: an
// app may have the apex when the console is somewhere else. Asked each time
// an address is checked, because a routing adapter's settings can change
// while Pando runs.
func reservedHostnames(registry *adapterapi.Registry, external *url.URL) func(context.Context) []string {
	return func(ctx context.Context) []string {
		var out []string
		add := func(h string) {
			h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
			if h != "" {
				out = append(out, h)
			}
		}
		if external != nil {
			add(external.Hostname())
		}
		if registry == nil {
			return out
		}
		for _, ref := range registry.ByCategory(adapterapi.CategoryRouting) {
			// The registry admits only adapters that implement the
			// category's interface, so every routing ref is one; one that
			// cannot say what it serves reserves nothing.
			routing, _ := registry.Routing(ref)
			if caps, err := routing.Capabilities(ctx); err == nil {
				for _, h := range caps.ReservedHostnames {
					add(h)
				}
			}
		}
		return out
	}
}
