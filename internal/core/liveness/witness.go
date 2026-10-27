package liveness

import (
	"context"
	"slices"
	"time"

	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// Witness is what a person marking a host lost is checked against (R-421,
// R-153). Marking a host lost starts its apps' standbys, so it is refused
// unless the host has been silent, to everyone who can say, for at least
// Period:
//
//   - this control host's own Monitor has watched it without contact for
//     Period (a host already marked lost is no longer watched, and is not
//     asked this);
//   - its machine does not answer a probe now, not even to turn it away;
//   - no other host that answers says it had contact with it within Period.
//
// A host whose Pando process is down while its machine runs answers the probe
// by refusing the connection, so it is not marked lost by hand while its apps
// may be running.
type Witness struct {
	// Hosts lists the cluster's hosts, as Watch.Hosts.
	Hosts func(ctx context.Context) ([]Peer, error)

	// Probe is Watch.Probe.
	Probe func(ctx context.Context, p Peer) ([]byte, error)

	// Local is this control host's Monitor's UnreachedFor; nil when it has
	// none yet.
	Local func(hostID string) (time.Duration, bool)

	Config Config
}

// Silent returns nil when h may be marked lost, and otherwise why not.
func (w *Witness) Silent(ctx context.Context, h state.Host) error {
	cfg := w.Config.Normalize()
	list, err := w.Hosts(ctx)
	if err != nil {
		return errs.Wrap(errs.Internal, "Pando could not read the cluster's hosts, so it cannot tell whether this host is gone.", err).
			WithRemedy("Try again once the database answers.")
	}
	i := slices.IndexFunc(list, func(p Peer) bool { return p.ID == h.ID })
	if i < 0 {
		return errs.Newf(errs.NotFound, "Host %s is not one of the cluster's hosts.", h.ID)
	}
	target := list[i]
	if !target.Lost {
		if err := w.watched(h, cfg.Period); err != nil {
			return err
		}
	}

	pctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	_, err = w.Probe(pctx, target)
	cancel()
	if contact(err) {
		return heard(h, cfg.Period, "Its machine answered Pando's check just now.")
	}

	others := slices.DeleteFunc(slices.Clone(list), func(p Peer) bool { return p.ID == h.ID })
	_, views := probeAll(ctx, others, cfg.Timeout, w.Probe)
	for _, v := range views {
		if d, ok := v.Unreached()[h.ID]; ok && d < cfg.Period {
			return heard(h, cfg.Period, "Host "+v.Host+" had contact with it "+d.Round(time.Second).String()+" ago.")
		}
	}
	return nil
}

// watched is this control host's own word: it must have watched h without
// contact for at least period.
func (w *Witness) watched(h state.Host, period time.Duration) error {
	if w.Local == nil {
		return errs.Newf(errs.StateInvalid,
			"This control host is not watching the cluster's hosts yet, so Pando cannot tell whether host %s (%s) is gone.", h.Name, h.ID).
			WithRemedy("Wait a minute and mark the host lost again.")
	}
	d, ok := w.Local(h.ID)
	switch {
	case !ok:
		return errs.Newf(errs.StateInvalid,
			"Host %s (%s) is not one this control host watches, either because it is this host or because it joined moments ago, so Pando cannot tell whether it is gone.", h.Name, h.ID).
			WithRemedy("Mark the host lost from another control host, or wait a minute and try again.")
	case d < period:
		return heard(h, period, "This control host had contact with it "+d.Round(time.Second).String()+" ago, or has watched it for no longer than that.")
	}
	return nil
}

// heard refuses marking h lost because it was heard from lately.
func heard(h state.Host, period time.Duration, why string) error {
	return errs.Newf(errs.StateInvalid,
		"Host %s (%s) has been heard from in the last %s, so it is not gone. %s Its apps may still be running, and starting their standbys now would run a second copy of each.",
		h.Name, h.ID, period, why).
		WithRemedy("If its apps should move, turn the machine off or disconnect it from the network, wait " + period.String() + ", then mark it lost again.")
}
