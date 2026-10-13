package detect

import (
	"bufio"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/spec"
)

// Required variables: the values an app cannot start without, found before it
// is deployed rather than after it has crash-looped (R-132).
//
// O-4 is the open question this works around the edge of. Whether a variable
// is required has no general answer, and the [P] fallback stands — a variable
// is optional unless something says otherwise. What this file adds is a list of
// the things that do say otherwise, each one a statement by the app or its
// framework rather than a guess by Pando:
//
//   - a framework's own convention: Rails in production signs with
//     SECRET_KEY_BASE, Laravel encrypts with APP_KEY, a Phoenix release raises
//     without SECRET_KEY_BASE, Symfony refuses an empty APP_SECRET, Strapi
//     refuses to start without APP_KEYS;
//   - a schema the app validates its environment with at boot: AdonisJS's
//     start/env.ts, envalid, t3-env and zod — a key there with no default and
//     no `optional` stops the app;
//   - code that reads a variable with no fallback at import: Python's
//     `os.environ["X"]`, Elixir's `System.fetch_env!`;
//   - the trial run's log, when the app stops and names what it is missing.
//
// Each lands on the plan as a required slot, typed where its name or the
// framework says what it holds (R-130), so the console's Generate produces a
// value the app accepts (spec.SecretFormat).

// requiredVar is one variable a source says the app cannot start without.
type requiredVar struct {
	Key      string
	Format   spec.SecretFormat
	Evidence string

	// Value is set when the source itself says what the value should be, so
	// nothing has to be asked: HOST is 0.0.0.0 in a container whatever the
	// sample says.
	Value *string
}

// requireVars puts required variables on a draft's workload.
//
// A variable already given a value — by the repository, by a compose file, by
// a person — is left alone: the requirement is met. One declared empty, the
// way a .env.example names it, becomes the slot that asks for it. One that is
// already a slot is marked required, keeping how it was found.
func requireVars(d Draft, workload int, vars []requiredVar) Draft {
	if workload < 0 || workload >= len(d.Workloads) {
		return d
	}
	w := &d.Workloads[workload]
	for _, v := range vars {
		at := -1
		for i, e := range w.Env {
			if e.Key == v.Key {
				at = i
				break
			}
		}
		if at >= 0 {
			e := w.Env[at]
			switch {
			case e.SlotRef != nil:
				d.Slots = markRequired(d.Slots, *e.SlotRef, v)
				continue
			case e.SecretRef != nil:
				continue
			case e.Value != nil && (*e.Value != "" || (e.Source != spec.EnvFromDetection && e.Source != "")):
				continue
			}
		}

		entry := spec.EnvEntry{Key: v.Key, Source: spec.EnvFromDetection}
		if v.Value != nil {
			value := *v.Value
			entry.Value = &value
		} else {
			ref := v.Key
			entry.SlotRef = &ref
			if _, declared := slotIndex(d.Slots, v.Key); declared {
				d.Slots = markRequired(d.Slots, v.Key, v)
			} else {
				d.Slots = append(d.Slots, spec.Slot{
					Key:      v.Key,
					Type:     slotTypeFor(v.Key, ""),
					Required: true,
					Format:   v.Format,
					Evidence: []string{v.Evidence},
				})
			}
		}
		if at >= 0 {
			w.Env[at] = entry
		} else {
			w.Env = append(w.Env, entry)
		}
	}
	return d
}

func slotIndex(slots []spec.Slot, key string) (int, bool) {
	for i, s := range slots {
		if s.Key == key {
			return i, true
		}
	}
	return -1, false
}

// markRequired promotes a declared slot, unless somebody already filled it.
func markRequired(slots []spec.Slot, key string, v requiredVar) []spec.Slot {
	i, ok := slotIndex(slots, key)
	if !ok || slots[i].Resolution != nil {
		return slots
	}
	if !slots[i].Required || !containsString(slots[i].Evidence, v.Evidence) {
		slots[i].Evidence = append(slots[i].Evidence, v.Evidence)
	}
	slots[i].Required = true
	if slots[i].Format == "" {
		slots[i].Format = v.Format
	}
	return slots
}

// withSourceRequirements adds what the repository says the primary workload
// needs, and returns the evidence lines to show for it.
//
// dir is where the workload's source sits in the repository: the root for a
// Dockerfile or a buildpack, a compose service's build context otherwise.
// dockerfile, when set, is read for the ENV it bakes into the image — a
// variable the image sets is one the app already has.
func withSourceRequirements(src api.SourceView, d Draft, dir, dockerfile string) (Draft, []string) {
	return requirementsFor(src, d, primaryIndex(d), dir, dockerfile)
}

// primaryIndex is the primary workload's index, or -1.
func primaryIndex(d Draft) int {
	for i, w := range d.Workloads {
		if w.Primary {
			return i
		}
	}
	if len(d.Workloads) == 1 {
		return 0
	}
	return -1
}

