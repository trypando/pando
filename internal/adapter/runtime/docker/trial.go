package docker

import (
	"context"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
)

// trialImage is the sidecar used to read the trial container's listening
// sockets. Busybox, because the only thing asked of it is `cat /proc/net/tcp`.
const trialImage = "busybox:1.37"

// Trial starts a workload once, in throwaway isolation, and reports what it did
// (R-097).
//
// The point is that ports are discovered by watching, not by asking. R-005 says
// the person deploying may not know what a port is, so a question about one is
// a question they cannot answer — and this is the mechanism that avoids having
// to ask it.
//
// Everything created here is labeled with the trial ID and removed at the end,
// including on the failure paths. A trial that leaks a container leaks a
// running copy of a stranger's app.
func (a *Adapter) Trial(ctx context.Context, req api.TrialRequest) (api.TrialResult, error) {
	if req.TrialID == "" {
		return api.TrialResult{}, errs.Newf(errs.Internal, "A trial run needs an identifier.")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}

	// The trial gets its own network, private like any other bundle (R-026).
	// Pando does not join it: nothing needs to reach the app, and an app that
	// is about to be observed is the last thing to give a route to.
	networkID, err := a.trialNetwork(ctx, req.TrialID)
	if err != nil {
		return api.TrialResult{}, err
	}
	defer func() {
		_, _ = a.cli.NetworkRemove(context.WithoutCancel(ctx), networkID, client.NetworkRemoveOptions{})
	}()

	if err := a.ensureImageWith(ctx, req.Image, req.PullAuth, forTrial); err != nil {
		return api.TrialResult{}, err
	}

	id, err := a.startTrialContainer(ctx, req, networkID)
	if err != nil {
		return api.TrialResult{}, err
	}
	defer func() {
		// WithoutCancel so that a canceled or timed-out trial still cleans up.
		// The context that bounds the observation must not also decide whether
		// the container is removed.
		_ = a.removeContainer(context.WithoutCancel(ctx), id)
	}()

	result := a.watch(ctx, id, timeout)

	// watch already polls for ports while the container is up, which is the
	// only time they can be read: a container that has exited has released its
	// sockets and its network namespace, and there is nothing left to look at.
	if a.observes() {
		result.ObservedWrites = a.observeWrites(ctx, id, req.DeclaredPaths)
	}
	result.ImageVolumes = a.imageVolumes(ctx, req.Image)

	logs := a.trialLogs(ctx, id)
	result.Log = logs
	if req.LogSink != nil && logs != "" {
		_, _ = io.WriteString(req.LogSink, logs)
	}
	return result, nil
}

func (a *Adapter) trialNetwork(ctx context.Context, trialID string) (string, error) {
	created, err := a.createNetwork(ctx, "pando-trial-"+trialID, client.NetworkCreateOptions{
		Driver:   "bridge",
		Internal: false, // the app may legitimately need to fetch something to start
		Labels:   map[string]string{labelManaged: "true", labelTrial: trialID},
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Could not create a network for the trial run.", err)
	}
	return created.ID, nil
}

func (a *Adapter) startTrialContainer(ctx context.Context, req api.TrialRequest, networkID string) (string, error) {
	env := make([]string, 0, len(req.Env))
	for k, v := range req.Env {
		env = append(env, k+"="+v.Reveal())
	}
	sort.Strings(env)

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image:      req.Image,
			Cmd:        req.Command,
			Entrypoint: req.Entrypoint,
			WorkingDir: req.WorkingDir,
			Env:        env,
			Labels:     map[string]string{labelManaged: "true", labelTrial: req.TrialID},
		},
		HostConfig: &container.HostConfig{
			// Never restart. A trial that crashes has produced its result, and
			// restarting would turn "this app needs a database" into a loop.
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
			AutoRemove:    false, // the container is inspected after it exits

			// The same runtime the app will be deployed on. A trial runs code
			// nobody has reviewed yet, so it gets the boundary the deploy gets:
			// running it under runc on a sandboxed install would put the least
			// trusted moment of an app's life outside the sandbox.
			Runtime: a.config.OCIRuntime,

			// Throwaway storage wherever the image declares it, so an app that
			// checks for its volume starts the way it will when deployed with
			// one — Vaultwarden refuses to start without /data mounted (issue
			// #55) — and a trial leaves no anonymous volume behind.
			Tmpfs: tmpfsFor(a.imageVolumes(ctx, req.Image)),
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkID: {NetworkID: networkID},
			},
		},
		Name: "pando-trial-" + req.TrialID,
	})
	if err != nil {
		return "", errs.Wrap(errs.AdapterFailed, "Could not create the trial container.", err)
	}
	if err := a.verifyRuntime(ctx, created.ID, "the trial run"); err != nil {
		return "", err
	}

	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		_ = a.removeContainer(context.WithoutCancel(ctx), created.ID)
		return "", errs.Wrap(errs.AdapterFailed, "Could not start the trial container.", err)
	}
	return created.ID, nil
}

