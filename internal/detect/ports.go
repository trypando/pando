package detect

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// observedPorts orders what a trial watched so that the first port is the one
// people open in a browser, and reports the ports it could not choose between.
//
// The proxy sends traffic to the first port that carries HTTP, so this order is
// the routing decision, and it used to be the order the sockets happened to be
// listed in. Ten published images went to the wrong one (issue #90): epmd
// before RabbitMQ's management UI and Livebook, MQTT before EMQX's dashboard,
// OTLP's gRPC port before Jaeger's UI, Tomcat's shutdown port before its
// connector, BitTorrent before qBittorrent's web UI.
//
// The evidence, strongest first:
//
//   - which ports answered an HTTP request during the trial, where the runtime
//     could send one (SupportsHTTPProbe). Listening is not serving.
//   - among those, the ones that answered with a page — a success, a redirect
//     or a sign-in — over the ones that answered "not found" or an error, which
//     is what a metrics or API port says to GET /.
//   - a port outside the ephemeral range over one inside it, which a runtime
//     opens for its own bookkeeping more often than an app chooses.
//   - a port the image or the compose file declares (declared) over one it
//     does not: lsio images run nginx on their declared 80 in front of the
//     app's own server on an undeclared one.
//   - which of the remaining ports the app's own log names as the HTTP one
//     ("http-nio-8080", "running at http://localhost:8080"), and failing that,
//     which it says it is listening on.
//
// Each narrows only when something is left. When the probe ran and several
// ports are still equally likely, that is a real question (R-102: ask, never
// guess), and tied lists them. Without a probe the order is the best the
// heuristics give, as it was before: a runtime that cannot probe would
// otherwise turn every multi-port app into a question.
func observedPorts(t Trial, declared map[int]bool) (ports []spec.Port, tied []int) {
	if len(t.ObservedPorts) == 0 {
		return nil, nil
	}

	answered := map[int]int{}
	for _, a := range t.HTTPAnswers {
		answered[a.Port] = a.Status
	}

	probed := t.HTTPProbed && len(answered) > 0
	pool := candidatePorts(t, answered, probed, declared)
	if probed && len(pool) > 1 {
		tied = pool
	}

	chosen := t.ObservedPorts[0]
	if len(pool) > 0 {
		chosen = pool[0]
	}

	// The chosen port first. The others are kept, so the review shows
	// everything the app opened; a port that was probed and did not answer
	// HTTP, or that carries a protocol known not to be HTTP, is marked as
	// plain TCP so that nothing routes a browser to it.
	ports = append(ports, spec.Port{Number: chosen, Protocol: "http", Source: spec.PortObserved})
	for _, p := range t.ObservedPorts {
		if p != chosen {
			ports = append(ports, spec.Port{Number: p, Protocol: observedProtocol(p, answered, probed), Source: spec.PortObserved})
		}
	}
	if probed {
		// The chosen port answered HTTP, whatever its number usually carries.
		return ports, tied
	}
	return webPortsFirst(ports), tied
}

// candidatePorts narrows what the trial watched by the evidence observedPorts
// lists, strongest first; the first left is the one to choose.
func candidatePorts(t Trial, answered map[int]int, probed bool, declared map[int]bool) []int {
	pool := t.ObservedPorts
	if probed {
		pool = filterPorts(pool, func(p int) bool { _, ok := answered[p]; return ok })
		pool = prefer(pool, func(p int) bool { return servesAPage(answered[p]) })
	} else {
		pool = prefer(pool, func(p int) bool { return !notHTTP[p] })
	}
	pool = prefer(pool, func(p int) bool { return p < ephemeralPorts })
	pool = prefer(pool, func(p int) bool { return declared[p] })
	return namedInLog(t.Log, pool)
}

// observedProtocol is "tcp" for a port that was probed and did not answer
// HTTP, or that carries a protocol known not to be HTTP, and "http" otherwise.
func observedProtocol(p int, answered map[int]int, probed bool) string {
	_, answeredHTTP := answered[p]
	if (!probed && notHTTP[p]) || (probed && !answeredHTTP) {
		return "tcp"
	}
	return "http"
}

// ephemeralPorts is where Linux's ephemeral range starts. A server rarely
// listens there on purpose; a runtime does, for its own bookkeeping — Open
// Liberty opened 47373 seconds before 9080, and an Erlang node opens its
// distribution port there (issue #90).
const ephemeralPorts = 32768

// servesAPage reports an HTTP status that means a person would see something:
// a page, a redirect to one, or a sign-in.
func servesAPage(status int) bool {
	return (status >= 200 && status < 400) || status == 401 || status == 403
}

// prefer narrows ports to the ones that pass, unless none do.
func prefer(ports []int, keep func(int) bool) []int {
	if narrowed := filterPorts(ports, keep); len(narrowed) > 0 {
		return narrowed
	}
	return ports
}

func filterPorts(ports []int, keep func(int) bool) []int {
	var out []int
	for _, p := range ports {
		if keep(p) {
			out = append(out, p)
		}
	}
	return out
}

