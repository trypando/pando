package detect

import (
	"bufio"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/adapter/api"
)

// composeVars is the environment a compose file is interpolated against.
//
// `docker compose` substitutes variables across the whole file before it reads
// any of it: an image tag, a build context, a bind-mount source, an
// environment value. Pando had a version of that for environment values and
// mount sources only, which knew defaults and nothing else, so
// `image: ghcr.io/x/y:${VERSION:-release}` was pulled as written and refused as
// an invalid reference, `${PWD}/sharry.conf` became a volume name Docker
// rejects, and a value the committed `.env` held reached the app as the text
// `${PGADMIN_MAIL}` (issue #90).
//
// The values are what compose would have from the same checkout: the `.env`
// file beside the compose file, and PWD, which is the directory compose runs in
// — the one the file is in, so `${PWD}/x` is `./x`. There is no shell, so
// nothing else is set.
type composeVars struct {
	values map[string]string

	// missing are the names referenced with no value and no default, in the
	// order first met. Compose substitutes an empty string and warns.
	missing []string
	seen    map[string]bool

	// envSlots are environment entries whose whole value is one variable
	// nothing sets — `ROOT_URL: ${ROOT_URL}` — keyed by service, then by the
	// entry's key, to the variable's name. Each becomes a required slot: the
	// author wrote that the value comes from outside the file, and nothing
	// here supplies it (R-130, R-132).
	envSlots map[string]map[string]string
}

// envFileName is the file compose reads variables from, beside the compose file.
const envFileName = ".env"

// newComposeVars reads the variables compose would interpolate with.
func newComposeVars(src api.SourceView, composeFile string) *composeVars {
	v := &composeVars{
		values:   map[string]string{},
		seen:     map[string]bool{},
		envSlots: map[string]map[string]string{},
	}
	if src != nil {
		if f, err := src.Open(path.Join(path.Dir(composeFile), envFileName)); err == nil {
			for _, kv := range parseDotenv(f) {
				v.values[kv.Key] = kv.Value
			}
			_ = f.Close()
		}
	}
	if _, set := v.values["PWD"]; !set {
		v.values["PWD"] = "."
	}
	return v
}

func (v *composeVars) miss(name string) {
	if !v.seen[name] {
		v.seen[name] = true
		v.missing = append(v.missing, name)
	}
}

// interpolate applies compose's substitution rules to every string in a
// decoded compose file, returning the file with every one resolved.
//
// Only values are substituted, never keys, as compose does.
func (v *composeVars) interpolate(node any) any {
	return v.walk(node, nil)
}

func (v *composeVars) walk(node any, at []string) any {
	switch n := node.(type) {
	case map[string]any:
		return v.walkMap(n, at)
	case []any:
		return v.walkList(n, at)
	case string:
		return typed(n, v.expand(n))
	default:
		return node
	}
}

// walkMap is walk for a mapping; at grows by each key.
func (v *composeVars) walkMap(n map[string]any, at []string) map[string]any {
	out := make(map[string]any, len(n))
	for k, child := range n {
		if env, ok := v.environmentEntry(at, k, child); ok {
			out[k] = env
			continue
		}
		out[k] = v.walk(child, append(at[:len(at):len(at)], k))
	}
	return out
}

// walkList is walk for a sequence; its items share its path.
func (v *composeVars) walkList(n []any, at []string) []any {
	out := make([]any, len(n))
	for i, child := range n {
		if env, ok := v.environmentItem(at, child); ok {
			out[i] = env
			continue
		}
		out[i] = v.walk(child, at)
	}
	return out
}

// environmentItem handles one `KEY=value` entry of a service's list-shaped
// environment.
func (v *composeVars) environmentItem(at []string, item any) (any, bool) {
	s, ok := item.(string)
	if !ok || !isEnvironment(at) {
		return nil, false
	}
	key, raw, found := strings.Cut(s, "=")
	if !found {
		return nil, false
	}
	name, whole := v.wholeMissing(raw)
	if !whole {
		return nil, false
	}
	v.slot(at[1], key, name)
	return key + "=", true
}

// environmentEntry handles one entry of a service's map-shaped environment.
func (v *composeVars) environmentEntry(at []string, key string, value any) (any, bool) {
	if !isEnvironment(at) {
		return nil, false
	}
	raw, ok := value.(string)
	if !ok {
		return nil, false
	}
	if name, whole := v.wholeMissing(raw); whole {
		v.slot(at[1], key, name)
		return "", true
	}
	return nil, false
}