// requirementsFor is withSourceRequirements for one workload, by index.
func requirementsFor(src api.SourceView, d Draft, workload int, dir, dockerfile string) (Draft, []string) {
	if workload < 0 || workload >= len(d.Workloads) {
		return d, nil
	}

	view := subView(src, dir)
	baked := map[string]bool{}
	if dockerfile != "" {
		baked = dockerfileEnv(src, dockerfile)
	}

	var vars []requiredVar
	var evidence []string
	add := func(found []requiredVar, why string) {
		var kept []requiredVar
		for _, v := range found {
			if !baked[v.Key] {
				kept = append(kept, v)
			}
		}
		if len(kept) == 0 {
			return
		}
		vars = append(vars, kept...)
		evidence = append(evidence, why)
	}

	samples := envSamples(view)
	if isRailsApp(view) && !exists(view, "config/master.key") {
		add([]requiredVar{{Key: "SECRET_KEY_BASE", Format: spec.FormatRandomLong,
			Evidence: "a Rails app running in production signs its sessions with SECRET_KEY_BASE"}},
			"a Rails app, run in production: SECRET_KEY_BASE is needed before it can start")
	}
	if isLaravelApp(view) {
		add([]requiredVar{{Key: "APP_KEY", Format: spec.FormatLaravelKey, Evidence: laravelKeyEvidence}},
			"a Laravel app: APP_KEY is needed before it can serve a page")
	}
	if found := elixirRequired(view); len(found) > 0 {
		add(found, "config/runtime.exs stops the app without "+keysOf(found))
	}
	if found := symfonySecret(view); len(found) > 0 {
		add(found, "a Symfony app: APP_SECRET is needed before it can serve a page")
	}
	if found := strapiSecrets(view); len(found) > 0 {
		add(found, "a Strapi app: it refuses to start without "+keysOf(found))
	}
	if found, file := nodeSchemaRequired(view, samples); len(found) > 0 {
		add(found, file+" validates the environment at start and requires "+keysOf(found))
	}
	if found, file := pythonRequired(view); len(found) > 0 {
		add(found, file+" reads "+keysOf(found)+" with no fallback when it starts")
	}

	return requireVars(d, workload, vars), evidence
}

// composeRequirements reads each service the compose file builds from this
// repository the way a Dockerfile at its context would be read: what its
// framework or its environment schema requires is required here too. A
// service that runs a published image has nothing in the repository to read.
func composeRequirements(src api.SourceView, d Draft) Draft {
	for i, w := range d.Workloads {
		if w.Build == nil {
			continue
		}
		context := path.Clean(w.Build.Context)
		name := w.Build.Dockerfile
		if name == "" {
			name = "Dockerfile"
		}
		dockerfile := path.Join(context, name)
		d, _ = requirementsFor(src, d, i, context, dockerfile)
		if dir := path.Dir(dockerfile); dir != context {
			// rallly builds apps/web/Dockerfile from the root, and its
			// schema is apps/web/src/env.ts.
			d, _ = requirementsFor(src, d, i, dir, dockerfile)
		}
	}
	return d
}

func keysOf(vars []requiredVar) string {
	var keys []string
	for _, v := range vars {
		if v.Value == nil {
			keys = append(keys, v.Key)
		}
	}
	if len(keys) == 0 {
		for _, v := range vars {
			keys = append(keys, v.Key)
		}
	}
	return strings.Join(truncate(keys, 6), ", ")
}

func exists(src api.SourceView, name string) bool {
	info, err := src.Stat(name)
	return err == nil && !info.IsDir
}

func readSmall(src api.SourceView, name string) (string, bool) {
	return readLimited(src, name, 256<<10)
}

// --- framework conventions -------------------------------------------------

var (
	elixirFetch  = regexp.MustCompile(`System\.fetch_env!\(\s*"([A-Z][A-Z0-9_]*)"\s*\)`)
	elixirOrDies = regexp.MustCompile(`System\.get_env\(\s*"([A-Z][A-Z0-9_]*)"\s*\)\s*\|\|\s*raise`)
)

// elixirRequired reads config/runtime.exs, which a Phoenix release runs at
// boot: `System.get_env("SECRET_KEY_BASE") || raise …` and
// `System.fetch_env!("X")` are the app saying it stops without X.
func elixirRequired(src api.SourceView) []requiredVar {
	body, ok := readSmall(src, "config/runtime.exs")
	if !ok {
		return nil
	}
	var out []requiredVar
	seen := map[string]bool{}
	for _, re := range []*regexp.Regexp{elixirOrDies, elixirFetch} {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			if seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			out = append(out, requiredVar{
				Key: m[1], Format: formatFor(m[1], "phoenix"),
				Evidence: "config/runtime.exs raises when " + m[1] + " is not set",
			})
		}
	}
	return out
}

