package screening

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// MaxAmendments bounds one screening.
//
// Not a safety property — every amendment is validated individually — but a
// review a person cannot read is a review that does not happen (R-098), and a
// screener returning ninety changes to a spec with four workloads has
// misunderstood the question rather than found ninety things.
const MaxAmendments = 32

// Env is what Apply needs beyond the spec to decide.
type Env struct {
	// Source is the repository, read-only. Used to check that the paths an
	// amendment rests on exist (R-334) — the cheapest available check on
	// whether the screener read this repository or recalled a framework.
	//
	// A nil Source refuses every amendment: evidence that cannot be checked is
	// evidence that was not checked, and the honest failure is the one that
	// keeps the deterministic proposal.
	Source api.SourceView

	// Trial is what was observed (R-333), and whether the app crashed, which
	// is what decides whether a screener-added slot may be Required (O-4).
	Trial api.TrialSummary
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Apply folds amendments into s, returning what landed and what did not.
//
// It never returns an error. Every way an amendment can be wrong produces a
// Refused with a reason, because a screening that fails a detection is the
// thing R-335 forbids — and one bad amendment must not discard the good ones
// beside it.
func Apply(s *spec.AppSpec, env Env, amendments []api.Amendment) (applied []Applied, refused []Refused) {
	for i, a := range amendments {
		if i >= MaxAmendments {
			refused = append(refused, Refused{Amendment: a, Reason: fmt.Sprintf(
				"Pando applies at most %d amendments from one screening, and this screening proposed %d.",
				MaxAmendments, len(amendments))})
			continue
		}

		if reason := check(env, a); reason != "" {
			refused = append(refused, Refused{Amendment: a, Reason: reason})
			continue
		}

		summary, reason := one(s, env, a)
		if reason != "" {
			refused = append(refused, Refused{Amendment: a, Reason: reason})
			continue
		}
		applied = append(applied, Applied{Amendment: a, Summary: summary})
	}
	return applied, refused
}

// check is what every amendment must satisfy regardless of kind.
func check(env Env, a api.Amendment) string {
	if strings.TrimSpace(a.Reason) == "" {
		return "The amendment gave no reason. R-334: an amendment states why it was made."
	}
	if len(a.Evidence) == 0 {
		return "The amendment named no file it rests on. R-334: an amendment resting on nothing is refused."
	}
	if env.Source == nil {
		return "Pando had no readable copy of the repository to check the amendment's evidence against."
	}
	for _, e := range a.Evidence {
		clean := strings.TrimSpace(e)
		if clean == "" {
			continue
		}
		if !exists(env.Source, clean) {
			return fmt.Sprintf(
				"The amendment cites %s, which is not in this repository. Pando only accepts an "+
					"amendment whose evidence it can find.", clean)
		}
	}
	return ""
}

// one applies a single amendment. An empty reason means it landed.
func one(s *spec.AppSpec, env Env, a api.Amendment) (summary, reason string) {
	switch a.Kind {
	case api.AmendSetCommand:
		return setCommand(s, a)
	case api.AmendSetEnv:
		return setEnv(s, a)
	case api.AmendSetPort:
		return setPort(s, a)
	case api.AmendSetHealth:
		return setHealth(s, a)
	case api.AmendAddSlot:
		return addSlot(s, env, a)
	case api.AmendSetBuildContext:
		return setBuildContext(s, env, a)
	case api.AmendSetDockerfile:
		return setDockerfile(s, env, a)
	case api.AmendSetStaticDir:
		return setStaticDir(s, env, a)
	case api.AmendAddVolume:
		return addVolume(s, a)
	case api.AmendAddWarning:
		return addWarning(s, a)
	case api.AmendAnswerQuestion:
		// Handled by Split before Apply is called, through the same machinery a
		// person's answers go through. One arriving here is a caller bug.
		return "", "An answer to a detection question is not applied here."
	default:
		// The closed set doing its job (R-332). A kind nobody implemented says
		// nothing Pando can act on, which is the property, not a gap.
		return "", fmt.Sprintf("Pando has no amendment of kind %q.", a.Kind)
	}
}

func setCommand(s *spec.AppSpec, a api.Amendment) (string, string) {
	i, ok := workload(s, a.Workload)
	if !ok {
		return "", noWorkload(a.Workload)
	}

	command := a.Command
	if len(command) == 0 && strings.TrimSpace(a.Value) != "" {
		// A command line rather than an argv. Split on whitespace breaks the
		// first quoted argument anyone writes, so it is handed to a shell —
		// the same choice withStartCommand makes for a person's answer.
		command = []string{"sh", "-c", strings.TrimSpace(a.Value)}
	}
	if len(command) == 0 {
		return "", "The amendment set an empty command."
	}

	s.Workloads[i].Command = command
	return fmt.Sprintf("%s: command set to %s", s.Workloads[i].Name, strings.Join(command, " ")), ""
}

func setEnv(s *spec.AppSpec, a api.Amendment) (string, string) {
	i, ok := workload(s, a.Workload)
	if !ok {
		return "", noWorkload(a.Workload)
	}
	key := strings.TrimSpace(a.Key)
	if !envKey.MatchString(key) {
		return "", fmt.Sprintf("%q is not a usable environment variable name.", a.Key)
	}

	value := a.Value

	// A key the spec fills from a slot. A service slot is a resolved
	// dependency, not a literal somebody forgot: writing a literal over it
	// would replace a database URL Pando is about to provision with a string a
	// model wrote. A value slot — a key named with no value in .env.example,
	// type unknown — is a blank the deploy is refused without (R-132), and a
	// value the repository shows (the app's own domain, a mailto: address in
	// the README) is exactly what a screener is for. It fills the slot, left
	// readable; one already filled is not overwritten.
	for _, slot := range s.Slots {
		if slot.Key != key {
			continue
		}
		if slot.Type != spec.SlotUnknown {
			return "", fmt.Sprintf(
				"%s is filled from a declared dependency, so a literal value cannot be set for it.", key)
		}
		if slot.Resolution != nil {
			return "", fmt.Sprintf("%s already has a value, and a screening does not overwrite it.", key)
		}
		if strings.TrimSpace(value) == "" {
			return "", fmt.Sprintf("The amendment set %s to nothing.", key)
		}
		spec.FillSlotValue(s, key, value)
		return fmt.Sprintf("%s set to %s", key, value), ""
	}
	for j, e := range s.Workloads[i].Env {
		if e.Key != key {
			continue
		}
		if e.SlotRef != nil || e.SecretRef != nil {
			return "", fmt.Sprintf(
				"%s already resolves from a dependency or a secret, and a screening does not overwrite that.", key)
		}
		if e.Source == spec.EnvFromUser {
			// R-022's rule, applied here: a decision a person made outlives
			// anything Pando worked out for itself, and a screener is not a
			// person.
			return "", fmt.Sprintf("%s was set by hand, and a screening does not overwrite that.", key)
		}
		s.Workloads[i].Env[j].Value = &value
		s.Workloads[i].Env[j].SlotRef = nil
		s.Workloads[i].Env[j].SecretRef = nil
		s.Workloads[i].Env[j].Source = spec.EnvFromScreening
		return fmt.Sprintf("%s: %s set to %s", s.Workloads[i].Name, key, value), ""
	}

	s.Workloads[i].Env = append(s.Workloads[i].Env, spec.EnvEntry{
		Key: key, Value: &value, Source: spec.EnvFromScreening,
	})
	return fmt.Sprintf("%s: %s set to %s", s.Workloads[i].Name, key, value), ""
}

// setPort is R-333's refusal.
//
// The asymmetry runs one way only. Where nothing was observed a screened port
// beats a question; where something was, R-097 exists because watching the
// process bind beats asking about it, and a model's reading of a framework's
// documentation is a more elaborate form of asking.
func setPort(s *spec.AppSpec, a api.Amendment) (string, string) {
	i, ok := workload(s, a.Workload)
	if !ok {
		return "", noWorkload(a.Workload)
	}
	if a.Port <= 0 || a.Port > 65535 {
		return "", fmt.Sprintf("%d is not a usable port number.", a.Port)
	}
	for _, p := range s.Workloads[i].Ports {
		if p.Source == spec.PortObserved {
			return "", fmt.Sprintf(
				"Pando watched this app bind port %d while testing it. An observation is not "+
					"overruled by a screening.", p.Number)
		}
	}

	s.Workloads[i].Ports = []spec.Port{{
		Number: a.Port, Protocol: "http", Source: spec.PortScreened,
	}}
	return fmt.Sprintf("%s: port set to %d", s.Workloads[i].Name, a.Port), ""
}

func setHealth(s *spec.AppSpec, a api.Amendment) (string, string) {
	if !strings.HasPrefix(a.Path, "/") {
		return "", fmt.Sprintf("%q is not a usable health check path — it has to start with /.", a.Path)
	}
	port := a.Port
	if port == 0 {
		if i, ok := workload(s, a.Workload); ok {
			port, _ = s.Workloads[i].HTTPPort()
		}
	}
	if port <= 0 || port > 65535 {
		return "", "The amendment gave no port for the health check, and Pando has none to fall back on."
	}

	// Interval, timeout and retries are left alone: they came from the
	// install's defaults (R-104 configuration), and a screener has nothing to
	// say about them.
	s.Health.Source = spec.HealthFromHTTP
	s.Health.Path = a.Path
	s.Health.Port = port
	return fmt.Sprintf("health check set to GET %s on port %d", a.Path, port), ""
}

// addSlot declares a dependency the detectors missed.
//
// Required is honored only on a crashed trial run. That is not a rule invented
// here: O-4's [P] fallback in design 01 §2.5 already says to default
// Required: false and let the trial run settle it, promoting a slot whose
// absence crashed the app with the crash log as evidence. A screener that could
// mark slots required freely would turn "this app might not start" into "this
// app cannot be deployed" (R-132) — a blocker where configuration would do
// (R-104).
func addSlot(s *spec.AppSpec, env Env, a api.Amendment) (string, string) {
	key := strings.TrimSpace(a.Key)
	if !envKey.MatchString(key) {
		return "", fmt.Sprintf("%q is not a usable name for a dependency.", a.Key)
	}
	for _, slot := range s.Slots {
		if slot.Key == key {
			return "", fmt.Sprintf("%s is already declared as a dependency.", key)
		}
	}

	t := a.SlotType
	if t == "" {
		t = spec.SlotUnknown
	}
	switch t {
	case spec.SlotPostgres, spec.SlotMySQL, spec.SlotRedis, spec.SlotMongoDB, spec.SlotS3, spec.SlotSMTP, spec.SlotUnknown:
	default:
		return "", fmt.Sprintf("Pando does not recognize %q as a kind of dependency.", a.SlotType)
	}

	required := a.Required && env.Trial.Crashed
	evidence := append([]string{a.Reason}, a.Evidence...)
	s.Slots = append(s.Slots, spec.Slot{
		Key: key, Type: t, Required: required, Evidence: evidence,
	})

	if required {
		return fmt.Sprintf("%s declared as a required %s dependency", key, t), ""
	}
	return fmt.Sprintf("%s declared as an optional %s dependency", key, t), ""
}

func setBuildContext(s *spec.AppSpec, env Env, a api.Amendment) (string, string) {
	clean, reason := sourcePath(env, a.Path)
	if reason != "" {
		return "", reason
	}
	s.Build.Context = clean
	return "build context set to " + clean, ""
}

func setDockerfile(s *spec.AppSpec, env Env, a api.Amendment) (string, string) {
	clean, reason := sourcePath(env, a.Path)
	if reason != "" {
		return "", reason
	}
	s.Build.Dockerfile = clean
	s.Build.Strategy = spec.BuildDockerfile
	return "built from " + clean, ""
}

func setStaticDir(s *spec.AppSpec, env Env, a api.Amendment) (string, string) {
	clean, reason := sourcePath(env, a.Path)
	if reason != "" {
		return "", reason
	}
	s.Build.StaticDir = clean
	s.Build.Strategy = spec.BuildStatic
	return "served as static files from " + clean, ""
}

func addVolume(s *spec.AppSpec, a api.Amendment) (string, string) {
	i, ok := workload(s, a.Workload)
	if !ok {
		return "", noWorkload(a.Workload)
	}
	mount := path.Clean(a.Path)
	if !strings.HasPrefix(mount, "/") {
		return "", fmt.Sprintf("%q is not a usable place to keep data — it has to be an absolute path.", a.Path)
	}
	for _, m := range s.Workloads[i].Mounts {
		if path.Clean(m.Path) == mount {
			return "", fmt.Sprintf("%s already keeps its data at %s.", s.Workloads[i].Name, mount)
		}
	}

	name := volumeName(s, mount)
	s.Volumes = append(s.Volumes, spec.Volume{
		ID: name, Name: name, Declared: spec.VolumeFromScreening,
	})
	s.Workloads[i].Mounts = append(s.Workloads[i].Mounts, spec.Mount{
		VolumeID: name, Path: mount,
	})
	return fmt.Sprintf("%s: %s kept in a volume", s.Workloads[i].Name, mount), ""
}

// addWarning says something and changes nothing.
//
// The code is not the screener's to pick. The codes in design 01 §2.8 mean
// specific things that specific parts of Pando produced, and a screener able to
// emit WARN_COMPOSE_CONSTRUCT_REWRITTEN can claim the compose importer rewrote
// something it never touched.
func addWarning(s *spec.AppSpec, a api.Amendment) (string, string) {
	message := strings.TrimSpace(a.Value)
	if message == "" {
		message = strings.TrimSpace(a.Reason)
	}
	s.Warnings = append(s.Warnings, spec.Warning{
		Code: spec.WarnScreeningAdvisory, Message: message,
	})
	return "noted: " + message, ""
}

// sourcePath validates a path an amendment points at inside the repository.
func sourcePath(env Env, p string) (string, string) {
	clean := strings.TrimSpace(p)
	if clean == "" {
		return "", "The amendment named no path."
	}
	if strings.HasPrefix(clean, "/") {
		return "", fmt.Sprintf("%q is outside the repository. Paths are relative to it.", p)
	}
	clean = path.Clean(clean)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Sprintf("%q points outside the repository.", p)
	}
	if !exists(env.Source, clean) {
		return "", fmt.Sprintf("%s is not in this repository.", clean)
	}
	return clean, ""
}

