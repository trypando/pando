package detect

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/core/spec"
)

// A database configured as separate variables — PGHOST, PGUSER and PGPASSWORD;
// JOOMLA_DB_HOST and JOOMLA_DB_PASSWORD; Laravel's DB_HOST, DB_DATABASE,
// DB_USERNAME and DB_PASSWORD — is one database, and it is one slot: one
// answer, provisioned or bound, whose connection string each variable takes
// its part of (spec.EnvEntry.SlotPart). Asked for as four untyped values, it
// could only be filled by somebody who already had a database, and Pando never
// offered to provision one (issue #90).

// hostSuffixes name the variable that says where a database is. The prefix
// before one is what its siblings share.
var hostSuffixes = []string{"HOSTNAME", "HOST"}

// partSuffixes are the siblings of a host variable, and the part each takes.
var partSuffixes = []struct {
	suffix string
	part   spec.SlotPart
}{
	{"PORT", spec.SlotPartPort},
	{"USERNAME", spec.SlotPartUser},
	{"USER", spec.SlotPartUser},
	{"PASSWORD", spec.SlotPartPassword},
	{"PASS", spec.SlotPartPassword},
	{"DATABASE", spec.SlotPartDatabase},
	{"NAME", spec.SlotPartDatabase},
}

// groupConnections folds each database a workload is configured for in
// separate variables into one typed slot with parts.
//
// Only where something says it is required — a framework's convention, a read
// with no fallback, a trial run that stopped for want of it — and only where
// the kind of database is known: from the names (PGHOST, MYSQL_HOST) or from
// what the trial run's log says it tried to reach (Joomla's
// MYSQL_PORT_3306_TCP). DB_HOST alone says a database and not which, and a
// slot of the wrong type provisions the wrong one.
func groupConnections(d Draft, workload int, log string) Draft {
	if workload < 0 || workload >= len(d.Workloads) {
		return d
	}
	for _, host := range append([]spec.EnvEntry(nil), d.Workloads[workload].Env...) {
		prefix, ok := hostPrefix(host.Key)
		if !ok || !open(d, host) {
			continue
		}
		members := connectionParts(d, d.Workloads[workload].Env, host.Key, prefix)
		if !anyRequired(d, members) {
			continue
		}
		if t := connectionType(prefix, log); t != spec.SlotUnknown {
			d = foldInto(d, workload, host.Key, t, members)
		}
	}
	return d
}

// connectionParts are the open variables beside a host variable that name a
// part of the same connection, one variable to a part.
func connectionParts(d Draft, env []spec.EnvEntry, hostKey, prefix string) map[string]spec.SlotPart {
	members := map[string]spec.SlotPart{hostKey: spec.SlotPartHost}
	for _, s := range partSuffixes {
		key := prefix + s.suffix
		i := envIndex(env, key)
		if i < 0 || !open(d, env[i]) {
			continue
		}
		if _, taken := partTaken(members, s.part); !taken {
			members[key] = s.part
		}
	}
	return members
}

// connectionType is the kind of database a prefix names. The log decides only
// for a prefix that says database: SMTP_HOST beside a MySQL error is still a
// mail server.
func connectionType(prefix, log string) spec.SlotType {
	t := prefixType(prefix)
	if t == spec.SlotUnknown && namesADatabase(prefix) {
		t = logType(log)
	}
	return t
}

// namesADatabase reports a prefix with DB or DATABASE as a word of it.
func namesADatabase(prefix string) bool {
	for _, word := range strings.FieldsFunc(strings.ToUpper(prefix), func(r rune) bool { return r == '_' }) {
		if word == "DB" || word == "DATABASE" {
			return true
		}
	}
	return false
}

// hostPrefix is what a host variable's siblings share: "PG" for PGHOST,
// "JOOMLA_DB_" for JOOMLA_DB_HOST.
func hostPrefix(key string) (string, bool) {
	upper := strings.ToUpper(key)
	for _, suffix := range hostSuffixes {
		if prefix, ok := strings.CutSuffix(upper, suffix); ok && prefix != "" {
			if prefix != key[:len(prefix)] {
				return "", false
			}
			return prefix, true
		}
	}
	return "", false
}