// watch runs the trial and reports what happened.
//
// It ends as soon as it knows the answer, in any of three ways: the container
// exited, it bound a port, or the clock ran out with it still up. The last of
// those is a passing outcome too — what was being checked is whether the app
// starts and stays running.
//
// Ports are polled from inside the loop rather than read once at the end, so a
// healthy app that binds immediately costs a few seconds instead of the whole
// timeout. Detection runs this for every app somebody adds, and waiting the
// full budget on every success would make the trial the slowest thing Pando
// does for no information at all.
func (a *Adapter) watch(ctx context.Context, id string, timeout time.Duration) api.TrialResult {
	deadline, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	result := api.TrialResult{}
	wait := a.cli.ContainerWait(deadline, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})

	// The first tick is also the settle period: a container asked about the
	// instant it starts can report "running" before its process has had a
	// chance to fail.
	poll := time.NewTicker(min(portPollInterval, timeout))
	defer poll.Stop()

	for {
		select {
		case body := <-wait.Result:
			code := int(body.StatusCode)
			result.ExitCode = &code
			result.Started = a.everStarted(ctx, id)
			return result

		case <-wait.Error:
			return result

		case <-deadline.Done():
			// Still up when the clock ran out, which is the app working.
			result.Started = a.everStarted(ctx, id)
			return result

		case <-poll.C:
			if !a.everStarted(ctx, id) {
				continue
			}
			result.Started = true

			// Nothing to look at through a sandbox (Capabilities). The trial
			// then runs to its timeout, which is the price of the sandbox: it
			// cannot end early on a port it cannot see.
			if !a.observes() {
				continue
			}

			// Only a successful observation is allowed to replace an earlier
			// one. A check that could not run — the sidecar canceled as the
			// deadline approached — returns nothing, and letting that overwrite
			// what a previous tick saw would erase the answer at random.
			routable, loopback, observed := a.observePorts(deadline, id)
			if !observed {
				continue
			}
			result.LoopbackPorts = loopback
			if len(routable) > 0 {
				result.ObservedPorts = routable
				return result
			}
		}
	}
}

func tmpfsFor(paths []string) map[string]string {
	if len(paths) == 0 {
		return nil
	}
	out := make(map[string]string, len(paths))
	for _, p := range paths {
		out[p] = ""
	}
	return out
}

// imageVolumes reads the paths the image declares with VOLUME. Vaultwarden
// declares /data and refuses to start without storage there (issue #55).
func (a *Adapter) imageVolumes(ctx context.Context, ref string) []string {
	inspect, err := a.cli.ImageInspect(ctx, ref)
	if err != nil || inspect.Config == nil {
		return nil
	}
	paths := make([]string, 0, len(inspect.Config.Volumes))
	for p := range inspect.Config.Volumes {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	return paths
}

// portPollInterval is how often a running trial is checked for listening
// sockets. Each check starts a sidecar, so this is not free — but it is far
// cheaper than waiting out the timeout on every app that works.
const portPollInterval = 2 * time.Second

// everStarted reports whether the container ever reached a running state.
//
// "Never started" and "started and exited" are different situations: the first
// is usually a bad image or command, the second is usually a missing dependency
// (R-107). Reading StartedAt rather than Running keeps them apart after exit.
func (a *Adapter) everStarted(ctx context.Context, id string) bool {
	inspect, err := a.cli.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil || inspect.Container.State == nil {
		return false
	}
	return inspect.Container.State.Running || (inspect.Container.State.StartedAt != "" &&
		!strings.HasPrefix(inspect.Container.State.StartedAt, "0001-01-01"))
}

// observePorts reads the trial container's listening sockets (R-097).
//
// A sidecar joined to the container's network namespace reads /proc/net/tcp,
// rather than exec-ing in the app's own container. The app's image may contain
// no shell and no tools at all — a FROM scratch Go binary is the normal case,
// not an exotic one — and a port discovery that only works on images with a
// shell would fail exactly where it is most needed.
//
// Failure here is not an error. An unobserved port turns a deferred question
// back into one a person answers, which is worse but not broken.
func (a *Adapter) observePorts(ctx context.Context, targetID string) (routable, loopback []int, ok bool) {
	if err := a.ensureImage(ctx, trialImage, imageClaim{}); err != nil {
		return nil, nil, false
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: trialImage,
			Cmd:   []string{"sh", "-c", "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null"},
			Labels: map[string]string{
				labelManaged: "true", labelTrial: "observer",
			},
		},
		HostConfig: &container.HostConfig{
			// Sharing the target's network namespace is the whole mechanism:
			// the sidecar sees the app's sockets without the app's image
			// needing anything in it. It shares nothing else — not the process
			// namespace, not the filesystem.
			NetworkMode:   container.NetworkMode("container:" + targetID),
			RestartPolicy: container.RestartPolicy{Name: container.RestartPolicyDisabled},
		},
	})
	if err != nil {
		return nil, nil, false
	}
	defer func() { _ = a.removeContainer(context.WithoutCancel(ctx), created.ID) }()

	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return nil, nil, false
	}

	wait := a.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
	case <-wait.Error:
		return nil, nil, false
	case <-ctx.Done():
		return nil, nil, false
	}

	// Read the sidecar's output with a context of its own. By this point the
	// trial's deadline may have passed, and the observation is already made —
	// losing it to a canceled log read would throw away the answer.
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()

	routable, loopback = listeningPorts(a.trialLogs(read, created.ID))
	return routable, loopback, true
}