// isEnvironment reports whether a path is a service's `environment:`.
func isEnvironment(at []string) bool {
	return len(at) == 3 && at[0] == "services" && at[2] == "environment"
}

func (v *composeVars) slot(service, key, name string) {
	if v.envSlots[service] == nil {
		v.envSlots[service] = map[string]string{}
	}
	v.envSlots[service][strings.TrimSpace(key)] = name
}

// wholeRef matches a value that is one variable and nothing else, with no
// default: `${NAME}`, `$NAME`, and the error forms `${NAME:?why}` and
// `${NAME?why}`, which are the author saying outright that it is required.
var wholeRef = regexp.MustCompile(`^\$(?:([A-Za-z_][A-Za-z0-9_]*)|\{([A-Za-z_][A-Za-z0-9_]*)(?::?\?[^}]*)?\})$`)

// wholeMissing reports a value that is exactly one variable nothing sets.
func (v *composeVars) wholeMissing(raw string) (string, bool) {
	m := wholeRef.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	name := m[1] + m[2]
	if _, set := v.values[name]; set {
		return "", false
	}
	v.miss(name)
	return name, true
}

// expand substitutes variables in one string, as compose does.
//
// The forms are compose's: `$NAME` and `${NAME}`; `${NAME:-default}` (unset or
// empty) and `${NAME-default}` (unset); `${NAME:?err}` and `${NAME?err}`, which
// stop `docker compose` when unset; `${NAME:+alt}` and `${NAME+alt}`, the
// replacement when set. A default can hold a substitution of its own. `$$` is a
// literal `$`, which reaches the container's shell as one.
//
// A variable nothing sets becomes an empty string, as it does under compose,
// and is recorded so the import can say so. The error forms are recorded the
// same way: compose would refuse to start, and Pando says which variable it
// would have refused for.
func (v *composeVars) expand(text string) string {
	if !strings.Contains(text, "$") {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); {
		if text[i] != '$' || i+1 >= len(text) {
			out.WriteByte(text[i])
			i++
			continue
		}
		i = v.expandAt(text, i, &out)
	}
	return out.String()
}

// expandAt writes the substitution starting at the `$` at i, and returns
// where the text after it starts. An unclosed `${` is written as it stands,
// with the rest of the text.
func (v *composeVars) expandAt(text string, i int, out *strings.Builder) int {
	next := text[i+1]
	switch {
	case next == '$':
		out.WriteByte('$')
		return i + 2
	case next == '{':
		end := matchingBrace(text, i+1)
		if end < 0 {
			out.WriteString(text[i:])
			return len(text)
		}
		out.WriteString(v.substitute(text[i+2 : end]))
		return end + 1
	case isNameStart(next):
		j := nameEnd(text, i+1)
		name := text[i+1 : j]
		if value, ok := v.values[name]; ok {
			out.WriteString(value)
		} else {
			v.miss(name)
		}
		return j
	default:
		out.WriteByte('$')
		return i + 1
	}
}

// nameEnd is where the run of name characters starting at from ends.
func nameEnd(s string, from int) int {
	j := from
	for j < len(s) && isNameChar(s[j]) {
		j++
	}
	return j
}

// substitute resolves the body of one `${...}`.
func (v *composeVars) substitute(body string) string {
	j := nameEnd(body, 0)
	name, rest := body[:j], body[j:]
	if name == "" || !isNameStart(name[0]) {
		// Not a substitution compose understands. Kept as written, so what
		// Pando could not read stays visible.
		return "${" + body + "}"
	}
	op, arg, known := composeOp(rest)
	if !known {
		return "${" + body + "}"
	}
	value, set := v.values[name]

	// The forms with a colon treat an empty value as unset.
	present := set && (value != "" || !strings.HasPrefix(op, ":"))
	switch strings.TrimPrefix(op, ":") {
	case "-":
		if !present {
			return v.expand(arg)
		}
		return value
	case "+":
		if present {
			return v.expand(arg)
		}
		return ""
	default: // "" and "?"
		if !present {
			v.miss(name)
		}
		return value
	}
}

// composeOp splits what follows a name in `${...}` into its operator and
// argument, reporting false when it is not one compose understands.
func composeOp(rest string) (op, arg string, known bool) {
	if rest == "" {
		return "", "", true
	}
	for _, candidate := range []string{":-", ":?", ":+", "-", "?", "+"} {
		if strings.HasPrefix(rest, candidate) {
			return candidate, rest[len(candidate):], true
		}
	}
	return rest, "", false
}