var portNumber = regexp.MustCompile(`\b\d{2,5}\b`)

// namedInLog narrows pool to the ports the app's log names as its web server,
// or failing that as somewhere it listens. It never empties the pool: a log
// that names none of them leaves the choice where it was.
//
// "https" alone does not count. Fortio logs "Https redirector running on
// [::]:8081" beside "UI started - visit: http://localhost:8080/fortio/".
func namedInLog(log string, pool []int) []int {
	if len(pool) < 2 || log == "" {
		return pool
	}
	inPool := map[int]bool{}
	for _, p := range pool {
		inPool[p] = true
	}

	web, listening := portMentions(log, inPool)
	for _, named := range []map[int]bool{web, listening} {
		if narrowed := filterPorts(pool, func(p int) bool { return named[p] }); len(narrowed) > 0 {
			return narrowed
		}
	}
	return pool
}

// portMentions reads which ports in the pool the log names on a line about
// HTTP (web), and which on a line about listening.
func portMentions(log string, inPool map[int]bool) (web, listening map[int]bool) {
	web, listening = map[int]bool{}, map[int]bool{}
	for _, line := range strings.Split(strings.ToLower(log), "\n") {
		mentionsHTTP := mentionsPlainHTTP(line)
		mentionsListen := strings.Contains(line, "listen")
		if !mentionsHTTP && !mentionsListen {
			continue
		}
		for _, n := range portsIn(line, inPool) {
			if mentionsHTTP {
				web[n] = true
			}
			if mentionsListen {
				listening[n] = true
			}
		}
	}
	return web, listening
}

// portsIn is the numbers in a line that are ports in the pool.
func portsIn(line string, inPool map[int]bool) []int {
	var out []int
	for _, m := range portNumber.FindAllString(line, -1) {
		if n, err := strconv.Atoi(m); err == nil && inPool[n] {
			out = append(out, n)
		}
	}
	return out
}

// mentionsPlainHTTP reports "http" in a lowercase line other than as the start
// of "https".
func mentionsPlainHTTP(line string) bool {
	for i := 0; ; {
		idx := strings.Index(line[i:], "http")
		if idx < 0 {
			return false
		}
		end := i + idx + len("http")
		if end >= len(line) || line[end] != 's' {
			return true
		}
		i = end
	}
}

// choosePortQuestion asks which port is the app's when the trial left several
// that answered equally well (R-102). It replaces any port question already
// there: the trial has narrowed it to a choice among ports it watched.
func choosePortQuestion(questions []Question, tied []int) []Question {
	if len(tied) < 2 {
		return questions
	}
	listed := make([]string, 0, len(tied))
	for _, p := range tied {
		listed = append(listed, strconv.Itoa(p))
	}
	q := Question{
		Key:  KeyPrimaryPort,
		Kind: api.QuestionPort,
		Prompt: fmt.Sprintf(
			"Pando started this app and watched it. It answered web requests on %d ports — %s — and "+
				"nothing about the app says which of them is the one people open in a browser; the "+
				"others are usually an admin, metrics or API port. Valid answer: one of those port "+
				"numbers, such as %s.",
			len(tied), strings.Join(listed, ", "), listed[0]),
		Why:     "Pando sends the app's visitors to one port, and the wrong one shows them the wrong page or an error.",
		Options: listed,
	}
	return append(dropQuestion(questions, KeyPrimaryPort), q)
}

// loopbackQuestion explains an app that listened only on its own internal
// address, and asks for a command that makes it listen where Pando can reach
// it.
//
// A source build has no port question for the trial to resolve — the language's
// usual port stands in until the deploy watches the built app — so without this
// an app bound to 127.0.0.1 was proposed as ready, deployed, and either refused
// at deploy with nothing to answer or answered every request with a timeout
// (issue #90). The command is the one thing a person can change without
// changing the app: helloserver takes -addr, most dev servers take --host.
func loopbackQuestion(questions []Question, t Trial) []Question {
	if !t.Ran || len(t.ObservedPorts) > 0 || len(t.LoopbackPorts) == 0 {
		return questions
	}
	for _, q := range questions {
		if q.Kind == api.QuestionPort || q.Key == KeyStartCommand {
			// Already asked, and resolvePortQuestions has said what the trial
			// saw on the port question.
			return questions
		}
	}
	return append(questions, Question{
		Key:  KeyStartCommand,
		Kind: api.QuestionText,
		Prompt: whatTheTrialSaw(t) + " Pando can start the app with a command that tells it to listen on " +
			"0.0.0.0 instead. Valid answer: the shell command that starts the app listening on all " +
			"addresses, such as \"app -addr 0.0.0.0:8080\" or \"npm start -- --host 0.0.0.0\". If the " +
			"address is written into the app's code, the code has to change instead, and no command will help.",
		Why: "Pando's proxy cannot reach an app that listens only on its own internal address, so every request to it would time out.",
	})
}
