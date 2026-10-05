package statetest

import (
	"errors"
	"testing"
)

// The two start failures seen on CI are retried; anything else is the test's
// own problem and is reported at once.
func TestTransientStartErrors(t *testing.T) {
	for msg, want := range map[string]bool{
		// cmd/pando, CI run 37249916330.
		"run postgres: generic container: create container: reaper: new reaper: run container: container start: " +
			"Error response from daemon: failed to set up container networking: driver failed programming external " +
			"connectivity on endpoint reaper_c640: failed to listen on TCP socket: address already in use": true,
		// internal/core/audit, CI run 37249088020.
		`run postgres: generic container: create container: reaper: from container "41515c85": ` +
			"wait for reaper 41515c85: context deadline exceeded": true,
		"Bind for 0.0.0.0:32768 failed: port is already allocated": true,
		"pull access denied for postgres:17-alpinee":               false,
		"migrations failed: syntax error at or near":               false,
	} {
		if got := transient(errors.New(msg)); got != want {
			t.Errorf("transient(%q) = %v, want %v", msg, got, want)
		}
	}
}
