package notice

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/core/state"
)

// TestR423_AnAppWhoseBackupCopiedVolumesLiveSaysWhich asserts the notice R-423
// asks for: naming the volumes the last backup copied while the app ran, and
// not coming back with each day's backup, since it says the same thing.
func TestR423_AnAppWhoseBackupCopiedVolumesLiveSaysWhich(t *testing.T) {
	at := time.Date(2026, 10, 28, 3, 0, 0, 0, time.UTC)
	taken := &state.BackupAttempt{AttemptedAt: at, Outcome: state.AttemptTaken, LiveVolumes: []string{"uploads"}}

	n := find(t, Compute(Inputs{LastBackup: taken}), KeyBackupLive)
	require.Contains(t, n.Message, "its volume uploads while the app was running")
	require.Contains(t, n.Message, "may be inconsistent")
	require.NotContains(t, keys(Compute(Inputs{LastBackup: taken})), KeyBackup)

	tomorrow := *taken
	tomorrow.AttemptedAt = at.Add(24 * time.Hour)
	require.Equal(t, n.Fingerprint, find(t, Compute(Inputs{LastBackup: &tomorrow}), KeyBackupLive).Fingerprint)

	more := *taken
	more.LiveVolumes = []string{"uploads", "cache", "data"}
	n2 := find(t, Compute(Inputs{LastBackup: &more}), KeyBackupLive)
	require.Contains(t, n2.Message, "its volumes cache, data and uploads while")
	require.NotEqual(t, n.Fingerprint, n2.Fingerprint)

	for _, none := range []*state.BackupAttempt{
		nil,
		{Outcome: state.AttemptTaken},
		{Outcome: state.AttemptFailed, LiveVolumes: []string{"uploads"}},
	} {
		require.NotContains(t, keys(Compute(Inputs{LastBackup: none})), KeyBackupLive)
	}
}
