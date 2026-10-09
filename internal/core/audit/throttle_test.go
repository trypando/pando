package audit

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// TestR388_AStreamReaderIsRecordedOncePerWindow asserts R-388's throttle: one
// reader's stream reads are audited once per window, each reader separately,
// and again once the window has passed.
func TestR388_AStreamReaderIsRecordedOncePerWindow(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	th := &ReadThrottle{Every: time.Hour}

	assert.True(t, th.Due("usr_a", now), "the first read is recorded")
	assert.False(t, th.Due("usr_a", now.Add(59*time.Minute)), "not again inside the window")
	assert.True(t, th.Due("usr_b", now.Add(time.Minute)), "another reader is recorded on its own")
	assert.True(t, th.Due("usr_a", now.Add(time.Hour)), "and again once the window has passed")

	var none *ReadThrottle
	assert.True(t, none.Due("usr_a", now), "no throttle records every read")
	assert.True(t, none.Due("usr_a", now), "every one")

	def := &ReadThrottle{}
	assert.True(t, def.Due("usr_c", now))
	assert.False(t, def.Due("usr_c", now.Add(DefaultReadEvery-time.Second)), "the default window is an hour")
}

// TestReadThrottleForgetsReadersOutsideTheWindow asserts the map does not
// only grow on a busy install.
func TestReadThrottleForgetsReadersOutsideTheWindow(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	th := &ReadThrottle{Every: time.Minute}
	for i := 0; i < 10_001; i++ {
		th.Due("usr_"+strconv.Itoa(i), now)
	}
	th.Due("usr_late", now.Add(2*time.Minute))
	assert.LessOrEqual(t, len(th.last), 2, "readers outside the window are forgotten")
}
