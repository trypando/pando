package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/edge"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/specgate"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// handlePlan is the dry run (design 04 §2.3).
//
// It exists as its own endpoint because every plan-time failure is more useful
// before a user commits than during a deploy, and because it is side-effect-free
// the console can call it on every spec edit.
func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app has no spec to plan yet.").
			WithRemedy("Write a spec for the app and pin it first."))
		return
	}

	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil || !found {
		Error(w, r, orNotFound(err))
		return
	}

	plan, err := s.Planner.Check(r.Context(), rev.Body)
	if err != nil {
		// A plan-time refusal is the point of this endpoint, not a failure of
		// it. It is returned with its own status and left unaudited: planning
		// creates nothing, and auditing every keystroke of a console spec editor
		// would bury the events that matter.
		Error(w, r, err)
		return
	}

	notes := plan.Notes
	if notes == nil {
		notes = []string{}
	}
	body := map[string]any{
		"app_id":    plan.AppID,
		"revision":  rev.Revision,
		"checks":    plan.Checks,
		"workloads": len(plan.Bundle.Workloads),
		"volumes":   len(plan.Bundle.Volumes),
		"egress":    plan.Egress, // R-188
		"notes":     notes,
	}

	// Whether deploying it would wait for somebody's approval, and why
	// (R-154), so nobody finds out only after pressing Deploy. Asked of the
	// approval service, which is what a deploy itself asks; specgate's answer
	// is the fallback for a server built without one.
	if s.Approvals != nil {
		reasons, err := s.Approvals.Needed(r.Context(), app, rev, state.TriggerManual)
		if err != nil {
			Error(w, r, err)
			return
		}
		described := make([]state.ApprovalReason, 0, len(reasons))
		for _, reason := range reasons {
			described = append(described, state.ApprovalReason{Reason: string(reason), Message: reason.Message()})
		}
		body["approval"] = map[string]any{"required": len(reasons) > 0, "reasons": described}
	} else {
		doc, err := s.hostPolicy(r.Context())
		if err != nil {
			Error(w, r, err)
			return
		}
		var running *spec.AppSpec
		if s.Deployments != nil {
			runningID, err := s.Deployments.RunningSpecID(r.Context(), app.ID)
			if err != nil {
				Error(w, r, err)
				return
			}
			if runningID != "" {
				if rr, found, err := s.Apps.RevisionByID(r.Context(), runningID); err != nil {
					Error(w, r, err)
					return
				} else if found {
					running = rr.Body
				}
			}
		}
		body["approval"] = specgate.ApprovalFor(doc, app.ID, running, rev.Body)
	}
	JSON(w, http.StatusOK, body)
}

// handleAdapterKinds lists the kinds of adapter this build can run and the
// settings each takes, so the console and the CLI can offer a form for adding
// one rather than a JSON body to write by hand (R-261).
func (s *Server) handleAdapterKinds(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}
	kinds := s.AdapterKinds
	if kinds == nil {
		kinds = []api.KindInfo{}
	}
	JSON(w, http.StatusOK, map[string]any{"kinds": kinds})
}

