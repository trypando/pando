package spec

import (
	"fmt"
	"sort"
)

// Class is how consequential a change is.
//
// The classification drives the UI treatment (design 01 §4): benign collapses,
// restart and rebuild are shown, and destructive requires explicit confirmation.
// Re-detection, rollback review, and promoting a compose service all present the
// same diff through the same machinery.
type Class string

const (
	// Benign is display-only: retention, warnings.
	Benign Class = "benign"

	// Restart requires the workloads to come back: env, resources, health.
	Restart Class = "restart"

	// Rebuild triggers a build: source, build configuration.
	Rebuild Class = "rebuild"

	// Destructive can lose data or break access, and requires explicit
	// confirmation: a volume removed, a slot moved from provisioned to bound, a
	// routing mode changed, a runtime adapter changed (R-257).
	Destructive Class = "destructive"
)

// rank orders classes so the overall class of a diff is its worst change.
var rank = map[Class]int{Benign: 0, Restart: 1, Rebuild: 2, Destructive: 3}

// Change is one difference between two specs.
type Change struct {
	Class Class  `json:"class"`
	Path  string `json:"path"`

	// Summary is written for the person confirming the change, not for a log.
	Summary string `json:"summary"`

	From any `json:"from,omitempty"`
	To   any `json:"to,omitempty"`
}

// Diff is a classified comparison.
type Diff struct {
	Changes []Change `json:"changes"`
}

// Class returns the worst class present, or Benign when there are no changes.
func (d Diff) Class() Class {
	worst := Benign
	for _, c := range d.Changes {
		if rank[c.Class] > rank[worst] {
			worst = c.Class
		}
	}
	return worst
}

// Empty reports whether the specs are equivalent.
func (d Diff) Empty() bool { return len(d.Changes) == 0 }

// RequiresConfirmation reports whether a human must explicitly accept this.
func (d Diff) RequiresConfirmation() bool { return d.Class() == Destructive }

// Compare classifies every difference from old to new.
func Compare(old, next *AppSpec) Diff {
	var d Diff
	add := func(class Class, path, summary string, from, to any) {
		d.Changes = append(d.Changes, Change{Class: class, Path: path, Summary: summary, From: from, To: to})
	}

	compareSource(old, next, add)
	compareBuild(old, next, add)
	compareRuntimeAndRouting(old, next, add)
	compareVolumes(old, next, add)
	compareSlots(old, next, add)
	compareWorkloads(old, next, add)
	compareOperational(old, next, add)

	sort.SliceStable(d.Changes, func(i, j int) bool {
		return rank[d.Changes[i].Class] > rank[d.Changes[j].Class]
	})
	return d
}

func compareSource(old, next *AppSpec, add func(Class, string, string, any, any)) {
	if old.Source.URL != next.Source.URL {
		add(Rebuild, "source.url", "This app will be built from a different repository.", old.Source.URL, next.Source.URL)
	}
	if old.Source.Ref != next.Source.Ref {
		add(Rebuild, "source.ref", fmt.Sprintf("This app will track %s instead of %s.", next.Source.Ref, old.Source.Ref), old.Source.Ref, next.Source.Ref)
	}
	if old.Source.Commit != next.Source.Commit {
		add(Rebuild, "source.commit", "This app will be rebuilt from a different commit.", old.Source.Commit, next.Source.Commit)
	}
	if old.Source.Subdir != next.Source.Subdir {
		add(Rebuild, "source.subdir", "The app will be built from a different directory in the repository.", old.Source.Subdir, next.Source.Subdir)
	}
	if old.Source.Image != next.Source.Image {
		add(Rebuild, "source.image", "A different image will be run.", old.Source.Image, next.Source.Image)
	}
	// The digest is what an image app runs (issue #41), so a new one is a
	// different image even under the same tag — the case pinning exists for.
	if old.Source.Digest != next.Source.Digest && old.Source.Image == next.Source.Image {
		add(Rebuild, "source.digest", "A different build of the same image will be run.", old.Source.Digest, next.Source.Digest)
	}
}

