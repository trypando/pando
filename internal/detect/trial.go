package detect

import (
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// Trial is what a trial run told detection (R-097).
type Trial struct {
	// Ran is false when no trial happened — no runtime configured, or one that
	// does not support it. Distinguished from a trial that ran and observed
	// nothing, because the first leaves questions unanswered and the second is
	// information.
	Ran bool

	Started bool
	Crashed bool

	// Exited is a process that finished on its own with status 0 while it was
	// watched: a command-line program or a script, not a server (R-107).
	Exited bool

	ObservedPorts []int
	LoopbackPorts []int

	// HTTPProbed is true when the runtime sent each observed port an HTTP
	// request, and HTTPAnswers are the ports that answered (observedPorts).
	HTTPProbed  bool
	HTTPAnswers []api.HTTPAnswer

	ObservedWrites []string
	ImageVolumes   []string
	Log            string
}

// FromTrialResult reads an adapter's result, given what the runtime said it
// could observe.
//
// Capabilities are consulted rather than inferred from empty fields: a runtime
// that cannot observe ports and one that observed no listening port both return
// an empty slice, and they mean opposite things. The first leaves the question
// for a person; the second is evidence the app does not serve HTTP.
func FromTrialResult(caps api.RuntimeCapabilities, r api.TrialResult) Trial {
	t := Trial{
		Ran:     true,
		Started: r.Started,
		Crashed: r.ExitCode != nil && *r.ExitCode != 0,
		Exited:  r.ExitCode != nil && *r.ExitCode == 0,
		// Without NUL bytes. Some apps write them (Grafana does), and the
		// proposal is stored as jsonb, which refuses the \u0000 they encode
		// to — so the proposal could not be saved and the detection stayed
		// running for good (issue #55).
		Log: strings.ReplaceAll(r.Log, "\x00", ""),
	}
	if caps.SupportsPortObservation {
		t.ObservedPorts = r.ObservedPorts
		t.LoopbackPorts = r.LoopbackPorts
		if caps.SupportsHTTPProbe {
			t.HTTPProbed = true
			t.HTTPAnswers = r.HTTPAnswers
		}
	}
	if caps.SupportsWriteObservation {
		t.ObservedWrites = r.ObservedWrites
	}
	t.ImageVolumes = r.ImageVolumes
	return t
}

// ApplyTrial folds a trial run's observations into a draft and its questions.
//
// This is where R-097 pays for itself. A port that was going to be a question
// becomes an observation, and the question disappears rather than being asked
// of someone R-005 says may not know what a port is.
func ApplyTrial(draft Draft, questions []Question, t Trial) (Draft, []Question) {
	if !t.Ran {
		// No trial means no answers. Anything that was waiting on one has to be
		// asked after all — deferring a question to something that never
		// happened would drop it silently.
		//
		// The storage warning is not one of those, and used to be dropped here
		// by accident. R-201 says a repository that declares no volume gets the
		// warning "at setup", and R-202 only says the trial *improves* it by
		// naming the directory it saw written to. Attaching it inside the
		// trial's branch made the base warning conditional on the improvement,
		// so every source build — every strategy that has no image to start,
		// which is dockerfile, buildpack and static — got neither.
		//
		// R-203 is explicit that this is the worst failure mode in the system:
		// an undeclared Postgres fails loudly on first boot, while undeclared
		// persistence works perfectly until the second deploy and then discards
		// everything while reporting healthy. It cannot be the one warning that
		// silently does not fire.
		draft.Warnings = append(draft.Warnings, persistenceWarnings(draft, t)...)
		return draft, undefer(questions)
	}

	draft, tied := applyObservedPorts(draft, t)
	t.ImageVolumes = append(append([]string(nil), t.ImageVolumes...), mountsAskedFor(t.Log)...)
	draft = applyImageVolumes(draft, t)
	questions = resolvePortQuestions(questions, t)
	questions = choosePortQuestion(questions, tied)
	questions = loopbackQuestion(questions, t)

	draft.Slots = promoteSlots(draft.Slots, t)
	draft = requireFromTrial(draft, t)
	draft.Warnings = append(draft.Warnings, persistenceWarnings(draft, t)...)

	return draft, questions
}

// applyObservedPorts replaces guessed ports with watched ones.
//
// An observed port supersedes rather than joins: a framework default of 3000
// sitting beside an observed 8080 would leave the review UI showing two ports
// where the app has one, and the user deciding between a fact and a guess.
//
// The web port first: traffic goes to the first, and Gitea's image listens on
// SSH as well as HTTP, and Mailpit's on SMTP. Routed to 22 or 1025, the proxy's
// HTTP request was a protocol error (issue #55). observedPorts decides which
// that is, and reports the ports it could not choose between.
func applyObservedPorts(draft Draft, t Trial) (Draft, []int) {
	if len(t.ObservedPorts) == 0 {
		return draft, nil
	}

	// Source is what the review UI shows, and it is the whole point of R-097:
	// this was watched, not guessed. What the image or compose file declared
	// is evidence for which of them is the web port.
	for i, w := range draft.Workloads {
		if !w.Primary {
			continue
		}
		declared := map[int]bool{}
		for _, p := range w.Ports {
			if p.Source == spec.PortExpose || p.Source == spec.PortCompose {
				declared[p.Number] = true
			}
		}
		if len(w.Ports) > 0 && !showsWebPort(t) {
			// Nothing watched says where the app serves; the guess stands.
			return draft, nil
		}
		var tied []int
		draft.Workloads[i].Ports, tied = observedPorts(t, declared)
		return draft, tied
	}
	return draft, nil
}

// showsWebPort reports a trial that watched a port the app could be serving
// on: one outside the ephemeral range that answered HTTP or, failing an
// answer, carries no protocol known not to be HTTP.
//
// A trial that saw only the others says nothing about the web port, and
// replacing the guess with what it saw made things worse (issue #90). A Node
// server that calls listen(process.env.PORT) is given no PORT by the trial and
// listens on a random ephemeral port; recorded as watched, it was started
// without PORT at deploy as well, on another random port, and answered 502.
// A Phoenix release that crashed before its endpoint started left only epmd's
// 4369. Kept as a guess, the port is the one the deploy tells the app through
// PORT and checks again once the app is built.
func showsWebPort(t Trial) bool {
	answered := map[int]bool{}
	for _, a := range t.HTTPAnswers {
		answered[a.Port] = true
	}
	for _, p := range t.ObservedPorts {
		if MayServeWeb(p, answered[p]) {
			return true
		}
	}
	return false
}

// MayServeWeb reports a watched port that could be where an app serves: one
// outside the ephemeral range that answered HTTP or, failing an answer,
// carries no protocol known not to be HTTP (see showsWebPort).
func MayServeWeb(port int, answeredHTTP bool) bool {
	return port < ephemeralPorts && (answeredHTTP || !notHTTP[port])
}

// applyImageVolumes gives the primary workload storage wherever its image
// declares it (R-200).
//
// A Dockerfile's VOLUME is the image's author saying where its data lives, the
// same declaration a compose file's volumes are. Without it the data went into
// the container's writable layer and was lost with the container — and
// Vaultwarden, which checks, refused to start at all: "No persistent volume!"
// (issue #55). A path the workload already mounts is left as it is.
func applyImageVolumes(draft Draft, t Trial) Draft {
	for i, w := range draft.Workloads {
		if !w.Primary {
			continue
		}
		mounted := map[string]bool{}
		for _, m := range w.Mounts {
			mounted[path.Clean(m.Path)] = true
		}
		taken := map[string]bool{}
		for _, v := range draft.Volumes {
			taken[v.ID] = true
		}
		for _, p := range t.ImageVolumes {
			clean := path.Clean(p)
			if !path.IsAbs(clean) || clean == "/" || mounted[clean] {
				continue
			}
			name := volumeNameFor("", clean)
			for n := 2; taken[name]; n++ {
				name = fmt.Sprintf("%s-%d", volumeNameFor("", clean), n)
			}
			taken[name] = true
			draft.Volumes = append(draft.Volumes, spec.Volume{ID: name, Name: name, Declared: spec.VolumeFromImage})
			draft.Workloads[i].Mounts = append(draft.Workloads[i].Mounts, spec.Mount{VolumeID: name, Path: clean})
			mounted[clean] = true
		}
		break
	}
	return draft
}

// mountAsked is an app saying a path must be mounted: "ERROR: /etc/letsencrypt
// is not mounted!", "/data must be mounted", "/config is not a mountpoint".
var mountAsked = regexp.MustCompile(`(?i)(/[A-Za-z0-9._/-]*[A-Za-z0-9._-])['"]?\s+(?:is not mounted|isn't mounted|must be mounted|should be mounted|needs to be mounted|is not a mount ?point)`)

// mountsAskedFor reads the paths an app's own output says must be mounted.
//
// The image's VOLUME is one way an image names where its data lives; its
// startup check is another. Nginx Proxy Manager declares /data and checks for
// /etc/letsencrypt as well, and stopped at "/etc/letsencrypt is not mounted!"
// on every start (issue #90). The app named the path; Pando gives it a volume,
// as it does for a VOLUME (R-200).
func mountsAskedFor(log string) []string {
	var paths []string
	seen := map[string]bool{}
	for _, m := range mountAsked.FindAllStringSubmatch(log, -1) {
		p := path.Clean(m[1])
		if p == "/" || seen[p] {
			continue
		}
		seen[p] = true
		paths = append(paths, p)
	}
	return paths
}

// onlyNonHTTP reports observed ports that are all ports of protocols other than
// HTTP — a database or a mail server with nothing for a browser to open.
func onlyNonHTTP(ports []int) bool {
	if len(ports) == 0 {
		return false
	}
	for _, p := range ports {
		if !notHTTP[p] {
			return false
		}
	}
	return true
}

// resolvePortQuestions drops port questions the trial answered, and promotes
// the ones it did not.
func resolvePortQuestions(questions []Question, t Trial) []Question {
	observed := len(t.ObservedPorts) > 0

	var out []Question
	for _, q := range questions {
		if q.Kind != api.QuestionPort {
			out = append(out, q)
			continue
		}
		if observed {
			// Answered. Nobody is asked.
			continue
		}
		// The trial ran and saw nothing traffic could reach. That is now a real
		// question, and it has to say what was tried — a question that
		// reappears without explanation looks like Pando forgot it had already
		// checked.
		q.Deferred = false
		q.Prompt += " " + whatTheTrialSaw(t)
		out = append(out, q)
	}
	return out
}

// whatTheTrialSaw explains why the port still has to be asked about.
//
// An app bound only to 127.0.0.1 is the case worth spelling out. It is a dev
// server default, the container reports itself healthy, and every request times
// out with nothing anywhere saying why — so "it opened no port" would be both
// wrong and the least useful thing Pando could say when it knows better.
func whatTheTrialSaw(t Trial) string {
	if len(t.LoopbackPorts) > 0 {
		return fmt.Sprintf(
			"Pando started the app and watched it. The app is listening on %s, but only on its own "+
				"internal address (127.0.0.1), which nothing outside the app can reach — so requests "+
				"to it would time out even though the app looks healthy. Apps usually have a setting "+
				"for which address to listen on; it normally needs to be 0.0.0.0 rather than "+
				"127.0.0.1 or localhost.",
			joinPorts(t.LoopbackPorts))
	}
	if !t.Started {
		return "Pando tried to start the app and it did not start, so this could not be worked out " +
			"automatically."
	}
	return "Pando started the app and watched it, and saw it open no network port at all, so this " +
		"could not be worked out automatically."
}

func joinPorts(ports []int) string {
	parts := make([]string, 0, len(ports))
	for _, p := range ports {
		parts = append(parts, fmt.Sprint(p))
	}
	if len(parts) == 1 {
		return "port " + parts[0]
	}
	return "ports " + strings.Join(parts, ", ")
}

// undefer turns every deferred question back into one a person answers.
func undefer(questions []Question) []Question {
	out := make([]Question, 0, len(questions))
	for _, q := range questions {
		q.Deferred = false
		out = append(out, q)
	}
	return out
}

// promoteSlots is the O-4 fallback (design 01 §2.5).
//
// `Required` has no reliable derivation, so everything not typed to a known
// service starts optional and the trial run settles it: a slot whose absence
// crashes the app becomes required, with the crash log as the evidence.
//
// The restraint is the important part. Promoting every unfilled slot on any
// crash would turn one syntax error into forty required slots and forty
// PLAN_SLOT_UNFILLED blockers — the false-block rate the phase plan says to
// measure rather than assume away. So a slot is promoted only when the app
// itself named it in the log.
//
// That is not the inference R-107 forbids. R-107 is about a repository that
// needs Postgres and never mentions it anywhere; Pando must show the log and
// stop rather than invent a database. This is the opposite situation: the slot
// was already declared, and the running app said it was missing.
func promoteSlots(slots []spec.Slot, t Trial) []spec.Slot {
	if !t.Crashed || t.Log == "" {
		return slots
	}
	log := strings.ToLower(t.Log)

	for i, slot := range slots {
		if slot.Required || slot.Resolution != nil {
			continue
		}
		mention, found := mentionedIn(log, slot)
		if !found {
			continue
		}
		slots[i].Required = true
		slots[i].Evidence = append(slots[i].Evidence,
			fmt.Sprintf("the app failed to start and its log mentions %s", mention))
	}
	return slots
}

// mentionedIn reports whether a crash log names this slot.
func mentionedIn(lowercaseLog string, slot spec.Slot) (string, bool) {
	if key := strings.ToLower(slot.Key); key != "" && strings.Contains(lowercaseLog, key) {
		return slot.Key, true
	}

	// A Postgres driver's connection error usually names the database rather
	// than the environment variable that was supposed to configure it.
	for _, word := range serviceWords[slot.Type] {
		if strings.Contains(lowercaseLog, word) {
			return word, true
		}
	}
	return "", false
}

var serviceWords = map[spec.SlotType][]string{
	spec.SlotPostgres: {"postgres", "postgresql", "pg_hba", "libpq"},
	spec.SlotMySQL:    {"mysql", "mariadb"},
	spec.SlotRedis:    {"redis", "valkey"},
	spec.SlotS3:       {"s3", "bucket"},
	spec.SlotSMTP:     {"smtp"},
}

// persistenceWarnings produces WARN_NO_PERSISTENT_VOLUME (R-201, R-202).
//
// R-203 is why this exists at all, when inference is otherwise forbidden: an
// undeclared Postgres fails loudly on first boot, but undeclared persistence
// works perfectly until the second deploy and then silently discards everything
// while reporting healthy. It is the worst failure mode in the system, so it
// earns a warning even though Pando will not act on it.
func persistenceWarnings(draft Draft, t Trial) []spec.Warning {
	if len(draft.Volumes) > 0 && len(t.ObservedWrites) == 0 {
		return nil
	}

	// R-202: the trial run improves the warning by naming the directory. A
	// warning that says "somewhere" is one people dismiss; one that says
	// /app/uploads is one they act on.
	if len(t.ObservedWrites) > 0 {
		dirs := append([]string(nil), t.ObservedWrites...)
		sort.Strings(dirs)
		return []spec.Warning{{
			Code: spec.WarnNoPersistentVolume,
			Message: fmt.Sprintf(
				"While Pando was testing this app, it wrote to %s — %s outside any storage the app "+
					"declares it wants to keep. Anything written there is discarded the next time "+
					"the app is deployed, and the app will keep reporting itself healthy while that "+
					"happens. If that directory holds data you need, define a volume for it here. "+
					"If it does not, you can dismiss this.",
				strings.Join(dirs, ", "), plural(len(dirs), "a directory", "directories")),
		}}
	}

	if len(draft.Volumes) == 0 {
		return []spec.Warning{{
			Code: spec.WarnNoPersistentVolume,
			Message: "No persistent volume found. If your app doesn't store data, you can ignore " +
				"this. Otherwise define one here.",
		}}
	}
	return nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// PathRoutingWarning produces WARN_PATH_ROUTING_INCOMPATIBLE (R-168).
//
// A warning, never a fix. R-028 is the rule it serves: Pando does not rewrite
// an app's response bodies to make path routing work, because an app that only
// works because its HTML was rewritten in flight is an app that breaks in ways
// nobody can debug. So where this is likely to bite, it is said out loud and
// left to the user.
//
// The signal is an asset reference rooted at "/" in the app's own HTML. Served
// under a path prefix, "/static/app.js" resolves against the domain rather than
// the prefix, and the app loads a blank page with no error anywhere.
func PathRoutingWarning(src api.SourceView, draft Draft) []spec.Warning {
	var checked string
	for _, candidate := range []string{"index.html", "public/index.html", "dist/index.html", "site/index.html"} {
		if info, err := src.Stat(candidate); err == nil && !info.IsDir {
			checked = candidate
			break
		}
	}
	if checked == "" {
		return nil
	}

	body, ok := readLimited(src, checked, 512<<10)
	if !ok {
		return nil
	}

	var refs []string
	for _, attr := range []string{`src="/`, `href="/`, `src='/`, `href='/`} {
		if idx := strings.Index(body, attr); idx >= 0 {
			refs = append(refs, sample(body[idx+len(attr)-1:]))
		}
	}
	if len(refs) == 0 {
		return nil
	}

	return []spec.Warning{{
		Code: spec.WarnPathRoutingIncompatible,
		Message: fmt.Sprintf(
			"This app loads files using addresses that start at the top of a domain, such as %q in %s. "+
				"If you give the app an address ending in a path — example.com/notes rather than "+
				"notes.example.com — the browser will look for those files at the top of the domain "+
				"instead, and the page will come up blank with nothing in the logs to say why. "+
				"Pando does not rewrite pages to work around this. Giving the app its own subdomain "+
				"avoids it entirely; otherwise the app needs to be built with a base path set.",
			refs[0], checked),
	}}
}

func sample(s string) string {
	end := strings.IndexAny(s, `"'`)
	if end <= 0 || end > 60 {
		end = min(60, len(s))
	}
	return s[:end]
}

func readLimited(src api.SourceView, name string, limit int64) (string, bool) {
	f, err := src.Open(name)
	if err != nil {
		return "", false
	}
	defer func() { _ = f.Close() }()

	raw, err := io.ReadAll(io.LimitReader(f, limit))
	if err != nil {
		return "", false
	}
	return string(raw), true
}
