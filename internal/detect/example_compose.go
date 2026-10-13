package detect

import (
	"net/url"
	"path"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// exampleComposeSkip are directories whose compose files describe something
// other than how this app runs: dependencies, fixtures, tests.
var exampleComposeSkip = map[string]bool{
	"node_modules": true, "vendor": true, "testdata": true, "test": true, "tests": true,
	"fixtures": true, ".git": true,
}

// WithExampleCompose gives a draft built from the repository itself the backing
// services an example compose file elsewhere in the repository runs beside it.
//
// Yopass builds from its own Dockerfile and keeps its compose files under
// deploy/docker-compose/, where each runs the project's published image beside
// a memcached and points MEMCACHED at it. The Dockerfile won the auction, the
// compose files were never read, and the app deployed without the cache:
// pages served, and the first secret anybody stored failed on
// "dial tcp 127.0.0.1:11211: connect: connection refused". The trial run cannot
// see it, because yopass connects on its first write, not at start.
//
// That compose file is the author declaring the dependency (R-021), and R-130
// names a compose image name as one of the ways a slot is typed, so this is a
// slot Pando fills and not topology it invents. Only what the example wires to
// a service Pando provisions is taken — the variable that reaches it and the
// slot it fills, provisioned as the example runs it [P] — and only from a
// service that is this app: one that builds from the app's own directory, or
// that runs the image named for the repository it came from. The rest of the
// example (its ports, its proxy, its other settings) describes one way to run
// the app and is not imported; R-096's "complete answer" is a compose file at
// the root, which is a different reading and bids for itself.
func WithExampleCompose(view api.SourceView, src spec.Source, d Draft) Draft {
	if d.Build.Strategy != spec.BuildDockerfile && d.Build.Strategy != spec.BuildBuildpack {
		return d
	}
	primary := -1
	for i, w := range d.Workloads {
		if w.Primary {
			primary = i
		}
	}
	if primary < 0 {
		return d
	}
	// Copied before it is written: the draft's slices are the caller's.
	d.Workloads = append([]spec.Workload(nil), d.Workloads...)
	d.Workloads[primary].Env = append([]spec.EnvEntry(nil), d.Workloads[primary].Env...)
	d.Slots = append([]spec.Slot(nil), d.Slots...)

	taken := map[string]bool{}
	for _, s := range d.Slots {
		taken[s.Key] = true
	}
	for _, e := range d.Workloads[primary].Env {
		taken[e.Key] = true
	}

	for _, file := range exampleComposeFiles(view) {
		example, err := ImportCompose(view, file)
		if err != nil {
			continue
		}
		app, ok := exampleApp(example, file, src)
		if !ok {
			continue
		}
		for _, e := range app.Env {
			if e.SlotRef == nil || taken[e.Key] {
				continue
			}
			slot, ok := provisionedSlot(example.Slots, *e.SlotRef)
			if !ok {
				continue
			}
			taken[e.Key] = true
			slot.Key = e.Key
			slot.Evidence = append([]string{
				file + " runs a " + slot.Type.DisplayName() + " beside this app and points " + e.Key + " at it",
			}, slot.Evidence...)
			d.Slots = append(d.Slots, slot)
			ref := e.Key
			d.Workloads[primary].Env = append(d.Workloads[primary].Env, spec.EnvEntry{
				Key: e.Key, SlotRef: &ref, Source: spec.EnvFromCompose,
			})
		}
	}
	return d
}

// exampleComposeFiles are the compose files below the root, in a stable order.
// One at the root is not an example: it bid as the answer, or was passed over
// for a reason of its own (legacyCompose).
func exampleComposeFiles(view api.SourceView) []string {
	seen := map[string]bool{}
	var files []string
	for _, name := range composeNames {
		matches, err := view.Glob(name)
		if err != nil {
			continue
		}
		for _, m := range matches {
			m = path.Clean(strings.ReplaceAll(m, "\\", "/"))
			if path.Base(m) != name || path.Dir(m) == "." || seen[m] || skippedExample(m) {
				continue
			}
			seen[m] = true
			files = append(files, m)
		}
	}
	sort.Strings(files)
	return files
}

func skippedExample(file string) bool {
	for _, dir := range strings.Split(path.Dir(file), "/") {
		if exampleComposeSkip[strings.ToLower(dir)] {
			return true
		}
	}
	return false
}

// exampleApp is the one workload in an example compose file that is this app.
// Two that both qualify say nothing about which the app is, and none is taken
// (R-102).
func exampleApp(example Draft, file string, src spec.Source) (spec.Workload, bool) {
	owner, name := repoOwnerAndName(src)
	var found []spec.Workload
	for _, w := range example.Workloads {
		switch {
		case w.Build != nil && path.Clean(path.Join(path.Dir(file), orDot(w.Build.Context))) == ".":
			found = append(found, w)
		case w.Build == nil && name != "" && src.Subdir == "" && imageIsRepo(w.Image, owner, name):
			found = append(found, w)
		}
	}
	if len(found) != 1 {
		return spec.Workload{}, false
	}
	return found[0], true
}

func orDot(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}

// provisionedSlot is the slot an example compose file wired to a service
// Pando provisions in its place (composeImport.wire).
func provisionedSlot(slots []spec.Slot, key string) (spec.Slot, bool) {
	for _, s := range slots {
		if s.Key == key && s.Type != spec.SlotUnknown && s.Resolution != nil &&
			s.Resolution.Mode == spec.ResolutionProvisioned {
			return s, true
		}
	}
	return spec.Slot{}, false
}

// repoOwnerAndName reads "jhaals" and "yopass" out of a git URL such as
// https://github.com/jhaals/yopass.git. Empty when the source has none.
func repoOwnerAndName(src spec.Source) (owner, name string) {
	if src.URL == "" {
		return "", ""
	}
	raw := src.URL
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		raw = u.Path
	} else if _, after, ok := strings.Cut(raw, ":"); ok && strings.Contains(raw, "@") {
		// git@github.com:owner/name.git
		raw = after
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(raw, ".git"), "/"), "/")
	if len(parts) < 2 {
		return "", ""
	}
	return strings.ToLower(parts[len(parts)-2]), strings.ToLower(parts[len(parts)-1])
}

// imageIsRepo reports an image reference named for the repository:
// jhaals/yopass, docker.io/jhaals/yopass:latest, ghcr.io/jhaals/yopass@sha256:….
func imageIsRepo(image, owner, name string) bool {
	ref := strings.ToLower(strings.TrimSpace(image))
	if at := strings.Index(ref, "@"); at >= 0 {
		ref = ref[:at]
	}
	if colon := strings.LastIndex(ref, ":"); colon > strings.LastIndex(ref, "/") {
		ref = ref[:colon]
	}
	parts := strings.Split(ref, "/")
	if len(parts) > 2 && (strings.ContainsAny(parts[0], ".:") || parts[0] == "localhost") {
		parts = parts[1:]
	}
	return len(parts) == 2 && parts[0] == owner && parts[1] == name
}
