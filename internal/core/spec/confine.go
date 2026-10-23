package spec

import (
	"path"
	"regexp"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// What a spec may say about the machine it runs on, past its own shape:
// which upload it deploys, how much of the host each workload takes, where a
// volume may be mounted, and which names a workload answers to. Each is a
// field an author writes and a runtime acts on, so each is held here rather
// than trusted to arrive as Pando would have written it.

// CheckUploadOwner refuses an uploaded source that names an upload other than
// the app's own. An app's upload is stored under its own ID (design 04 §4),
// and naming another would deploy what was uploaded to that app.
func CheckUploadOwner(src Source, appID string) error {
	if src.Type != SourceUpload || src.UploadID == "" || src.UploadID == appID {
		return nil
	}
	return errs.Newf(errs.ValidInvalid,
		"This app's source is an upload, and %q is not this app's upload. An app deploys only the source uploaded to it.", src.UploadID).
		WithRemedy("Leave source.upload_id out, or set it to " + appID + ".")
}

// WorkloadLimits is what a workload runs limited to (R-240): its own CPU and
// memory where it sets them, the app's where it does not, and never more than
// the app's. A runtime reads zero as no limit at all, so a workload that set
// its resources to nothing would otherwise run unbounded.
func (s *AppSpec) WorkloadLimits(w Workload) ResourceLimits {
	var own ResourceLimits
	if w.Resources != nil {
		own = *w.Resources
	}
	return LimitWithin(own, s.Resources)
}

// LimitWithin is own, with each value that is unset or more than the app's
// replaced by the app's.
func LimitWithin(own ResourceLimits, app Resources) ResourceLimits {
	out := ResourceLimits{CPUMillis: app.CPUMillis, MemoryBytes: app.MemoryBytes}
	if own.CPUMillis > 0 && (app.CPUMillis <= 0 || own.CPUMillis < app.CPUMillis) {
		out.CPUMillis = own.CPUMillis
	}
	if own.MemoryBytes > 0 && (app.MemoryBytes <= 0 || own.MemoryBytes < app.MemoryBytes) {
		out.MemoryBytes = own.MemoryBytes
	}
	return out
}

// validateResources refuses a negative limit, which no runtime reads as a
// size. Zero is unset, and is the app's (WorkloadLimits).
func validateResources(s *AppSpec, add func(*errs.Error)) {
	if s.Resources.CPUMillis < 0 || s.Resources.MemoryBytes < 0 || s.Resources.DiskBytes < 0 {
		add(errs.New(errs.ValidInvalid, "This app's CPU, memory or disk limit is negative.").
			WithRemedy("Give each limit as a positive number, or leave it out to use the installation's default."))
	}
	for _, w := range s.Workloads {
		if w.Resources != nil && (w.Resources.CPUMillis < 0 || w.Resources.MemoryBytes < 0) {
			add(errs.Newf(errs.ValidInvalid, "Workload %q has a negative CPU or memory limit.", w.Name).
				WithRemedy("Give each limit as a positive number, or leave it out to use the app's."))
		}
	}
}

// reservedMountRoots are where the kernel presents itself inside a container.
// Storage mounted over one hides or replaces what the runtime put there.
var reservedMountRoots = []string{"/proc", "/sys", "/dev"}

// MountPathProblem is why a mount path cannot be used, or empty when it can.
func MountPathProblem(p string) string {
	if !strings.HasPrefix(p, "/") {
		return "is not an absolute path"
	}
	if strings.Contains(p, ":") {
		// A runtime that joins volume, path and options with colons would
		// read what follows as options.
		return "contains a colon"
	}
	clean := path.Clean(p)
	for _, root := range reservedMountRoots {
		if clean == root || strings.HasPrefix(clean, root+"/") {
			return "is under " + root + ", which the container runtime provides"
		}
	}
	return ""
}

// aliasPattern is a name a container answers to on a network: what compose
// accepts as a container_name, which is how most aliases arrive (issue #90).
var aliasPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)

// ReservedName reports whether a workload name or alias is in the space
// Pando names its own containers and networks in ("pando-<app>-<workload>",
// "pando-egress"). Pando's own container shares every app's network, so an
// app answering to one of those names could receive traffic meant for another.
func ReservedName(name string) bool {
	n := strings.ToLower(name)
	return n == "pando" || strings.HasPrefix(n, "pando-")
}

// AliasProblem is why a network alias cannot be used, or empty when it can.
func AliasProblem(alias string) string {
	switch {
	case !aliasPattern.MatchString(alias):
		return "is not a network name: letters, digits, dots, hyphens and underscores, starting with a letter or digit, up to 63 characters"
	case ReservedName(alias):
		return "starts with pando-, which Pando keeps for its own containers"
	}
	return ""
}

// validateNames holds workload names and aliases to the names a workload may
// answer to on its network.
func validateNames(s *AppSpec, add func(*errs.Error)) {
	names := make(map[string]bool, len(s.Workloads))
	for _, w := range s.Workloads {
		names[w.Name] = true
		if ReservedName(w.Name) {
			add(errs.Newf(errs.ValidInvalid,
				"A workload is named %q, and names starting with pando- are kept for Pando's own containers.", w.Name).
				WithRemedy("Rename the workload, for example to " + strings.TrimPrefix(strings.ToLower(w.Name), "pando-") + "-app."))
		}
	}
	for _, w := range s.Workloads {
		for _, a := range w.Aliases {
			if problem := AliasProblem(a); problem != "" {
				add(errs.Newf(errs.ValidInvalid, "Workload %q answers to %q, which %s.", w.Name, a, problem).
					WithRemedy("Remove the alias, or use a name like " + w.Name + "-db."))
				continue
			}
			if a != w.Name && names[a] {
				add(errs.Newf(errs.ValidInvalid,
					"Workload %q answers to %q, which is another workload's name.", w.Name, a).
					WithRemedy("Remove the alias: the other workload already answers to that name."))
			}
		}
	}
}

// NamesImage reports whether ref is an image the spec itself names — an image
// app's image, or the published one a workload without a build runs — rather than one an app's build
// produced. Compared by repository, so a tag or digest the deploy pinned it to
// is the same image. The install registry's credential is for an app's own
// builds only: an author who writes down another app's build gets no
// credential to pull it with.
func (s *AppSpec) NamesImage(ref string) bool {
	if s == nil || ref == "" {
		return false
	}
	repo := imageRepository(ref)
	if s.Source.Image != "" && strings.EqualFold(imageRepository(s.Source.Image), repo) {
		return true
	}
	for _, w := range s.Workloads {
		// A workload with a build of its own runs what the build produced,
		// which may be pushed to the repository its image names.
		if w.Build != nil {
			continue
		}
		if w.Image != "" && strings.EqualFold(imageRepository(w.Image), repo) {
			return true
		}
	}
	return false
}

// imageRepository is a reference without its digest or tag.
func imageRepository(ref string) string {
	ref = strings.TrimSpace(ref)
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	return ref
}
