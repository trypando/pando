package spec

import (
	"path"
	"path/filepath"
	"strings"

	"github.com/trypando/pando/internal/egress"
	"github.com/trypando/pando/internal/errs"
)

// Validate checks a spec's internal consistency.
//
// Run on every spec before it is pinned, and again at plan time (design 01 §3).
// Everything here is answerable from the spec alone — rules needing adapters or
// policy belong to the planner, which is phase 3, and are deliberately absent so
// that validation can run without any of that being configured.
//
// Errors accumulate rather than short-circuiting. Someone hand-writing a spec
// deserves every problem at once, not one per round trip.
func Validate(s *AppSpec) error {
	var problems []*errs.Error

	add := func(e *errs.Error) { problems = append(problems, e) }

	if s.SchemaVersion != SchemaVersion {
		add(errs.Newf(errs.ValidInvalid,
			"This spec is written for schema version %d, and this version of Pando understands version %d.",
			s.SchemaVersion, SchemaVersion).
			WithRemedy("Export the spec again from the version of Pando that produced it, or write it against the current schema."))
	}

	validatePrimaryWorkload(s, add)
	validateWorkloadNames(s, add)
	validateMounts(s, add)
	validateFiles(s, add)
	validateEnv(s, add)
	validateDependencies(s, add)
	validateSlots(s, add)
	validateRouting(s, add)
	validateBuild(s, add)
	validateEgress(s, add)
	validateAutoDeploy(s, add)

	switch len(problems) {
	case 0:
		return nil
	case 1:
		return problems[0]
	default:
		// Multiple problems collapse into one envelope carrying all of them,
		// because the API returns a single error and dropping the rest would
		// mean the caller fixes one and learns about the next on the next try.
		details := make([]map[string]any, 0, len(problems))
		for _, p := range problems {
			details = append(details, map[string]any{
				"code":    string(p.Code),
				"message": p.Message,
			})
		}
		return errs.Newf(errs.ValidInvalid, "This spec has %d problems.", len(problems)).
			WithDetail("problems", details)
	}
}

// validateBuild keeps generated build inputs inside the build context.
//
// A generated file is written into the checkout before the build, so a path
// that climbs out of it writes wherever it likes on the machine running the
// build. The paths are Pando's own today, and that is exactly the assumption
// worth not depending on: a spec is editable, exportable and importable, and an
// imported one is untrusted input like any other (design 01 §5).
func validateBuild(s *AppSpec, add func(*errs.Error)) {
	// A compose app builds per workload, so at least one of them has to say
	// how. Without this, a compose spec with no buildable service and no images
	// deploys workloads that have nothing to run, and the first sign is a
	// container that exits immediately.
	if s.Build.Strategy == BuildCompose {
		var buildable, imaged int
		for _, w := range s.Workloads {
			if w.Build != nil {
				buildable++
			}
			if w.Image != "" {
				imaged++
			}
		}
		if buildable+imaged < len(s.Workloads) {
			add(errs.New(errs.ValidInvalid,
				"Every part of a compose app needs either an image or instructions for building one.").
				WithRemedy("Give each service in the compose file an `image:` or a `build:`."))
		}
	}

	for name := range s.Build.GeneratedFiles {
		clean := path.Clean("/" + filepath.ToSlash(name))
		if strings.TrimPrefix(clean, "/") != filepath.ToSlash(name) || name == "" {
			add(errs.Newf(errs.ValidInvalid,
				"A generated build file is written to %q, which is not a path inside the app's source.", name).
				WithRemedy("Generated build files are relative paths without a leading slash or any \"..\" segment."))
		}
	}
}

// validatePrimaryWorkload asserts exactly one primary workload.
//
// The app has one canonical endpoint (R-026 and the bundle model). Zero means
// the app's URL resolves to nothing; more than one means it resolves
// arbitrarily, which is worse than failing.
func validatePrimaryWorkload(s *AppSpec, add func(*errs.Error)) {
	var primary []string
	for _, w := range s.Workloads {
		if w.Primary {
			primary = append(primary, w.Name)
		}
	}

	switch len(primary) {
	case 1:
		return
	case 0:
		if len(s.Workloads) == 0 {
			add(errs.New(errs.ValidPrimaryWorkload, "This app has no workloads, so there is nothing to run.").
				WithRemedy("Add at least one workload, and mark the one your app's URL should point at as primary."))
			return
		}
		add(errs.New(errs.ValidPrimaryWorkload,
			"None of this app's workloads is marked as the primary one, so Pando does not know which to send traffic to.").
			WithRemedy("Mark exactly one workload as primary.").
			WithDetail("workloads", workloadNames(s)))
	default:
		add(errs.Newf(errs.ValidPrimaryWorkload,
			"This app marks %d workloads as primary, and only one can be.", len(primary)).
			WithRemedy("Mark exactly one workload as primary. The others can still be exposed.").
			WithDetail("primary_workloads", primary))
	}
}