// symfonySecret asks for APP_SECRET where Symfony would read an empty one.
//
// The committed .env is Symfony's defaults and names APP_SECRET with nothing
// in it; the value lives in an uncommitted .env.local, so the app built from
// the repository answers every page with "A non-empty secret is required".
func symfonySecret(src api.SourceView) []requiredVar {
	var manifest struct {
		Require map[string]string `json:"require"`
	}
	if !readJSON(src, "composer.json", &manifest) {
		return nil
	}
	if _, ok := manifest.Require["symfony/framework-bundle"]; !ok {
		return nil
	}
	body, ok := readSmall(src, ".env")
	if !ok {
		return nil
	}
	for _, line := range strings.Split(body, "\n") {
		m := envAssignment.FindStringSubmatch(line)
		if m == nil || m[1] != "APP_SECRET" {
			continue
		}
		if v := unquote(strings.TrimSpace(m[2])); v != "" && !isPlaceholder(v) {
			return nil
		}
		return []requiredVar{{Key: "APP_SECRET", Format: spec.FormatRandom,
			Evidence: "Symfony signs with APP_SECRET, and the committed .env leaves it empty; the app " +
				"refuses every request with \"A non-empty secret is required\" until it has one"}}
	}
	return nil
}

// strapiKeys are the secrets a Strapi app's generated configuration reads with
// no default.
var strapiKeys = []string{"APP_KEYS", "API_TOKEN_SALT", "ADMIN_JWT_SECRET", "TRANSFER_TOKEN_SALT", "JWT_SECRET", "ENCRYPTION_KEY"}

var strapiEnvRead = regexp.MustCompile(`env(?:\.\w+)?\(\s*['"]([A-Z][A-Z0-9_]*)['"]\s*\)`)

// strapiSecrets asks for the keys a Strapi app signs and encrypts with.
//
// Its .env.example names every one of them with "tobemodified" in it, which
// is a placeholder rather than a value — and it is .env.example, so on its own
// it says nothing about what is required (O-4). Strapi itself does: its
// config reads each with no default, and it stops at start without them
// ("App keys are required"). A key counts when the config reads it that way
// or the template names it.
func strapiSecrets(src api.SourceView) []requiredVar {
	var manifest struct {
		Dependencies map[string]string `json:"dependencies"`
	}
	if !readJSON(src, "package.json", &manifest) {
		return nil
	}
	if _, ok := manifest.Dependencies["@strapi/strapi"]; !ok {
		return nil
	}

	named := map[string]bool{}
	for _, file := range []string{"server", "admin", "plugins"} {
		for _, ext := range []string{".js", ".ts"} {
			body, ok := readSmall(src, "config/"+file+ext)
			if !ok {
				continue
			}
			for _, m := range strapiEnvRead.FindAllStringSubmatch(body, -1) {
				named[m[1]] = true
			}
		}
	}
	for key := range envSamples(src) {
		named[key] = true
	}

	var out []requiredVar
	for _, key := range strapiKeys {
		if named[key] {
			out = append(out, requiredVar{Key: key, Format: spec.FormatRandom,
				Evidence: "Strapi signs and encrypts with " + key + " and stops at start without it"})
		}
	}
	return out
}

// --- environment schemas (Node) --------------------------------------------

// nodeSchemaFiles are where an app conventionally validates its environment.
var nodeSchemaFiles = []string{
	"start/env.ts", "start/env.js",
	"env.js", "env.mjs", "env.ts",
	"src/env.js", "src/env.mjs", "src/env.ts",
	"src/env/server.mjs", "src/env/server.ts", "src/env/server.js",
	"server/env.js", "server/env.ts", "src/server/env.js", "src/server/env.ts",
	"config/env.js", "config/env.ts", "src/config/env.js", "src/config/env.ts",
	"src/lib/env.js", "src/lib/env.ts", "lib/env.js", "lib/env.ts",
}

