package detect

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/shlex"
	"go.yaml.in/yaml/v3"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// ImportCompose turns a compose file into a draft spec.
//
// R-096: a compose file is a complete answer, not a hint. The author already
// said what runs, in what order, with which volumes and healthchecks — so this
// imports it rather than treating it as evidence for a guess.
//
// R-099 is the other half. Some compose constructs cannot survive the boundary
// Pando puts around an app, and this returns one of two outcomes for each:
//
//	rejected  — PLAN_COMPOSE_CONSTRUCT_REJECTED, naming the service and the
//	            construct. There is nothing to rewrite it to.
//	rewritten — imported with different mechanism and the same behavior, and
//	            a WARN_COMPOSE_CONSTRUCT_REWRITTEN saying exactly what changed.
//
// The split is not about severity. It is about whether the app still does what
// its author meant afterwards. Publishing a host port is a mechanism Pando
// replaces with its own routing and the app is still reachable, so it is
// rewritten. `privileged: true` has no replacement that keeps the isolation the
// rest of the system depends on, so it is refused.
func ImportCompose(src api.SourceView, name string) (Draft, error) {
	f, err := src.Open(name)
	if err != nil {
		return Draft{}, errs.Wrap(errs.ValidInvalid, "Could not read "+name+".", err)
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		return Draft{}, errs.Wrap(errs.ValidInvalid, "Could not read "+name+".", err)
	}

	invalid := func(err error) error {
		return errs.Wrap(errs.ValidInvalid,
			"Could not understand "+name+" — it is not valid compose YAML.", err).
			WithRemedy("Check the file with `docker compose config` and fix what it reports.")
	}

	// Interpolated before anything reads it, as `docker compose` does: every
	// value in the file, against the `.env` beside it (see composeVars).
	var tree map[string]any
	if err := yaml.Unmarshal(raw, &tree); err != nil {
		return Draft{}, invalid(err)
	}
	vars := newComposeVars(src, name)
	resolved, err := yaml.Marshal(vars.interpolate(tree))
	if err != nil {
		return Draft{}, invalid(err)
	}
	var file composeFile
	if err := yaml.Unmarshal(resolved, &file); err != nil {
		return Draft{}, invalid(err)
	}
	if len(file.Services) == 0 {
		return Draft{}, errs.Newf(errs.ValidInvalid,
			"%s declares no services, so there is nothing to run.", name).
			WithRemedy("Add a `services:` section naming at least one service.")
	}

	imp := &composeImport{file: file, source: name, src: src, vars: vars}
	if err := imp.rejectIncompatible(); err != nil {
		return Draft{}, err
	}
	return imp.draft(), nil
}

// legacyCompose reports a compose file in the version 1 format: services at
// the top level, with no `services:` key. Compose v2 refuses to run one, so
// it says nothing about how the app runs today, and refusing the repository
// over it left nbviewer undeployed beside a working Dockerfile (issue #90).
// It bids nothing, and the rest of the repository is read instead.
func legacyCompose(src api.SourceView, name string) bool {
	f, err := src.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	var top map[string]any
	if err := yaml.NewDecoder(io.LimitReader(f, 4<<20)).Decode(&top); err != nil || len(top) == 0 {
		return false
	}
	if _, ok := top["services"]; ok {
		return false
	}
	for key, v := range top {
		if key == "version" {
			continue
		}
		service, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if _, ok := service["image"]; ok {
			return true
		}
		if _, ok := service["build"]; ok {
			return true
		}
	}
	return false
}

// --- the compose file, as much of it as Pando reads -------------------------

type composeFile struct {
	Services map[string]composeService `yaml:"services"`
	Volumes  map[string]any            `yaml:"volumes"`
	Secrets  map[string]composeSecret  `yaml:"secrets"`
}

// composeSecret is a top-level secret. Only a file-backed one has anything in
// the repository to carry.
type composeSecret struct {
	File string `yaml:"file"`
}

type composeService struct {
	Image       string         `yaml:"image"`
	Build       any            `yaml:"build"`
	Command     any            `yaml:"command"`
	Entrypoint  any            `yaml:"entrypoint"`
	WorkingDir  string         `yaml:"working_dir"`
	Ports       []any          `yaml:"ports"`
	Expose      []any          `yaml:"expose"`
	Volumes     []any          `yaml:"volumes"`
	Environment any            `yaml:"environment"`
	EnvFile     any            `yaml:"env_file"`
	Secrets     []any          `yaml:"secrets"`
	DependsOn   any            `yaml:"depends_on"`
	Healthcheck *composeHealth `yaml:"healthcheck"`
	Profiles    []string       `yaml:"profiles"`

	// Constructs that are rejected or rewritten (R-099).
	Restart       string         `yaml:"restart"`
	ContainerName string         `yaml:"container_name"`
	NetworkMode   string         `yaml:"network_mode"`
	Privileged    bool           `yaml:"privileged"`
	PID           string         `yaml:"pid"`
	IPC           string         `yaml:"ipc"`
	Devices       []string       `yaml:"devices"`
	CapAdd        []string       `yaml:"cap_add"`
	Deploy        *composeDeploy `yaml:"deploy"`
}

type composeHealth struct {
	Test     any    `yaml:"test"`
	Interval string `yaml:"interval"`
	Timeout  string `yaml:"timeout"`
	Retries  int    `yaml:"retries"`
}

type composeDeploy struct {
	Replicas *int `yaml:"replicas"`
}

type composeImport struct {
	file   composeFile
	source string

	// src is the tree the compose file came out of, kept so the import can
	// ask it what a path is. A bind mount says nothing about whether its
	// source is a directory of data or a single configuration file, and the
	// two import differently — see the mount rejection below.
	src api.SourceView

	warnings []spec.Warning

	// valueSlots are values the app needs that nothing in the repository
	// holds: names an env_file template leaves empty, and variables an
	// environment entry takes from a shell Pando does not have.
	valueSlots []spec.Slot

	// vars is what the file was interpolated against.
	vars *composeVars

	// passedThrough are environment entries with no value, which compose
	// takes from its shell and Pando has nowhere to take from.
	passedThrough []string
}

// isFile reports whether a relative compose path names a regular file in the
// repository. An unreadable or missing path is not one: compose creates a
// directory for a bind mount that does not exist yet, and so a path Pando
// cannot see is a directory as far as this is concerned.
func (c *composeImport) isFile(hostPath string) bool {
	if c.src == nil {
		return false
	}
	clean := path.Clean(strings.TrimPrefix(hostPath, "./"))
	if clean == "." || strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
		return false
	}
	info, err := c.src.Stat(clean)
	return err == nil && !info.IsDir
}

// names returns the service names in a stable order.
//
// Compose services live in a map, and Go randomizes map iteration. Without
// this, which service Pando calls primary — and the order of the options in the
// question asking about it — would change between runs of the same repository.
//
// A service behind a profile is left out, as `docker compose up` leaves it out:
// the author made it opt-in. The voting app's `seed` ran against a vote service
// that was not ready and exited 50 into an app that was otherwise starting
// (issue #55). A file where every service has a profile has no default set,
// and all of them are kept.
func (c *composeImport) names() []string {
	names := make([]string, 0, len(c.file.Services))
	for name, s := range c.file.Services {
		if len(s.Profiles) == 0 && !scaledToZero(s) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		for name := range c.file.Services {
			names = append(names, name)
		}
	}
	sort.Strings(names)

	// A proxy that routes by reading the runtime socket is left out when
	// there is something else to run (see routesBySocket).
	var apps []string
	for _, name := range names {
		if !routesBySocket(c.file.Services[name]) {
			apps = append(apps, name)
		}
	}
	if len(apps) > 0 {
		return apps
	}
	return names
}

// socketProxies are reverse proxies that discover what to route to by
// reading the container runtime's socket.
var socketProxies = []string{"traefik", "nginx-proxy", "caddy-docker-proxy", "docker-gen"}

// routesBySocket reports a service that is a reverse proxy finding the other
// services through the container runtime's socket.
//
// Pando's own proxy is what gives an app its address and routes to it
// (R-023), and no app is given the runtime socket (R-099), so a compose
// file's Traefik in front of its services has nothing left to do. It was
// refused for the socket, and the whole stack with it — the full-stack
// FastAPI template, and every app whose compose file brings its own Traefik
// (issue #90). It is left out, with a warning, when the file has something
// else to run.
func routesBySocket(s composeService) bool {
	image := strings.ToLower(s.Image)
	proxy := false
	for _, p := range socketProxies {
		if strings.Contains(image, p) {
			proxy = true
			break
		}
	}
	if !proxy {
		return false
	}
	for _, m := range parseMounts(s.Volumes) {
		if strings.HasSuffix(path.Clean(m.hostPath), ".sock") {
			return true
		}
	}
	return false
}

