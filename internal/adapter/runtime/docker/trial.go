package docker

import (
	"context"
	"io"
	"path"
	"regexp"
	"slices"
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
	created, err := a.createNetwork(ctx, "pando-trial-"+trialID, a.blockBits(), client.NetworkCreateOptions{
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
// exited, its ports settled, or the clock ran out with it still up. The last of
// those is a passing outcome too — what was being checked is whether the app
// starts and stays running.
//
// Ports are polled from inside the loop rather than read once at the end, so a
// healthy app that binds immediately costs a few seconds instead of the whole
// timeout. Detection runs this for every app somebody adds, and waiting the
// full budget on every success would make the trial the slowest thing Pando
// does for no information at all.
//
// It does not end at the first port, though. Ending there routed apps to
// whichever socket opened first: epmd before a Phoenix or RabbitMQ web UI, an
// ephemeral port Open Liberty opens seconds before 9080, Tomcat's shutdown port
// beside its connector (issue #90). Each port is sent an HTTP request, and the
// watch ends once one has answered and the set has stopped changing — or, with
// nothing answering, once it has stopped changing for long enough that nothing
// more is coming (R-097).
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

	ports := &portWatch{answered: map[int]int{}, probes: map[int]int{}}

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
			if a.pollPorts(ctx, deadline, id, &result, ports) {
				return result
			}
		}
	}
}

// portWatch is what the trial's watch carries from one poll to the next: when
// the observed ports last changed, the HTTP status each port has answered, and
// how many times a probe has reached each port.
type portWatch struct {
	lastChange time.Time
	answered   map[int]int
	probes     map[int]int
}

// pollPorts is one tick of watch: it records whether the container has
// started, which ports it listens on and which of them answer HTTP, and
// reports whether the ports have settled and the watch can end.
func (a *Adapter) pollPorts(ctx, deadline context.Context, id string, result *api.TrialResult, ports *portWatch) bool {
	if !a.everStarted(ctx, id) {
		return false
	}
	result.Started = true

	// Nothing to look at through a sandbox (Capabilities). The trial
	// then runs to its timeout, which is the price of the sandbox: it
	// cannot end early on a port it cannot see.
	if !a.observes() {
		return false
	}

	// Only a successful observation is allowed to replace an earlier
	// one. A check that could not run — the sidecar canceled as the
	// deadline approached — returns nothing, and letting that overwrite
	// what a previous tick saw would erase the answer at random.
	routable, loopback, observed := a.observePorts(deadline, id)
	if !observed {
		return false
	}
	result.LoopbackPorts = loopback
	if len(routable) == 0 {
		return false
	}

	now := time.Now()
	if !slices.Equal(routable, result.ObservedPorts) {
		result.ObservedPorts = routable
		ports.lastChange = now
	}

	// Asked again each tick until it answers: a server can bind its
	// port a moment before it is ready to serve on it.
	unanswered := unansweredPorts(routable, ports.answered)
	answers, probed := a.probeHTTP(deadline, id, unanswered)
	if probed {
		for _, p := range unanswered {
			ports.probes[p]++
		}
	}
	for p, status := range answers {
		ports.answered[p] = status
	}
	result.HTTPAnswers = answersFor(routable, ports.answered)
	result.HTTPProbedPorts = probedPorts(routable, ports)

	return portsSettled(routable, ports.answered, now.Sub(ports.lastChange))
}

// unansweredPorts returns the ports in routable that have not yet answered HTTP.
func unansweredPorts(routable []int, answered map[int]int) []int {
	var unanswered []int
	for _, p := range routable {
		if _, ok := answered[p]; !ok {
			unanswered = append(unanswered, p)
		}
	}
	return unanswered
}

// minProbes is how many unanswered requests make a port one that does not
// serve HTTP (TrialResult.HTTPProbedPorts).
const minProbes = 3

const (
	// httpSettle is how long the ports must stay as they are, once one has
	// answered HTTP, before the watch ends. Livebook opens a second web server
	// beside its main endpoint; ending at the first answer could miss the one
	// people open.
	httpSettle = 5 * time.Second

	// pendingSettle is the same while a port outside the ephemeral range has
	// not answered yet. Longer, because something is listening there
	// and may be about to serve; bounded, because a database's or a broker's
	// port never will.
	pendingSettle = 45 * time.Second
)