// handleListAdapters returns configured adapters with their LIVE capabilities.
//
// Live rather than stored (design 04 §2.8), so the console can grey out routing
// modes an adapter does not support instead of offering choices that fail at
// plan time. Stored capabilities would drift the first time an adapter was
// upgraded.
// install.view rather than something app-scoped: this is the install's
// inventory, including which adapters are configured and whether each is
// reachable, which is operational detail about the host rather than about any
// app.
func (s *Server) handleListAdapters(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}

	configured, err := s.Adapters.List(r.Context())
	if err != nil {
		Error(w, r, err)
		return
	}

	health := s.Registry.HealthCheckAll(r.Context())

	// Names only. The console shows "an API key is set"; nothing can read one
	// back (O-20).
	credentials := map[string][]string{}
	if s.AdapterCredentials != nil {
		if credentials, err = s.AdapterCredentials.Fields(r.Context()); err != nil {
			Error(w, r, err)
			return
		}
	}

	// The stored settings, for whoever may change them: the console's Change
	// form fills itself from them, rather than saving over every setting left
	// blank. Not for install.view alone — settings name hosts and paths, the
	// same operator detail the error messages below are withheld for.
	// Credentials are never among them: the database refuses them in config.
	showConfig := false
	if s.Verbs != nil {
		if held, err := s.Verbs.InstallVerbsFor(r.Context(), PrincipalFrom(r.Context())); err == nil {
			for _, v := range held {
				showConfig = showConfig || v == string(authz.InstallAdaptersManage)
			}
		}
	}

	// Adapters declared in the config file come first and are read-only
	// (R-271). A stored adapter one of them overrides — same ID, or an AI
	// adapter of the same provider — is listed as overridden, so an operator
	// can see what applies again once the declaration is removed.
	declared := s.declaredAdapters()
	declaredSources := map[string]config.Source{}
	declaredIDs := map[string]config.AdapterDecl{}
	declaredAIKinds := map[string]config.AdapterDecl{}
	for _, d := range declared {
		declaredIDs[d.ID] = d
		if d.Category == string(api.CategoryAI) && d.Enabled {
			declaredAIKinds[d.Kind] = d
		}
	}
	for _, d := range declared {
		c := state.AdapterConfig{ID: d.ID, Category: d.Category, Kind: d.Kind, Name: d.Name,
			IsDefault: d.Default, Enabled: d.Enabled}
		if showConfig && len(d.Config) > 0 {
			c.Config, _ = json.Marshal(d.Config)
		}
		for field := range d.Credentials {
			credentials[d.ID] = append(credentials[d.ID], field)
		}
		sort.Strings(credentials[d.ID])
		declaredAt := d.Source
		configured = append([]state.AdapterConfig{c}, configured...)
		declaredSources[d.ID] = declaredAt
	}

	restartNeeded := false
	out := make([]map[string]any, 0, len(configured))
	for _, c := range configured {
		src, isDeclared := declaredSources[c.ID]
		if !isDeclared {
			// A stored row the file overrides.
			over, byID := declaredIDs[c.ID]
			if !byID && c.Category == string(api.CategoryAI) {
				over, byID = declaredAIKinds[c.Kind]
			}
			if byID {
				out = append(out, map[string]any{
					"id": c.ID, "category": c.Category, "kind": c.Kind, "name": c.Name,
					"is_default": c.IsDefault, "enabled": c.Enabled, "healthy": false,
					"status": "overridden", "overridden_by": over.Source,
				})
				continue
			}
		}
		entry := map[string]any{
			"id":         c.ID,
			"category":   c.Category,
			"kind":       c.Kind,
			"name":       c.Name,
			"is_default": c.IsDefault,
			"enabled":    c.Enabled,
			"healthy":    health[c.ID] == nil,
		}
		if fields := credentials[c.ID]; len(fields) > 0 {
			entry["credentials_set"] = fields
		}
		if isDeclared {
			entry["source"] = src
			entry["declared"] = true
		}
		if showConfig && len(c.Config) > 0 {
			entry["config"] = c.Config
		}

		// An unhealthy adapter's error message is not returned. It can carry a
		// host, a port, or a path from the operator's infrastructure, and this
		// endpoint is readable by anyone signed in.
		if err := health[c.ID]; err != nil {
			entry["status"] = "unreachable"
		}

		// Saved since this process started, so not what is running: a new
		// adapter is not loaded at all, a changed one still runs as it was.
		// Without this a just-added adapter reads as reachable, having never
		// been asked.
		// A source connection is the exception: it is built from its row
		// each time it is used (core/sourceconn), so it never waits for one.
		if !isDeclared && !s.StartedAt.IsZero() && c.UpdatedAt.After(s.StartedAt) &&
			c.Category != string(api.CategorySource) {
			entry["pending_restart"] = true
			entry["status"] = "pending_restart"
			restartNeeded = true
		}

		switch api.Category(c.Category) {
		case api.CategoryRuntime:
			if rt, ok := s.Registry.Runtime(c.ID); ok {
				if caps, err := rt.Capabilities(r.Context()); err == nil {
					entry["capabilities"] = caps
				}
			}
		case api.CategoryRouting:
			if rte, ok := s.Registry.Routing(c.ID); ok {
				if caps, err := rte.Capabilities(r.Context()); err == nil {
					entry["capabilities"] = caps
				}
			}
			// The process Pando runs in front of itself for this adapter
			// (R-174). Whether it runs is for anyone; why it does not is
			// operator detail — a port, an image — held back like the
			// health error above.
			if st, ok := s.edgeStatus(c.ID); ok {
				view := map[string]any{"running": st.Running, "checked_at": st.CheckedAt.UTC().Format(time.RFC3339)}
				if showConfig && st.Message != "" {
					view["message"] = st.Message
				}
				entry["edge"] = view
				if !st.Running && entry["status"] == nil {
					entry["status"] = "edge_down"
				}
			}
		case api.CategoryBuilder:
			if b, ok := s.Registry.Builder(c.ID); ok {
				if caps, err := b.Capabilities(r.Context()); err == nil {
					entry["capabilities"] = caps
				}
			}
		case api.CategoryAI:
			if ai, ok := s.Registry.AI(c.ID); ok {
				if caps, err := ai.Capabilities(r.Context()); err == nil {
					entry["capabilities"] = caps
				}
			}
		}
		out = append(out, entry)
	}

	body := map[string]any{"adapters": out, "restart_needed": restartNeeded}
	if !s.StartedAt.IsZero() {
		body["started_at"] = s.StartedAt.UTC().Format(time.RFC3339Nano)
	}
	JSON(w, http.StatusOK, body)
}