func compareBuild(old, next *AppSpec, add func(Class, string, string, any, any)) {
	if old.Build.Strategy != next.Build.Strategy {
		add(Rebuild, "build.strategy", fmt.Sprintf("This app will be built as %s instead of %s.", next.Build.Strategy, old.Build.Strategy), old.Build.Strategy, next.Build.Strategy)
	}
	if old.Build.Dockerfile != next.Build.Dockerfile {
		add(Rebuild, "build.dockerfile", "A different Dockerfile will be used.", old.Build.Dockerfile, next.Build.Dockerfile)
	}
	if old.Build.Context != next.Build.Context || old.Build.Target != next.Build.Target {
		add(Rebuild, "build.context", "The build context or target changed.", old.Build.Context, next.Build.Context)
	}
	if !sameKV(old.Build.Args, next.Build.Args) {
		add(Rebuild, "build.args", "Build arguments changed.", kvMap(old.Build.Args), kvMap(next.Build.Args))
	}
	if old.Build.IsolationFloor != next.Build.IsolationFloor {
		add(Rebuild, "build.isolation_floor", "Builds will run at a different isolation level.", old.Build.IsolationFloor, next.Build.IsolationFloor)
	}
}

func compareRuntimeAndRouting(old, next *AppSpec, add func(Class, string, string, any, any)) {
	// O-8: swapping the runtime adapter is neither a migration nor a plain
	// redeploy. It is destructive, and volumes go through the keep-or-discard
	// flow — Pando cannot move volume contents between adapters that have no
	// common representation of a volume (R-206).
	if old.Runtime.AdapterRef != next.Runtime.AdapterRef {
		add(Destructive, "runtime.adapter_ref",
			"This app will move to a different runtime. Its storage does not move with it — you will be asked what to do with each volume.",
			old.Runtime.AdapterRef, next.Runtime.AdapterRef)
	}
	if old.Runtime.IsolationFloor != next.Runtime.IsolationFloor {
		add(Restart, "runtime.isolation_floor", "This app will run at a different isolation level.", old.Runtime.IsolationFloor, next.Runtime.IsolationFloor)
	}

	if old.Routing.Mode != next.Routing.Mode {
		add(Destructive, "routing.mode",
			"The address people use to reach this app will change. Existing links and bookmarks will stop working.",
			old.Routing.Mode, next.Routing.Mode)
	}
	if old.Routing.AdapterRef != next.Routing.AdapterRef {
		add(Destructive, "routing.adapter_ref",
			"Traffic will reach this app a different way. Existing links may stop working.",
			old.Routing.AdapterRef, next.Routing.AdapterRef)
	}
	if old.Routing.Hostname != next.Routing.Hostname {
		add(Destructive, "routing.hostname",
			fmt.Sprintf("This app will move from %s to %s. Existing links will stop working.", orNone(old.Routing.Hostname), orNone(next.Routing.Hostname)),
			old.Routing.Hostname, next.Routing.Hostname)
	}
	if old.Routing.PathPrefix != next.Routing.PathPrefix {
		add(Destructive, "routing.path_prefix",
			fmt.Sprintf("This app will move from %s to %s. Existing links will stop working.", orNone(old.Routing.PathPrefix), orNone(next.Routing.PathPrefix)),
			old.Routing.PathPrefix, next.Routing.PathPrefix)
	}
	if old.Routing.Port != next.Routing.Port {
		add(Destructive, "routing.port", "This app will be served on a different port.", old.Routing.Port, next.Routing.Port)
	}
}

func compareVolumes(old, next *AppSpec, add func(Class, string, string, any, any)) {
	nextByID := map[string]Volume{}
	for _, v := range next.Volumes {
		nextByID[v.ID] = v
	}

	for _, v := range old.Volumes {
		if _, ok := nextByID[v.ID]; !ok {
			// The single most consequential change in the system. Removing a
			// volume from a spec is how data gets lost quietly.
			add(Destructive, "volumes."+v.ID,
				fmt.Sprintf("The storage %q will no longer be attached to this app. Anything in it will be left behind.", v.Name),
				v.Name, nil)
		}
	}

	oldByID := map[string]Volume{}
	for _, v := range old.Volumes {
		oldByID[v.ID] = v
	}
	for _, v := range next.Volumes {
		if _, ok := oldByID[v.ID]; !ok {
			add(Restart, "volumes."+v.ID, fmt.Sprintf("New storage %q will be attached.", v.Name), nil, v.Name)
		}
	}
}