func exists(src api.SourceView, p string) bool {
	if src == nil {
		return false
	}
	_, err := src.Stat(p)
	return err == nil
}

// workload resolves a name to an index. An empty name means the primary, which
// is what an amendment that does not say means.
func workload(s *spec.AppSpec, name string) (int, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		for i, w := range s.Workloads {
			if w.Primary {
				return i, true
			}
		}
		if len(s.Workloads) == 1 {
			return 0, true
		}
		return 0, false
	}
	for i, w := range s.Workloads {
		if w.Name == name {
			return i, true
		}
	}
	return 0, false
}

func noWorkload(name string) string {
	if strings.TrimSpace(name) == "" {
		return "The amendment did not say which part of the app it meant, and this app has no single primary one."
	}
	return fmt.Sprintf("This app has no part called %q.", name)
}

// volumeName derives a stable name from the mount path, the way the compose
// importer uses a declared name: an ID that is the same across two runs of the
// same detection, so accepting a proposal twice does not make two volumes.
func volumeName(s *spec.AppSpec, mount string) string {
	base := strings.Trim(mount, "/")
	base = strings.ReplaceAll(base, "/", "-")
	if base == "" {
		base = "data"
	}

	name := base
	for n := 2; taken(s, name); n++ {
		name = fmt.Sprintf("%s-%d", base, n)
	}
	return name
}

func taken(s *spec.AppSpec, name string) bool {
	for _, v := range s.Volumes {
		if v.ID == name || v.Name == name {
			return true
		}
	}
	return false
}