func validateWorkloadNames(s *AppSpec, add func(*errs.Error)) {
	seen := make(map[string]struct{}, len(s.Workloads))
	for _, w := range s.Workloads {
		if strings.TrimSpace(w.Name) == "" {
			add(errs.New(errs.ValidInvalid, "A workload has no name.").
				WithRemedy("Give every workload a name — it is how mounts, dependencies, and logs refer to it."))
			continue
		}
		if _, dup := seen[w.Name]; dup {
			add(errs.Newf(errs.ValidInvalid, "Two workloads are both named %q.", w.Name).
				WithRemedy("Workload names must be unique within an app."))
		}
		seen[w.Name] = struct{}{}
	}
}

// validateMounts asserts every mount resolves to a declared volume.
//
// A dangling mount would otherwise produce a workload that starts with an empty
// directory where its data should be — healthy-looking and wrong, which is the
// failure mode R-203 exists to avoid.
func validateMounts(s *AppSpec, add func(*errs.Error)) {
	for _, w := range s.Workloads {
		for _, m := range w.Mounts {
			if _, ok := s.Volume(m.VolumeID); !ok {
				add(errs.Newf(errs.ValidDanglingMount,
					"Workload %q mounts storage at %q that this app does not declare.", w.Name, m.Path).
					WithRemedy("Add the volume to the app, or remove the mount.").
					WithDetail("workload", w.Name).
					WithDetail("volume_id", m.VolumeID))
			}
			if !strings.HasPrefix(m.Path, "/") {
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q mounts storage at %q, which is not an absolute path.", w.Name, m.Path).
					WithRemedy("Mount paths must start with /, for example /app/data."))
			}
		}
	}
}

// validateFiles asserts a carried file has somewhere to land and is small
// enough to be carried.
func validateFiles(s *AppSpec, add func(*errs.Error)) {
	for _, w := range s.Workloads {
		seen := map[string]bool{}
		for _, f := range w.Files {
			switch {
			case !strings.HasPrefix(f.Path, "/"):
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q carries a file for %q, which is not an absolute path.", w.Name, f.Path).
					WithRemedy("File paths must start with /, for example /etc/caddy/Caddyfile."))
			case strings.HasSuffix(f.Path, "/"):
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q carries a file for %q, which names a directory.", w.Name, f.Path).
					WithRemedy("Give the full path including the file name."))
			case seen[f.Path]:
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q carries two files for %q.", w.Name, f.Path).
					WithRemedy("Remove one of them. Which would win is not defined."))
			}
			seen[f.Path] = true

			if len(f.Content) > FileSizeLimit {
				add(errs.Newf(errs.ValidInvalid,
					"The file %q on workload %q is %d KB, and Pando carries files up to %d KB.",
					f.Path, w.Name, len(f.Content)/1024, FileSizeLimit/1024).
					WithRemedy("Build it into the image instead — a file this size is a build input."))
			}

			// A mount and a file at the same path is two mechanisms for one
			// location, and which one the container ends up with depends on
			// the runtime.
			for _, m := range w.Mounts {
				if m.Path == f.Path {
					add(errs.Newf(errs.ValidInvalid,
						"Workload %q both mounts storage and carries a file at %q.", w.Name, f.Path).
						WithRemedy("Keep one. Storage is for data the app writes; a carried file is configuration."))
				}
			}
		}
	}
}

// validateEnv asserts every entry has exactly one source, and that slot and
// secret references resolve.
func validateEnv(s *AppSpec, add func(*errs.Error)) {
	for _, w := range s.Workloads {
		for _, e := range w.Env {
			if strings.TrimSpace(e.Key) == "" {
				add(errs.Newf(errs.ValidInvalid, "Workload %q has an environment variable with no name.", w.Name))
				continue
			}

			sources := 0
			for _, set := range []bool{e.Value != nil, e.SlotRef != nil, e.SecretRef != nil} {
				if set {
					sources++
				}
			}
			switch {
			case sources == 0:
				add(errs.Newf(errs.ValidEnvAmbiguous,
					"Environment variable %s on workload %q has no value, and no slot or secret to take one from.",
					e.Key, w.Name).
					WithRemedy("Give it a literal value, point it at a slot, or point it at a secret."))
			case sources > 1:
				add(errs.Newf(errs.ValidEnvAmbiguous,
					"Environment variable %s on workload %q has more than one source, so Pando cannot tell which to use.",
					e.Key, w.Name).
					WithRemedy("Use exactly one of: a literal value, a slot reference, or a secret reference."))
			}

			if e.SlotRef != nil {
				if _, ok := s.Slot(*e.SlotRef); !ok {
					add(errs.Newf(errs.ValidDanglingSlotRef,
						"Environment variable %s on workload %q refers to a slot %q that this app does not declare.",
						e.Key, w.Name, *e.SlotRef).
						WithRemedy("Declare the slot, or point the variable somewhere else.").
						WithDetail("slot_key", *e.SlotRef))
				}
			}
		}
	}
}