// scaledToZero reports a service with `deploy.replicas: 0`, which `docker
// compose up` creates no container for. It is a tool the author runs by hand —
// Vapor's templates declare `migrate` and `revert` this way, beside the app and
// sharing its image — and Pando started both alongside the app (issue #90).
func scaledToZero(s composeService) bool {
	return s.Deploy != nil && s.Deploy.Replicas != nil && *s.Deploy.Replicas == 0
}

// profiled returns the services left out because they sit behind a profile
// or are scaled to zero.
func (c *composeImport) profiled() []string {
	kept := map[string]bool{}
	for _, name := range c.names() {
		kept[name] = true
	}
	var out []string
	for name := range c.file.Services {
		if !kept[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// --- rejection (R-099) ------------------------------------------------------

// rejection is one construct that cannot be imported.
type rejection struct {
	Service   string `json:"service"`
	Construct string `json:"construct"`
	Reason    string `json:"reason"`
}

// rejectIncompatible reports every construct that cannot cross the boundary.
//
// Every service is checked before anything is reported, so someone fixing a
// compose file gets the whole list rather than discovering the next problem
// after fixing the last.
//
// None of these is overridable. R-099 says "host policy governs whether an
// admin may override", but R-272 is the general pattern and it says policy can
// only raise the floor — an admin can narrow what Pando accepts and cannot
// widen it. See docs/design/notes-compose-override-conflict.md.
func (c *composeImport) rejectIncompatible() error {
	var found []rejection

	for _, name := range c.names() {
		s := c.file.Services[name]

		if mode := strings.TrimSpace(s.NetworkMode); mode != "" && mode != "bridge" && mode != "none" {
			found = append(found, rejection{name, "network_mode: " + mode,
				"Pando puts each app on its own private network and reaches it through the proxy. " +
					"A service sharing the host's network stack is not inside that boundary, " +
					"and nothing else in the system would still hold."})
		}
		if s.Privileged {
			found = append(found, rejection{name, "privileged: true",
				"A privileged container can reach the host it runs on, which is the one thing " +
					"the isolation between apps exists to prevent."})
		}
		if strings.TrimSpace(s.PID) == "host" {
			found = append(found, rejection{name, "pid: host",
				"Sharing the host's process namespace lets the service see and signal every " +
					"other process on the machine, including Pando's."})
		}
		if strings.TrimSpace(s.IPC) == "host" {
			found = append(found, rejection{name, "ipc: host",
				"Sharing the host's IPC namespace reaches memory belonging to other apps."})
		}
		if len(s.Devices) > 0 {
			found = append(found, rejection{name, "devices: " + strings.Join(s.Devices, ", "),
				"Passing a host device through gives the app direct hardware access, which " +
					"crosses the same boundary as a privileged container."})
		}
		if s.Deploy != nil && s.Deploy.Replicas != nil && *s.Deploy.Replicas > 1 {
			found = append(found, rejection{name,
				"deploy.replicas: " + strconv.Itoa(*s.Deploy.Replicas),
				"Pando runs one instance of an app in one place (R-010). It is not an " +
					"orchestrator, and pretending to honor a replica count would be a lie " +
					"about where the app is running."})
		}
		for _, mount := range serviceMounts(name, s.Volumes) {
			if mount.hostPath == "" {
				continue
			}
			if isAbsoluteHostPath(mount.hostPath) {
				if hostMountKind(mount) != hostRefused {
					// A directory of the app's data, or the host's clock:
					// rewritten, not refused (see mounts).
					continue
				}
				found = append(found, rejection{name, "volume " + mount.raw,
					"This mounts a path from the host machine into the app. Pando has no way to " +
						"honor it: the app may not run on the machine holding that path, and if it " +
						"did, the mount would reach outside the app's own storage."})
				continue
			}

			// A bind mount of a single file out of the repository — a
			// Caddyfile, an nginx.conf, an init.sql. A directory of them
			// becomes a volume Pando manages (see mounts below); a file
			// cannot, twice over. Nothing is read from the repository at
			// deploy time (R-020), so there would be nothing to put in that
			// volume, and Docker refuses to mount a directory over a file in
			// the image anyway — which is how this used to surface: an app
			// that imported and scanned and built, and then failed at the
			// last step with `source /var/lib/docker/... is not directory`.
			// A single file out of the repository — a Caddyfile, an
			// nginx.conf, an init.sql. Pando carries it in the spec and places
			// it in the container at start, so the app runs the way its author
			// wrote it. What cannot be carried is refused here: too big to
			// belong in a spec, or not text.
			//
			// Unless the service builds from a context that already contains
			// it, in which case there is nothing to carry: the file is in the
			// image. See files below.
			if c.isFile(mount.hostPath) && !c.inBuildContext(name, mount.hostPath) {
				if why, ok := c.uncarryable(mount.hostPath); !ok {
					found = append(found, rejection{name, "volume " + mount.raw, why})
				}
			}
		}
	}

	if len(found) == 0 {
		return nil
	}

	details := make([]map[string]any, 0, len(found))
	summary := make([]string, 0, len(found))
	for _, r := range found {
		details = append(details, map[string]any{
			"service": r.Service, "construct": r.Construct, "reason": r.Reason,
		})
		summary = append(summary, r.Service+": "+r.Construct)
	}

	// "1 construct(s)" is how a message written for the plural case reads in
	// the single one, which is the common one.
	count := fmt.Sprintf("%d constructs", len(found))
	if len(found) == 1 {
		count = "a construct"
	}

	return errs.Newf(errs.PlanComposeConstructRejected,
		"%s uses %s that cannot run inside the boundary Pando puts around an app: %s.",
		c.source, count, strings.Join(summary, "; ")).
		WithDetail("rejected", details).
		WithRemedy("Each entry above says why. Remove or change those lines in " + c.source +
			", or deploy without the compose file by supplying an image and a command instead.")
}

// build resolves how the app is built, from the compose file, into the spec.
//
// This used to emit `strategy: compose` with a pointer to the file, and two
// things followed. No builder implements compose — BuildKit declares
// `dockerfile` and nothing else — so every compose app that needed building was
// refused at plan time with "bld_buildkit cannot build this app the way it is
// set up", naming the builder rather than the cause. And it left the build
// instructions in the repository to be re-read later, which R-020 forbids
// outright: the spec is the sole record of how an app runs, and a `build:`
// stanza that can change under the app between deploys is exactly what that
// requirement exists to prevent.
//
// The importer has already parsed the file. Resolving the build into the spec
// here is the same work it does for ports, volumes and environment, and it is
// what makes the result deployable by the builder that exists.
func (c *composeImport) build() spec.Build {
	// The service that builds. A compose file whose services all carry an
	// `image:` needs no builder at all.
	var building string
	for _, name := range c.names() {
		if c.file.Services[name].Build != nil && building == "" {
			building = name
		}
	}

	if building == "" {
		// Nothing to build: every service names an image that already exists.
		return spec.Build{Strategy: spec.BuildPrebuilt, ComposeFile: c.source}
	}

	// A compose app is built as a compose app. Which services build, and from
	// where, is recorded per workload — see Workload.Build — because a file with
	// two buildable services needs two images and one Build block cannot say so.
	return spec.Build{Strategy: spec.BuildCompose, ComposeFile: c.source}
}

// workloadBuild is a service's `build:` as the spec records it.
func workloadBuild(raw any) *spec.WorkloadBuild {
	context, dockerfile, target := buildFields(raw)
	return &spec.WorkloadBuild{Context: context, Dockerfile: dockerfile, Target: target, Args: buildArgs(raw)}
}

// builtBy returns the service that builds the image another names, if one
// does: a service with both `build:` and an `image:` equal to this one.
func (c *composeImport) builtBy(service, image string) string {
	if strings.TrimSpace(image) == "" {
		return ""
	}
	var names []string
	for name, s := range c.file.Services {
		if name != service && s.Build != nil && s.Image == image {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return names[0]
}

// buildFields reads compose's two spellings of `build:`.
//
// A string is the context directory. A map carries context, dockerfile and
// target separately. Anything else is treated as absent rather than guessed at.
func buildFields(raw any) (context, dockerfile, target string) {
	switch b := raw.(type) {
	case string:
		return b, "", ""
	case map[string]any:
		context, _ = b["context"].(string)
		dockerfile, _ = b["dockerfile"].(string)
		target, _ = b["target"].(string)
		return context, dockerfile, target
	}
	return "", "", ""
}

// buildArgs reads a service's `build: args:`, in either of compose's shapes —
// a list of KEY=VALUE or a map, interpolated with the rest of the file. They
// were not imported, so a Dockerfile told `NODE_ENV=development` built as
// production (issue #55). An argument with no value in the file is left out:
// compose would take it from the shell, and there is none here.
func buildArgs(raw any) []spec.KV {
	b, ok := raw.(map[string]any)
	if !ok {
		return nil
	}
	var args []spec.KV
	switch a := b["args"].(type) {
	case []any:
		for _, entry := range a {
			if k, v, found := strings.Cut(scalar(entry), "="); found && k != "" {
				args = append(args, spec.KV{Key: k, Value: v})
			}
		}
	case map[string]any:
		keys := make([]string, 0, len(a))
		for k := range a {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if a[k] == nil {
				continue
			}
			args = append(args, spec.KV{Key: k, Value: scalar(a[k])})
		}
	}
	return args
}

// --- import -----------------------------------------------------------------

func (c *composeImport) draft() Draft {
	names := c.names()

	// The services Pando supplies rather than runs.
	//
	// `db: image: postgres:16` is the author saying this app needs a
	// PostgreSQL beside it. That is a dependency, and a dependency is a slot
	// (R-131) — so Pando provisions one, manages its storage and backs it up
	// with the app, and the service itself is not imported as a workload.
	//
	// Importing both produced two databases: the compose container, and the
	// one Pando provisioned for the slot it had also created. The app connected
	// to the first with whatever password the compose file interpolated from a
	// shell that does not exist here, which is how `DATABASE_URL is required`
	// became the whole of an app's logs.
	candidates := map[string]spec.SlotType{}
	for _, name := range names {
		if slotType, ok := backingService(name, c.file.Services[name].Image); ok {
			candidates[name] = slotType
		}
	}

	d := Draft{
		Build:    c.build(),
		Warnings: nil, // filled at the end, after every rewrite is known
	}

	for _, name := range c.profiled() {
		if routesBySocket(c.file.Services[name]) {
			c.rewrote(name, "service "+name,
				"This service is a reverse proxy that finds the others by reading the container "+
					"runtime's socket. Pando gives the app its own address and routes to it, and no "+
					"app is given the runtime socket, so the service was not imported. Routing labels "+
					"on the other services are not used either.")
			continue
		}
		if scaledToZero(c.file.Services[name]) {
			c.rewrote(name, "deploy.replicas: 0",
				"Pando runs what `docker compose up` runs, which starts no container for a service "+
					"scaled to zero. The service was not imported.")
			continue
		}
		c.rewrote(name, "profiles: "+strings.Join(c.file.Services[name].Profiles, ", "),
			"Pando runs what `docker compose up` runs, which leaves out a service behind a "+
				"profile. The service was not imported.")
	}

	var running []string
	for _, name := range names {
		if _, candidate := candidates[name]; !candidate {
			running = append(running, name)
			d.Workloads = append(d.Workloads, c.workload(name, false))
		}
	}

	// Only a service the app reaches through a connection URL is replaced.
	//
	// Swapping the compose container for a provisioned one works because the
	// variable that holds the URL is rewired to the new instance, credentials
	// and all. An app that reaches the service any other way — `REDIS_HOST:
	// redis`, a host written into its code, a JDBC URL — has nothing Pando can
	// rewire: it went looking for a host called `redis` with no password and
	// found a provisioned Redis under another name that wanted one. Rewriting
	// the bare host into a URL was worse, and handed Python a whole DSN as a
	// hostname ("label too long"). Those services are imported as the compose
	// file wrote them, which is what R-096 asks for in the first place
	// (issue #55).
	backing := map[string]spec.SlotType{}
	for _, name := range names {
		slotType, candidate := candidates[name]
		if !candidate {
			continue
		}
		if reachedByURL(d.Workloads, c.hostnames(name), slotType) {
			backing[name] = slotType
			continue
		}
		running = append(running, name)
		d.Workloads = append(d.Workloads, c.workload(name, false))
	}
	if len(d.Workloads) == 1 {
		d.Workloads[0].Primary = true
		d.Workloads[0].Exposed = true
	}

	d.Slots = append(c.wire(backing, d.Workloads), c.valueSlots...)
	d.Volumes = c.volumes(running)

	// A service that is no longer a workload is no longer something to wait
	// for. Pando starts a provisioned service before the app either way, and
	// one left out — a proxy, a tool behind a profile — never starts.
	imported := map[string]bool{}
	for _, w := range d.Workloads {
		imported[w.Name] = true
	}
	for i := range d.Workloads {
		var kept []string
		for _, on := range d.Workloads[i].DependsOn {
			if imported[on] {
				kept = append(kept, on)
			}
		}
		d.Workloads[i].DependsOn = kept
	}

	c.unsetWarnings()
	d.Warnings = c.warnings
	return d
}

// wire turns each service Pando supplies into a slot, and points the variables
// that named it at that slot instead.
//
// The compose file already says which variable reaches the database: the app's
// own `DATABASE_URL: postgres://…@db:5432/…`. Pando fills that variable from
// the service it provisions, so the app is wired the way its author wired it
// and nobody types a connection string.
//
// The slot takes that variable's name, because a name somebody reading the app
// will recognize beats one Pando made up. Only services some variable reaches
// by URL arrive here (see draft); the rest are imported as the compose file
// wrote them.
func (c *composeImport) wire(backing map[string]spec.SlotType, workloads []spec.Workload) []spec.Slot {
	services := make([]string, 0, len(backing))
	for name := range backing {
		services = append(services, name)
	}
	sort.Strings(services)

	var slots []spec.Slot
	for _, service := range services {
		image := c.file.Services[service].Image
		key := strings.ToUpper(service) + "_URL"

		// The variables whose value points at this service.
		var filled []string
		for i := range workloads {
			for j := range workloads[i].Env {
				e := &workloads[i].Env[j]
				// A connection URL only. A bare `REDIS_HOST: redis` given the
				// whole DSN is a hostname nothing can resolve.
				if e.Value == nil || schemeType(*e.Value) != backing[service] || !pointsAtAny(*e.Value, c.hostnames(service)) {
					continue
				}
				if len(filled) == 0 {
					key = e.Key
				}
				filled = append(filled, e.Key)
				ref := key
				e.Value = nil
				e.SlotRef = &ref
			}
		}

		evidence := []string{fmt.Sprintf("compose service %q runs %s", service, orUnknownImage(image))}
		if len(filled) > 0 {
			evidence = append(evidence, strings.Join(filled, ", ")+" pointed at it")
		}

		slots = append(slots, spec.Slot{
			Key:      key,
			Type:     backing[service],
			Required: true,
			Evidence: evidence,

			// The provisioned instance answers to the service's names too, so
			// a start command waiting on `db:5432` still finds it.
			Aliases: c.hostnames(service),

			// Filled, because the compose file already answered it: it runs a
			// database in a container beside the app, and "Pando runs one
			// inside this app" is that same sentence in Pando's vocabulary.
			// Leaving it empty turned an app that worked under
			// `docker compose up` into one that was accepted and then refused
			// at deploy — asking a question whose answer was in the file being
			// imported.
			Resolution: &spec.Resolution{Mode: spec.ResolutionProvisioned},
		})

		c.rewrote(service, "service "+service,
			"Pando runs the "+backing[service].DisplayName()+" this app needs, rather than the "+
				"container the compose file describes: it manages the storage, backs it up with "+
				"the app, and fills "+key+" with the address. Bind it to a database you already "+
				"run instead on the app's dependencies.")
	}
	return slots
}

// reachedByURL reports whether some workload holds a connection URL for the
// service, in a scheme the provisioned replacement's own URL uses.
func reachedByURL(workloads []spec.Workload, hosts []string, t spec.SlotType) bool {
	for _, w := range workloads {
		for _, e := range w.Env {
			if e.Value == nil || !strings.Contains(*e.Value, "://") {
				continue
			}
			if schemeType(*e.Value) == t && pointsAtAny(*e.Value, hosts) {
				return true
			}
		}
	}
	return false
}

// hostnames are the names a service is reached by: its own, and its
// container_name, which compose also resolves.
func (c *composeImport) hostnames(service string) []string {
	names := []string{service}
	if cn := strings.TrimSpace(c.file.Services[service].ContainerName); cn != "" && cn != service {
		names = append(names, cn)
	}
	return names
}

func pointsAtAny(value string, hosts []string) bool {
	for _, h := range hosts {
		if pointsAt(value, h) {
			return true
		}
	}
	return false
}

// pointsAt reports whether a value names a compose service as a host.
//
// The shapes that matter are the ones a connection string takes:
// `postgres://user:pw@db:5432/app`, `redis://cache:6379`, and a value that is
// just the service name. Matching a bare substring would catch a password that
// happens to contain "db".
func pointsAt(value, service string) bool {
	if value == service {
		return true
	}
	for _, shape := range []string{"@" + service + ":", "@" + service + "/", "//" + service + ":", "//" + service + "/", "=" + service + ":"} {
		if strings.Contains(value, shape) {
			return true
		}
	}
	return strings.HasSuffix(value, "@"+service) || strings.HasSuffix(value, "//"+service)
}

func (c *composeImport) workload(name string, only bool) spec.Workload {
	s := c.file.Services[name]

	w := spec.Workload{
		Name:       name,
		Image:      s.Image,
		Command:    stringList(s.Command),
		Entrypoint: stringList(s.Entrypoint),
		WorkingDir: s.WorkingDir,
		Env:        c.env(name, s),
		Ports:      c.ports(name, s),
		Mounts:     c.mounts(name, s),
		Files:      c.files(name, s),
		DependsOn:  dependsOn(s.DependsOn),
		Health:     health(s.Healthcheck),

		// Which service is the app's one canonical endpoint is not something a
		// compose file says (R-026). With one service there is no question;
		// with several, the detector asks rather than picking.
		Primary: only,
		Exposed: only,
	}

	// A service that builds says so here rather than at the app level, so a
	// compose file with two of them produces two images instead of one and a
	// warning.
	if s.Build != nil {
		w.Build = workloadBuild(s.Build)
	} else if builder := c.builtBy(name, s.Image); builder != "" {
		// `image: app` naming the image another service builds is that image,
		// as it is under compose: the build tags it locally and this service
		// runs it. Pando pulled docker.io/library/app instead, which does not
		// exist (issue #90). The same build is recorded, and the deploy builds
		// it once for both (see deploy's buildAll).
		w.Build = workloadBuild(c.file.Services[builder].Build)
	}

	// Pando names containers itself, because two apps importing compose files
	// that both say `container_name: web` would otherwise collide install-wide.
	//
	// The name still resolves inside the app, as it does under compose, where
	// it is how the other services reach this one: an nginx upstream of
	// `appseed_app:5005`, a DATABASE_URL naming the database's container
	// (issue #90). Only inside the app's own network, so nothing collides.
	if s.ContainerName != "" {
		if _, taken := c.file.Services[s.ContainerName]; !taken && s.ContainerName != name {
			w.Aliases = []string{s.ContainerName}
		}
		c.rewrote(name, "container_name: "+s.ContainerName,
			"Pando names containers itself so that two apps importing compose files "+
				"cannot collide over the same name. The other services in this app still reach it as "+
				s.ContainerName+".")
	}

	// Restart policy is configuration, not a question (R-104). Pando applies
	// its own, changeable in settings afterwards.
	if s.Restart != "" && s.Restart != "no" {
		c.rewrote(name, "restart: "+s.Restart,
			"Pando restarts apps according to its own policy, which you can change in settings.")
	}

	// cap_add is not rejected — a capability is narrower than privileged and
	// some are ordinary (NET_BIND_SERVICE). But it is not silently honored
	// either, because the runtime adapter decides what it grants.
	if len(s.CapAdd) > 0 {
		c.rewrote(name, "cap_add: "+strings.Join(s.CapAdd, ", "),
			"Added capabilities are not carried over. If the app needs one, the runtime's "+
				"isolation settings are where to grant it.")
	}

	return w
}

// ports imports the container port and drops the host port.
//
// A compose file publishing "8080:80" is saying two things: the service listens
// on 80, and it should be reachable on the host at 8080. The first is imported.
// The second is replaced by Pando's own routing — the app gets an address
// through the proxy (R-026), which is why nothing is lost by dropping it.
func (c *composeImport) ports(name string, s composeService) []spec.Port {
	var ports []spec.Port
	seen := map[int]bool{}

	add := func(n int, source spec.PortSource) {
		if n <= 0 || seen[n] {
			return
		}
		seen[n] = true
		ports = append(ports, spec.Port{Number: n, Protocol: "http", Source: source})
	}

	for _, raw := range s.Ports {
		text := scalar(raw)
		if text == "" {
			// The long form: { target: 80, published: 8080 }.
			if m, ok := raw.(map[string]any); ok {
				add(toInt(m["target"]), spec.PortCompose)
				if published := toInt(m["published"]); published != 0 {
					c.rewrotePublishedPort(name, fmt.Sprintf("%d:%d", published, toInt(m["target"])))
				}
			}
			continue
		}

		// "80", "8080:80", "127.0.0.1:8080:80", "8080:80/tcp"
		text = strings.SplitN(text, "/", 2)[0]
		parts := strings.Split(text, ":")
		container := parts[len(parts)-1]
		add(atoi(container), spec.PortCompose)
		if len(parts) > 1 {
			c.rewrotePublishedPort(name, scalar(raw))
		}
	}

	for _, raw := range s.Expose {
		add(atoi(scalar(raw)), spec.PortCompose)
	}
	return webPortsFirst(ports)
}

// notHTTP are ports whose protocol is known and is not HTTP.
var notHTTP = map[int]bool{
	21: true, 22: true, 23: true, 25: true, 53: true, 110: true, 143: true,
	465: true, 587: true, 993: true, 995: true, 1433: true, 1521: true,
	2222: true, 3306: true, 5432: true, 5672: true, 6379: true, 9042: true,
	11211: true, 27017: true, 1025: true, 1143: true, 2525: true,

	// Brokers, cluster links and binary protocols an image lists beside its
	// web UI (issue #90): MySQL's X protocol, MQTT and MQTT over TLS, AMQP
	// over TLS, STOMP, epmd and Erlang distribution (RabbitMQ, EMQX), Neo4j's
	// Bolt, OTLP and Jaeger over gRPC, BitTorrent, ZooKeeper, Kafka, NATS, and
	// Elasticsearch's transport.
	33060: true, 1883: true, 8883: true, 5671: true, 61613: true,
	4369: true, 25672: true, 4370: true, 5369: true, 7687: true,
	4317: true, 14250: true, 6881: true, 2181: true, 9092: true,
	4222: true, 6222: true, 9300: true,
}

// webPortsFirst orders a service's ports so the first is one a browser can use.
//
// The proxy sends traffic to a workload's first port, and a compose file lists
// ports in whatever order its author wrote them. Gitea's lists SSH next to
// HTTP, and the app was routed to port 22 (issue #55). A port whose protocol is
// known not to be HTTP is marked as plain TCP and moved after the rest; the
// order among the others is the author's.
func webPortsFirst(ports []spec.Port) []spec.Port {
	var web, other []spec.Port
	for _, p := range ports {
		if notHTTP[p.Number] {
			p.Protocol = "tcp"
			other = append(other, p)
			continue
		}
		web = append(web, p)
	}
	return append(web, other...)
}

func (c *composeImport) rewrotePublishedPort(service, mapping string) {
	c.rewrote(service, "ports: "+mapping,
		"Pando gives the app its own address and routes to it, so the app does not need "+
			"a port published on the host. The container's port is kept; the host's is not.")
}

// volumes imports named volumes. R-200: persistence declared in a compose file
// is imported and honored, and nothing special happens.
//
// Only what the imported workloads mount. A `pgdata` volume belonging to a
// compose database Pando now provisions itself is storage for a container that
// no longer exists — the provisioned service brings its own, and an app
// carrying an empty volume nothing writes to still has to answer for it at
// delete time (R-204).
func (c *composeImport) volumes(running []string) []spec.Volume {
	mounted := map[string]bool{}
	for _, service := range running {
		for _, m := range serviceMounts(service, c.file.Services[service].Volumes) {
			if m.volumeName != "" {
				mounted[m.volumeName] = true
			}
		}
	}

	var named []string
	for name := range c.file.Volumes {
		if mounted[name] || !c.mountedAnywhere(name) {
			named = append(named, name)
		}
	}

	// Anonymous and relative-bind mounts also become volumes, so that data an
	// author meant to keep is kept. Collected across services first so a volume
	// used by two of them is declared once.
	for _, service := range running {
		for _, m := range serviceMounts(service, c.file.Services[service].Volumes) {
			// A single file is carried in the spec, not stored in a volume.
			// The host's clock is not mounted at all.
			if m.hostPath != "" && c.isFile(m.hostPath) || isHostClock(m) {
				continue
			}
			if k := c.dirMount(service, m); k == dirInImage || k == dirCarried {
				continue
			}
			if m.volumeName != "" && !containsString(named, m.volumeName) {
				named = append(named, m.volumeName)
			}
		}
	}
	sort.Strings(named)

	volumes := make([]spec.Volume, 0, len(named))
	for _, name := range named {
		volumes = append(volumes, spec.Volume{
			ID: name, Name: name, Declared: spec.VolumeFromCompose,
		})
	}
	return volumes
}

// mountedAnywhere reports whether any service in the file mounts this volume.
// A declared volume nothing mounts is the author's, and is kept as it always
// was; one mounted only by a service Pando now provisions is not.
func (c *composeImport) mountedAnywhere(volume string) bool {
	for _, service := range c.names() {
		for _, m := range serviceMounts(service, c.file.Services[service].Volumes) {
			if m.volumeName == volume {
				return true
			}
		}
	}
	return false
}

// files carries single-file bind mounts into the spec.
//
// `./Caddyfile:/etc/caddy/Caddyfile` is configuration the app cannot start
// without, and neither mechanism Pando has for a path fits it: storage is a
// directory, and a host bind mount would mean reading the repository at deploy
// time, which R-020 forbids. So the file itself is read once, here, and stored
// in the spec — pinned to the revision, replayed at every start, the same way
// a generated build file already is.
//
// It is a snapshot, and the warning says so: editing the file in the repository
// changes nothing until somebody re-detects. That is R-020 working as intended
// rather than a gap — the spec is the record of how the app runs, and a file
// re-read from a branch at deploy time would make the same revision deploy
// differently tomorrow.
func (c *composeImport) files(name string, s composeService) []spec.File {
	var files []spec.File

	for _, m := range serviceMounts(name, s.Volumes) {
		switch c.dirMount(name, m) {
		case dirInImage:
			c.rewrote(name, "volume "+m.raw,
				m.hostPath+" is inside this service's build context, so it is already in the "+
					"image the build produces. The mount showed edits without rebuilding; Pando "+
					"deploys a built image, and a deploy is how a change reaches this app.")
			continue
		case dirCarried:
			for _, rel := range c.repoFiles(m.hostPath) {
				content, _ := c.read(path.Join(m.hostPath, rel))
				files = append(files, spec.File{
					Path: path.Join(m.containerPath, rel), Content: content, Mode: fileMode(content),
				})
			}
			c.rewrote(name, "volume "+m.raw,
				"Pando copied the files in "+m.hostPath+" out of the repository and carries them in "+
					"this app's configuration, placing them under "+m.containerPath+" each time the "+
					"service starts. It is a copy taken now: editing them in the repository changes "+
					"nothing until this app is read again.")
			continue
		}
		if m.hostPath == "" || !c.isFile(m.hostPath) {
			continue
		}

		// A file inside this service's own build context is already in the
		// image the build produces. The mount is there so edits show up
		// without rebuilding — `./backend/package.json:/code/package.json`
		// beside `build: backend` — and under Pando the image is built from
		// the pinned commit, so dropping it loses nothing and carrying it
		// would ship a second copy of a file the build already placed.
		//
		// It is also what keeps a `package-lock.json` from refusing an import:
		// a lockfile is far too big to travel in a spec, and it never needed
		// to.
		if c.inBuildContext(name, m.hostPath) {
			c.rewrote(name, "volume "+m.raw,
				m.hostPath+" is inside this service's build context, so it is already in the "+
					"image the build produces. The mount showed edits without rebuilding; Pando "+
					"deploys a built image, and a deploy is how a change reaches this app.")
			continue
		}

		content, ok := c.read(m.hostPath)
		if !ok {
			continue
		}

		files = append(files, spec.File{Path: m.containerPath, Content: content, Mode: fileMode(content)})
		c.rewrote(name, "volume "+m.raw,
			"Pando copied "+m.hostPath+" out of the repository and carries it in this app's "+
				"configuration, placing it at "+m.containerPath+" each time the service starts. "+
				"It is a copy taken now: editing "+m.hostPath+" in the repository changes nothing "+
				"until this app is read again.")
	}
	return append(files, c.secretFiles(name, s)...)
}

// secretFiles places a service's file-backed compose secrets where compose
// does: /run/secrets/<name>, or the target the service names.
//
// They were dropped, so a database told POSTGRES_PASSWORD_FILE=/run/secrets/…
// found no file, never initialized, and every service that needed it failed
// (issue #55). The file is in the repository, which is what the compose file
// reads it from too; it travels in the spec like any other carried file.
func (c *composeImport) secretFiles(service string, s composeService) []spec.File {
	var files []spec.File
	for _, raw := range s.Secrets {
		source, target := scalar(raw), ""
		if m, ok := raw.(map[string]any); ok {
			source, target = scalar(m["source"]), scalar(m["target"])
		}
		if source == "" {
			continue
		}
		if target == "" {
			target = source
		}
		if !strings.HasPrefix(target, "/") {
			target = "/run/secrets/" + target
		}
		secret, ok := c.file.Secrets[source]
		if !ok || secret.File == "" {
			continue
		}
		content, ok := c.read(secret.File)
		if !ok {
			continue
		}
		files = append(files, spec.File{Path: path.Clean(target), Content: content})
		c.rewrote(service, "secrets: "+source,
			"Pando copied "+secret.File+" out of the repository and places it at "+path.Clean(target)+
				" when the service starts, as compose does. Anyone who can read this app's "+
				"configuration can read it, as anyone who can read the repository already could.")
	}
	return files
}

// dirKind is what a relative bind mount of a directory imports as.
type dirKind int

const (
	dirNone    dirKind = iota // not a repository directory
	dirData                   // a volume Pando manages (see mounts)
	dirInImage                // dropped: the service's build already has it
	dirCarried                // its files travel in the spec
)

// carriedDirFiles caps how many files a mounted directory may carry. A
// directory of health check scripts or init SQL is a handful; a source tree is
// a build input.
const carriedDirFiles = 16

// dirMount classifies a relative bind mount of a directory.
//
// It became an empty volume, whatever it held. The voting app mounts
// `./healthchecks:/healthchecks` into images it does not build, found no
// scripts there, and its database was never healthy (issue #55). A directory
// the repository has files in is the author's configuration or source, not
// data — data directories are created by compose or hold only a .gitkeep — and
// it is carried when it is small text, dropped when it is not but the service
// builds from a context that contains it, and a volume only otherwise.
func (c *composeImport) dirMount(service string, m mount) dirKind {
	if m.hostPath == "" || isAbsoluteHostPath(m.hostPath) || c.isFile(m.hostPath) {
		return dirNone
	}
	files := c.repoFiles(m.hostPath)
	if len(files) == 0 {
		return dirData
	}
	if c.carryable(m.hostPath, files) {
		// Even inside the build context: a development stage often copies
		// nothing and relies on the mount for its source. The voting app builds
		// `target: dev`, whose stage has no `COPY . .`, and exited with "can't
		// open file app.py". The copy is of the same commit the image is built
		// from, so where the image does have the files, it changes nothing.
		return dirCarried
	}
	if c.inBuildContext(service, path.Join(m.hostPath, files[0])) {
		return dirInImage
	}
	return dirData
}

// carryable reports whether every file under a mounted directory can travel
// in the spec.
func (c *composeImport) carryable(hostPath string, files []string) bool {
	if len(files) > carriedDirFiles {
		return false
	}
	for _, rel := range files {
		if _, ok := c.read(path.Join(hostPath, rel)); !ok {
			return false
		}
	}
	return true
}

// repoFiles lists the files the repository holds under a directory, relative
// to it, up to one more than carriedDirFiles. Placeholder files that keep an
// empty directory in git are not content.
func (c *composeImport) repoFiles(hostPath string) []string {
	if c.src == nil {
		return nil
	}
	root := path.Clean(strings.TrimPrefix(hostPath, "./"))
	if root == "." || strings.HasPrefix(root, "..") || strings.ContainsAny(root, `*?[\`) {
		return nil
	}
	var files []string
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		entries, err := c.src.Glob(dir + "/*")
		if err != nil || depth > 4 {
			return
		}
		sort.Strings(entries)
		for _, entry := range entries {
			if len(files) > carriedDirFiles {
				return
			}
			entry = filepath.ToSlash(entry)
			if path.Dir(entry) != dir {
				continue
			}
			info, err := c.src.Stat(entry)
			if err != nil {
				continue
			}
			if info.IsDir {
				walk(entry, depth+1)
				continue
			}
			switch path.Base(entry) {
			case ".gitkeep", ".keep", ".gitignore":
				continue
			}
			files = append(files, strings.TrimPrefix(entry, root+"/"))
		}
	}
	walk(root, 0)
	return files
}

// fileMode marks a carried script executable. The repository's permission
// bits do not survive the trip, and a mounted script is usually run directly.
func fileMode(content string) int {
	if strings.HasPrefix(content, "#!") {
		return 0o755
	}
	return 0
}

// inBuildContext reports whether a path is inside the build context of the
// service that mounts it — which is to say, whether the image already has it.
func (c *composeImport) inBuildContext(service, hostPath string) bool {
	s := c.file.Services[service]
	if s.Build == nil {
		return false
	}

	context, _, _ := buildFields(s.Build)
	dir := path.Clean(strings.TrimPrefix(strings.TrimSpace(context), "./"))
	if dir == "" || dir == "." {
		// The whole repository is the context, so everything in it is.
		return true
	}

	file := path.Clean(strings.TrimPrefix(hostPath, "./"))
	return strings.HasPrefix(file, dir+"/")
}

// read returns a repository file's contents, if it is one Pando can carry.
func (c *composeImport) read(hostPath string) (string, bool) {
	if c.src == nil {
		return "", false
	}
	f, err := c.src.Open(path.Clean(strings.TrimPrefix(hostPath, "./")))
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	body, err := io.ReadAll(io.LimitReader(f, spec.FileSizeLimit+1))
	if err != nil || len(body) > spec.FileSizeLimit || !utf8.Valid(body) {
		return "", false
	}
	return string(body), true
}

// uncarryable reports why a file cannot travel in the spec, when it cannot.
//
// Two reasons, and both are about what a spec is: something a person reads and
// a database row holds. A megabyte of anything is a build input, and a binary
// is not configuration.
func (c *composeImport) uncarryable(hostPath string) (string, bool) {
	if _, ok := c.read(hostPath); ok {
		return "", true
	}

	clean := path.Clean(strings.TrimPrefix(hostPath, "./"))
	if info, err := c.src.Stat(clean); err == nil && info.Size > spec.FileSizeLimit {
		return "This mounts " + hostPath + ", which is " + fmt.Sprintf("%d KB", info.Size/1024) +
			". Pando carries a configuration file up to " + fmt.Sprintf("%d KB", spec.FileSizeLimit/1024) +
			" in the app's own configuration; a file this size is a build input. Copy it into the " +
			"image instead, with a `COPY " + path.Base(hostPath) + "` line in the service's Dockerfile.", false
	}

	return "This mounts " + hostPath + ", which is not a text file. Pando carries a configuration " +
		"file in the app's own configuration, and that is somewhere a person reads. Copy it into " +
		"the image instead, with a `COPY " + path.Base(hostPath) + "` line in the service's " +
		"Dockerfile.", false
}

func (c *composeImport) mounts(name string, s composeService) []spec.Mount {
	var mounts []spec.Mount
	for _, m := range serviceMounts(name, s.Volumes) {
		if m.volumeName == "" {
			continue
		}
		// A single file is carried in the spec instead (see files above).
		// Making it a volume as well is two mechanisms for one path, and the
		// one Docker refuses.
		if m.hostPath != "" && c.isFile(m.hostPath) {
			continue
		}
		// So is a directory of them, and one the image already has is left
		// to the image (see files above).
		if k := c.dirMount(name, m); k == dirInImage || k == dirCarried {
			continue
		}
		if isHostClock(m) {
			c.rewrote(name, "volume "+m.raw,
				"This mounts the host's time zone into the app. Pando does not mount paths from the "+
					"host machine, so the app keeps its image's time zone, usually UTC. Set TZ on the "+
					"app's variables if it needs another.")
			continue
		}
		mounts = append(mounts, spec.Mount{
			VolumeID: m.volumeName, Path: m.containerPath, ReadOnly: m.readOnly,
		})

		// A relative bind mount is the author saying "this directory matters",
		// without saying whether it holds data or source. Pando keeps the data
		// reading, because that is the one where being wrong is silent: a
		// source mount that turns up empty fails loudly on the trial run, while
		// discarded data looks like a healthy deploy until the second one
		// (R-203).
		if isAbsoluteHostPath(m.hostPath) {
			c.rewrote(name, "volume "+m.raw,
				"The host directory "+m.hostPath+" became a volume Pando manages, mounted at "+
					m.containerPath+". It starts empty: Pando cannot mount a path from the host "+
					"machine, so what is in that directory there is not copied. Restore it into the "+
					"volume if the app needs it.")
		} else if m.hostPath != "" {
			c.rewrote(name, "volume "+m.raw,
				"The host directory "+m.hostPath+" became a volume Pando manages, mounted at "+
					m.containerPath+". Pando cannot mount a path from the host machine, and "+
					"discarding the data instead is the failure that looks like success.")
		}
	}
	return mounts
}

// env imports a service's environment, as interpolated (see composeVars).
//
// `APP_DOMAIN: ${APP_DOMAIN:-localhost}` used to arrive at the container
// verbatim, as the fourteen characters "${APP_DOMAIN:-localhost}" — a value no
// app can use and nothing explains, from a file whose own semantics say an
// unset variable takes its default.
//
// A value that is one variable nothing sets — `ROOT_URL: ${ROOT_URL}` — is the
// author saying the value comes from outside the file. It reached the app as
// the literal text "${ROOT_URL}", which Rails read as a truthy FORCE_SSL and
// Meteor as a URL (issue #90). It is a required slot now: the plan asks for it
// and the deploy waits until it has one (R-130, R-132).
//
// An entry with no value at all — `- SECRET` — passes the variable through
// from the shell running compose, and is left out when the shell does not set
// it. With no shell here, the `.env` beside the file is the only place it can
// come from; otherwise it lands on the app's variables with nothing in it,
// which is what a hole is (R-130) and what an untyped .env.example key does.
func (c *composeImport) env(name string, s composeService) []spec.EnvEntry {
	// `env_file:` first, because `environment:` overrides it — compose's own
	// precedence, and the reason DATABASE_URL keeps the value the compose file
	// spells out rather than whatever a template beside it suggests.
	entries := c.fromEnvFiles(name, s)

	at := map[string]int{}
	for i, e := range entries {
		at[e.Key] = i
	}

	put := func(entry spec.EnvEntry) {
		if i, seen := at[entry.Key]; seen {
			entries[i] = entry
			return
		}
		at[entry.Key] = len(entries)
		entries = append(entries, entry)
	}
	add := func(key, value string, bare bool) {
		key = strings.TrimSpace(key)
		if key == "" {
			return
		}
		if variable, ok := c.vars.envSlots[name][key]; ok {
			ref := variable
			c.requireVariable(name, key, variable)
			put(spec.EnvEntry{Key: key, SlotRef: &ref, Source: spec.EnvFromCompose})
			return
		}
		if bare {
			v, ok := c.vars.values[key]
			if !ok {
				empty := ""
				c.passedThrough = append(c.passedThrough, key)
				put(spec.EnvEntry{Key: key, Value: &empty, Source: spec.EnvFromDetection})
				return
			}
			value = v
		}
		put(spec.EnvEntry{Key: key, Value: &value, Source: spec.EnvFromCompose})
	}

	switch v := s.Environment.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			add(k, scalar(v[k]), v[k] == nil)
		}
	case []any:
		for _, raw := range v {
			k, v, found := strings.Cut(scalar(raw), "=")
			add(k, v, !found)
		}
	}
	return entries
}

// requireVariable records a variable an environment entry takes from a shell
// Pando does not have, as a required slot named for the variable.
func (c *composeImport) requireVariable(service, key, variable string) {
	for _, s := range c.valueSlots {
		if s.Key == variable {
			return
		}
	}
	evidence := fmt.Sprintf("the compose service %q sets %s to ${%s}, and nothing in the repository gives %s a value",
		service, key, variable, variable)
	if key == variable {
		evidence = fmt.Sprintf("the compose service %q takes %s from the shell running `docker compose`, "+
			"and nothing in the repository gives it a value", service, key)
	}
	c.valueSlots = append(c.valueSlots, spec.Slot{
		Key:      variable,
		Type:     slotTypeFor(variable, ""),
		Required: true,
		Evidence: []string{evidence},
	})
}

// fromEnvFiles imports what a service's `env_file:` points at.
//
// Ignoring the directive entirely is how crewmate deployed with two of its
// variables and none of the other eight: the compose file said
// `env_file: .env`, Pando read `environment:` and nothing else, and the app
// crash-looped on `APP_BASE_URL is required` with no sign of it anywhere in the
// console. R-096 says a compose file is imported, and this is part of it.
//
// The file itself is usually absent, because a `.env` holds an app's passwords
// and is the first thing a `.gitignore` names. The template beside it — the
// `.env.example` its README tells you to copy — names the variables the app
// reads, so Pando takes the names and leaves the values empty, which is what a
// hole is (R-130). It says so in a warning: the values were never in the
// repository, and a variable nobody has filled in reaches nothing.
func (c *composeImport) fromEnvFiles(service string, s composeService) []spec.EnvEntry {
	var entries []spec.EnvEntry

	for _, name := range envFileNames(s.EnvFile) {
		if declared, ok := c.readEnvAssignments(name); ok {
			for _, kv := range declared {
				value := kv.Value
				entries = append(entries, spec.EnvEntry{
					Key: kv.Key, Value: &value, Source: spec.EnvFromCompose,
				})
			}
			continue
		}

		template, declared, ok := c.templateFor(name)
		if !ok {
			c.rewrote(service, "env_file: "+name,
				"This points at "+name+", which is not in the repository, and there is no template "+
					"beside it to read the variable names from. Add the variables this app needs on "+
					"its own settings — nothing else in the repository says what they are.")
			continue
		}

		// A name the template gives no value is a value the app has to be
		// given. Compose treats an env_file as required — `docker compose up`
		// stops without it — so these are required by the author's own file,
		// and a required slot is how Pando asks for one before deploying rather
		// than after the app has refused to start (R-132). They had been left
		// as empty variables, and the app crash-looped on "Missing required
		// environment variables" (issue #55). A name with a sample value is a
		// setting with a default, and stays a variable.
		var named []string
		for _, kv := range declared {
			// A connection URL at one of this file's own services is not the
			// author's machine: it is the address `docker compose up` gives
			// the app, and the one Pando rewires to what it provisions
			// (wire). Claper's .env.sample says
			// DATABASE_URL=postgres://claper:claper@db:5432/claper; left
			// empty, the app had no database and the db service nothing
			// pointing at it.
			if c.atOwnService(kv.Value) {
				value := kv.Value
				entries = append(entries, spec.EnvEntry{Key: kv.Key, Value: &value, Source: spec.EnvFromCompose})
				continue
			}
			empty := ""
			entry := spec.EnvEntry{Key: kv.Key, Value: &empty, Source: spec.EnvFromDetection}
			// A placeholder is no value either: `SESSION_SECRET=change-me`
			// says the same thing `SESSION_SECRET=` does, louder.
			if strings.TrimSpace(kv.Value) == "" || isPlaceholder(kv.Value) {
				ref := kv.Key
				entry = spec.EnvEntry{Key: kv.Key, SlotRef: &ref, Source: spec.EnvFromDetection}
				c.requireValue(kv.Key, template, name)
			}
			entries = append(entries, entry)
			named = append(named, kv.Key)
		}

		c.rewrote(service, "env_file: "+name,
			"This points at "+name+", which is not in the repository — it holds this app's own "+
				"values and is not committed. Pando took the variable names from "+template+
				" and left them empty: "+strings.Join(truncate(named, 8), ", ")+
				". Set them on this app's variables.")
	}
	return entries
}

// atOwnService reports a connection URL whose host is a service in this
// compose file.
func (c *composeImport) atOwnService(value string) bool {
	if schemeType(value) == spec.SlotUnknown {
		return false
	}
	for _, name := range c.names() {
		if pointsAtAny(value, c.hostnames(name)) {
			return true
		}
	}
	return false
}

// requireValue records a value an env_file template says the app needs.
func (c *composeImport) requireValue(key, template, envFile string) {
	for _, s := range c.valueSlots {
		if s.Key == key {
			return
		}
	}
	c.valueSlots = append(c.valueSlots, spec.Slot{
		Key:      key,
		Type:     spec.SlotUnknown,
		Required: true,
		Format:   formatFor(key, ""),
		Evidence: []string{fmt.Sprintf("named with no value in %s, the template for %s, which the compose file requires", template, envFile)},
	})
}

// envFileNames reads the `env_file:` shapes compose allows: one path, a list of
// them, or a list of `{path, required}` maps.
func envFileNames(v any) []string {
	switch f := v.(type) {
	case nil:
		return nil
	case []any:
		var out []string
		for _, entry := range f {
			if text := scalar(entry); text != "" {
				out = append(out, text)
				continue
			}
			if m, ok := entry.(map[string]any); ok {
				if text := scalar(m["path"]); text != "" {
					out = append(out, text)
				}
			}
		}
		return out
	default:
		if text := scalar(v); text != "" {
			return []string{text}
		}
		return nil
	}
}

// assignment is one KEY=VALUE line.
type assignment struct{ Key, Value string }

// readEnvAssignments parses a dotenv file out of the repository.
func (c *composeImport) readEnvAssignments(name string) ([]assignment, bool) {
	if c.src == nil {
		return nil, false
	}
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	if strings.HasPrefix(clean, "..") || strings.HasPrefix(clean, "/") {
		return nil, false
	}

	f, err := c.src.Open(clean)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()

	// Read as compose reads it: quotes removed, an inline comment dropped.
	// The value is what reaches the container, and `PORT=8080 # web` is 8080.
	return parseDotenv(f), true
}

// templateFor finds the committed template beside an env file that is not.
func (c *composeImport) templateFor(name string) (string, []assignment, bool) {
	clean := path.Clean(strings.TrimPrefix(name, "./"))
	candidates := []string{clean + ".example", clean + ".sample", clean + ".template"}
	if path.Base(clean) == ".env" {
		candidates = append(candidates, path.Join(path.Dir(clean), ".env.example"),
			path.Join(path.Dir(clean), ".env.sample"), path.Join(path.Dir(clean), ".env.template"))
	}

	for _, candidate := range candidates {
		if declared, ok := c.readEnvAssignments(candidate); ok && len(declared) > 0 {
			return candidate, declared, true
		}
	}
	return "", nil, false
}

// unsetWarnings says which variables the file reads that nothing sets.
//
// Compose substitutes an empty string for each and warns; so does Pando, once
// for the file rather than once per place. A variable that is the whole of an
// environment value is a slot instead, and asked for (see env), so it is not
// repeated here.
func (c *composeImport) unsetWarnings() {
	asked := map[string]bool{}
	for _, s := range c.valueSlots {
		asked[s.Key] = true
	}
	var unset []string
	for _, name := range c.vars.missingNames() {
		if !asked[name] {
			unset = append(unset, name)
		}
	}
	if len(unset) > 0 {
		c.warnings = append(c.warnings, spec.Warning{
			Code: spec.WarnComposeConstructRewritten,
			Message: fmt.Sprintf("%s reads %s from the shell running `docker compose`, and neither the "+
				"repository nor a %s file beside it sets them. Pando left them empty, as compose does. "+
				"If the app needs one, set it on this app's variables.",
				c.source, strings.Join(truncate(unset, 8), ", "), envFileName),
		})
	}
	if len(c.passedThrough) > 0 {
		c.warnings = append(c.warnings, spec.Warning{
			Code: spec.WarnComposeConstructRewritten,
			Message: fmt.Sprintf("%s passes %s through from the shell running `docker compose`, which "+
				"Pando does not have. They are on this app's variables with no value; set the ones "+
				"the app needs.", c.source, strings.Join(truncate(c.passedThrough, 8), ", ")),
		})
	}
}

func (c *composeImport) rewrote(service, construct, why string) {
	c.warnings = append(c.warnings, spec.Warning{
		Code: spec.WarnComposeConstructRewritten,
		Message: fmt.Sprintf("In the compose service %q, `%s` was not carried over as written. %s",
			service, construct, why),
	})
}

// --- compose value shapes ---------------------------------------------------

// mount is one entry in a service's `volumes:` list.
type mount struct {
	raw           string
	volumeName    string // named volume, or a name Pando made for a bind mount
	hostPath      string // non-empty when the entry was a bind mount
	containerPath string
	readOnly      bool
	anonymous     bool // a volume with no name, which compose makes per container
}

// serviceMounts is parseMounts for one service.
//
// An anonymous volume belongs to its container, not the project. It was named
// by its path alone, so a frontend and a backend that each keep
// `/usr/src/app/node_modules` in an anonymous volume shared one: the backend's
// packages filled it first, and the frontend exited with "react-scripts: not
// found" (issue #55). Its name now carries the service's.
func serviceMounts(service string, entries []any) []mount {
	mounts := parseMounts(entries)
	for i, m := range mounts {
		if m.anonymous {
			mounts[i].volumeName = volumeNameFor("", service) + "-" + m.volumeName
		}
	}
	return mounts
}

// parseMounts reads the short and long forms of a compose volume entry.
func parseMounts(entries []any) []mount {
	var mounts []mount

	for _, entry := range entries {
		if text := scalar(entry); text != "" {
			mounts = append(mounts, parseShortMount(text))
			continue
		}
		m, ok := entry.(map[string]any)
		if !ok {
			continue
		}
		source, target := scalar(m["source"]), scalar(m["target"])
		out := mount{
			raw:           source + ":" + target,
			containerPath: target,
			readOnly:      m["read_only"] == true,
		}
		if scalar(m["type"]) == "bind" || strings.HasPrefix(source, ".") || isAbsoluteHostPath(source) {
			out.hostPath = source
			out.volumeName = volumeNameFor(source, target)
		} else if source == "" && scalar(m["type"]) != "tmpfs" {
			out.volumeName = volumeNameFor("", target)
			out.anonymous = true
		} else {
			out.volumeName = source
		}
		mounts = append(mounts, out)
	}
	return mounts
}

func parseShortMount(text string) mount {
	parts := splitMountSpec(text)
	switch len(parts) {
	case 1:
		// An anonymous volume: "/var/lib/data".
		return mount{raw: text, containerPath: parts[0], volumeName: volumeNameFor("", parts[0]), anonymous: true}
	default:
		if strings.TrimSpace(parts[0]) == "" {
			// `${DATA_DIR}:/var/data` with DATA_DIR set nowhere. Compose would
			// mount an empty source, which is the directory it runs in — the
			// repository root. A volume of the app's own at the same path is
			// what the author was after: somewhere that data is kept.
			m := mount{raw: text, containerPath: parts[1], volumeName: volumeNameFor("", parts[1]), anonymous: true}
			m.readOnly = len(parts) > 2 && parts[2] == "ro"
			return m
		}
		m := mount{raw: text, containerPath: parts[1]}
		if len(parts) > 2 {
			m.readOnly = parts[2] == "ro"
		}
		if strings.HasPrefix(parts[0], ".") || isAbsoluteHostPath(parts[0]) || strings.HasPrefix(parts[0], "~") {
			m.hostPath = parts[0]
			m.volumeName = volumeNameFor(parts[0], parts[1])
		} else {
			m.volumeName = parts[0]
		}
		return m
	}
}

// splitMountSpec splits a compose volume entry on ":", ignoring colons inside
// a ${...} substitution.
//
// "${CONFIG_DIR:-./config}:/app/config:ro" has five colons and three fields.
// Splitting naively produces a volume named "${CONFIG_DIR" mounted at
// "-./config}", which is not a parse error anywhere — it is a bundle that comes
// up with a garbage volume attached to a nonsense path, and an app that cannot
// find its configuration for reasons nothing explains.
func splitMountSpec(text string) []string {
	var parts []string
	var current strings.Builder
	depth := 0

	for i := 0; i < len(text); i++ {
		switch {
		case text[i] == '$' && i+1 < len(text) && text[i+1] == '{':
			depth++
			current.WriteByte(text[i])
		case text[i] == '}' && depth > 0:
			depth--
			current.WriteByte(text[i])
		case text[i] == ':' && depth == 0:
			parts = append(parts, current.String())
			current.Reset()
		default:
			current.WriteByte(text[i])
		}
	}
	parts = append(parts, current.String())
	return parts
}

// hostKind is what a bind mount of an absolute host path imports as.
type hostKind int

const (
	// hostRefused reaches the host itself: a runtime socket, a system
	// directory, a single file. There is nothing to rewrite it to (R-099).
	hostRefused hostKind = iota

	// hostClock is the host's time zone, mounted so the container's clock
	// reads local time. Dropped: the app runs on UTC, as Pando does.
	hostClock

	// hostData is a directory the app keeps data in, which becomes a volume
	// Pando manages — the same rewrite a relative directory gets.
	hostData
)

// hostSystem are host paths whose contents are the machine's, not an app's.
var hostSystem = []string{"/var/run", "/run", "/proc", "/sys", "/dev", "/etc", "/var/lib/docker", "/var/lib/containers", "/boot", "/usr", "/bin", "/sbin", "/lib"}

// hostMountKind classifies a bind mount of an absolute host path.
//
// Every one was refused, which turned away an app over the directory it keeps
// its data in: whitebophir's `/opt/app/server-data`, ihatemoney's
// `/path/to/local/dir/for/sqlite/db:/database` placeholder (issue #90). The
// host's copy of that directory was never going to reach the app — it may not
// run on that machine — but a directory of its own at the same path is what
// the line asks for, and R-099 says rewrite where the app still does what its
// author meant. What reaches the host's own machinery is still refused.
func hostMountKind(m mount) hostKind {
	host := path.Clean(m.hostPath)
	switch host {
	case "/etc/localtime", "/etc/timezone", "/usr/share/zoneinfo":
		return hostClock
	case "/":
		return hostRefused
	}
	if strings.HasSuffix(host, ".sock") || strings.HasPrefix(host, "~") && strings.Contains(host, "/.") {
		return hostRefused
	}
	for _, system := range hostSystem {
		if host == system || strings.HasPrefix(host, system+"/") {
			return hostRefused
		}
	}
	// A file over a file: a volume is a directory, and Docker refuses to
	// mount one where the image has a file.
	if ext := path.Ext(path.Base(m.containerPath)); ext != "" && path.Base(m.containerPath) != ext {
		return hostRefused
	}
	return hostData
}

func isHostClock(m mount) bool {
	return isAbsoluteHostPath(m.hostPath) && hostMountKind(m) == hostClock
}

// isAbsoluteHostPath reports whether a mount source names a path on the host.
//
// A leading "." is relative to the compose file and handled separately: it is
// rewritten into a managed volume rather than refused, because it is usually
// the app's own data directory. An absolute path is classified by
// hostMountKind.
func isAbsoluteHostPath(source string) bool {
	return strings.HasPrefix(source, "/") || strings.HasPrefix(source, "~")
}

// volumeNameFor derives a stable volume name for a mount that had none.
func volumeNameFor(hostPath, containerPath string) string {
	base := path.Base(strings.TrimSuffix(containerPath, "/"))
	if base == "" || base == "." || base == "/" {
		base = path.Base(strings.TrimSuffix(hostPath, "/"))
	}
	name := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, base)
	name = strings.Trim(name, "-")
	if name == "" {
		return "data"
	}
	return name
}

func dependsOn(raw any) []string {
	var names []string
	switch v := raw.(type) {
	case []any:
		for _, entry := range v {
			if name := scalar(entry); name != "" {
				names = append(names, name)
			}
		}
	case map[string]any:
		// The long form: { db: { condition: service_healthy } }. The names are
		// imported; the runtime starts a dependent only once a dependency with
		// a health check reports healthy, which is that condition.
		for name := range v {
			names = append(names, name)
		}
		sort.Strings(names)
	}
	return names
}

func health(h *composeHealth) *spec.Healthcheck {
	if h == nil {
		return nil
	}
	// Compose's test is ["CMD", "curl", ...], ["CMD-SHELL", "..."] or a
	// string, which means the same as CMD-SHELL. The first element says how to
	// run the rest, not what to run.
	var command []string
	switch t := h.Test.(type) {
	case string:
		if strings.TrimSpace(t) != "" {
			command = []string{"sh", "-c", t}
		}
	default:
		list := stringList(t)
		switch {
		case len(list) == 0 || list[0] == "NONE":
			return nil
		case list[0] == "CMD-SHELL":
			// A shell line, run by a shell. Passing it on as one argv element
			// asked the runtime to execute a file named "curl -f http://…",
			// and the workload reported unhealthy for as long as it ran.
			command = []string{"sh", "-c", strings.Join(list[1:], " ")}
		case list[0] == "CMD":
			command = list[1:]
		default:
			command = list
		}
	}
	if len(command) == 0 {
		return nil
	}

	return &spec.Healthcheck{
		Command:         command,
		IntervalSeconds: int(seconds(h.Interval)),
		TimeoutSeconds:  int(seconds(h.Timeout)),
		Retries:         h.Retries,
	}
}

// seconds reads a compose duration: "30s", "1m30s", "500ms".
func seconds(text string) float64 {
	text = strings.TrimSpace(text)
	if text == "" {
		return 0
	}

	var total float64
	var number strings.Builder
	for _, r := range text {
		if (r >= '0' && r <= '9') || r == '.' {
			number.WriteRune(r)
			continue
		}
		n, _ := strconv.ParseFloat(number.String(), 64)
		number.Reset()
		switch r {
		case 'h':
			total += n * 3600
		case 'm':
			total += n * 60
		case 's':
			total += n
		}
	}
	return total
}

// scalar renders a YAML scalar as a string, and anything else as "".
func scalar(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

// stringList reads compose's "either a string or a list" shape.
func stringList(v any) []string {
	switch t := v.(type) {
	case string:
		// Split the way a shell would, as compose does. strings.Fields broke
		// `sh -c "npm run migrate && npm start"` into pieces and the shell
		// reported "unexpected EOF while looking for matching" (issue #55).
		// A string shlex cannot parse is kept whole rather than guessed at.
		words, err := shlex.Split(t)
		if err != nil {
			return []string{t}
		}
		return words
	case []any:
		var out []string
		for _, entry := range t {
			if s := scalar(entry); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func toInt(v any) int { return atoi(scalar(v)) }

func atoi(text string) int {
	n, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return 0
	}
	return n
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
