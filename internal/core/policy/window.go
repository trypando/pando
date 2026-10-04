package policy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Window is a parsed maintenance window (R-361): the weekdays it opens on, the
// UTC time it opens and how long it stays open. A window that runs past
// midnight belongs to the day it opened on.
type Window struct {
	Days   []time.Weekday
	Start  time.Duration // since midnight UTC
	Length time.Duration
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

// ParseWindow reads "sun,wed 02:00 2h": weekdays by their first three
// letters, or "daily", a 24-hour UTC start time, and a whole number of hours
// from 1 to 24. Errors are written for the administrator who typed it (R-105).
func ParseWindow(s string) (Window, error) {
	const shape = `use weekdays, a UTC start time and a length in hours, such as "sun,wed 02:00 2h" or "daily 03:30 1h"`
	parts := strings.Fields(strings.ToLower(s))
	if len(parts) != 3 {
		return Window{}, fmt.Errorf("maintenance_window %q is not a window; %s", s, shape)
	}

	var w Window
	if parts[0] == "daily" {
		w.Days = []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday}
	} else {
		seen := map[time.Weekday]bool{}
		for _, d := range strings.Split(parts[0], ",") {
			day, ok := weekdays[d]
			if !ok {
				return Window{}, fmt.Errorf("maintenance_window: %q is not a weekday; use sun, mon, tue, wed, thu, fri or sat, or daily", d)
			}
			if !seen[day] {
				seen[day] = true
				w.Days = append(w.Days, day)
			}
		}
	}

	hh, mm, ok := strings.Cut(parts[1], ":")
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if !ok || errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 || len(mm) != 2 {
		return Window{}, fmt.Errorf("maintenance_window: %q is not a time; use a 24-hour UTC time such as 02:00", parts[1])
	}
	w.Start = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute

	n, err := strconv.Atoi(strings.TrimSuffix(parts[2], "h"))
	if err != nil || !strings.HasSuffix(parts[2], "h") || n < 1 || n > 24 {
		return Window{}, fmt.Errorf("maintenance_window: %q is not a length; use whole hours from 1h to 24h", parts[2])
	}
	w.Length = time.Duration(n) * time.Hour
	return w, nil
}

// Open reports whether t falls inside the window.
func (w Window) Open(t time.Time) bool {
	t = t.UTC()
	for _, back := range []int{0, 1} {
		day := t.AddDate(0, 0, -back)
		opens := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC).Add(w.Start)
		if !w.on(day.Weekday()) {
			continue
		}
		if !t.Before(opens) && t.Before(opens.Add(w.Length)) {
			return true
		}
	}
	return false
}

func (w Window) on(d time.Weekday) bool {
	for _, x := range w.Days {
		if x == d {
			return true
		}
	}
	return false
}

// ValidateUpgrades refuses automatic upgrades that could never run, or that
// have no window to run in (R-361).
func (d Document) ValidateUpgrades() error {
	if d.MaintenanceWindow != "" {
		if _, err := ParseWindow(d.MaintenanceWindow); err != nil {
			return err
		}
	}
	if !d.AutoUpgradePatches {
		return nil
	}
	if !d.UpgradeInPlace {
		return fmt.Errorf("auto_upgrade_patches is on, but upgrade_in_place is off, so there is nothing to upgrade with; turn upgrade_in_place on or auto_upgrade_patches off")
	}
	if d.MaintenanceWindow == "" {
		return fmt.Errorf(`auto_upgrade_patches is on without a maintenance_window, so it could never start; set one, such as "sun 02:00 2h"`)
	}
	return nil
}