// validateDependencies asserts depends_on names exist and form no cycle.
func validateDependencies(s *AppSpec, add func(*errs.Error)) {
	names := make(map[string]struct{}, len(s.Workloads))
	for _, w := range s.Workloads {
		names[w.Name] = struct{}{}
	}

	edges := make(map[string][]string, len(s.Workloads))
	for _, w := range s.Workloads {
		for _, dep := range w.DependsOn {
			if _, ok := names[dep]; !ok {
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q says it depends on %q, which this app does not define.", w.Name, dep).
					WithRemedy("Check the name, or remove the dependency."))
				continue
			}
			edges[w.Name] = append(edges[w.Name], dep)
		}
	}

	if cycle := findCycle(edges); cycle != nil {
		add(errs.Newf(errs.ValidDependencyCycle,
			"These workloads depend on each other in a loop, so none of them could ever start: %s.",
			strings.Join(cycle, " → ")).
			WithRemedy("Break the loop by removing one of the dependencies.").
			WithDetail("cycle", cycle))
	}
}

// findCycle returns a cycle if one exists, as a readable path.
func findCycle(edges map[string][]string) []string {
	const (
		unvisited = 0
		inStack   = 1
		done      = 2
	)
	state := map[string]int{}
	var path []string
	var cycle []string

	var visit func(string) bool
	visit = func(n string) bool {
		state[n] = inStack
		path = append(path, n)

		for _, next := range edges[n] {
			switch state[next] {
			case inStack:
				// Trim the path to where the cycle actually begins, so the
				// message names the loop rather than the route to it.
				for i, p := range path {
					if p == next {
						cycle = append(append([]string{}, path[i:]...), next)
						return true
					}
				}
				return true
			case unvisited:
				if visit(next) {
					return true
				}
			}
		}

		path = path[:len(path)-1]
		state[n] = done
		return false
	}

	names := make([]string, 0, len(edges))
	for n := range edges {
		names = append(names, n)
	}
	sortStrings(names)

	for _, n := range names {
		if state[n] == unvisited && visit(n) {
			return cycle
		}
	}
	return nil
}

// validateSlots asserts slot keys are unique and resolutions are well formed.
//
// Whether a *required* slot is filled is checked at plan time, not here: R-132
// is a PLAN_ error because it blocks a deploy, and a spec with an unfilled slot
// is still a valid spec worth saving.
func validateSlots(s *AppSpec, add func(*errs.Error)) {
	seen := make(map[string]struct{}, len(s.Slots))
	for _, sl := range s.Slots {
		if strings.TrimSpace(sl.Key) == "" {
			add(errs.New(errs.ValidInvalid, "A slot has no key."))
			continue
		}
		if _, dup := seen[sl.Key]; dup {
			add(errs.Newf(errs.ValidInvalid, "Two slots are both named %q.", sl.Key))
		}
		seen[sl.Key] = struct{}{}

		if sl.Resolution == nil {
			continue
		}
		switch sl.Resolution.Mode {
		case ResolutionProvisioned:
			// ServiceRef is filled in at deploy time, so it may be empty here.
		case ResolutionBound:
			if sl.Resolution.Target == "" {
				add(errs.Newf(errs.ValidInvalid,
					"Slot %s is set to connect to something existing, but no address was given.", sl.Key).
					WithRemedy("Provide the address to connect to, or choose a different way to fill the slot."))
			}
		case ResolutionLiteral:
			if sl.Resolution.SecretRef == "" {
				add(errs.Newf(errs.ValidInvalid,
					"Slot %s is set to a literal value, but no stored secret holds it.", sl.Key).
					WithRemedy("Set the value through the secrets API — literal slot values are stored as secrets so that exporting a spec stays safe."))
			}
		default:
			add(errs.Newf(errs.ValidInvalid,
				"Slot %s has an unrecognized resolution mode %q.", sl.Key, sl.Resolution.Mode).
				WithRemedy("Use one of: provisioned, bound, literal."))
		}
	}
}

