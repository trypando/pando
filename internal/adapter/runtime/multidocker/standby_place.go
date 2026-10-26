package multidocker

import (
	"context"
	"sync"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// chooseStandbyHost is where a bundle's standby goes (R-419): another host
// open to new apps that answers, never the bundle's own. The host it was on
// before, prefer, is kept while it qualifies, so a standby is not moved, and
// its copies not shipped again from nothing, on every pass. Otherwise the
// roomiest host that fits, by the same rule as placing an app (choose).
//
// [P] A stand-in: capacity reservation (R-422) replaces this with the check
// that any one host can be lost and its apps fit on the others.
func (a *Adapter) chooseStandbyHost(ctx context.Context, home *host, prefer string, n api.Fit) (*host, error) {
	var mu sync.Mutex
	var candidates []candidate
	a.eachHost(ctx, func(ctx context.Context, h *host) error {
		if h == home || h.cfg.NoPlacement {
			return nil
		}
		c, err := h.rt.Capacity(ctx)
		if err != nil {
			return err
		}
		cand := candidate{host: h}
		if c.LargestFit != nil {
			cand.free, cand.known = *c.LargestFit, true
		}
		mu.Lock()
		candidates = append(candidates, cand)
		mu.Unlock()
		return nil
	})
	for _, c := range candidates {
		if c.host.cfg.Name == prefer {
			return c.host, nil
		}
	}
	if len(candidates) == 0 {
		return nil, errs.Newf(errs.AdapterUnavailable,
			"No Docker host other than %s is open to new apps and answering, so this app has nowhere to keep a standby.", home.cfg.Name).
			WithRemedy("Open another host to new apps in the Docker hosts runtime's settings, or add one.")
	}
	return choose(candidates, n, a.hosts)
}