// open reports an entry nothing has given a value: a slot nobody filled, or a
// name read from a template and left empty.
func open(d Draft, e spec.EnvEntry) bool {
	switch {
	case e.SlotRef != nil:
		i, ok := slotIndex(d.Slots, *e.SlotRef)
		return ok && d.Slots[i].Resolution == nil && d.Slots[i].Type == spec.SlotUnknown && e.SlotPart == ""
	case e.SecretRef != nil:
		return false
	case e.Value != nil:
		return *e.Value == "" && (e.Source == spec.EnvFromDetection || e.Source == "")
	}
	return false
}

func partTaken(members map[string]spec.SlotPart, part spec.SlotPart) (string, bool) {
	for key, p := range members {
		if p == part {
			return key, true
		}
	}
	return "", false
}

func anyRequired(d Draft, members map[string]spec.SlotPart) bool {
	for key := range members {
		if i, ok := slotIndex(d.Slots, key); ok && d.Slots[i].Required {
			return true
		}
	}
	return false
}

// prefixType is the kind of database a prefix names, by its words: PG,
// MYSQL_, MONGO_. DB and DATABASE say nothing about which.
func prefixType(prefix string) spec.SlotType {
	words := strings.FieldsFunc(strings.ToUpper(prefix), func(r rune) bool { return r == '_' })
	for _, word := range words {
		switch word {
		case "PG", "POSTGRES", "POSTGRESQL", "PSQL":
			return spec.SlotPostgres
		case "MYSQL", "MARIADB":
			return spec.SlotMySQL
		case "MONGO", "MONGODB":
			return spec.SlotMongoDB
		case "REDIS", "VALKEY":
			return spec.SlotRedis
		}
	}
	return spec.SlotUnknown
}

var (
	mysqlInLog    = regexp.MustCompile(`(?i)\bmysql|\bmariadb|:3306\b|_3306_`)
	postgresInLog = regexp.MustCompile(`(?i)postgres|\bpgsql\b|\blibpq\b|:5432\b|\.s\.pgsql\.5432`)
	mongoInLog    = regexp.MustCompile(`(?i)\bmongo|:27017\b`)
)

// logType is the one kind of database a trial run's log names, or unknown
// when it names none or more than one.
func logType(log string) spec.SlotType {
	var found []spec.SlotType
	for _, c := range []struct {
		re *regexp.Regexp
		t  spec.SlotType
	}{{mysqlInLog, spec.SlotMySQL}, {postgresInLog, spec.SlotPostgres}, {mongoInLog, spec.SlotMongoDB}} {
		if c.re.MatchString(log) {
			found = append(found, c.t)
		}
	}
	if len(found) != 1 {
		return spec.SlotUnknown
	}
	return found[0]
}

// foldInto makes members the parts of one slot of type t, named for the host
// variable: the slots each was asked for as are dropped, and their evidence
// kept on the one that replaces them.
func foldInto(d Draft, workload int, key string, t spec.SlotType, members map[string]spec.SlotPart) Draft {
	names := make([]string, 0, len(members))
	for k := range members {
		names = append(names, k)
	}
	sort.Strings(names)

	evidence := []string{fmt.Sprintf("%s say where one %s is and how to sign in to it, so they are filled from one",
		joinNames(names), t.DisplayName())}
	var kept []spec.Slot
	for _, s := range d.Slots {
		if _, member := members[s.Key]; member && s.Resolution == nil && s.Type == spec.SlotUnknown {
			evidence = append(evidence, s.Evidence...)
			continue
		}
		kept = append(kept, s)
	}
	d.Slots = append(kept, spec.Slot{Key: key, Type: t, Required: true, Evidence: dedupe(evidence)})

	d.Workloads = append([]spec.Workload(nil), d.Workloads...)
	w := &d.Workloads[workload]
	w.Env = append([]spec.EnvEntry(nil), w.Env...)
	for i, e := range w.Env {
		part, member := members[e.Key]
		if !member {
			continue
		}
		ref := key
		w.Env[i] = spec.EnvEntry{Key: e.Key, SlotRef: &ref, SlotPart: part, Source: spec.EnvFromDetection}
	}
	return d
}

