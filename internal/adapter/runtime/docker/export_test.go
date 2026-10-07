package docker

import "time"

// SetReclaimMinAge sets how old an empty network must be before a's
// ReclaimNetworks takes it, for a test that makes its networks a moment
// before reclaiming them.
func SetReclaimMinAge(a *Adapter, d time.Duration) { a.reclaimAgeOverride = &d }
