package multidocker

import (
	"context"
	"io"
	"sort"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/errs"
)

// Shipping a standby's volumes (R-420): live, incremental, host to host.
//
// Each ship lists the app's volume on its host, compares the listing with the
// one the standby's newest copy was made from, and sends only what differs:
// a tar of those entries read on the app's host, streamed through Pando into
// a helper on the standby's host that adds them to a hard-linked clone of the
// newest copy. Nothing is paused — the app writes while it is read, which is
// why the copy may be inconsistent and the app carries a notice saying so —
// and nothing changes either host's filesystem: both ends are Docker volumes.
//
// Changed means a different type, mode, size or modification time, or a
// modification time no earlier than the second the last listing was taken
// in, since a file written twice in one second can keep its size and time.

// ShipVolumes sends one copy of the bundle's volumes to its standby.
func (a *Adapter) ShipVolumes(ctx context.Context, req api.ShipRequest) (api.ShipResult, error) {
	if err := a.standby.busy(req.BundleID); err != nil {
		return api.ShipResult{}, err
	}
	home, err := a.mustLocate(ctx, req.BundleID)
	if err != nil {
		return api.ShipResult{}, err
	}
	target, err := a.hostNamed(req.Host)
	if err != nil {
		return api.ShipResult{}, err
	}
	if target == home {
		return api.ShipResult{}, errs.Newf(errs.ValidInvalid,
			"This app runs on %s, so its standby cannot be kept there too.", home.cfg.Name)
	}
	from, err := standbyOf(home)
	if err != nil {
		return api.ShipResult{}, err
	}
	to, err := standbyOf(target)
	if err != nil {
		return api.ShipResult{}, err
	}
	keep := req.Keep
	if keep <= 0 {
		keep = api.DefaultStandbyCopies
	}
	out := api.ShipResult{Unchanged: true}
	for i, volumeID := range req.VolumeIDs {
		got, err := shipVolume(ctx, from, to, req.BundleID, volumeID, keep)
		if err != nil {
			return out, err
		}
		if i == 0 || got.Copies < out.Copies {
			out.Copies = got.Copies
		}
		out.FilesSent += got.FilesSent
		out.BytesSent += got.BytesSent
		out.Unchanged = out.Unchanged && got.Unchanged
	}
	return out, nil
}

func shipVolume(ctx context.Context, from, to standbyHost, bundleID, volumeID string, keep int) (api.ShipResult, error) {
	have, err := to.StandbyCopies(ctx, bundleID, volumeID)
	if err != nil {
		return api.ShipResult{}, err
	}
	now, err := from.ListVolume(ctx, bundleID, volumeID)
	if err != nil {
		return api.ShipResult{}, err
	}
	d := diffListings(have.Newest, now, len(have.Seqs) > 0)
	if len(have.Seqs) > 0 && d.empty() {
		return api.ShipResult{Copies: len(have.Seqs), Unchanged: true}, nil
	}
	if err := stage(ctx, from, to, bundleID, volumeID, d); err != nil {
		return api.ShipResult{}, err
	}
	seq := 1
	if n := len(have.Seqs); n > 0 {
		seq = have.Seqs[n-1] + 1
	}
	all := append(append([]int(nil), have.Seqs...), seq)
	var drop []int
	if len(all) > keep {
		drop, all = all[:len(all)-keep], all[len(all)-keep:]
	}
	if err := to.CommitStandbyCopy(ctx, bundleID, volumeID, seq, now, drop); err != nil {
		return api.ShipResult{}, err
	}
	return api.ShipResult{Copies: len(all), FilesSent: len(d.send), BytesSent: d.bytes}, nil
}

// stage streams what changed from the app's host into a new copy on the
// standby's.
func stage(ctx context.Context, from, to standbyHost, bundleID, volumeID string, d listingDiff) error {
	if len(d.send) == 0 {
		return to.StageStandbyCopy(ctx, bundleID, volumeID, d.remove, nil)
	}
	pr, pw := io.Pipe()
	read := make(chan error, 1)
	go func() {
		err := from.ReadVolumeFiles(ctx, bundleID, volumeID, d.send, pw)
		_ = pw.CloseWithError(err)
		read <- err
	}()
	err := to.StageStandbyCopy(ctx, bundleID, volumeID, d.remove, pr)
	// Unblocks the reader if the writing side gave up first.
	_ = pr.CloseWithError(io.ErrClosedPipe)
	if rerr := <-read; err == nil {
		err = rerr
	}
	return err
}

// listingDiff is what a ship sends and removes.
type listingDiff struct {
	send   []string
	remove []string
	bytes  int64
}

func (d listingDiff) empty() bool { return len(d.send) == 0 && len(d.remove) == 0 }

// diffListings compares the newest copy's listing with the volume's now.
// Without a previous copy everything is sent.
func diffListings(prev, next docker.VolumeListing, havePrev bool) listingDiff {
	var d listingDiff
	before := map[string]docker.VolumeEntry{}
	if havePrev {
		for _, e := range prev.Entries {
			before[e.Path] = e
		}
	}
	seen := make(map[string]bool, len(next.Entries))
	for _, e := range next.Entries {
		seen[e.Path] = true
		send, remove := changed(before, e, prev.TakenAt, havePrev)
		if remove {
			d.remove = append(d.remove, e.Path)
		}
		if send {
			d.send = append(d.send, e.Path)
			if !e.IsDir() {
				d.bytes += e.Size
			}
		}
	}
	for p := range before {
		if !seen[p] {
			d.remove = append(d.remove, p)
		}
	}
	sort.Strings(d.send)
	sort.Strings(d.remove)
	return d
}

// changed is whether one entry is sent, and whether what the copy has at its
// path is removed first. A directory that is still a directory is never
// removed: its entries are compared on their own, and sending it puts back
// only its owner, mode and time.
func changed(before map[string]docker.VolumeEntry, e docker.VolumeEntry, takenAt int64, havePrev bool) (send, remove bool) {
	if !havePrev {
		return true, false
	}
	old, ok := before[e.Path]
	if !ok {
		return true, false
	}
	if old.IsDir() != e.IsDir() || (old.Mode&0o170000) != (e.Mode&0o170000) {
		return true, true
	}
	same := old.Mode == e.Mode && old.MTime == e.MTime && (e.IsDir() || old.Size == e.Size)
	if e.IsDir() {
		return !same, false
	}
	if !same || e.MTime >= takenAt {
		return true, true
	}
	return false, false
}
