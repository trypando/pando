package docker

import (
	"context"
	"strconv"
	"strings"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// What every container that runs an app's code is held to: its workloads and
// its trial run (design 03 §4.4; findings 8 and 13 of the 2026-10-16 security
// assessment).
//
// Memory and CPU caps did not stop a fork bomb: with no PID limit an app, or a
// repository's trial run, could exhaust the host's process table and take
// every app and Pando with it. And an app kept Docker's default capabilities
// and could gain more through a setuid binary.
//
// The capability set is Podman's default: Docker's minus NET_RAW, MKNOD and
// AUDIT_WRITE. It is a set images are already known to start under, because
// every image run on Podman starts under it, and it keeps what an ordinary
// image needs to start as root and drop to its own user — nginx and postgres
// chown their directories and setuid. Dropping everything, as the egress
// gateway does, suits a binary Pando wrote and breaks most images Pando did
// not. Without NET_RAW an app cannot forge packets on its network; ping still
// works, through the unprivileged ICMP sockets Docker and Podman allow.
// Named in full after dropping ALL, so a daemon configured with a wider
// default does not widen it.
var workloadCaps = []string{
	"CHOWN", "DAC_OVERRIDE", "FOWNER", "FSETID", "KILL", "NET_BIND_SERVICE",
	"SETFCAP", "SETGID", "SETPCAP", "SETUID", "SYS_CHROOT",
}

// DefaultPidsLimit is how many processes and threads a container may hold at
// once when the adapter's configuration does not say [P]. Well above what a
// web app, a JVM or a database runs with, and far below what exhausts a host.
const DefaultPidsLimit = 1024

// noNewPrivileges keeps a process from gaining privileges through execve —
// a setuid binary, or file capabilities.
const noNewPrivileges = "no-new-privileges"

// labelHardening marks a container created with harden, so one created before
// it existed is recreated rather than left running without it (matchesPlan).
// Bump the value when harden changes what it sets.
const (
	labelHardening = "pando.hardening"
	hardeningLevel = "1"
)

// harden applies the limits every container running an app's code gets.
func (a *Adapter) harden(h *container.HostConfig) {
	h.CapDrop = []string{"ALL"}
	h.CapAdd = append([]string(nil), workloadCaps...)
	h.SecurityOpt = append(h.SecurityOpt, noNewPrivileges)
	if pids := a.pidsLimit(); pids > 0 {
		h.PidsLimit = &pids
	}
}

// pidsLimit is the configured limit: zero is the default, negative is none.
func (a *Adapter) pidsLimit() int64 {
	switch {
	case a.config.PidsLimit < 0:
		return 0
	case a.config.PidsLimit == 0:
		return DefaultPidsLimit
	default:
		return a.config.PidsLimit
	}
}

// matchesPidsLimit reports whether a container runs under the process limit
// now configured. Docker reports no limit as nil, 0 or -1; all are none.
func (a *Adapter) matchesPidsLimit(hostCfg *container.HostConfig) bool {
	var have int64
	if hostCfg != nil && hostCfg.PidsLimit != nil && *hostCfg.PidsLimit > 0 {
		have = *hostCfg.PidsLimit
	}
	return have == a.pidsLimit()
}

// hardened reports whether a container was created with harden's current
// limits.
func hardened(labels map[string]string) bool {
	return labels[labelHardening] == hardeningLevel
}

// What the daemon has shown it can do about sizing a container's own layer.
const (
	layerQuotaUnknown  int32 = iota
	layerQuotaLikely         // the driver sizes layers; no create has said yet
	layerQuotaEnforced       // a create with a size succeeded on such a driver
	layerQuotaUnsupported
)

// createSized creates a workload's container with its own layer capped at the
// app's disk limit, where the daemon can (R-240, R-403).
//
// Best effort, and said so: only some storage drivers can size a layer, and
// none can size a volume, so the cap narrows how far one workload can write
// between two disk readings and R-403's measurement is what holds the app to
// its limit everywhere. Docker has no way to ask, so the first create with a
// size is the question; a refusal is remembered and the create made again
// without one.
func (a *Adapter) createSized(ctx context.Context, opts client.ContainerCreateOptions, diskBytes int64) (client.ContainerCreateResult, error) {
	if diskBytes > 0 && a.layerQuota.Load() == layerQuotaUnknown {
		a.decideLayerSizing(ctx)
	}
	return a.createSizedWith(ctx, opts, diskBytes, a.cli.ContainerCreate)
}

// decideLayerSizing rules sizing out for a daemon that would accept a size
// and not apply it. Docker with the containerd image store, which Docker
// Desktop uses, takes --storage-opt size and writes past it; so does any
// engine whose driver has no quota to apply it with. Only a driver that sizes
// a layer is asked, and a refusal from one of them (XFS without pquota) is
// still caught at create. Not knowing leaves it unknown, to be asked again.
func (a *Adapter) decideLayerSizing(ctx context.Context) {
	info, err := a.cli.Info(ctx, client.InfoOptions{})
	if err != nil {
		return
	}
	podman, err := a.podman(ctx)
	if err != nil {
		return
	}
	next := layerQuotaUnsupported
	if sizesLayers(info.Info.Driver, info.Info.DriverStatus, podman) {
		next = layerQuotaLikely
	}
	a.layerQuota.CompareAndSwap(layerQuotaUnknown, next)
}

// sizesLayers reports whether a storage driver applies a container's layer
// size. Podman is left out: its compatible API's answer is not one Pando has
// seen enforced, and claiming a cap that is not there is worse than not
// claiming one (R-254).
func sizesLayers(driver string, status [][2]string, podman bool) bool {
	if podman {
		return false
	}
	for _, kv := range status {
		if kv[0] == "driver-type" && strings.Contains(kv[1], "snapshotter") {
			return false
		}
	}
	switch driver {
	case "btrfs", "zfs":
		return true
	case "overlay2":
		for _, kv := range status {
			if kv[0] == "Backing Filesystem" {
				return kv[1] == "xfs"
			}
		}
	}
	return false
}

func (a *Adapter) createSizedWith(
	ctx context.Context, opts client.ContainerCreateOptions, diskBytes int64,
	create func(context.Context, client.ContainerCreateOptions) (client.ContainerCreateResult, error),
) (client.ContainerCreateResult, error) {
	if state := a.layerQuota.Load(); diskBytes <= 0 || opts.HostConfig == nil ||
		(state != layerQuotaLikely && state != layerQuotaEnforced) {
		return create(ctx, opts)
	}
	opts.HostConfig.StorageOpt = map[string]string{"size": strconv.FormatInt(diskBytes, 10)}
	created, err := create(ctx, opts)
	if err == nil {
		a.layerQuota.Store(layerQuotaEnforced)
		return created, nil
	}
	if !storageOptUnsupported(err) {
		return created, err
	}
	a.layerQuota.Store(layerQuotaUnsupported)
	opts.HostConfig.StorageOpt = nil
	return create(ctx, opts)
}

// matchesLayerSize reports whether a container's layer cap is the one the plan
// asks for. Only where the driver sizes layers: elsewhere no container has a
// cap and none is asked for, so there is nothing to compare.
func (a *Adapter) matchesLayerSize(ctx context.Context, hostCfg *container.HostConfig, diskBytes int64) bool {
	have := ""
	if hostCfg != nil {
		have = hostCfg.StorageOpt["size"]
	}
	want := ""
	if diskBytes > 0 {
		want = strconv.FormatInt(diskBytes, 10)
	}
	if have == want {
		return true
	}
	if a.layerQuota.Load() == layerQuotaUnknown {
		a.decideLayerSizing(ctx)
	}
	state := a.layerQuota.Load()
	return state != layerQuotaLikely && state != layerQuotaEnforced
}

// layerQuotaDetail is what Capacity says about it, for the operator.
func (a *Adapter) layerQuotaDetail() string {
	switch a.layerQuota.Load() {
	case layerQuotaEnforced:
		return "enforced"
	case layerQuotaUnsupported:
		return "not enforced: this storage driver cannot size a container; disk use is measured instead"
	default:
		return "not yet known: no app has been started with a disk limit since Pando started"
	}
}

// storageOptUnsupported reports whether a create failed only because the
// daemon cannot size a container's own layer: overlay2 off XFS, or on XFS
// mounted without pquota, and vfs and fuse-overlayfs, which rootless engines
// use. Docker says "--storage-opt is supported only for overlay over xfs with
// 'pquota' mount option"; Podman says the same of its "size" option.
func storageOptUnsupported(err error) bool {
	if err == nil {
		return false
	}
	// Only the engines' own phrases: a create can fail for many reasons that
	// mention a size or a quota, and taking one of those for this would stop
	// asking for a layer cap for the life of the process.
	msg := strings.ToLower(err.Error())
	for _, phrase := range []string{"storage-opt", "storage opt", "storageopt", "pquota", "backingfs"} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}