// matchingBrace finds the `}` closing the `{` at open, counting nested `${`.
func matchingBrace(text string, open int) int {
	depth := 0
	for i := open; i < len(text); i++ {
		switch text[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

func isNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isNameChar(c byte) bool { return isNameStart(c) || (c >= '0' && c <= '9') }

// canonicalInt is an integer written the way YAML would have read it as one.
var canonicalInt = regexp.MustCompile(`^(0|-?[1-9][0-9]*)$`)

// typed keeps a substituted number a number. `replicas: ${N:-1}` is a string
// to the YAML parser and an integer to compose, which types the file after
// interpolating it; a string left where a count belongs fails to decode.
func typed(raw, expanded string) any {
	if raw != expanded && canonicalInt.MatchString(expanded) {
		if n, err := strconv.Atoi(expanded); err == nil {
			return n
		}
	}
	return expanded
}

// missingNames returns the variables nothing set, sorted for stable output.
func (v *composeVars) missingNames() []string {
	out := append([]string(nil), v.missing...)
	sort.Strings(out)
	return out
}

// dotenvKey is a variable name in a dotenv file. Compose accepts lower case
// and dots; the uppercase-only pattern .env.example is read with is about
// which names look like configuration, which is a different question.
var dotenvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// parseDotenv reads a dotenv file the way compose does.
//
// `KEY=value`, optionally after `export`. An unquoted value ends at a `#` with
// whitespace before it — `PYTHON_VERSION=314 # which dockerfile` is 314, and
// was read as the whole line, so the Dockerfile path built from it named a
// directory that does not exist (issue #90). A single-quoted value is literal;
// a double-quoted one has its escapes read. Unquoted and double-quoted values
// are interpolated against what the file set above them; there is no shell to
// read anything else from.
func parseDotenv(r io.Reader) []assignment {
	var out []assignment
	local := map[string]string{}
	vars := &composeVars{values: local, seen: map[string]bool{}}

	scanner := bufio.NewScanner(io.LimitReader(r, 64<<10))
	for scanner.Scan() {
		key, raw, ok := dotenvLine(scanner.Text())
		if !ok {
			continue
		}
		value := dotenvValue(vars, raw)
		local[key] = value
		out = append(out, assignment{Key: key, Value: value})
	}
	return out
}

// dotenvLine splits one line of a dotenv file into its key and its raw value,
// reporting false for a blank line, a comment, or a line that sets nothing.
func dotenvLine(text string) (key, value string, ok bool) {
	line := strings.TrimSpace(text)
	if line == "" || strings.HasPrefix(line, "#") {
		return "", "", false
	}
	line = strings.TrimSpace(strings.TrimPrefix(line, "export "))
	key, value, found := strings.Cut(line, "=")
	key = strings.TrimSpace(key)
	if !found || !dotenvKey.MatchString(key) {
		return "", "", false
	}
	return key, strings.TrimLeft(value, " \t"), true
}

// dotenvValue reads a raw value by how it is quoted, interpolating it against
// vars where compose would.
func dotenvValue(vars *composeVars, value string) string {
	switch {
	case strings.HasPrefix(value, "'"):
		if end := strings.Index(value[1:], "'"); end >= 0 {
			return value[1 : end+1]
		}
		return value[1:]
	case strings.HasPrefix(value, `"`):
		body, closed := closingQuote(value[1:])
		if !closed {
			body = value[1:]
		}
		return vars.expand(unescape(body))
	default:
		return vars.expand(strings.TrimSpace(stripInlineComment(value)))
	}
}

// closingQuote returns a double-quoted value's body, up to the first `"` not
// escaped with a backslash.
func closingQuote(s string) (string, bool) {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			i++
		case '"':
			return s[:i], true
		}
	}
	return s, false
}

func unescape(s string) string {
	return strings.NewReplacer(`\n`, "\n", `\t`, "\t", `\r`, "\r", `\"`, `"`, `\\`, `\`).Replace(s)
}

// stripInlineComment cuts an unquoted value at a ` #` comment.
func stripInlineComment(value string) string {
	for i := 1; i < len(value); i++ {
		if value[i] == '#' && (value[i-1] == ' ' || value[i-1] == '\t') {
			return value[:i]
		}
	}
	return value
}
