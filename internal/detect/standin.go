package detect

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// An app that stops at its first missing variable names one at a time:
// Flatnotes says FLATNOTES_USERNAME, and only once it has one says
// FLATNOTES_PASSWORD; Joomla wants JOOMLA_DB_HOST, then JOOMLA_DB_PASSWORD;
// Zipline checks its database before it says CORE_SECRET. A single trial run
// found the first, the person filled it, and the deploy crashed on the second
// (issue #90).
//
// So when a failed trial names variables, detection runs the app again with a
// stand-in for each — a value of the right shape that reaches nothing — and
// reads what it names next. The stand-ins exist only in the trial: the draft
// gets the requirement, never the value (R-132 still asks a person).

// maxStandInRounds bounds the extra trial runs. Each named variable costs at
// most one round, and an app that stops for want of something every time is
// asking for more than stand-ins can say.
const maxStandInRounds = 3

// standInHost resolves nowhere, by RFC 2606: a stand-in can never reach a real
// service.
const standInHost = "pando-trial.invalid"

// rerunWithStandIns runs the trial again while the last one failed and named
// a variable not yet given. It returns the draft with every requirement any
// round found, the trial draft with the stand-ins the last round ran with, and
// the last trial, whose log carries every round's.
func (j *Job) rerunWithStandIns(ctx context.Context, draft, trialDraft Draft, auth *api.RegistryAuth, t Trial) (Draft, Draft, Trial) {
	log := t.Log
	for round := 0; round < maxStandInRounds && trialFailed(t); round++ {
		primary, ok := primaryWorkload(trialDraft)
		if !ok {
			break
		}
		fresh := notGivenInTrial(primary, varsFromLog(t.Log))
		if len(fresh) == 0 {
			break
		}
		draft = requireVars(draft, primaryIndex(draft), fresh)
		var names []string
		trialDraft, names = withStandIns(trialDraft, fresh)
		next := j.trialWith(ctx, trialDraft, auth)
		if !next.Ran {
			break
		}
		log += fmt.Sprintf("\n--- Pando ran the app again with stand-in values for %s ---\n%s",
			strings.Join(names, ", "), next.Log)
		t = next
	}
	t.Log = log
	return draft, trialDraft, t
}

// notGivenInTrial are the variables of vars the trial run has not set.
func notGivenInTrial(w spec.Workload, vars []requiredVar) []requiredVar {
	var fresh []requiredVar
	for _, v := range vars {
		if !givenInTrial(w, v.Key) {
			fresh = append(fresh, v)
		}
	}
	return fresh
}

// withStandIns is the trial draft with a stand-in, or the value the log gave,
// for each of vars, and their names.
func withStandIns(trialDraft Draft, vars []requiredVar) (Draft, []string) {
	names := make([]string, 0, len(vars))
	for _, v := range vars {
		value := standIn(v)
		if v.Value != nil {
			value = *v.Value
		}
		trialDraft = withEnv(trialDraft, v.Key, value)
		names = append(names, v.Key)
	}
	return trialDraft, names
}

// givenInTrial reports a variable the trial run already set to something.
func givenInTrial(w spec.Workload, key string) bool {
	for _, e := range w.Env {
		if e.Key == key && e.Value != nil && *e.Value != "" {
			return true
		}
	}
	return false
}

// standIn is a value of the shape v takes that connects to nothing: a URL of
// the right scheme at a host that does not resolve, a key of the format the
// app checks, a word for anything else.
func standIn(v requiredVar) string {
	switch t := slotTypeFor(v.Key, ""); t {
	case spec.SlotPostgres:
		return "postgresql://pando:pando@" + standInHost + ":5432/pando"
	case spec.SlotMySQL:
		return "mysql://pando:pando@" + standInHost + ":3306/pando"
	case spec.SlotRedis:
		return "redis://" + standInHost + ":6379/0"
	case spec.SlotMongoDB:
		return "mongodb://pando:pando@" + standInHost + ":27017/pando"
	case spec.SlotMemcached:
		return standInHost + ":11211"
	case spec.SlotUnknown:
	default:
		return "https://" + standInHost
	}
	switch v.Format {
	case spec.FormatLaravelKey:
		return "base64:" + base64.StdEncoding.EncodeToString(randomBytes(32))
	case spec.FormatHex64:
		return hex.EncodeToString(randomBytes(32))
	case spec.FormatRandomLong, spec.FormatDjangoKey:
		return hex.EncodeToString(randomBytes(48))
	case spec.FormatRandom:
		return hex.EncodeToString(randomBytes(24))
	}
	upper := strings.ToUpper(v.Key)
	switch {
	case hasAnySuffix(upper, "_HOST", "HOST", "_SERVER", "_HOSTNAME"):
		return standInHost
	case hasAnySuffix(upper, "_URL", "_URI", "_ENDPOINT"):
		return "https://" + standInHost
	case hasAnySuffix(upper, "_PORT"):
		return "1"
	case looksSecret(upper) || hasAnySuffix(upper, "_USER", "_USERNAME"):
		return hex.EncodeToString(randomBytes(24))
	}
	return "pando-trial"
}

func hasAnySuffix(s string, suffixes ...string) bool {
	for _, suffix := range suffixes {
		if strings.HasSuffix(s, suffix) {
			return true
		}
	}
	return false
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read does not fail on the platforms Pando supports, and a
	// stand-in that came out as zeros is still a stand-in.
	_, _ = rand.Read(b)
	return b
}
