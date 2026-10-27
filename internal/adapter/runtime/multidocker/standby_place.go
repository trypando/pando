package multidocker

import (
	"context"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// standbyRequest is a new app's standby, to be placed (R-422).
type standbyRequest struct {
	// App is the app's bundle ID, and Primary the name of the host it runs on.
	App     string
	Primary string

	// Need is what the app's workloads are limited to, summed (need(plan)):
	// what the standby takes when it starts.
	Need api.Fit

	// DiskBytes is what the standby's copies hold on its host, all the time:
	// standbyDisk(app's disk limit, reported size).
	DiskBytes int64

	// AllowOversubscription is host policy allow_failover_oversubscription,
	// as BundlePlan.Failover carries it. CPU and memory may then be placed
	// past the reservation; disk never is (R-242).
	AllowOversubscription bool
}

// standbyChoice is where a standby goes, or why it goes nowhere.
type standbyChoice struct {
	// Host is the standby's host; nil when none has room.
	Host *host

	// NoRoom, when Host is nil, is the app's notice: it names the largest
	// room any host has (R-105). Never a blocker: the app deploys and runs
	// without a standby (R-422).
	NoRoom *errs.Error
}

// chooseStandbyHost picks the host for a new app's standby so that the loss
// of any one host still leaves room on the others for its apps (R-422).
// existing is every standby already placed, which keeps its reservation: a
// standby is never moved or dropped to make room for a new one.
//
// The error is only for not being able to read any other host; a standby
// that fits nowhere is a choice with NoRoom set.
func (a *Adapter) chooseStandbyHost(ctx context.Context, req standbyRequest, existing []standbyLoad) (standbyChoice, error) {
	rooms := a.readRooms(ctx)
	others := 0
	for _, r := range rooms {
		if r.Name != req.Primary && r.Open {
			others++
		}
	}
	if others == 0 && a.anyDown(rooms, req.Primary) {
		return standbyChoice{}, errs.New(errs.AdapterUnavailable,
			"Pando cannot reach Docker on any host other than this app's, so it cannot choose a host for its standby.").
			WithRemedy("Bring the hosts back; the standby is placed on the next deploy.")
	}
	name, miss := chooseReserve(reservation{hosts: rooms, standbys: existing}, standbyAsk{
		App:           req.App,
		Primary:       req.Primary,
		Need:          amount{CPUMillis: req.Need.CPUMillis, MemoryBytes: req.Need.MemoryBytes, DiskBytes: req.DiskBytes},
		Oversubscribe: req.AllowOversubscription,
	})
	if miss != nil {
		return standbyChoice{NoRoom: miss.envelope()}, nil
	}
	return standbyChoice{Host: a.byName(name)}, nil
}

// anyDown reports a host other than primary, open to new apps, that did not
// answer.
func (a *Adapter) anyDown(rooms []hostRoom, primary string) bool {
	for i, r := range rooms {
		if r.Name != primary && !r.Open && !a.hosts[i].cfg.NoPlacement {
			return true
		}
	}
	return false
}

// placeStandby is where ApplyStandby puts a bundle's standby (R-419): the
// host it was on before, prefer, while that host still qualifies, so its
// copies are not shipped again from nothing; otherwise the host the failover
// reservation chooses (R-422). No room is returned as the notice's error, and
// the app keeps running without a standby.
func (a *Adapter) placeStandby(ctx context.Context, home *host, prefer string, plan api.BundlePlan) (*host, error) {
	if h := a.byName(prefer); h != nil && h != home && !h.cfg.NoPlacement {
		return h, nil
	}
	if !a.otherOpenHost(home) {
		return nil, errs.Newf(errs.AdapterUnavailable,
			"No Docker host other than %s is open to new apps, so this app has nowhere to keep a standby.", home.cfg.Name).
			WithRemedy("Open another host to new apps in the Docker hosts runtime's settings, or add one.")
	}
	var existing []standbyLoad
	if a.standbys != nil {
		var err error
		if existing, err = a.standbys(ctx); err != nil {
			return nil, err
		}
	}
	n := need(plan)
	choice, err := a.chooseStandbyHost(ctx, standbyRequest{
		App: plan.BundleID, Primary: home.cfg.Name, Need: n,
		DiskBytes:             standbyDisk(diskLimit(plan), 0),
		AllowOversubscription: plan.Failover.AllowOversubscription,
	}, existing)
	if err != nil {
		return nil, err
	}
	if choice.Host == nil {
		return nil, choice.NoRoom
	}
	return choice.Host, nil
}

// diskLimit is the app's disk limit, which every workload carries the same
// (R-240): its standby's copies are counted from it (standbyDisk).
func diskLimit(plan api.BundlePlan) int64 {
	for _, w := range plan.Workloads {
		if w.Resources.DiskBytes > 0 {
			return w.Resources.DiskBytes
		}
	}
	return 0
}

// otherOpenHost reports a host besides home that takes new apps.
func (a *Adapter) otherOpenHost(home *host) bool {
	for _, h := range a.hosts {
		if h != home && !h.cfg.NoPlacement {
			return true
		}
	}
	return false
}