var (
	adonisEntry       = regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*)\s*:\s*Env\.schema\.\w+(\.optional)?`)
	envalidEntry      = regexp.MustCompile(`(?m)^\s*([A-Z][A-Z0-9_]*)\s*:\s*(?:str|num|bool|email|host|port|url|json)\s*\(([^)]*)\)`)
	envalidHasDefault = regexp.MustCompile(`(^|[^A-Za-z])default\s*:`)
	zodKey            = regexp.MustCompile(`^(\s*)([A-Z][A-Z0-9_]*)\s*:\s*(.*)$`)
)

// nodeSchemaRequired reads the first environment schema it finds: AdonisJS's
// start/env.ts, an envalid cleanEnv, or a t3-env / zod createEnv. A key with
// no default and nothing making it optional stops the app at start — the
// schema's whole job.
func nodeSchemaRequired(src api.SourceView, samples map[string]string) ([]requiredVar, string) {
	for _, file := range nodeSchemaFiles {
		body, ok := readSmall(src, file)
		if !ok {
			continue
		}
		var keys []string
		switch {
		case strings.Contains(body, "Env.schema."):
			for _, m := range adonisEntry.FindAllStringSubmatch(body, -1) {
				if m[2] == "" {
					keys = append(keys, m[1])
				}
			}
		case strings.Contains(body, "cleanEnv("):
			// devDefault does not count: the app runs in production, where
			// envalid ignores it.
			for _, m := range envalidEntry.FindAllStringSubmatch(body, -1) {
				if !envalidHasDefault.MatchString(m[2]) {
					keys = append(keys, m[1])
				}
			}
		case strings.Contains(body, "createEnv(") && strings.Contains(body, "z."):
			keys = zodRequired(body)
		}
		if len(keys) == 0 {
			continue
		}
		return schemaVars(keys, samples, file), file
	}
	return nil, ""
}

// zodRequired reads the keys of a t3-env schema's server, shared and client
// blocks. An
// entry runs from its key to the next key at the same indentation, so a chain
// split over lines is read whole.
func zodRequired(body string) []string {
	lines := strings.Split(body, "\n")
	var keys []string
	inBlock, depth := false, ""
	var key, chain string
	flush := func() {
		if key != "" && strings.Contains(chain, "z.") &&
			!strings.Contains(chain, ".optional(") && !strings.Contains(chain, ".default(") &&
			!strings.Contains(chain, ".catch(") {
			keys = append(keys, key)
		}
		key, chain = "", ""
	}
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		// The client block too: t3-env validates it on the server at start,
		// which is where NEXT_PUBLIC_BASE_URL stopped a self-hosted app.
		if strings.HasPrefix(trimmed, "server:") || strings.HasPrefix(trimmed, "shared:") ||
			strings.HasPrefix(trimmed, "client:") {
			flush()
			inBlock, depth = true, ""
			continue
		}
		if strings.HasPrefix(trimmed, "runtimeEnv:") || strings.HasPrefix(trimmed, "experimental__runtimeEnv:") {
			flush()
			inBlock = false
			continue
		}
		if !inBlock {
			continue
		}
		if m := zodKey.FindStringSubmatch(line); m != nil && (depth == "" || m[1] == depth) {
			flush()
			depth, key, chain = m[1], m[2], m[3]
			continue
		}
		if strings.HasPrefix(trimmed, "},") || trimmed == "}" {
			if depth != "" && len(line)-len(strings.TrimLeft(line, " \t")) < len(depth) {
				flush()
				inBlock = false
				continue
			}
		}
		if key != "" && !strings.HasPrefix(trimmed, "//") && !strings.HasPrefix(trimmed, "*") &&
			!strings.HasPrefix(trimmed, "/*") {
			chain += " " + trimmed
		}
	}
	flush()
	return keys
}

// schemaVars turns a schema's required keys into what to set or ask for.
//
// A schema requires settings as well as secrets, and asking a person for each
// one would put LOG_LEVEL in front of somebody who does not know what a log
// level is (R-104). So: PORT is Pando's to set and already is; HOST is where a
// container listens, which is everywhere; NODE_ENV is production. A setting
// the .env.example gives a real sample for takes it. What is left — secrets,
// and anything with no sample — is asked for.
func schemaVars(keys []string, samples map[string]string, file string) []requiredVar {
	var out []requiredVar
	for _, key := range keys {
		evidence := file + " requires " + key + " and stops the app at start without it"
		value, skip := conventionalValue(key)
		if skip {
			continue
		}
		// Not an address: a sample's host is the author's machine or their
		// example domain, and this app's own URL is something Pando knows.
		if sample, ok := samples[key]; value == nil && ok && sample != "" && !isPlaceholder(sample) &&
			!looksSecret(key) && !strings.Contains(sample, "://") && !namesAConnection(key) &&
			slotTypeFor(key, sample) == spec.SlotUnknown {
			v := sample
			value = &v
		}
		out = append(out, requiredVar{Key: key, Format: formatFor(key, ""), Evidence: evidence, Value: value})
	}
	return out
}

// conventionalValue is the value a required variable takes in any container
// Pando runs, so nobody is asked for it. PORT is skipped altogether: Pando
// sets it to the port it routes to at every deploy.
func conventionalValue(key string) (value *string, skip bool) {
	var v string
	switch key {
	case "PORT":
		return nil, true
	case "HOST":
		v = "0.0.0.0"
	case "NODE_ENV":
		v = "production"
	default:
		return nil, false
	}
	return &v, false
}

// --- Python ----------------------------------------------------------------

var (
	pythonEnvRead    = regexp.MustCompile(`os\.environ\[\s*["']([A-Z][A-Z0-9_]*)["']\s*\](\s*=[^=])?`)
	djangoSettingsRe = regexp.MustCompile(`DJANGO_SETTINGS_MODULE["']\s*,\s*["']([\w.]+)["']`)
)

// pythonEntryFiles are the modules a Python web app conventionally starts
// from. Read at import, so a KeyError there stops the app before it serves.
var pythonEntryFiles = []string{
	"main.py", "app.py", "server.py", "wsgi.py", "asgi.py",
	"app/main.py", "app/__init__.py", "src/main.py", "src/app.py",
}

// pythonRequired finds `os.environ["X"]` read at module level — no default, so
// a missing X raises KeyError and the app does not start — in the entry
// module, or in the Django settings module manage.py names.
func pythonRequired(src api.SourceView) ([]requiredVar, string) {
	files := append([]string(nil), pythonEntryFiles...)
	django := ""
	if body, ok := readSmall(src, "manage.py"); ok {
		if m := djangoSettingsRe.FindStringSubmatch(body); m != nil {
			base := strings.ReplaceAll(m[1], ".", "/")
			django = base + ".py"
			files = append([]string{django, base + "/__init__.py", base + "/base.py", base + "/production.py"}, files...)
		}
	}
	for _, file := range files {
		body, ok := readSmall(src, file)
		if !ok {
			continue
		}
		framework := ""
		if file == django || strings.HasPrefix(file, strings.TrimSuffix(django, ".py")+"/") {
			framework = "django"
		}
		var out []requiredVar
		seen := map[string]bool{}
		for _, line := range moduleLevel(body) {
			for _, m := range pythonEnvRead.FindAllStringSubmatch(line, -1) {
				if m[2] != "" || seen[m[1]] {
					continue // an assignment, not a read
				}
				seen[m[1]] = true
				out = append(out, requiredVar{Key: m[1], Format: formatFor(m[1], framework),
					Evidence: file + " reads " + m[1] + " with no fallback, and the app stops without it"})
			}
		}
		if len(out) > 0 {
			return out, file
		}
	}
	return nil, ""
}

// moduleLevel is the lines that run unconditionally at import: top-level
// statements, with whatever they continue onto (a dict literal spread over
// lines is one statement). The body of a def does not run at import, and the
// body of an if, a try or a loop may not — a read under
// `if os.getenv("DB") == "mysql":` is required only of somebody who chose
// MySQL.
func moduleLevel(body string) []string {
	var out []string
	skipping := false
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if indented := line[0] == ' ' || line[0] == '\t'; !indented {
			skipping = false
			for _, opener := range compoundOpeners {
				if strings.HasPrefix(trimmed, opener) {
					skipping = true
					break
				}
			}
			if skipping && !strings.HasPrefix(trimmed, "def ") && !strings.HasPrefix(trimmed, "async def ") &&
				!strings.HasPrefix(trimmed, "@") && !strings.HasPrefix(trimmed, "else") &&
				!strings.HasPrefix(trimmed, "except") && !strings.HasPrefix(trimmed, "finally") {
				// The condition itself runs; its body is what may not.
				if i := strings.LastIndex(trimmed, ":"); i >= 0 {
					out = append(out, trimmed[:i])
				}
			}
			if !skipping {
				out = append(out, line)
			}
			continue
		}
		if !skipping {
			out = append(out, line)
		}
	}
	return out
}