func compareSlots(old, next *AppSpec, add func(Class, string, string, any, any)) {
	oldByKey := map[string]Slot{}
	for _, s := range old.Slots {
		oldByKey[s.Key] = s
	}

	for _, n := range next.Slots {
		o, existed := oldByKey[n.Key]
		if !existed {
			add(Restart, "slots."+n.Key, fmt.Sprintf("This app now needs %s.", n.Key), nil, string(n.Type))
			continue
		}

		om, nm := modeOf(o), modeOf(n)
		if om == nm {
			continue
		}

		// Moving away from a provisioned service means the data in the service
		// Pando was running for this app is no longer what the app reads.
		if om == ResolutionProvisioned && nm != ResolutionProvisioned {
			add(Destructive, "slots."+n.Key,
				fmt.Sprintf("%s will point somewhere else. The %s Pando runs for this app will no longer be used, and its data will not move.", n.Key, n.Type),
				string(om), string(nm))
			continue
		}
		add(Restart, "slots."+n.Key, fmt.Sprintf("%s will be filled a different way.", n.Key), string(om), string(nm))
	}

	nextByKey := map[string]Slot{}
	for _, s := range next.Slots {
		nextByKey[s.Key] = s
	}
	for _, o := range old.Slots {
		if _, ok := nextByKey[o.Key]; !ok {
			add(Restart, "slots."+o.Key, fmt.Sprintf("This app no longer needs %s.", o.Key), string(o.Type), nil)
		}
	}
}

func compareWorkloads(old, next *AppSpec, add func(Class, string, string, any, any)) {
	oldByName := map[string]Workload{}
	for _, w := range old.Workloads {
		oldByName[w.Name] = w
	}
	nextByName := map[string]Workload{}
	for _, w := range next.Workloads {
		nextByName[w.Name] = w
	}

	for _, w := range old.Workloads {
		if _, ok := nextByName[w.Name]; !ok {
			add(Destructive, "workloads."+w.Name,
				fmt.Sprintf("The workload %q will be removed.", w.Name), w.Name, nil)
		}
	}

	for _, n := range next.Workloads {
		o, existed := oldByName[n.Name]
		if !existed {
			add(Restart, "workloads."+n.Name, fmt.Sprintf("A new workload %q will be added.", n.Name), nil, n.Name)
			continue
		}

		if !sameEnv(o.Env, n.Env) {
			add(Restart, "workloads."+n.Name+".env",
				fmt.Sprintf("Environment variables for %q changed, so it will restart.", n.Name),
				envKeys(o.Env), envKeys(n.Env))
		}
		if !sameStrings(o.Command, n.Command) || !sameStrings(o.Entrypoint, n.Entrypoint) {
			add(Restart, "workloads."+n.Name+".command", fmt.Sprintf("The command for %q changed.", n.Name), o.Command, n.Command)
		}
		if o.Image != n.Image {
			add(Restart, "workloads."+n.Name+".image", fmt.Sprintf("A different image will run for %q.", n.Name), o.Image, n.Image)
		}
		if !sameMounts(o.Mounts, n.Mounts) {
			add(Restart, "workloads."+n.Name+".mounts", fmt.Sprintf("Storage attached to %q changed.", n.Name), mountPaths(o.Mounts), mountPaths(n.Mounts))
		}
		if o.Primary != n.Primary {
			add(Destructive, "workloads."+n.Name+".primary",
				"The workload this app's URL points at will change.", o.Primary, n.Primary)
		}
		if o.Exposed != n.Exposed {
			add(Restart, "workloads."+n.Name+".exposed", fmt.Sprintf("Whether %q is reachable changed.", n.Name), o.Exposed, n.Exposed)
		}
	}
}

