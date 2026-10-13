package deploy

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// EnvFingerprint summarizes the environment an app was applied with.
//
// This is the whole mechanism behind R-193 — "a rotated secret reaches the app"
// — and it exists because `Observe` returns no environment and deliberately
// never will. Reading environment back would require every runtime adapter to
// handle secret-bearing data, which is precisely what WorkloadPlan.Env's
// one-way flow is designed to prevent. So the comparison happens state-side:
// what was applied, against what would be applied now.
//
// **It hashes secret versions, never secret values.** A fingerprint is stored
// in a column, shown in logs when it changes, and survives in backups. Hashing
// the value would put an oracle for every secret in the app into all three —
// an attacker with the column and a guess could confirm the guess. Hashing
// (key, version) answers the only question being asked, which is whether the
// thing changed, and answers nothing else.
func EnvFingerprint(s *spec.AppSpec, versions map[string]int) string {
	var parts []string

	for _, w := range s.Workloads {
		for _, e := range w.Env {
			switch {
			case e.Value != nil:
				// A literal in the spec is not a secret — it is already in the
				// spec revision, readable by anyone who can read the app.
				parts = append(parts, w.Name+"\x00"+e.Key+"\x00literal\x00"+*e.Value)

			case e.SecretRef != nil:
				parts = append(parts, w.Name+"\x00"+e.Key+"\x00secret\x00"+*e.SecretRef+
					"\x00v"+strconv.Itoa(versions[*e.SecretRef]))

			case e.SlotRef != nil:
				part := w.Name + "\x00" + e.Key + "\x00slot\x00" + *e.SlotRef +
					"\x00" + slotResolution(s, *e.SlotRef)
				// Only when set, so a spec with no parts fingerprints as it
				// always did and no running app reads as drifted.
				if e.SlotPart != "" {
					part += "\x00part\x00" + string(e.SlotPart)
				}
				parts = append(parts, part)
			}
		}
	}

	// Sorted, because a spec whose workloads or env entries were reordered
	// without changing has not drifted, and reporting that it had would restart
	// every app the first time someone tidied a YAML file.
	sort.Strings(parts)

	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(sum[:])
}

// slotResolution renders how a slot is filled, without its value.
//
// A slot's resolution can change from provisioned to bound, or point at a
// different target, and either means the app is talking to something else — so
// it belongs in the fingerprint. The secret holding a literal value does not:
// its version covers it.
func slotResolution(s *spec.AppSpec, key string) string {
	slot, ok := s.Slot(key)
	if !ok || slot.Resolution == nil {
		return "unresolved"
	}
	r := slot.Resolution
	return string(r.Mode) + "\x00" + r.ServiceRef + "\x00" + r.Target + "\x00" + r.SecretRef
}

// PlanFingerprint summarizes a resolved bundle plan's environment.
//
// Used where the spec is not at hand but the plan is. It takes the resolved
// values, so it hashes them — which is safe only because it is compared
// in-process and never stored. EnvFingerprint is the one that persists.
func PlanFingerprint(p api.BundlePlan) string {
	var parts []string
	for _, w := range p.Workloads {
		for k := range w.Env {
			parts = append(parts, w.Name+"\x00"+k)
		}
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1e")))
	return hex.EncodeToString(sum[:])
}
