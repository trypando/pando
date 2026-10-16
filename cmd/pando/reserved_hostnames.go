package main

import (
	"context"
	"net/url"
	"strings"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
)

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
			routing, ok := registry.Routing(ref)
			if !ok {
				continue
			}
			caps, err := routing.Capabilities(ctx)
			if err != nil {
				continue
			}
			for _, h := range caps.ReservedHostnames {
				add(h)
			}
		}
		return out
	}
}