// ephemeralPorts is where Linux's ephemeral range starts. A server rarely
// listens there on purpose; a runtime does, for its own bookkeeping.
const ephemeralPorts = 32768

// portsSettled reports whether the observed ports have been unchanged long
// enough that the watch can end, given which of them have answered HTTP.
//
// A page on a port outside the ephemeral range is what the watch is for, and
// ends it soon after. Anything less ends it only after pendingSettle while a
// port outside that range is listening without having answered, and not at all
// while every port is in it with none answering. Each of those is usually a web
// server that has not finished starting: Grist's supervisor binds 8484 seconds
// before the server behind it is ready, and the plugin server's ephemeral port
// answered first; Kanboard's nginx answered on 443 before 80; Open Liberty
// binds an ephemeral port tens of seconds before 9080 (issue #90). Ending early
// routed each to the wrong port. The trial's own timeout bounds every wait.
func portsSettled(routable []int, answered map[int]int, quiet time.Duration) bool {
	var page, other, pending bool
	for _, p := range routable {
		status, ok := answered[p]
		switch {
		case ok && p < ephemeralPorts && servesAPage(status):
			page = true
		case ok:
			other = true
		case p < ephemeralPorts:
			pending = true
		}
	}
	switch {
	case page:
		return quiet >= httpSettle
	case pending:
		return quiet >= pendingSettle
	case other:
		return quiet >= httpSettle
	}
	// Every port is in the ephemeral range, and none has answered.
	return false
}

// servesAPage is detect.ServesAPage, which an adapter may not import (R-027):
// a status that means a person would see something.
func servesAPage(status int) bool {
	return (status >= 200 && status < 400) || status == 401 || status == 403
}

// probedPorts lists the ports still listening that a probe has reached, in
// port order: one that answered, or one asked at least minProbes times. A
// server can bind its port a moment before it serves on it, so a single
// unanswered request is not yet evidence that it never will.
func probedPorts(routable []int, ports *portWatch) []int {
	var out []int
	for _, p := range routable {
		if _, answered := ports.answered[p]; answered || ports.probes[p] >= minProbes {
			out = append(out, p)
		}
	}
	return out
}

// answersFor lists the HTTP answers for the ports still listening, in port
// order.
func answersFor(routable []int, answered map[int]int) []api.HTTPAnswer {
	var out []api.HTTPAnswer
	for _, p := range routable {
		if status, ok := answered[p]; ok {
			out = append(out, api.HTTPAnswer{Port: p, Status: status})
		}
	}
	return out
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
	out, ok := a.sidecar(ctx, targetID, "cat /proc/net/tcp /proc/net/tcp6 2>/dev/null")
	if !ok {
		return nil, nil, false
	}
	routable, loopback = listeningPorts(out)
	return routable, loopback, true
}

// probeHTTP sends each port a plain HTTP request from inside the trial
// container's network namespace, and returns the status of each that answered
// one (R-097).
//
// Listening is not serving. epmd, Erlang distribution, MQTT, Bolt, OTLP's gRPC
// and Tomcat's shutdown port all listen, and a browser sent to any of them gets
// nothing (issue #90). The request is the first thing a browser would send, and
// what counts as an answer is a reply that starts the way HTTP's does — an echo
// server repeating the request back does not.
//
// From the same busybox sidecar as observePorts, for the same reason: the app's
// image may have no shell and no tools. The ports are probed in parallel, each
// bounded by nc's own timeout, so a port that never replies costs seconds once
// rather than once per port.
func (a *Adapter) probeHTTP(ctx context.Context, targetID string, ports []int) (answers map[int]int, probed bool) {
	if len(ports) == 0 {
		return nil, false
	}
	out, ok := a.sidecar(ctx, targetID, probeScript(ports))
	if !ok {
		return nil, false
	}
	return httpAnswers(out), true
}

