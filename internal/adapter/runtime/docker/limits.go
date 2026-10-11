package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/errs"
)

// limitSupport is what the daemon says it can enforce, read from `docker info`
// (R-240, issue #130).
//
// Docker accepts a CPU or memory limit it cannot apply, and starts the
// container without it: the create succeeds, a warning nobody reads says
// "Limitation discarded", and the app has the whole host. Rootless Docker does
// this whenever systemd has not delegated the cpu or memory cgroup controller
// to the user it runs as, which is the default on most distributions for cpu.
// So the adapter asks before claiming SupportsResourceLimits, and the planner
// refuses a deploy rather than start an app with limits that are not there.
type limitSupport struct {
	known         bool
	podman        bool
	cpu, memory   bool
	rootless      bool
	cgroupVersion string
	cgroupDriver  string
}

// limits reads limit support from the daemon. Not cached, unlike platform: the
// fix is a change to the daemon's host, and a deploy retried after it should
// see it without restarting Pando.
//
// A daemon that cannot say is an error, never a yes: the health check only
// pings, so a failure here would otherwise let a deploy through on a host that
// drops the limits (CodeRabbit on #177).
func (a *Adapter) limits(ctx context.Context) (limitSupport, error) {
	res, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return limitSupport{}, errs.Wrap(errs.AdapterUnavailable, "Could not ask Docker whether it applies CPU and memory limits.", err)
	}
	return a.support(ctx, res.Info)
}

// support reads limit support from the engine's info, or from Podman's own.
func (a *Adapter) support(ctx context.Context, info system.Info) (limitSupport, error) {
	podman, err := a.podman(ctx)
	if err != nil {
		return limitSupport{}, err
	}
	if podman {
		// Podman's Docker-compatible info reports CpuCfsQuota false on a
		// cgroup v2 host whose cpu controller is delegated (Podman 4.9), so
		// it is not an answer; Podman's own info is (issue #179).
		return a.podmanSupport(ctx)
	}
	return dockerSupport(info), nil
}

// dockerSupport reads Docker's (not Podman's) answer from its info.
func dockerSupport(info system.Info) limitSupport {
	// Driver "none" is rootless Docker without cgroup v2: it manages no
	// cgroups, so it ignores every resource flag whatever the flags say
	// (CodeRabbit on #177).
	managed := info.CgroupDriver != "none"
	return limitSupport{
		known: true,
		// NanoCPUs is a CFS quota over a period; either missing and it is
		// dropped.
		cpu:           managed && info.CPUCfsQuota && info.CPUCfsPeriod,
		memory:        managed && info.MemoryLimit,
		rootless:      slices.Contains(info.SecurityOptions, "name=rootless"),
		cgroupVersion: info.CgroupVersion,
		cgroupDriver:  info.CgroupDriver,
	}
}

// podman reports whether the engine is Podman. Not knowing is an error: taken
// for Docker, Podman's info would be read as refusing limits it applies, and
// every deploy refused (CodeRabbit on #177).
func (a *Adapter) podman(ctx context.Context) (bool, error) {
	v, err := a.cli.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return false, errs.Wrap(errs.AdapterUnavailable, "Could not ask the container engine which it is.", err)
	}
	return isPodman(v), nil
}

// libpodInfo is the part of Podman's own info, GET /libpod/info, that says
// which limits it can apply.
type libpodInfo struct {
	Host struct {
		CgroupManager     string   `json:"cgroupManager"`
		CgroupVersion     string   `json:"cgroupVersion"`
		CgroupControllers []string `json:"cgroupControllers"`
		Security          struct {
			Rootless bool `json:"rootless"`
		} `json:"security"`
	} `json:"host"`
}

// podmanSupport asks Podman through /libpod/info, served on the same socket
// as its Docker-compatible API. The moby client has no call for it, so the
// request goes over the client's own connection to the engine. Podman serves
// its own API only under a version, and 4.0.0's info has these fields on
// Podman 4 and 5 alike.
func (a *Adapter) podmanSupport(ctx context.Context) (limitSupport, error) {
	unavailable := func(err error) (limitSupport, error) {
		return limitSupport{}, errs.Wrap(errs.AdapterUnavailable, "Could not ask Podman whether it applies CPU and memory limits.", err)
	}
	dial := a.cli.Dialer()
	httpc := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }},
	}
	defer httpc.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://podman/v4.0.0/libpod/info", nil)
	if err != nil {
		return unavailable(err)
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return unavailable(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return unavailable(fmt.Errorf("GET /libpod/info: %s", resp.Status))
	}
	var info libpodInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&info); err != nil {
		return unavailable(err)
	}
	return readLibpod(info), nil
}