// handleCapacity aggregates what the runtime adapters report (R-243), beside
// what Pando has committed to apps on each — the planner's own arithmetic
// (R-242), so the screen and a refused deploy always agree.
//
// A total of 0 is a runtime that does not know it. in_use_* is present only
// when the runtime reports usage (R-245); sampling it takes about a second,
// so the runtimes are read in parallel.
func (s *Server) handleCapacity(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.requireInstall(w, r, authz.InstallView); !ok {
		return
	}

	refs := s.Registry.ByCategory(api.CategoryRuntime)
	out := make([]map[string]any, len(refs))
	failed := make([]error, len(refs))
	var wg sync.WaitGroup
	for i, ref := range refs {
		rt, ok := s.Registry.Runtime(ref)
		if !ok {
			continue
		}
		wg.Add(1)
		go func(ctx context.Context) {
			defer wg.Done()
			// A runtime that does not answer is an entry saying so, not a
			// failed request: the others still have something to say.
			capacity, err := rt.Capacity(ctx)
			if err != nil {
				out[i] = map[string]any{"adapter_ref": ref, "status": "unreachable"}
				return
			}
			out[i], failed[i] = s.runtimeCapacity(ctx, ref, rt, capacity)
		}(r.Context())
	}
	wg.Wait()

	runtimes := make([]map[string]any, 0, len(refs))
	for i := range refs {
		if failed[i] != nil {
			Error(w, r, failed[i])
			return
		}
		if out[i] != nil {
			runtimes = append(runtimes, out[i])
		}
	}
	JSON(w, http.StatusOK, map[string]any{"runtimes": runtimes})
}

// runtimeCapacity is one answering runtime's entry in GET /capacity.
func (s *Server) runtimeCapacity(ctx context.Context, ref string, rt api.RuntimeAdapter, capacity api.Capacity) (map[string]any, error) {
	allocated, err := s.Allocations.AllocatedOn(ctx, ref, "")
	if err != nil {
		return nil, err
	}
	entry := map[string]any{
		"adapter_ref":            ref,
		"status":                 "ok",
		"total_cpu_millis":       capacity.TotalCPUMillis,
		"total_memory_bytes":     capacity.TotalMemoryBytes,
		"total_disk_bytes":       capacity.TotalDiskBytes,
		"allocated_cpu_millis":   allocated.CPUMillis,
		"allocated_memory_bytes": allocated.MemoryBytes,
		"allocated_disk_bytes":   allocated.DiskBytes,
		"running_workloads":      capacity.RunningWorkloads,
		"details":                capacity.Details,
		"reported":               capacity.Reported,
	}
	if caps, err := rt.Capabilities(ctx); err == nil && caps.ReportsUsage {
		if inUse, err := rt.InUse(ctx); err == nil {
			entry["in_use_cpu_millis"] = inUse.CPUMillis
			entry["in_use_memory_bytes"] = inUse.MemoryBytes
		}
	}
	return entry, nil
}

// declaredAdapters are the adapters the config file declares (R-271).
func (s *Server) declaredAdapters() []config.AdapterDecl {
	if s.Startup == nil {
		return nil
	}
	return s.Startup.Adapters
}

// edgeStatus is a routing adapter's edge, when there is a service to ask.
func (s *Server) edgeStatus(ref string) (edge.Status, bool) {
	if s.Edges == nil {
		return edge.Status{}, false
	}
	return s.Edges.Status(ref)
}