var compoundOpeners = []string{
	"def ", "async def ", "@", "if ", "elif ", "else", "try", "except", "finally",
	"for ", "async for ", "while ", "with ", "async with ", "match ",
}

// --- what a variable holds -------------------------------------------------

// formatFor says what shape a required key's value takes, from its name and,
// where the name is shared between frameworks, which framework reads it.
func formatFor(key, framework string) spec.SecretFormat {
	switch key {
	case "SECRET_KEY_BASE":
		return spec.FormatRandomLong
	case "APP_KEY":
		if framework == "laravel" {
			return spec.FormatLaravelKey
		}
	case "SECRET_KEY":
		if framework == "django" {
			return spec.FormatDjangoKey
		}
	}
	if slotTypeFor(key, "") != spec.SlotUnknown || !looksSecret(key) {
		return ""
	}
	return spec.FormatRandom
}

// looksSecret reports a name that holds a secret rather than a setting.
func looksSecret(key string) bool {
	for _, word := range strings.Split(strings.ToUpper(key), "_") {
		switch word {
		case "SECRET", "SECRETS", "KEY", "KEYS", "TOKEN", "SALT", "PASSWORD", "PASS", "PWD", "PASSPHRASE":
			return true
		}
	}
	return false
}

// placeholderValue is a template value that stands for "put yours here".
var placeholderValue = regexp.MustCompile(`(?i)^(change[-_ .]?(me|it|this)\b.*|replace[-_ ]?(me|this)\b.*|to[-_ ]?be[-_ ]?(modified|changed|replaced)\d*(,to[-_ ]?be[-_ ]?(modified|changed|replaced)\d*)*|your[-_ ].*|<.*>|x{3,}|\*{3,}|\.{3}|todo|fixme|placeholder|secret|password|null|none|not-a-secret.*)$`)