// validateRouting checks the shape of the routing block.
//
// Whether the adapter *supports* the mode is a plan-time question
// (PLAN_CAPABILITY_UNSUPPORTED), because it depends on configured adapters.
func validateRouting(s *AppSpec, add func(*errs.Error)) {
	switch s.Routing.Mode {
	case RoutingSubdomain:
		if s.Routing.Hostname == "" {
			add(errs.New(errs.ValidInvalid, "This app is set to use its own hostname, but no hostname was given.").
				WithRemedy("Provide the hostname, for example notes.example.com."))
		}
	case RoutingPath:
		if s.Routing.PathPrefix == "" {
			add(errs.New(errs.ValidInvalid, "This app is set to be reached at a path, but no path was given.").
				WithRemedy("Provide the path prefix, for example /notes."))
		} else if err := CheckPathPrefix(s.Routing.PathPrefix); err != nil {
			add(err)
		}
	case RoutingPort:
		if s.Routing.Port <= 0 || s.Routing.Port > 65535 {
			add(errs.Newf(errs.ValidInvalid, "%d is not a usable port number.", s.Routing.Port).
				WithRemedy("Use a port between 1 and 65535."))
		}
	case "":
		add(errs.New(errs.ValidInvalid, "This app does not say how it should be reached.").
			WithRemedy("Choose a routing mode: its own hostname, a path, or a port."))
	default:
		add(errs.Newf(errs.ValidInvalid, "%q is not a routing mode Pando recognizes.", s.Routing.Mode).
			WithRemedy("Use one of: subdomain, path, port."))
	}
}

func workloadNames(s *AppSpec) []string {
	out := make([]string, 0, len(s.Workloads))
	for _, w := range s.Workloads {
		out = append(out, w.Name)
	}
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// validateEgress checks an app's egress rules can be read (R-185). Whether
// they are allowed is policy's question, answered against the install's rules
// (R-183); this is only whether they mean anything.
func validateEgress(s *AppSpec, add func(*errs.Error)) {
	e := s.Egress
	e.Normalize()
	switch e.Mode {
	case EgressInherit:
		if len(e.List) > 0 {
			add(errs.New(errs.ValidInvalid,
				"This app lists egress destinations of its own but does not say whether they are allowed or denied.").
				WithRemedy("Set egress.mode to allowlist (only these may be reached) or denylist (these may not be reached), or remove egress.list."))
		}
	case EgressAllowlist, EgressDenylist:
	default:
		add(errs.Newf(errs.ValidInvalid,
			"Egress mode %q is not one Pando knows.", e.Mode).
			WithRemedy("Use inherit to keep the installation's rules, or allowlist or denylist for a list of the app's own on top of them."))
	}
	for _, field := range []struct {
		name  string
		items []string
	}{{"egress.list", e.List}, {"egress.add", e.Add}, {"egress.remove", e.Remove}} {
		for _, raw := range field.items {
			if _, err := egress.Parse(raw); err != nil {
				add(errs.Newf(errs.ValidInvalid, "In %s, %s.", field.name, err.Error()))
			}
		}
	}
	if s.Deploy.RequireApproval && s.Deploy.AutoDeploy.Enabled {
		add(errs.New(errs.ValidInvalid,
			"This app asks for its deploys to be approved and also deploys automatically, and the two cannot be combined.").
			WithRemedy("Turn off automatic deploys, or stop requiring approval. An approval request for every push would queue up faster than anyone could read it (R-158)."))
	}
}

// validateAutoDeploy checks the automatic deploy settings (R-141).
func validateAutoDeploy(s *AppSpec, add func(*errs.Error)) {
	ad := s.Deploy.AutoDeploy
	switch ad.Trigger {
	case "", TriggerBranchUpdated, TriggerReleaseTagged:
	default:
		add(errs.Newf(errs.ValidInvalid,
			"Automatic deploy trigger %q is not one Pando knows.", ad.Trigger).
			WithRemedy("Use branch_updated to deploy each new commit on a branch, or release_tagged to deploy each new release tag."))
	}
	if _, err := path.Match(ad.TagPattern, ""); err != nil {
		add(errs.Newf(errs.ValidInvalid,
			"The release tag pattern %q is not a valid pattern.", ad.TagPattern).
			WithRemedy("Use a pattern such as release-* or v*, where * matches any characters. Leave it empty to count tags like v1.2.3 as releases."))
	}
}
