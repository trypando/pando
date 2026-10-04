// Package update knows whether a newer Pando has been released (R-349–R-352).
//
// It reads GitHub's release list for trypando/pando, which the release
// workflow writes: one release per tag, its body exactly that version's
// CHANGELOG.md section, prerelease set for a tag with a suffix. Nothing else
// is published for it to read, so there is nothing else to keep in step.
//
// It tells; it does not upgrade. Replacing the running container is issue #53's
// second half, and is opt-in through the deployment config.
package update

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/mod/semver"

	"github.com/trypando/pando/internal/core/clock"
	"github.com/trypando/pando/internal/core/policy"
)

// DefaultInterval is how often the check runs. Four requests a day is well
// inside GitHub's unauthenticated limit of sixty an hour per address, and a
// security release is seen within a working morning of being published.
const DefaultInterval = 6 * time.Hour

// Release is one published version.
type Release struct {
	Version     string    `json:"version"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	URL         string    `json:"url"`
	// Notes is the version's CHANGELOG.md section, as Markdown.
	Notes string `json:"notes"`
	// Security is set when the Security section names an advisory rather than
	// saying "No new advisories." (docs/releasing.md).
	Security bool `json:"security"`
	// Breaking is set when the version may break what the one before it did:
	// a MAJOR bump, or a MINOR one before 1.0 (docs/releasing.md).
	Breaking bool `json:"breaking"`
}

// Settings is what host policy says about the check.
type Settings struct {
	Enabled bool
	Channel policy.UpdateChannel
}

// SettingsFrom reads the check's settings from a policy document.
func SettingsFrom(d policy.Document) Settings {
	return Settings{Enabled: !d.DisableUpdateCheck, Channel: d.Channel()}
}

// Source lists published releases, newest or not, prereleases included.
type Source interface {
	Releases(ctx context.Context) ([]Release, error)
}

// Status is what the check knows, as every surface shows it (R-351).
type Status struct {
	Current string `json:"current"`
	// Development is set for a binary no release built, which has no version
	// to compare: it is told what the latest is, never that it is behind.
	Development bool                 `json:"development"`
	Enabled     bool                 `json:"enabled"`
	Channel     policy.UpdateChannel `json:"channel"`
	CheckedAt   *time.Time           `json:"checked_at,omitempty"`
	// Error is why the last check failed, when it did. What an earlier check
	// found is still reported beside it.
	Error  string `json:"error,omitempty"`
	Latest string `json:"latest,omitempty"`
	// Available is set when Latest is newer than Current.
	Available bool `json:"available"`
	// Releases are the versions after Current up to Latest, newest first.
	Releases []Release `json:"releases"`
	Security bool      `json:"security"`
	Breaking bool      `json:"breaking"`
	// Upgrade says how to move this installation to Latest (R-352).
	Upgrade *Upgrade `json:"upgrade,omitempty"`
}

// Checker asks Source on an interval while policy allows it, and answers
// Status from what it last heard.
type Checker struct {
	Source   Source
	Settings func(ctx context.Context) (Settings, error)
	// Current is this binary's version, "dev" for a development build.
	Current  string
	Install  Install
	Clock    clock.Clock
	Logger   *zap.Logger
	Interval time.Duration

	mu        sync.Mutex
	releases  []Release
	checkedAt time.Time
	lastErr   error
}

// Run checks at startup and every Interval until ctx ends. While the check is
// off nothing is sent (R-349); turning it back on is seen at the next tick.
func (c *Checker) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}
	for {
		c.Check(ctx)
		select {
		case <-ctx.Done():
			return
		case <-c.Clock.After(interval):
		}
	}
}

// Check asks Source once, if policy allows it.
func (c *Checker) Check(ctx context.Context) {
	s, err := c.Settings(ctx)
	if err != nil {
		c.Logger.Warn("could not read host policy for the update check", zap.Error(err))
		return
	}
	if !s.Enabled {
		return
	}
	releases, err := c.Source.Releases(ctx)
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.checkedAt = c.Clock.Now()
	c.lastErr = err
	if err != nil {
		// Warn, not error: an install with no route to GitHub is a
		// configuration, and the remedy is the policy setting.
		c.Logger.Warn("could not check for a newer Pando release", zap.Error(err))
		return
	}
	c.releases = releases
}

// Status reports what the last check found, filtered by the channel policy
// names now, so changing the channel needs no new request.
func (c *Checker) Status(ctx context.Context) (Status, error) {
	s, err := c.Settings(ctx)
	if err != nil {
		return Status{}, err
	}
	current := strings.TrimPrefix(c.Current, "v")
	st := Status{
		Current:     current,
		Development: !semver.IsValid("v" + current),
		Enabled:     s.Enabled,
		Channel:     s.Channel,
		Releases:    []Release{},
	}
	if !s.Enabled {
		return st, nil
	}

	c.mu.Lock()
	releases, checkedAt, lastErr := c.releases, c.checkedAt, c.lastErr
	c.mu.Unlock()
	if !checkedAt.IsZero() {
		at := checkedAt
		st.CheckedAt = &at
	}
	if lastErr != nil {
		st.Error = "Pando could not reach GitHub to check for a newer release: " + lastErr.Error() +
			". If this installation has no internet access, turn the update check off in host policy (disable_update_check)."
	}

	offered := Offered(releases, s.Channel)
	if len(offered) == 0 {
		return st, nil
	}
	st.Latest = offered[0].Version
	if st.Development {
		return st, nil
	}
	st.Releases = Between(offered, current)
	st.Available = len(st.Releases) > 0
	for _, r := range st.Releases {
		st.Security = st.Security || r.Security
		st.Breaking = st.Breaking || r.Breaking
	}
	if st.Available {
		u := c.Install.Upgrade(st.Latest)
		st.Upgrade = &u
	}
	return st, nil
}

// Offered is the releases the channel offers, newest first, invalid versions
// dropped and Breaking set against the release before each.
func Offered(all []Release, channel policy.UpdateChannel) []Release {
	var out []Release
	for _, r := range all {
		v := "v" + strings.TrimPrefix(r.Version, "v")
		if !semver.IsValid(v) {
			continue
		}
		if r.Prerelease && channel != policy.UpdateChannelPrerelease {
			continue
		}
		r.Version = v[1:]
		out = append(out, r)
	}
	sortNewestFirst(out)
	for i := range out {
		if i+1 < len(out) {
			out[i].Breaking = Breaks(out[i+1].Version, out[i].Version)
		}
	}
	return out
}

// Between is the releases newer than current, from a newest-first list. The
// oldest of them is marked Breaking against current itself.
func Between(offered []Release, current string) []Release {
	cur := "v" + strings.TrimPrefix(current, "v")
	out := []Release{}
	for _, r := range offered {
		if semver.Compare("v"+r.Version, cur) <= 0 {
			break
		}
		out = append(out, r)
	}
	if n := len(out); n > 0 {
		out[n-1].Breaking = Breaks(current, out[n-1].Version)
	}
	return out
}

// Breaks reports whether moving from one version to the next may break
// something: a MAJOR bump, or before 1.0 a MINOR one (docs/releasing.md).
func Breaks(from, to string) bool {
	f, t := "v"+strings.TrimPrefix(from, "v"), "v"+strings.TrimPrefix(to, "v")
	if semver.Major(f) != semver.Major(t) {
		return true
	}
	return semver.Major(t) == "v0" && semver.MajorMinor(f) != semver.MajorMinor(t)
}

// Skewed reports whether two versions differ in a way their interfaces may:
// MAJOR or MINOR. A PATCH release changes no interface (docs/releasing.md).
// Development builds are never skewed — there is nothing to compare.
func Skewed(a, b string) bool {
	va, vb := "v"+strings.TrimPrefix(a, "v"), "v"+strings.TrimPrefix(b, "v")
	if !semver.IsValid(va) || !semver.IsValid(vb) {
		return false
	}
	return semver.MajorMinor(va) != semver.MajorMinor(vb)
}

// Newer reports whether b is a later version than a.
func Newer(a, b string) bool {
	return semver.Compare("v"+strings.TrimPrefix(b, "v"), "v"+strings.TrimPrefix(a, "v")) > 0
}

func sortNewestFirst(rs []Release) {
	sort.SliceStable(rs, func(i, j int) bool {
		return semver.Compare("v"+rs[i].Version, "v"+rs[j].Version) > 0
	})
}

// HasSecurityFix reads a CHANGELOG.md section's Security subsection. Every
// release carries one; "No new advisories." is what an empty one says.
func HasSecurityFix(notes string) bool {
	var body []string
	inside := false
	for _, line := range strings.Split(notes, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			if inside {
				break
			}
			inside = strings.EqualFold(strings.TrimSpace(strings.TrimLeft(trimmed, "#")), "Security")
			continue
		}
		if inside && trimmed != "" {
			body = append(body, trimmed)
		}
	}
	text := strings.Join(body, " ")
	return text != "" && !strings.HasPrefix(strings.ToLower(text), "no new advisories")
}