// isPlaceholder reports a sample value that is no value at all. A template
// that says `SESSION_SECRET=change-me` has said the app needs one and left it
// to the reader, exactly as `SESSION_SECRET=` would.
func isPlaceholder(value string) bool {
	return placeholderValue.MatchString(strings.TrimSpace(unquote(value)))
}

func unquote(value string) string {
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

// envSamples reads .env.example (or its siblings) into key → sample value.
func envSamples(src api.SourceView) map[string]string {
	out := map[string]string{}
	for _, name := range []string{".env.example", ".env.sample", ".env.template", ".example.env"} {
		f, err := src.Open(name)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(io.LimitReader(f, 64<<10))
		for scanner.Scan() {
			if m := envAssignment.FindStringSubmatch(scanner.Text()); m != nil {
				out[m[1]] = unquote(strings.TrimSpace(m[2]))
			}
		}
		_ = f.Close()
		break
	}
	return out
}

var dockerfileEnvLine = regexp.MustCompile(`(?i)^\s*ENV\s+(.+)$`)

// dockerfileEnv is every variable a Dockerfile's ENV sets to something.
func dockerfileEnv(src api.SourceView, name string) map[string]bool {
	out := map[string]bool{}
	f, err := src.Open(name)
	if err != nil {
		return out
	}
	defer func() { _ = f.Close() }()
	for _, line := range joinContinuations(bufio.NewScanner(io.LimitReader(f, 256<<10))) {
		m := dockerfileEnvLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		body := strings.TrimSpace(m[1])
		if !strings.Contains(strings.Fields(body)[0], "=") {
			// The legacy `ENV KEY value` form: one variable, the rest its value.
			if parts := strings.Fields(body); len(parts) > 1 {
				out[parts[0]] = true
			}
			continue
		}
		for _, field := range strings.Fields(body) {
			if k, v, ok := strings.Cut(field, "="); ok && v != "" && v != `""` && v != "''" {
				out[k] = true
			}
		}
	}
	return out
}

// subView is a SourceView rooted at a directory of another.
func subView(src api.SourceView, dir string) api.SourceView {
	dir = path.Clean(dir)
	if dir == "." || dir == "" || dir == "/" {
		return src
	}
	return prefixedView{src: src, dir: dir}
}

type prefixedView struct {
	src api.SourceView
	dir string
}

func (v prefixedView) Open(name string) (io.ReadCloser, error) {
	return v.src.Open(path.Join(v.dir, name))
}

func (v prefixedView) Stat(name string) (api.FileInfo, error) {
	return v.src.Stat(path.Join(v.dir, name))
}

func (v prefixedView) Glob(pattern string) ([]string, error) {
	found, err := v.src.Glob(path.Join(v.dir, pattern))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range found {
		if rel, ok := strings.CutPrefix(p, v.dir+"/"); ok {
			out = append(out, rel)
		}
	}
	return out, nil
}

// --- the trial run's log -----------------------------------------------------

// The shapes an app's "I cannot start without X" takes. Matched against the
// log of a trial run that failed, never against one that served: an app that
// says FOO is unset and runs anyway has told nobody FOO is required.
var (
	ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

	// A line saying something is missing or must be set.
	failurePhrase = regexp.MustCompile(`(?i)(missing|not set|isn't set|is unset|not defined|not provided|is required|are required|must be (set|defined|provided|specified|given)|need to (define|set|provide)|needs? to be (set|defined|provided)|is empty|are empty|cannot be empty|must not be empty|halting|aborting)`)

	// A line saying the absence is fine, or naming a variable for some
	// other reason: Jenkins' "missing rw permissions on JENKINS_HOME" is
	// about a directory, and a warning is the app carrying on.
	benignPhrase = regexp.MustCompile(`(?i)(\bwarn(ing)?\b|permission|defaulting|using (the )?default|falling back|fallback|optional|ignoring|skipping|disabled|not enabled|will be generated|generating a)`)

	// A line that names a variable as one.
	variablePhrase = regexp.MustCompile(`(?i)(environment variable|env var|\benv\b|variable)`)

	// VAR_NAME: upper case with at least one underscore, which is what tells
	// it apart from an ERROR or a FATAL.
	underscoreName = regexp.MustCompile(`\b[A-Z][A-Z0-9]*(?:_[A-Z0-9]+)+\b`)
	quotedName     = regexp.MustCompile("[\"'`]([A-Z][A-Z0-9_]{2,})[\"'`]")
	afterVariable  = regexp.MustCompile(`(?i)(?:environment variable|env var)s?\s*:?\s*([A-Z][A-Z0-9_]{2,})\b`)
	dockerRunEnv   = regexp.MustCompile(`-e\s+([A-Z][A-Z0-9_]*)=`)

	// Python's KeyError from os.environ["X"].
	keyError = regexp.MustCompile(`KeyError:\s*["']([A-Z][A-Z0-9_]{2,})["']`)

	// A list item under "Missing environment variables:" — envalid's
	// `    JWT_SECRET: undefined`, AdonisJS's `- Missing environment variable "X"`.
	listedName = regexp.MustCompile(`^\s*[-*•]?\s*["']?([A-Z][A-Z0-9_]*)["']?\s*:\s*(undefined|\[|required|invalid|missing)`)

	// zod and t3-env: `SUPPORT_EMAIL: [ 'Required' ]`.
	zodIssue = regexp.MustCompile(`["']?([A-Z][A-Z0-9_]*)["']?\s*:\s*\[\s*["'](Required|[^"']*received undefined)`)

	// A connection that failed, which makes a database variable named nearby
	// the reason rather than a line of configuration printed on the way.
	connectionFailure = regexp.MustCompile(`(?i)(connection refused|econnrefused|failed to connect|could not connect|unable to connect|can't connect|could not translate host|name does not resolve|no such host|name or service not known|getaddrinfo|enotfound)`)
)

// knownMessages are framework errors that do not name their variable, and
// the variable each one means. A framework's own words for a framework's own
// key — the shape is the framework's, not any one app's.
var knownMessages = []struct {
	phrase    string
	key       string
	framework string
}{
	{"missing `secret_key_base`", "SECRET_KEY_BASE", ""},
	{"missing secret_key_base", "SECRET_KEY_BASE", ""},
	{"no application encryption key has been specified", "APP_KEY", "laravel"},
	{"application key is missing", "APP_KEY", "laravel"},
	{"the secret_key setting must not be empty", "SECRET_KEY", "django"},
	{"(security.w009)", "SECRET_KEY", "django"},
}

// docker's legacy link variables (MYSQL_PORT_3306_TCP): set by --link, never
// by a person, so never worth asking for.
var dockerLinkName = regexp.MustCompile(`_PORT_\d+_(TCP|UDP)`)

// varsFromLog reads the variables a failed trial run's log says the app
// cannot start without.
func varsFromLog(log string) []requiredVar {
	log = ansiEscape.ReplaceAllString(log, "")
	lower := strings.ToLower(log)
	lines := strings.Split(log, "\n")

	found := map[string]requiredVar{}
	var order []string
	note := func(key, framework, line string) {
		if key == "" || dockerLinkName.MatchString(key) || strings.HasPrefix(key, "ERR_") ||
			strings.HasPrefix(key, "E_") {
			return
		}
		if _, seen := found[key]; seen {
			return
		}
		value, skip := conventionalValue(key)
		if skip {
			return
		}
		line = strings.TrimSpace(line)
		if len(line) > 160 {
			line = line[:157] + "…"
		}
		found[key] = requiredVar{
			Key:      key,
			Format:   formatFor(key, framework),
			Evidence: fmt.Sprintf("the app stopped when Pando tried it, and its log says: %q", line),
			Value:    value,
		}
		order = append(order, key)
	}

	framework := ""
	switch {
	case strings.Contains(lower, "laravel") || strings.Contains(lower, "illuminate") ||
		strings.Contains(lower, "artisan") || strings.Contains(lower, "application key"):
		framework = "laravel"
	case strings.Contains(lower, "django"):
		framework = "django"
	}

	for _, known := range knownMessages {
		if i := strings.Index(lower, known.phrase); i >= 0 {
			note(known.key, known.framework, lineAt(log, i))
		}
	}

	if strings.Contains(log, "environ") {
		for _, m := range keyError.FindAllStringSubmatch(log, -1) {
			note(m[1], framework, "KeyError: '"+m[1]+"'")
		}
	}

	for i, line := range lines {
		if zodIssue.MatchString(line) {
			for _, m := range zodIssue.FindAllStringSubmatch(line, -1) {
				note(m[1], framework, line)
			}
		}
		if !failurePhrase.MatchString(line) || benignPhrase.MatchString(line) {
			continue
		}

		// A heading over a list: "Missing environment variables:" and the
		// names under it, one a line.
		if strings.HasSuffix(strings.TrimSpace(line), ":") || strings.HasSuffix(strings.TrimSpace(line), "variables") {
			for _, next := range lines[i+1 : min(len(lines), i+30)] {
				if strings.TrimSpace(next) == "" {
					continue
				}
				m := listedName.FindStringSubmatch(next)
				if m == nil {
					if am := afterVariable.FindStringSubmatch(next); am != nil {
						note(am[1], framework, next)
						continue
					}
					break
				}
				note(m[1], framework, line+" "+strings.TrimSpace(next))
			}
		}

		// This line, and the two after it when they name a variable as one:
		// "The application key is missing, halting init!" is followed by
		// "And apply it to the APP_KEY environment variable."
		window := []string{line}
		for _, next := range lines[i+1 : min(len(lines), i+3)] {
			if variablePhrase.MatchString(next) || dockerRunEnv.MatchString(next) {
				window = append(window, next)
			}
		}
		for _, text := range window {
			for _, name := range namesIn(text) {
				note(name, framework, line)
			}
		}
	}

	// `docker run -e A=… -e B=…` in a message that is an instruction: the
	// image is telling whoever runs it what to set. Counted when the log also
	// says something is required, as CouchDB's "You *MUST* specify an admin
	// user and password … with -e COUCHDB_USER=admin -e COUCHDB_PASSWORD=…".
	if strings.Contains(lower, "must") || failurePhrase.MatchString(log) {
		for _, line := range lines {
			for _, m := range dockerRunEnv.FindAllStringSubmatch(line, -1) {
				note(m[1], framework, line)
			}
		}
	}

	// A database or cache the app could not reach, named by the variable that
	// was supposed to say where it is. R-107 forbids inventing a database a
	// repository never mentions; a log naming DATABASE_URL is a mention.
	if connectionFailure.MatchString(log) {
		for _, line := range lines {
			for _, name := range underscoreName.FindAllString(line, -1) {
				if slotTypeFor(name, "") != spec.SlotUnknown {
					note(name, framework, line)
				}
			}
		}
	}

	// "A or A_FILE must be set": the file form is the same answer.
	var out []requiredVar
	for _, key := range order {
		if base, ok := strings.CutSuffix(key, "_FILE"); ok {
			if _, both := found[base]; both {
				continue
			}
		}
		out = append(out, found[key])
	}
	return out
}

// namesIn is the variables a line names, in order, keeping only the first of
// alternatives: "either A or B is required (or set C=true)" asks for A, and
// "define A and B or B_FILE" for A and B. Asking for every name would block
// the deploy on values the app says it does without.
func namesIn(text string) []string {
	type found struct {
		name       string
		start, end int
	}
	var all []found
	addAt := func(locs [][]int, group int, keep func(string) bool) {
		for _, loc := range locs {
			name := text[loc[2*group]:loc[2*group+1]]
			if keep == nil || keep(name) {
				all = append(all, found{name, loc[0], loc[1]})
			}
		}
	}
	addAt(afterVariable.FindAllStringSubmatchIndex(text, -1), 1, nil)
	addAt(dockerRunEnv.FindAllStringSubmatchIndex(text, -1), 1, nil)
	addAt(quotedName.FindAllStringSubmatchIndex(text, -1), 1, func(name string) bool {
		return strings.Contains(name, "_") || variablePhrase.MatchString(text)
	})
	addAt(underscoreName.FindAllStringSubmatchIndex(text, -1), 0, nil)
	sort.SliceStable(all, func(i, j int) bool { return all[i].start < all[j].start })

	var names []string
	seen := map[string]bool{}
	end := -1
	for _, f := range all {
		if seen[f.name] {
			continue
		}
		if end >= 0 && f.start >= end {
			between := strings.ToLower(text[end:f.start])
			if orWord.MatchString(between) && !andWord.MatchString(between) {
				seen[f.name] = true
				end = f.end
				continue
			}
		}
		seen[f.name] = true
		names = append(names, f.name)
		end = f.end
	}
	return names
}

var (
	orWord  = regexp.MustCompile(`\bor\b`)
	andWord = regexp.MustCompile(`\band\b`)
)

func lineAt(text string, offset int) string {
	start := strings.LastIndexByte(text[:offset], '\n') + 1
	end := strings.IndexByte(text[offset:], '\n')
	if end < 0 {
		return text[start:]
	}
	return text[start : offset+end]
}

// requireFromTrial turns what a failed trial run's log names into required
// slots on the primary workload, so the person deploying is asked for them
// rather than shown a crash (R-132).
//
// Only when the trial failed: the app exited, or it never opened a port.
// An app that complains and serves anyway has not said it needs anything.
func requireFromTrial(d Draft, t Trial) Draft {
	if !t.Ran || t.Log == "" || (!t.Crashed && len(t.ObservedPorts) > 0) {
		return d
	}
	vars := varsFromLog(t.Log)
	if len(vars) == 0 {
		return d
	}
	return requireVars(d, primaryIndex(d), vars)
}
