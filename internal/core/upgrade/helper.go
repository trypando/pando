package upgrade

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/trypando/pando/internal/core/clock"
)

// What an upgrade came to, written where every version of Pando reads it.
//
// The helper outlives the Pando that started it and is gone before the next
// one starts, so neither can tell the other anything directly: the outcome is
// a file in Pando's data directory, written by whichever of them knows, and
// read by the Pando that starts next — new or restored — which records it in
// the audit log (R-356).
const (
	StateRunning    = "running"
	StateSucceeded  = "succeeded"
	StateRolledBack = "rolled_back"
	StateFailed     = "failed"
)

// Outcome is one upgrade, as far as it got.
type Outcome struct {
	ID           string    `json:"id"`
	From         string    `json:"from"`
	To           string    `json:"to"`
	Image        string    `json:"image"`
	Tag          string    `json:"tag,omitempty"`
	StartedAt    time.Time `json:"started_at"`
	StartedBy    string    `json:"started_by"`
	Automatic    bool      `json:"automatic"`
	BackupID     string    `json:"backup_id,omitempty"`
	SkipBackup   bool      `json:"skip_backup"`
	State        string    `json:"state"`
	FinishedAt   time.Time `json:"finished_at,omitzero"`
	Reason       string    `json:"reason,omitempty"`
	Logs         string    `json:"logs,omitempty"`
	SnapshotAt   time.Time `json:"snapshot_at,omitzero"`
	SnapshotGone bool      `json:"snapshot_gone"`
	// Recorded is set once a Pando has written the outcome to the audit log.
	Recorded bool `json:"recorded"`
}

// OutcomePath is where the outcome lives under Pando's data directory.
func OutcomePath(workDir string) string { return filepath.Join(workDir, "upgrade", "outcome.json") }

// ReadOutcome reads the last upgrade's outcome; nil when there has been none.
func ReadOutcome(path string) (*Outcome, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var o Outcome
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("the last upgrade's outcome at %s does not parse: %w", path, err)
	}
	return &o, nil
}

// WriteOutcome replaces the outcome atomically, so a reader never sees half.
func WriteOutcome(path string, o Outcome) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Swap is the runtime's half of the helper: Pando's container, replaced.
type Swap interface {
	Stop(ctx context.Context) error
	Recreate(ctx context.Context, image string) error
	Ready(ctx context.Context, timeout time.Duration) error
	Logs(ctx context.Context) string
	Discard(ctx context.Context)
	Restore(ctx context.Context) error
	Finish(ctx context.Context, image, tag string) error
}

// Database is core's half: the copy taken while Pando is stopped.
type Database interface {
	Snapshot(ctx context.Context) error
	Restore(ctx context.Context) error
}

// RunHelper replaces Pando (R-359) and records how it went at path. It puts
// the previous version back on its own at every step after which the new one
// might have touched anything — and says so plainly when it cannot.
func RunHelper(ctx context.Context, path string, swap Swap, db Database, readyTimeout time.Duration, clk clock.Clock) Outcome {
	o, err := ReadOutcome(path)
	if err != nil || o == nil {
		o = &Outcome{State: StateFailed, Reason: "The upgrade helper started without an upgrade to run."}
		_ = WriteOutcome(path, *o)
		return *o
	}
	finish := func(state, reason string) Outcome {
		o.State, o.Reason, o.FinishedAt = state, reason, clk.Now()
		_ = WriteOutcome(path, *o)
		return *o
	}

	if err := swap.Stop(ctx); err != nil {
		// Nothing has changed, and the old container may still be running.
		_ = swap.Restore(ctx)
		return finish(StateFailed, "Pando could not be stopped for the upgrade, so nothing was changed: "+err.Error())
	}
	if err := db.Snapshot(ctx); err != nil {
		if rerr := swap.Restore(ctx); rerr != nil {
			return finish(StateFailed, "Pando's database could not be copied ("+err.Error()+"), and the previous version could not be started again: "+rerr.Error()+". Start Pando's container by hand.")
		}
		return finish(StateRolledBack, "Pando's database could not be copied, so the upgrade did not go ahead and the previous version is running again: "+err.Error())
	}
	o.SnapshotAt = clk.Now()
	_ = WriteOutcome(path, *o)

	err = swap.Recreate(ctx, o.Image)
	if err == nil {
		err = swap.Ready(ctx, readyTimeout)
	}
	if err != nil {
		o.Logs = swap.Logs(ctx)
		swap.Discard(ctx)
		if derr := db.Restore(ctx); derr != nil {
			return finish(StateFailed, fmt.Sprintf("%s did not start (%s), and Pando's database could not be restored from the copy taken before it: %s. "+
				"The copy is still there; restore the full backup taken before the upgrade, or ask for help with the copy, before starting %s again.",
				o.To, err, derr, o.From))
		}
		o.SnapshotGone = true
		if rerr := swap.Restore(ctx); rerr != nil {
			return finish(StateFailed, fmt.Sprintf("%s did not start (%s). Pando's database was restored, but %s could not be started again: %s. Start Pando's container by hand.",
				o.To, err, o.From, rerr))
		}
		return finish(StateRolledBack, fmt.Sprintf("%s did not start, so Pando put %s back, with its database as it was before the upgrade: %s.", o.To, o.From, err))
	}

	if err := swap.Finish(ctx, o.Image, o.Tag); err != nil {
		// The new version is running and healthy; only the tidying failed.
		return finish(StateSucceeded, fmt.Sprintf("%s is running. Pointing %s at it did not work (%s), so set the image where Pando is deployed before the next apply puts %s back.",
			o.To, o.Tag, err, o.From))
	}
	return finish(StateSucceeded, "")
}
