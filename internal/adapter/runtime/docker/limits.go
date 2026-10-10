package docker

import (
	"context"
	"slices"
	"strings"

	"github.com/moby/moby/client"
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
	cpu, memory   bool
	rootless      bool
	cgroupVersion string
	cgroupDriver  string
}

// limits reads limit support from the daemon. Not cached, unlike platform: the
// fix is a change to the daemon's host, and a deploy retried after it should
// see it without restarting Pando.
func (a *Adapter) limits(ctx context.Context) limitSupport {
	res, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		// Unknown, not unsupported: a daemon that cannot be asked fails the
		// health check, which the planner refuses on first and more usefully.
		return limitSupport{}
	}
	info := res.Info
	if a.podman(ctx) {
		// Podman's Docker-compatible info reports CpuCfsQuota false on a
		// cgroup v2 host whose cpu controller is delegated (seen with Podman
		// 4.9), so it cannot be read as an answer. Unknown, as before this
		// check existed; reading Podman's own cgroup controllers is issue
		// #179.
		return limitSupport{rootless: slices.Contains(info.SecurityOptions, "name=rootless")}
	}
	return limitSupport{
		known: true,
		// NanoCPUs is a CFS quota over a period; either missing and it is
		// dropped.
		cpu:           info.CPUCfsQuota && info.CPUCfsPeriod,
		memory:        info.MemoryLimit,
		rootless:      slices.Contains(info.SecurityOptions, "name=rootless"),
		cgroupVersion: info.CgroupVersion,
		cgroupDriver:  info.CgroupDriver,
	}
}

// podman reports whether the engine is Podman. False when it cannot be asked.
func (a *Adapter) podman(ctx context.Context) bool {
	v, err := a.cli.ServerVersion(ctx, client.ServerVersionOptions{})
	return err == nil && isPodman(v)
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
