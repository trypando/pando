package spec

// Carrying a person's decisions across a re-detection.
//
// R-022 makes re-detection explicit and says it shows a diff against the
// pinned spec. What it does not say — and what accepting one used to do — is
// replace that spec wholesale. So somebody who deployed an app, set a few
// environment variables, chose how its database was filled, then added a
// Dockerfile and re-detected, lost all three the moment they accepted. The app
// kept running on the old spec, the data was still on disk, and the next deploy
// quietly started something configured differently.
//
// The rule is the one WithAnswers already states for answers: a decision a
// person made is worth more than anything Pando worked out for itself. Which
// decisions those are is not a guess — the spec already records provenance on
// ports (Port.Source), volumes (Volume.Declared) and now environment entries
// (EnvEntry.Source), because this is exactly the question those fields exist to
// answer.
//
// What deliberately does not carry: the build block, the workload set, and
// anything sourced from a file that was read again. Re-detecting *is* the
// request to take those from the repository, and a merge that preserved them
// would make re-detection do nothing.

// Carry returns next with the decisions a person made in pinned preserved.
//
// pinned may be nil, which is the first acceptance: there is nothing to carry
// and the proposal stands as it is.
func Carry(pinned, next *AppSpec) *AppSpec {
	if pinned == nil || next == nil {
		return next
	}

	out := *next
	out.Workloads = carryEnv(pinned, next)
	out.Slots = carrySlots(pinned, next)
	out.Volumes = carryVolumes(pinned, next)

	// Where the app is reached. Nothing about it is read from the repository,
	// so a re-detection has nothing to say about it — and taking the fresh
	// default instead moved an app somebody had given its own hostname back
	// to <slug>.<base domain>, breaking every bookmark to it (R-165).
	if pinned.Routing.Mode != "" {
		out.Routing = pinned.Routing
	}

	// What the app may reach, and whether its deploys need approval. Neither
	// is read from the repository either. Dropping them on a re-detection
	// would widen what the app can reach without anybody holding
	// app.egress.loosen deciding it (R-184), and would turn off the app's own
	// approval requirement without that change being approved (R-154).
	out.Egress = pinned.Egress
	out.Deploy.RequireApproval = pinned.Deploy.RequireApproval

	// How much of the host the app and each workload may take. Not read from
	// the repository either, and changed only under app.resources.override
	// (R-241): a re-detection that put the host's defaults back would change
	// the app's limits without anybody deciding it.
	if pinned.Resources != (Resources{}) {
		out.Resources = pinned.Resources
	}
	out.Workloads = carryLimits(pinned, out.Workloads)
	return &out
}

// carryLimits keeps a workload's own resource limits, matched on name, where
// the proposal gives that workload none.
func carryLimits(pinned *AppSpec, workloads []Workload) []Workload {
	kept := map[string]*ResourceLimits{}
	for _, w := range pinned.Workloads {
		if w.Resources != nil {
			own := *w.Resources
			kept[w.Name] = &own
		}
	}
	if len(kept) == 0 {
		return workloads
	}
	out := make([]Workload, len(workloads))
	copy(out, workloads)
	for i := range out {
		if own, ok := kept[out[i].Name]; ok && out[i].Resources == nil {
			out[i].Resources = own
		}
	}
	return out
}

// carryEnv keeps environment entries a person set.
//
// Matched on workload name and key. A variable set on a workload the new
// proposal no longer has is dropped with it — there is nowhere to put it, and
// inventing a workload to hold it would be worse than losing it.
func carryEnv(pinned, next *AppSpec) []Workload {
	kept := map[string][]EnvEntry{}
	for _, w := range pinned.Workloads {
		for _, e := range w.Env {
			if e.Source == EnvFromUser {
				kept[w.Name] = append(kept[w.Name], e)
			}
		}
	}
	if len(kept) == 0 {
		return next.Workloads
	}

	out := make([]Workload, 0, len(next.Workloads))
	for _, w := range next.Workloads {
		mine := kept[w.Name]
		if len(mine) == 0 {
			out = append(out, w)
			continue
		}

		// The person's entry wins on a key collision. Detection inferring a
		// value for something they set by hand is precisely the case where
		// theirs is the right one.
		env := make([]EnvEntry, 0, len(w.Env)+len(mine))
		theirs := map[string]bool{}
		for _, e := range mine {
			theirs[e.Key] = true
			env = append(env, e)
		}
		for _, e := range w.Env {
			if !theirs[e.Key] {
				env = append(env, e)
			}
		}
		w.Env = env
		out = append(out, w)
	}
	return out
}

// carrySlots keeps how a person chose to fill each dependency.
//
// This is the one that loses data rather than configuration. A slot resolved as
// provisioned has a database behind it; a proposal arrives with every slot
// unfilled, so accepting one used to unfill them — and the next deploy started
// the app with no database and the old one sitting unreferenced on disk.
func carrySlots(pinned, next *AppSpec) []Slot {
	resolved := map[string]*Resolution{}
	for _, s := range pinned.Slots {
		if s.Resolution != nil {
			resolved[s.Key] = s.Resolution
		}
	}
	if len(resolved) == 0 {
		return next.Slots
	}

	out := make([]Slot, 0, len(next.Slots))
	for _, s := range next.Slots {
		if r, ok := resolved[s.Key]; ok && s.Resolution == nil {
			s.Resolution = r
		}
		out = append(out, s)
	}
	return out
}

// carryVolumes keeps storage a person added.
//
// A volume dropped from the spec is not destroyed — R-204 keeps it, and the
// reconciler only reclaims one whose app is deleted and backed up. But it stops
// being mounted, which to the app is indistinguishable from having lost it.
func carryVolumes(pinned, next *AppSpec) []Volume {
	have := map[string]bool{}
	for _, v := range next.Volumes {
		have[v.Name] = true
	}

	out := next.Volumes
	for _, v := range pinned.Volumes {
		if v.Declared == VolumeFromUser && !have[v.Name] {
			out = append(out, v)
		}
	}
	return out
}
