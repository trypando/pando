package spec

import (
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// SetEnv sets a variable a person chose, on the workloads it belongs to.
//
// With a workload named, that workload only. Without one, every workload that
// already declares the key — a variable detection found in .env.example is
// usually read by one part, and naming the part is something the person should
// not have to know. A key no workload declares goes to the primary one: it is
// a variable the person is adding, and the primary workload is the app.
//
// The entry replaces any existing one with the key, and is marked as the
// person's (EnvFromUser), so it outlives a re-detection (R-022).
func SetEnv(s *AppSpec, workload, key string, entry EnvEntry) {
	entry.Key = key
	entry.Source = EnvFromUser

	set := func(w *Workload) {
		for i := range w.Env {
			if w.Env[i].Key == key {
				w.Env[i] = entry
				return
			}
		}
		w.Env = append(w.Env, entry)
	}

	if workload != "" {
		for i := range s.Workloads {
			if s.Workloads[i].Name == workload {
				set(&s.Workloads[i])
			}
		}
		return
	}

	declared := false
	for i := range s.Workloads {
		for _, e := range s.Workloads[i].Env {
			if e.Key == key {
				set(&s.Workloads[i])
				declared = true
				break
			}
		}
	}
	if declared {
		return
	}
	for i := range s.Workloads {
		if s.Workloads[i].Primary {
			set(&s.Workloads[i])
			return
		}
	}
	if len(s.Workloads) > 0 {
		set(&s.Workloads[0])
	}
}

// SlotSecretKey is the secret a literal slot value is stored under. The same
// name PUT /slots/{key} writes, so a value set during review and one set later
// on the settings screen are one value.
func SlotSecretKey(slot string) string { return "slot_" + slot }

// EnvSlot returns the slot a variable is filled from, when it is: the SlotRef
// on the entry with this key, on the named workload or, with none named, on
// any workload.
func EnvSlot(s *AppSpec, workload, key string) (string, bool) {
	for _, w := range s.Workloads {
		if workload != "" && w.Name != workload {
			continue
		}
		for _, e := range w.Env {
			// Not a part: a value typed for PGHOST is that host, not the
			// whole connection the slot holds.
			if e.Key == key && e.SlotRef != nil && e.SlotPart == "" {
				return *e.SlotRef, true
			}
		}
	}
	return "", false
}

// FillSlotLiteral fills a slot with a value somebody supplied, stored as the
// secret named secretRef (R-132).
//
// A variable filled from a slot was given a value by replacing its entry,
// which dropped the SlotRef and left the slot itself unfilled — and a required
// unfilled slot refuses the deploy, so a value typed during review did
// nothing but move the refusal to deploy time. The value belongs on the slot.
// A service slot filled this way stops being provisioned: the person has said
// what the app should connect to.
func FillSlotLiteral(s *AppSpec, slot, secretRef string) bool {
	return resolveSlot(s, slot, &Resolution{Mode: ResolutionLiteral, SecretRef: secretRef})
}

// FillSlotValue fills a slot with a value somebody chose not to keep secret:
// it is stored in the spec as the slot's target, which is what the variable
// is set to at deploy (a bound resolution). A URL or a domain name is not a
// secret, and forcing every slot value into one hid a value somebody wanted
// to read back.
func FillSlotValue(s *AppSpec, slot, value string) bool {
	return resolveSlot(s, slot, &Resolution{Mode: ResolutionBound, Target: value})
}

// MarkSlotOptional records that the app runs without a slot, because a person
// reviewing it said so: an optional slot nobody fills leaves its variable
// unset rather than refusing the deploy (R-132). Detection marks slots
// required from a crash or a template, and it can be wrong.
func MarkSlotOptional(s *AppSpec, slot string) bool {
	for i := range s.Slots {
		if s.Slots[i].Key == slot {
			s.Slots[i].Required = false
			return true
		}
	}
	return false
}

// hostnamePattern is a DNS name: dot-separated labels of letters, digits and
// inner hyphens.
var hostnamePattern = regexp.MustCompile(`^(?i)[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// ValidHostname reports whether h is a DNS name, with or without the dot
// that ends a fully qualified one.
func ValidHostname(h string) bool {
	h = strings.TrimSuffix(h, ".")
	return len(h) <= 253 && hostnamePattern.MatchString(h)
}

// SetHostname sets the address an app is served at, in subdomain mode — the
// one mode where the address is a name somebody chooses rather than a port
// Pando allocates or a prefix derived from the app's slug. Refused in any
// other mode, with the reason, rather than stored where nothing reads it.
func SetHostname(s *AppSpec, hostname string) error {
	hostname = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(hostname)), ".")
	if s.Routing.Mode != RoutingSubdomain {
		return errs.Newf(errs.ValidInvalid,
			"This app is reached by %s, so its address can't be set as a hostname.", s.Routing.Mode).
			WithRemedy("Leave the address as Pando assigned it, or configure a routing adapter that serves apps at their own hostname.")
	}
	if !ValidHostname(hostname) {
		return errs.Newf(errs.ValidInvalid, "%q isn't a hostname.", hostname).
			WithRemedy("Use a name like crew.example.com: letters, digits and hyphens, separated by dots.")
	}
	s.Routing.Hostname = hostname
	return nil
}

func resolveSlot(s *AppSpec, slot string, r *Resolution) bool {
	for i := range s.Slots {
		if s.Slots[i].Key == slot {
			s.Slots[i].Resolution = r
			return true
		}
	}
	return false
}

// ReadsSecret reports whether the spec puts a stored secret into a workload:
// an env entry that references it, or a slot filled with it.
//
// A secret is stored apart from the spec, and only a reference in the spec
// brings it into the app. One that nothing references is stored and never
// read — setting it and deploying changes nothing (R-193 covers a secret the
// app reads, not one it does not).
func ReadsSecret(s *AppSpec, key string) bool {
	if s == nil {
		return false
	}
	for _, w := range s.Workloads {
		for _, e := range w.Env {
			if e.SecretRef != nil && *e.SecretRef == key {
				return true
			}
		}
	}
	for _, sl := range s.Slots {
		if sl.Resolution != nil && sl.Resolution.SecretRef == key {
			return true
		}
	}
	return false
}