// probeScript is the shell probeHTTP runs: for each port, the first bytes of
// the reply to GET /, on one line after the port number. The ports are
// integers, so nothing from the app reaches the script's text.
//
// Enough of the reply to read past the headers into the body, where a TLS
// port says it was spoken to in plain HTTP (see tlsComplaint).
//
// The request's side of the connection stays open, as a browser's does, until
// the reply's first byte arrives or probeHoldTicks have passed. nc shuts its
// sending side the moment its input ends, and a server that reads that as the
// client leaving — hyper, uvicorn, Tornado, httpuv — drops the request
// unanswered: dufs, FastAPI, NiceGUI, Reflex, Streamlit and Shiny apps were all
// refused as answering no web request (issue #90).
//
// The hold ends at the reply rather than at nc's exit, because a server that
// waits for the client to close before closing itself — busybox httpd is one —
// would otherwise keep every answered probe open for the whole hold. dd reads
// exactly the first byte (head would read ahead and drop the rest), a marker
// file per port tells the hold it came, and the rest of the reply follows.
func probeScript(ports []int) string {
	listed := make([]string, 0, len(ports))
	for _, p := range ports {
		listed = append(listed, strconv.Itoa(p))
	}
	return "for p in " + strings.Join(listed, " ") + "; do " +
		`( r=$( (printf 'GET / HTTP/1.0\r\nHost: localhost\r\nUser-Agent: pando-trial\r\n\r\n'; ` +
		`i=0; while [ $i -lt ` + strconv.Itoa(probeHoldTicks) + ` ] && [ ! -e /tmp/.answered$p ]; do sleep 0.1; i=$((i+1)); done) | ` +
		`nc -w 3 127.0.0.1 "$p" 2>/dev/null | ` +
		`{ dd bs=1 count=1 2>/dev/null && touch /tmp/.answered$p && head -c 1023; } | tr '\r\n' '  '); echo "$p $r" ) & ` +
		"done; wait"
}

// probeHoldTicks is how long, in tenths of a second, the probe keeps its side of
// the connection open after the request while no reply has started
// (probeScript).
const probeHoldTicks = 30

// httpStatusLine matches what probeScript prints for a port that answered:
// the port, then the start of an HTTP status line.
var httpStatusLine = regexp.MustCompile(`^(\d{1,5}) HTTP/\d(?:\.\d)? (\d{3})`)

// tlsComplaint is a TLS port's answer to plain HTTP: nginx's "The plain HTTP
// request was sent to HTTPS port", Apache's "speaking plain HTTP to an
// SSL-enabled server port", Go's "Client sent an HTTP request to an HTTPS
// server". It is an HTTP status line, and it is not a port a browser can be
// sent to over HTTP: Kanboard was routed to its 443 and answered every request
// with a 400 (issue #90).
var tlsComplaint = regexp.MustCompile(`(?i)plain HTTP request was sent to HTTPS port|speaking plain HTTP to an SSL-enabled|sent an HTTP request to an HTTPS server`)

// httpAnswers reads probeScript's output. A port that answered with anything
// other than an HTTP status line — nothing, a binary greeting, its own request
// echoed back — is not in the result, and neither is a TLS port complaining
// about plain HTTP.
func httpAnswers(out string) map[int]int {
	answers := map[int]int{}
	for _, line := range strings.Split(out, "\n") {
		m := httpStatusLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || tlsComplaint.MatchString(line) {
			continue
		}
		port, _ := strconv.Atoi(m[1])
		status, _ := strconv.Atoi(m[2])
		if port > 0 && port <= 65535 {
			answers[port] = status
		}
	}
	return answers
}

// sidecar runs a shell command in a busybox container joined to the trial
// container's network namespace, and returns what it printed. ok is false when
// it could not run.
func (a *Adapter) sidecar(ctx context.Context, targetID, script string) (string, bool) {
	if err := a.ensureImage(ctx, trialImage, imageClaim{}); err != nil {
		return "", false
	}

	created, err := a.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Config: &container.Config{
			Image: trialImage,
			Cmd:   []string{"sh", "-c", script},
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
		return "", false
	}
	defer func() { _ = a.removeContainer(context.WithoutCancel(ctx), created.ID) }()

	if _, err := a.cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
		return "", false
	}

	wait := a.cli.ContainerWait(ctx, created.ID, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case <-wait.Result:
	case <-wait.Error:
		return "", false
	case <-ctx.Done():
		return "", false
	}

	// Read the sidecar's output with a context of its own. By this point the
	// trial's deadline may have passed, and the observation is already made —
	// losing it to a canceled log read would throw away the answer.
	read, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	return a.trialLogs(read, created.ID), true
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
