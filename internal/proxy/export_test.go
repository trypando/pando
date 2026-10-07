package proxy

import "time"

// SetClock fixes the time the visit log reads, for tests of its windows.
func (p *Proxy) SetClock(now func() time.Time) { p.clock = now }

// SetReauthInterval shortens how often a long-lived connection is
// re-authorized, which is assertion.Lifetime outside a test.
func (p *Proxy) SetReauthInterval(d time.Duration) { p.reauthEvery = d }