func compareOperational(old, next *AppSpec, add func(Class, string, string, any, any)) {
	if old.Resources != next.Resources {
		add(Restart, "resources", "Resource limits changed, so this app will restart.", old.Resources, next.Resources)
	}
	if old.Health != next.Health {
		add(Restart, "health", "How Pando checks this app's health changed.", old.Health, next.Health)
	}
	if old.Deploy.Strategy != next.Deploy.Strategy {
		add(Benign, "deploy.strategy", fmt.Sprintf("Deploys will use the %s strategy.", next.Deploy.Strategy), old.Deploy.Strategy, next.Deploy.Strategy)
	}
	if old.Deploy.AutoDeploy != next.Deploy.AutoDeploy {
		add(Benign, "deploy.auto_deploy", "Automatic deploy settings changed.", old.Deploy.AutoDeploy, next.Deploy.AutoDeploy)
	}
	if old.Deploy.AutoRollback != next.Deploy.AutoRollback {
		add(Benign, "deploy.auto_rollback", "Automatic rollback setting changed.", old.Deploy.AutoRollback, next.Deploy.AutoRollback)
	}

	// Whether an egress change loosens or tightens depends on the install's
	// rules, which a diff of two specs cannot see (R-182). The verb and policy
	// gates see them (R-183, R-184); here it is a restart, because nothing is
	// lost either way.
	if !sameEgress(old.Egress, next.Egress) {
		add(Restart, "egress", "Which addresses this app can reach changed.", old.Egress, next.Egress)
	}
	if old.Deploy.RequireApproval != next.Deploy.RequireApproval {
		add(Benign, "deploy.require_approval", "Whether this app's deploys need approval changed.", old.Deploy.RequireApproval, next.Deploy.RequireApproval)
	}

	if old.Retention != next.Retention {
		add(Benign, "retention", "How long logs, backups, and revisions are kept changed.", old.Retention, next.Retention)
	}
}

func modeOf(s Slot) ResolutionMode {
	if s.Resolution == nil {
		return ""
	}
	return s.Resolution.Mode
}

func orNone(s string) string {
	if s == "" {
		return "nothing"
	}
	return s
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func sameKV(a, b []KV) bool {
	if len(a) != len(b) {
		return false
	}
	am, bm := kvMap(a), kvMap(b)
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

func kvMap(kvs []KV) map[string]string {
	m := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		m[kv.Key] = kv.Value
	}
	return m
}

// sameEnv compares entries by key and source, never by resolved value — the
// spec holds no secret values, and comparing them would require resolving them.
func sameEnv(a, b []EnvEntry) bool {
	if len(a) != len(b) {
		return false
	}
	am, bm := envMap(a), envMap(b)
	for k, v := range am {
		if bm[k] != v {
			return false
		}
	}
	return true
}

func envMap(entries []EnvEntry) map[string]string {
	m := make(map[string]string, len(entries))
	for _, e := range entries {
		switch {
		case e.Value != nil:
			m[e.Key] = "value:" + *e.Value
		case e.SlotRef != nil:
			m[e.Key] = "slot:" + *e.SlotRef
		case e.SecretRef != nil:
			// The secret's identity, not its value. A rotated secret is
			// detected by the reconciler's fingerprint, not by the differ.
			m[e.Key] = "secret:" + *e.SecretRef
		default:
			m[e.Key] = "unset"
		}
	}
	return m
}

func envKeys(entries []EnvEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	sort.Strings(out)
	return out
}

func sameMounts(a, b []Mount) bool {
	if len(a) != len(b) {
		return false
	}
	am := map[string]Mount{}
	for _, m := range a {
		am[m.Path] = m
	}
	for _, m := range b {
		got, ok := am[m.Path]
		if !ok || got != m {
			return false
		}
	}
	return true
}

func mountPaths(mounts []Mount) []string {
	out := make([]string, 0, len(mounts))
	for _, m := range mounts {
		out = append(out, m.Path)
	}
	sort.Strings(out)
	return out
}

// Same reports whether two egress settings say the same thing once each is
// read in today's terms (Normalize). Whether a change needs app.egress.tighten
// or app.egress.loosen starts from this (R-184): a spec written before issue
// #79 that is saved again unchanged has not changed its egress.
func (e Egress) Same(o Egress) bool { return sameEgress(e, o) }

// sameEgress compares two egress settings as they read after Normalize.
func sameEgress(a, b Egress) bool {
	a.Normalize()
	b.Normalize()
	bp := func(p *bool) string {
		if p == nil {
			return "inherit"
		}
		return fmt.Sprint(*p)
	}
	return a.Mode == b.Mode && sameStrings(a.List, b.List) &&
		sameStrings(a.Add, b.Add) && sameStrings(a.Remove, b.Remove) &&
		bp(a.BlockPrivate) == bp(b.BlockPrivate)
}
