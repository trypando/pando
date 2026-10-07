package main

import (
	"fmt"
	"math"
	"time"
)

// Tier is one of the two scale targets in
// docs/design/notes-multiple-replicas-issue-72.md ("Targets"), with the
// traffic the harness drives at its peak.
//
// The seed half (users, apps, groups, admins, tokens) is how big the install
// is. The run half (console users, API and proxy rates) is how busy it is at
// the last step of the ramp; earlier steps are fractions of it (Ramp).
type Tier struct {
	Name string `json:"name"`

	Users  int `json:"users"`
	Apps   int `json:"apps"`
	Groups int `json:"groups"`
	// Admins hold the built-in administrator role install-wide.
	Admins int `json:"admins"`
	// Tokens are delegated API tokens, each owned by an app owner.
	Tokens int `json:"tokens"`
	// RealApps are deployed through the API as real containers, for proxy
	// load that reaches an app. Every other app is seeded without one.
	RealApps int `json:"real_apps"`

	ConsoleUsers int     `json:"console_users"`
	APIRate      float64 `json:"api_rate"`
	ProxyRate    float64 `json:"proxy_rate"`
}

// Tiers are the two targets. The console and request figures are the
// harness's reading of the targets table: the single VM has no stated
// concurrent-user figure, so it is taken as one user in ten online at once.
var Tiers = map[string]Tier{
	"vm": {
		Name: "vm", Users: 3_000, Apps: 1_000, Groups: 60, Admins: 5, Tokens: 100, RealApps: 10,
		ConsoleUsers: 300, APIRate: 50, ProxyRate: 200,
	},
	"cluster": {
		Name: "cluster", Users: 100_000, Apps: 20_000, Groups: 2_000, Admins: 20, Tokens: 2_000, RealApps: 20,
		ConsoleUsers: 5_000, APIRate: 500, ProxyRate: 2_000,
	},
}

// TierByName returns a named tier.
func TierByName(name string) (Tier, error) {
	t, ok := Tiers[name]
	if !ok {
		return Tier{}, fmt.Errorf("%q is not a tier; use vm or cluster", name)
	}
	return t, nil
}

// Validate checks that a tier, possibly with flags laid over it, can be seeded.
func (t Tier) Validate() error {
	switch {
	case t.Users < 1:
		return fmt.Errorf("a tier needs at least one user")
	case t.Apps < 1:
		return fmt.Errorf("a tier needs at least one app")
	case t.Groups < 1:
		return fmt.Errorf("a tier needs at least one group")
	case t.Admins < 0 || t.Admins > t.Users:
		return fmt.Errorf("admins (%d) must be between 0 and the number of users (%d)", t.Admins, t.Users)
	case t.Tokens < 0 || t.Tokens > t.Apps:
		return fmt.Errorf("tokens (%d) must be between 0 and the number of apps (%d), since each is owned by an app owner", t.Tokens, t.Apps)
	case t.RealApps < 0:
		return fmt.Errorf("real apps cannot be negative")
	}
	return nil
}

// Step is one level of the ramp.
type Step struct {
	Index        int           `json:"index"`
	ConsoleUsers int           `json:"console_users"`
	APIRate      float64       `json:"api_rate"`
	ProxyRate    float64       `json:"proxy_rate"`
	Hold         time.Duration `json:"hold"`
}

// DefaultFractions is the ramp: each step is this share of the tier's peak.
var DefaultFractions = []float64{0.1, 0.25, 0.5, 0.75, 1.0}

// Ramp turns a tier's peak into steps. Every step has at least one console
// user, so a small tier still exercises the console at its first step.
func Ramp(t Tier, fractions []float64, hold time.Duration) []Step {
	steps := make([]Step, 0, len(fractions))
	for i, f := range fractions {
		users := int(math.Round(float64(t.ConsoleUsers) * f))
		if users < 1 && t.ConsoleUsers > 0 {
			users = 1
		}
		steps = append(steps, Step{
			Index:        i,
			ConsoleUsers: users,
			APIRate:      round2(t.APIRate * f),
			ProxyRate:    round2(t.ProxyRate * f),
			Hold:         hold,
		})
	}
	return steps
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
