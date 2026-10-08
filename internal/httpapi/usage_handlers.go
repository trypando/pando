package httpapi

import (
	"net/http"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// WorkloadUsage is one part of an app and what it is using now (R-245).
// Limits of 0 mean none: the part may use what the host has, and HostCPUMillis
// and HostMemoryBytes on the reading say how much that is.
type WorkloadUsage struct {
	Name    string `json:"name"`
	Primary bool   `json:"primary"`
	Running bool   `json:"running"`

	CPUMillis      int   `json:"cpu_millis"`
	CPULimitMillis int   `json:"cpu_limit_millis"`
	MemoryBytes    int64 `json:"memory_bytes"`
	MemoryLimit    int64 `json:"memory_limit_bytes"`

	// DiskBytes is written outside the part's volumes; -1 when unknown.
	DiskBytes int64         `json:"disk_bytes"`
	Volumes   []VolumeUsage `json:"volumes"`
}

// VolumeUsage is one volume a part mounts and how much it holds; -1 when
// unknown.
type VolumeUsage struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Path  string `json:"path"`
	Bytes int64  `json:"bytes"`
}

// handleAppUsage reports what each part of an app is using right now: CPU,
// memory, disk and its volumes' sizes, beside its limits (R-245).
//
// A reading, not a history — Pando keeps no time series and is not an APM
// product (R-016). The console asks again while the page is open. Its own
// endpoint rather than part of /status because a CPU reading takes the runtime
// about a second to sample, and status should not wait for it.
func (s *Server) handleAppUsage(w http.ResponseWriter, r *http.Request) {
	app, ok := s.requireControl(w, r, authz.AppView)
	if !ok {
		return
	}
	if app.PinnedSpecID == "" {
		Error(w, r, errs.New(errs.StateInvalid, "This app is not running yet, so it is using nothing."))
		return
	}
	rev, found, err := s.Apps.RevisionByID(r.Context(), app.PinnedSpecID)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !found {
		Error(w, r, errs.New(errs.StateInvalid, "This app's configuration could not be found."))
		return
	}
	runtime, ok := s.Registry.Runtime(rev.Body.Runtime.AdapterRef)
	if !ok {
		Error(w, r, errs.New(errs.AdapterUnavailable, "The runtime this app runs on is not configured."))
		return
	}

	// From the same cache as the reading: every open usage panel polls this,
	// and what a runtime can do does not change between polls (issue #72).
	ref := rev.Body.Runtime.AdapterRef
	caps, err := s.Observations.Capabilities(r.Context(), ref, runtime)
	if err != nil {
		Error(w, r, err)
		return
	}
	if !caps.ReportsUsage {
		// Said, not shown as zeros: a part using nothing and a runtime that
		// cannot say are different answers.
		JSON(w, http.StatusOK, map[string]any{"supported": false})
		return
	}

	reading, err := s.Observations.Usage(r.Context(), ref, runtime, app.ID)
	if err != nil {
		Error(w, r, err)
		return
	}
	body := map[string]any{
		"supported":   true,
		"reported_at": reading.Reported.Format(time.RFC3339),
		"workloads":   workloadUsages(rev.Body, reading),
	}
	if capacity, err := s.Observations.Capacity(r.Context(), ref, runtime); err == nil {
		body["host_cpu_millis"] = capacity.TotalCPUMillis
		body["host_memory_bytes"] = capacity.TotalMemoryBytes
	}
	JSON(w, http.StatusOK, body)
}

// workloadUsages pairs the runtime's reading with the spec: its order, which
// part is primary, and which volumes each part mounts and where. Parts the
// spec declares that are not running appear with nothing in use; parts the
// runtime runs that the spec does not declare — a provisioned database —
// follow, as they do in /status.
func workloadUsages(s *spec.AppSpec, reading api.BundleUsage) []WorkloadUsage {
	primary, _ := s.PrimaryWorkload()

	byName := map[string]api.WorkloadUsage{}
	for _, u := range reading.Workloads {
		byName[u.Workload] = u
	}
	volumeBytes := map[string]int64{}
	for _, v := range reading.Volumes {
		volumeBytes[v.VolumeID] = v.Bytes
	}
	volumeName := map[string]string{}
	for _, v := range s.Volumes {
		volumeName[v.ID] = v.Name
	}

	out := []WorkloadUsage{}
	add := func(name string, u api.WorkloadUsage, mounts []spec.Mount, declared bool) {
		wu := WorkloadUsage{
			Name: name, Primary: declared && name == primary.Name, Running: u.Running,
			CPUMillis: u.CPUMillis, CPULimitMillis: u.CPULimitMillis,
			MemoryBytes: u.MemoryBytes, MemoryLimit: u.MemoryLimitBytes,
			DiskBytes: u.DiskBytes, Volumes: []VolumeUsage{},
		}
		if u.Workload == "" {
			wu.DiskBytes = -1
		}
		for _, m := range mounts {
			bytes, known := volumeBytes[m.VolumeID]
			if !known {
				bytes = -1
			}
			wu.Volumes = append(wu.Volumes, VolumeUsage{ID: m.VolumeID, Name: volumeName[m.VolumeID], Path: m.Path, Bytes: bytes})
		}
		out = append(out, wu)
	}
	for _, w := range s.Workloads {
		add(w.Name, byName[w.Name], w.Mounts, true)
		delete(byName, w.Name)
	}
	for _, u := range reading.Workloads {
		if _, still := byName[u.Workload]; still {
			add(u.Workload, u, nil, false)
			delete(byName, u.Workload)
		}
	}
	return out
}