// listeningPorts parses /proc/net/tcp for sockets in the LISTEN state, split
// into ones traffic can reach and ones it cannot.
//
// The format is fixed-width columns; the two that matter are local_address
// (hex address:hex port, address little-endian) and st, where 0A is LISTEN.
//
// The address is not decoration. Two things in that namespace are listening but
// are not the app being routed to:
//
//   - 127.0.0.11 is Docker's own embedded DNS resolver, present in every
//     container on a user-defined network, on a port that changes every run.
//     Reported as the app's port, it makes detection propose a random number.
//   - anything else on loopback is the app, but bound somewhere nothing outside
//     the container can reach. That is a real and common mistake — a dev server
//     default — and it is worth saying so rather than reporting no port at all.
func listeningPorts(procNetTCP string) (routable, loopback []int) {
	const listenState = "0A"

	seen := map[int]bool{}
	for _, line := range strings.Split(procNetTCP, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 4 || fields[3] != listenState {
			continue
		}

		hexAddr, hexPort, found := strings.Cut(fields[1], ":")
		if !found {
			continue
		}
		port, err := strconv.ParseInt(hexPort, 16, 32)
		if err != nil || port <= 0 || port > 65535 || seen[int(port)] {
			continue
		}
		seen[int(port)] = true

		switch classifyAddress(hexAddr) {
		case addrDockerDNS:
			// Not the app's, and not worth mentioning to anyone.
		case addrLoopback:
			loopback = append(loopback, int(port))
		default:
			routable = append(routable, int(port))
		}
	}
	sort.Ints(routable)
	sort.Ints(loopback)
	return routable, loopback
}

type addrClass int

const (
	addrRoutable addrClass = iota
	addrLoopback
	addrDockerDNS
)

// addressKind classifies a /proc/net local_address.
//
// IPv4 addresses are 8 hex digits, little-endian, so 127.0.0.1 is "0100007F"
// and Docker's resolver at 127.0.0.11 is "0B00007F". IPv6 is 32 digits, and the
// only one that matters here is ::1.
func classifyAddress(hexAddr string) addrClass {
	switch len(hexAddr) {
	case 8:
		if strings.EqualFold(hexAddr, "0B00007F") {
			return addrDockerDNS
		}
		// The last byte of the little-endian word is the first octet.
		if strings.EqualFold(hexAddr[6:8], "7F") {
			return addrLoopback
		}
	case 32:
		if strings.EqualFold(hexAddr, "00000000000000000000000001000000") {
			return addrLoopback
		}
	}
	return addrRoutable
}

// observeWrites reports directories the app wrote outside its declared storage
// (R-202).
//
// ContainerDiff compares the container's filesystem against the image it came
// from, so this is what the app actually did rather than what it might do.
// R-203 is why it earns the effort: undeclared persistence works perfectly
// until the second deploy, then silently discards everything while reporting
// healthy.
func (a *Adapter) observeWrites(ctx context.Context, id string, declared []string) []string {
	changes, err := a.cli.ContainerDiff(ctx, id, client.ContainerDiffOptions{})
	if err != nil {
		return nil
	}

	dirs := map[string]bool{}
	for _, change := range changes.Changes {
		p := change.Path
		if isExpectedWrite(p) || underAny(p, declared) {
			continue
		}
		// Report the directory, not every file in it. "The app wrote to
		// /app/uploads" is something a person can act on; four hundred
		// filenames is not.
		dirs[path.Dir(p)] = true
	}

	// Report the deepest directories, not their ancestors. ContainerDiff marks
	// a parent as changed whenever a child is added, so /app/uploads always
	// arrives with /app beside it — and "the app wrote to /app" is not
	// something anyone can act on.
	var out []string
	for dir := range dirs {
		if dir == "/" || dir == "" || hasDescendantIn(dir, dirs) {
			continue
		}
		out = append(out, dir)
	}
	sort.Strings(out)
	return out
}