func joinNames(names []string) string {
	if len(names) == 1 {
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func dedupe(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range items {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// --- Laravel ------------------------------------------------------------------

// laravelRefused is Laravel failing to reach its database: a QueryException
// with the connection it used, "(Connection: mysql, …".
var laravelRefused = regexp.MustCompile(`SQLSTATE\[HY000\] \[(?:2002|2003|2006|1045|7)\][^\n]*\(Connection: (mysql|mariadb|pgsql)`)

// setLaravelEntry sets one of Laravel's database variables on w, leaving one
// somebody already gave a value; DB_CONNECTION is Pando's to say.
func setLaravelEntry(d Draft, w *spec.Workload, entry spec.EnvEntry) {
	i := envIndex(w.Env, entry.Key)
	if i < 0 {
		w.Env = append(w.Env, entry)
		return
	}
	if open(d, w.Env[i]) || entry.Key == "DB_CONNECTION" {
		w.Env[i] = entry
	}
}

// withoutUntypedParts is slots less the unresolved, untyped ones named for a
// part of the connection, which the one typed slot now fills.
func withoutUntypedParts(slots []spec.Slot, parts map[string]spec.SlotPart) []spec.Slot {
	var kept []spec.Slot
	for _, s := range slots {
		if _, part := parts[s.Key]; part && s.Resolution == nil && s.Type == spec.SlotUnknown {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// laravelDatabase gives a Laravel app the database its log says it could not
// reach, configured the way Laravel reads one: DB_CONNECTION, then DB_HOST,
// DB_PORT, DB_DATABASE, DB_USERNAME and DB_PASSWORD from one slot.
//
// Laravel's own convention rather than any one app's. BookStack's image waits
// on "SQLSTATE[HY000] [2002]" for a MySQL nothing configured, and an app whose
// .env.example names SQLite still ran a migration against MySQL.
func laravelDatabase(d Draft, workload int, log string) Draft {
	if workload < 0 || workload >= len(d.Workloads) {
		return d
	}
	m := laravelRefused.FindStringSubmatch(log)
	if m == nil {
		return d
	}
	if i := envIndex(d.Workloads[workload].Env, "DB_HOST"); i >= 0 && !open(d, d.Workloads[workload].Env[i]) {
		return d // somebody said where the database is
	}
	d.Workloads = append([]spec.Workload(nil), d.Workloads...)
	w := &d.Workloads[workload]

	t := spec.SlotMySQL
	if m[1] == "pgsql" {
		t = spec.SlotPostgres
	}
	connection := m[1]
	w.Env = append([]spec.EnvEntry(nil), w.Env...)
	setLaravelEntry(d, w, spec.EnvEntry{Key: "DB_CONNECTION", Value: &connection, Source: spec.EnvFromDetection})
	ref := "DB_HOST"
	parts := map[string]spec.SlotPart{
		"DB_HOST": spec.SlotPartHost, "DB_PORT": spec.SlotPartPort, "DB_DATABASE": spec.SlotPartDatabase,
		"DB_USERNAME": spec.SlotPartUser, "DB_PASSWORD": spec.SlotPartPassword,
	}
	for _, key := range []string{"DB_HOST", "DB_PORT", "DB_DATABASE", "DB_USERNAME", "DB_PASSWORD"} {
		setLaravelEntry(d, w, spec.EnvEntry{Key: key, SlotRef: &ref, SlotPart: parts[key], Source: spec.EnvFromDetection})
	}

	line := strings.TrimSpace(lineAt(log, strings.Index(log, m[0])))
	if len(line) > 160 {
		line = line[:157] + "…"
	}
	d.Slots = append(withoutUntypedParts(d.Slots, parts), spec.Slot{
		Key: ref, Type: t, Required: true,
		Evidence: []string{fmt.Sprintf("a Laravel app that could not reach its %s database when Pando tried it: %q",
			t.DisplayName(), line)},
	})
	return d
}