// readLibpod reads Podman's answer from its info. A cgroup controller Podman
// can use is listed in cgroupControllers: on cgroup v2 rootless, the ones
// systemd delegated to the user. Rootless Podman on cgroup v1 applies no
// limits at all, whatever it lists.
func readLibpod(info libpodInfo) limitSupport {
	h := info.Host
	usable := h.CgroupVersion == "v2" || !h.Security.Rootless
	return limitSupport{
		known:         true,
		podman:        true,
		cpu:           usable && slices.Contains(h.CgroupControllers, "cpu"),
		memory:        usable && slices.Contains(h.CgroupControllers, "memory"),
		rootless:      h.Security.Rootless,
		cgroupVersion: strings.TrimPrefix(h.CgroupVersion, "v"),
		cgroupDriver:  h.CgroupManager,
	}
}

// enforced is whether both limits every app has (R-240) would be applied.
func (l limitSupport) enforced() bool { return !l.known || (l.cpu && l.memory) }

// missing names the limits that would be dropped, for the message.
func (l limitSupport) missing() string {
	var m []string
	if !l.cpu {
		m = append(m, "CPU")
	}
	if !l.memory {
		m = append(m, "memory")
	}
	return strings.Join(m, " and ")
}

// remedy says why the limits would be dropped and what fixes it, for the
// planner's refusal. Docker's vocabulary, which is why it is written here and
// carried as text: core never learns it (R-251).
func (l limitSupport) remedy() string {
	if l.enforced() {
		return ""
	}
	if l.podman {
		return l.podmanRemedy()
	}
	if l.cgroupDriver == "none" {
		return "Docker is running without a cgroup driver (docker info reports \"Cgroup Driver: none\"), which rootless Docker does on a host " +
			"using cgroup v1, so it would start apps without their CPU and memory limits. Rootless Docker applies limits only with cgroup v2: " +
			"boot the host with systemd.unified_cgroup_hierarchy=1 on the kernel command line, or use a distribution that defaults to cgroup v2, " +
			"then restart Docker with systemctl --user restart docker."
	}
	if l.rootless {
		return "Docker is running rootless without the " + strings.ToLower(l.missing()) + " cgroup controller delegated to it, " +
			"so it would start apps without their " + l.missing() + " limits. Delegate the controllers to the user Docker runs as: " +
			"create /etc/systemd/system/user@.service.d/delegate.conf containing \"[Service]\" and \"Delegate=cpu cpuset io memory pids\", " +
			"run sudo systemctl daemon-reload, then restart rootless Docker with systemctl --user restart docker. " +
			"docker info should then report no cgroup warnings."
	}
	return "Docker reports that it cannot apply " + l.missing() + " limits on this host, so it would start apps without them. " +
		"This is a kernel or cgroup setup issue: run docker info on the host and fix the warnings it prints about cgroups, " +
		"then retry the deploy."
}

// podmanRemedy is remedy in Podman's words.
func (l limitSupport) podmanRemedy() string {
	if l.rootless && l.cgroupVersion != "2" {
		return "Podman is running rootless on a host using cgroup v1, where it applies no CPU or memory limits, so it would start apps without them. " +
			"Rootless Podman applies limits only with cgroup v2: boot the host with systemd.unified_cgroup_hierarchy=1 on the kernel command line, " +
			"or use a distribution that defaults to cgroup v2."
	}
	if l.rootless {
		return "Podman is running rootless without the " + strings.ToLower(l.missing()) + " cgroup controller delegated to it, " +
			"so it would start apps without their " + l.missing() + " limits. Delegate the controllers to the user Podman runs as: " +
			"create /etc/systemd/system/user@.service.d/delegate.conf containing \"[Service]\" and \"Delegate=cpu cpuset io memory pids\", " +
			"run sudo systemctl daemon-reload, then log that user out and back in, or reboot. " +
			"podman info --format '{{.Host.CgroupControllers}}' should then list cpu and memory."
	}
	return "Podman reports that it cannot apply " + l.missing() + " limits on this host: podman info --format '{{.Host.CgroupControllers}}' " +
		"does not list the " + strings.ToLower(l.missing()) + " controller, so it would start apps without them. " +
		"Enable the controller in the host's cgroup configuration, then retry the deploy."
}
