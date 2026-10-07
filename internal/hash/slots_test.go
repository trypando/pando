package hash

import (
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/secret"
)

// TestR042_PasswordHashingRunsNoMoreThanOnePerCPU asserts that a burst of
// sign-ins (R-042, local passwords) holds at most one argon2id derivation per
// CPU at a time, and that every one of them still completes (issue #72).
func TestR042_PasswordHashingRunsNoMoreThanOnePerCPU(t *testing.T) {
	require.Equal(t, max(1, runtime.NumCPU()), cap(slots))

	encoded, err := New(secret.New("correct horse battery"))
	require.NoError(t, err)

	var peak atomic.Int64
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if n := int64(len(slots)); n > peak.Load() {
					peak.Store(n)
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
	}()

	callers := 4 * cap(slots)
	var wg sync.WaitGroup
	var matched atomic.Int64
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := Verify(secret.New("correct horse battery"), encoded)
			if err == nil && ok {
				matched.Add(1)
			}
		}()
	}
	wg.Wait()
	close(stop)

	require.EqualValues(t, callers, matched.Load(), "every caller is answered, none dropped")
	require.LessOrEqual(t, peak.Load(), int64(cap(slots)), "no more derivations at once than CPUs")
	require.Zero(t, len(slots), "every slot is given back")
}
