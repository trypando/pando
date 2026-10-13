package detect

import (
	"context"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// exitedRefusal is the trial's answer when there is no web app to run: the
// process finished on its own, successfully, without ever listening (R-107).
//
// A command-line tool prints its result and exits 0; so does `node index.js`
// in a library whose index only exports. Both were deployed as web apps, and
// the deploy then waited for a port that was never coming (issue #90). An app
// that crashes is a different case — it is missing something, and its log is
// the answer — so only a clean exit is refused.
//
// Nor is an app that stopped because a path was not mounted. Nginx Proxy
// Manager checks for /etc/letsencrypt, prints "/etc/letsencrypt is not
// mounted!" and exits 0, and it was refused as a command-line program — while
// the same trial gave it the volume it asked for (mountsAskedFor, issue #90).
// It stopped for want of something Pando now provides, which is the crash case.
func exitedRefusal(t Trial) *errs.Error {
	if !t.Ran || !t.Exited || len(t.ObservedPorts) > 0 || len(t.LoopbackPorts) > 0 {
		return nil
	}
	if len(mountsAskedFor(t.Log)) > 0 {
		return nil
	}
	return errs.New(errs.PlanCapabilityUnsupported,
		"Pando started this app to see how it runs, and it finished on its own a moment later, "+
			"successfully, without ever listening on a network port. A web app keeps running and waits "+
			"for requests, so there is nothing for a browser to open: this looks like a command-line "+
			"program, a script or a library rather than a web app. What it printed is shown with the "+
			"trial run.").
		WithRemedy("If this is a web app, add a Procfile or a Dockerfile whose command starts its server " +
			"and keeps it running in the foreground, then detect the app again.")
}

// nonHTTPRefusal refuses an image with nothing for a browser to open: every
// port it listens on, or failing a trial every port it declares, carries a
// protocol other than HTTP — a database's, a broker's, a mail server's.
//
// Redis was deployed and answered every request with a protocol error (issue
// #55). MySQL, Postgres and Mosquitto went the same way (issue #90): the two
// databases exit at once without a password, so the trial saw no port at all,
// and what the image declares with EXPOSE is the evidence left.
func nonHTTPRefusal(t Trial, draft Draft) *errs.Error {
	ports, verb := t.ObservedPorts, "listens"
	if len(ports) == 0 {
		if primary, ok := primaryWorkload(draft); ok {
			for _, p := range primary.Ports {
				if p.Source == spec.PortExpose {
					ports = append(ports, p.Number)
				}
			}
		}
		verb = "declares"
	}
	if !onlyNonHTTP(withoutEphemeral(ports, t)) {
		return nil
	}
	listed := make([]string, 0, len(ports))
	for _, p := range ports {
		listed = append(listed, strconv.Itoa(p))
	}
	return errs.Newf(errs.PlanCapabilityUnsupported,
		"This image %s only %s %s, which %s a protocol other than HTTP — a database's, a message "+
			"broker's or a mail server's — so there is no web page for Pando to serve.",
		verb, plural(len(listed), "port", "ports"), strings.Join(listed, ", "),
		plural(len(listed), "carries", "carry")).
		WithRemedy("To give an app a database, fill one of its slots instead: Pando runs the " +
			"database inside that app, where nothing else can reach it.")
}

// withoutEphemeral drops ports in the ephemeral range when the trial probed
// them and nothing answered HTTP: an Erlang node's distribution port beside
// epmd is still not a web server.
func withoutEphemeral(ports []int, t Trial) []int {
	if !t.HTTPProbed || len(t.HTTPAnswers) > 0 {
		return ports
	}
	return filterPorts(ports, func(p int) bool { return p < ephemeralPorts })
}

// listenEverywhere starts the app again with HOST=0.0.0.0 when the trial saw
// it listening only on 127.0.0.1, and keeps the setting if that is what moved
// it.
//
// HOST is the variable most servers that default to loopback read for their
// address — Nuxt, Astro, SvelteKit's Node adapter, and many Go and Node apps
// written to the same convention. An app bound to loopback is unreachable
// through Pando's proxy, and was deployed anyway (issue #90). It is set only
// when it was watched to work: an app that reads HOST as its public hostname
// is not listening on loopback because of it, and is left alone.
func (j *Job) listenEverywhere(ctx context.Context, draft, trialDraft Draft, auth *api.RegistryAuth, t Trial) (Draft, Trial, bool) {
	if !t.Ran || len(t.ObservedPorts) > 0 || len(t.LoopbackPorts) == 0 {
		return draft, t, false
	}
	if primary, ok := primaryWorkload(trialDraft); !ok || hasEnv(primary, "HOST") {
		return draft, t, false
	}
	retry := j.trialWith(ctx, withEnv(trialDraft, "HOST", "0.0.0.0"), auth)
	if len(retry.ObservedPorts) == 0 {
		return draft, t, false
	}
	return withEnv(draft, "HOST", "0.0.0.0"), retry, true
}

// listenedEverywhere is the evidence listenEverywhere's change carries.
const listenedEverywhere = "the app listened only on 127.0.0.1 until HOST=0.0.0.0 was set, so it is started with that setting"

func hasEnv(w spec.Workload, key string) bool {
	for _, e := range w.Env {
		if e.Key == key {
			return true
		}
	}
	return false
}

// withEnv returns the draft with key=value added to its primary workload,
// copying what it changes: a draft is shared with the proposal's candidates.
func withEnv(draft Draft, key, value string) Draft {
	draft.Workloads = append([]spec.Workload(nil), draft.Workloads...)
	for i, w := range draft.Workloads {
		if !w.Primary && len(draft.Workloads) > 1 {
			continue
		}
		v := value
		draft.Workloads[i].Env = append(append([]spec.EnvEntry(nil), w.Env...),
			spec.EnvEntry{Key: key, Value: &v, Source: spec.EnvFromDetection})
		break
	}
	return draft
}