// hasDescendantIn reports whether a deeper directory in the set is under dir.
func hasDescendantIn(dir string, dirs map[string]bool) bool {
	prefix := strings.TrimSuffix(dir, "/") + "/"
	for other := range dirs {
		if other != dir && strings.HasPrefix(other, prefix) {
			return true
		}
	}
	return false
}

// expectedWrites are paths every container writes and nobody means to keep.
//
// Naming them matters: a persistence warning that fires on /tmp for every app
// is a warning people learn to dismiss without reading, which is worse than no
// warning at all.
var expectedWrites = []string{
	"/tmp", "/var/tmp", "/run", "/var/run", "/proc", "/sys", "/dev",
	"/var/log", "/var/cache", "/var/lib/apt", "/root/.cache", "/home",
	"/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/etc/passwd", "/etc/group",
	"/var/lib/dpkg", "/usr/share", "/var/lib/postgresql/pgdata_placeholder",
}

func isExpectedWrite(p string) bool { return underAny(p, expectedWrites) }

// underAny reports whether p is one of the prefixes, or sits beneath one.
//
// "/" is never a usable prefix here — it is under everything, and treating it
// as one discards every observation.
func underAny(p string, prefixes []string) bool {
	for _, prefix := range prefixes {
		prefix = strings.TrimSuffix(prefix, "/")
		if prefix == "" {
			continue
		}
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			return true
		}
	}
	return false
}

// trialLogs reads a container's output, combined and demultiplexed.
func (a *Adapter) trialLogs(ctx context.Context, id string) string {
	rc, err := a.cli.ContainerLogs(ctx, id, client.ContainerLogsOptions{
		ShowStdout: true, ShowStderr: true, Tail: strconv.Itoa(trialLogLines),
	})
	if err != nil {
		return ""
	}
	defer func() { _ = rc.Close() }()

	raw, err := io.ReadAll(io.LimitReader(rc, trialLogBytes))
	if err != nil {
		return ""
	}
	return demultiplex(raw)
}

const (
	// trialLogLines and trialLogBytes bound the capture. On a crash this log is
	// the whole answer (R-107), so it is generous — but it is shown in a
	// console and stored on a proposal, so it is not unbounded.
	trialLogLines = 2000
	trialLogBytes = 256 << 10
)

// demultiplex strips Docker's stream framing.
//
// Without a TTY, container output arrives as 8-byte headers followed by
// payload. Returning it raw puts control bytes in the middle of the crash log
// the user is being shown.
func demultiplex(raw []byte) string {
	var out strings.Builder
	for len(raw) >= 8 {
		size := int(raw[4])<<24 | int(raw[5])<<16 | int(raw[6])<<8 | int(raw[7])
		if raw[0] > 2 || size < 0 || size > len(raw)-8 {
			// Not framed after all — a TTY-attached container, or a partial
			// read. Take what is left as text.
			break
		}
		out.Write(raw[8 : 8+size])
		raw = raw[8+size:]
	}
	if out.Len() == 0 {
		return string(raw)
	}
	out.Write(raw)
	return out.String()
}

// CleanupTrials removes anything a previous trial left behind.
//
// Called at startup: a trial interrupted by Pando restarting would otherwise
// leave a running copy of someone's app with nothing tracking it.
func (a *Adapter) CleanupTrials(ctx context.Context) error {
	containers, err := a.cli.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", labelTrial),
	})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not list leftover trial containers.", err)
	}
	for _, c := range containers.Items {
		_ = a.removeContainer(ctx, c.ID)
	}

	networks, err := a.cli.NetworkList(ctx, client.NetworkListOptions{Filters: make(client.Filters).Add("label", labelTrial)})
	if err != nil {
		return errs.Wrap(errs.AdapterFailed, "Could not list leftover trial networks.", err)
	}
	for _, n := range networks.Items {
		_, _ = a.cli.NetworkRemove(ctx, n.ID, client.NetworkRemoveOptions{})
	}
	return nil
}
