// Command gen-api-types emits TypeScript types for the console from the Go
// types the API actually returns.
//
// R-261: the API is the product, and the console, CLI and MCP are clients of it.
// Design 08 §1.2 says API types are generated and never hand-written, because a
// hand-written type is a second opinion about what the server returns and the
// two drift the first time a field is added.
//
// It reflects over Go types rather than reading an OpenAPI document, because
// **there is no OpenAPI document**. Writing one by hand to generate from would
// reintroduce exactly the drift the requirement forbids, one level removed: the
// spec would be the thing that fell out of step instead of the types. The Go
// structs are what the handlers serialize, so they are the only honest source.
//
// Run through `npm run types` in console/, which the build depends on.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/trypando/pando/internal/core/autodeploy"
	"github.com/trypando/pando/internal/core/idp"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/security"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/core/update"
	"github.com/trypando/pando/internal/core/upgrade"
	"github.com/trypando/pando/internal/detect"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/reference"
)

// exported are the types that cross the API boundary.
//
// Listed explicitly rather than discovered, so that adding a type to the
// console is a deliberate act and an internal struct cannot leak into the
// public surface by being referenced once. Anything these reference is followed
// automatically.
var exported = []any{
	state.App{},
	state.Section{},
	state.Revision{},
	state.Deployment{},
	// A deploy waiting for approval, as GET /approvals lists it (R-154).
	state.AwaitingApproval{},
	state.Detection{},
	state.GrantRow{},
	state.User{},
	detect.Proposal{},
	detect.Candidate{},
	detect.Question{},
	spec.AppSpec{},
	errs.Error{},

	// The API's own description. The console's API screen renders this, and a
	// hand-written interface for it would be the second opinion this generator
	// exists to prevent — the one place where a stale type would describe the
	// documentation of the API rather than the API.
	reference.Document{},
	state.Token{},

	// The security score (R-310). The console renders the report the API
	// serves, and a hand-written interface for it would drift the moment a
	// finding gains a field.
	security.Report{},
	policy.Document{},

	// The last scheduled backup of an app (issue #87), shown on the app and on
	// the Backups screen.
	state.BackupAttempt{},

	// External identity (issue #51): the providers screen, the sign-in page's
	// options, a test sign-in's report, and an account's identities.
	idp.ProviderView{},
	idp.SignInOptions{},
	idp.TestReport{},
	idp.Problem{},
	state.Identity{},
	state.Group{},

	// Whether a newer Pando is released (R-351), on the Updates screen.
	update.Status{},

	// The in-place upgrade (R-355 – R-360): its plan and its outcome.
	upgrade.Plan{},
	upgrade.Attempt{},

	// Automatic deploys (R-141, R-142): the deploy settings screen's view,
	// with the last check, and what a save answers.
	autodeploy.View{},
	autodeploy.Saved{},
}

func main() {
	out, err := generate()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// Relative to the working directory, which is `console/` — npm runs this
	// as the console's own `types` script. Run from anywhere else it used to
	// create a second src/api/types.gen.ts wherever you were standing, and one
	// of those was committed at the repository root and went stale there,
	// looking for all the world like a source file.
	if _, err := os.Stat("package.json"); err != nil {
		fmt.Fprintln(os.Stderr,
			"Run this from the console directory, where the types belong: cd console && npm run types")
		os.Exit(1)
	}

	path := filepath.Join("src", "api", "types.gen.ts")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	// G306: generated source, committed to the repository and read by every
	// developer and by CI. It holds no secret.
	if err := os.WriteFile(path, out, 0o644); err != nil { //nolint:gosec
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Printf("wrote %s\n", path)
}

// tsNames renames Go types whose names collide in TypeScript's one namespace.
var tsNames = map[reflect.Type]string{
	reflect.TypeOf(policy.Document{}):  "PolicyDocument",
	reflect.TypeOf(autodeploy.View{}):  "AutoDeployView",
	reflect.TypeOf(autodeploy.Saved{}): "AutoDeploySaved",
}

func tsName(t reflect.Type) string {
	if n, ok := tsNames[t]; ok {
		return n
	}
	return t.Name()
}

type generator struct {
	buf   bytes.Buffer
	done  map[reflect.Type]bool
	names map[string]reflect.Type
	queue []reflect.Type
}

func generate() ([]byte, error) {
	g := &generator{done: map[reflect.Type]bool{}, names: map[string]reflect.Type{}}

	g.buf.WriteString(`// Generated by cmd/gen-api-types. Do not edit.
//
// These are the Go types the API serializes, mechanically translated. R-261
// makes the API authoritative, and design 08 §1.2 says these are generated for
// that reason: a hand-written type is a second opinion about what the server
// returns, and the two drift the first time a field is added.
//
// Regenerate with: npm run types

`)

	for _, v := range exported {
		g.enqueue(reflect.TypeOf(v))
	}

	for len(g.queue) > 0 {
		t := g.queue[0]
		g.queue = g.queue[1:]
		if err := g.writeInterface(t); err != nil {
			return nil, err
		}
	}

	return g.buf.Bytes(), nil
}

func (g *generator) enqueue(t reflect.Type) {
	t = deref(t)
	if t.Kind() != reflect.Struct || g.done[t] || t == reflect.TypeOf(time.Time{}) {
		return
	}
	g.done[t] = true
	g.queue = append(g.queue, t)
}

func (g *generator) writeInterface(t reflect.Type) error {
	// Two Go types of one name would be two TypeScript interfaces of one
	// name, which TypeScript merges without a word: each type would then
	// claim the other's fields. reference.Document and policy.Document did
	// exactly that before this check existed.
	name := tsName(t)
	if g.names == nil {
		g.names = map[string]reflect.Type{}
	}
	if prev, ok := g.names[name]; ok {
		return fmt.Errorf("%s and %s are both called %s in TypeScript; add one to tsNames", prev, t, name)
	}
	g.names[name] = t

	fields, err := g.fieldsOf(t)
	if err != nil {
		return err
	}

	fmt.Fprintf(&g.buf, "export interface %s {\n", name)
	for _, f := range fields {
		g.buf.WriteString(f)
	}
	g.buf.WriteString("}\n\n")
	return nil
}

// fieldsOf renders a struct's JSON-visible fields.
//
// Embedded structs are flattened, because that is what encoding/json does and
// the console has to match the wire rather than the Go declaration.
func (g *generator) fieldsOf(t reflect.Type) ([]string, error) {
	var out []string

	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}

		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")

		if f.Anonymous && name == "" {
			nested, err := g.fieldsOf(deref(f.Type))
			if err != nil {
				return nil, err
			}
			out = append(out, nested...)
			continue
		}
		if name == "" {
			name = f.Name
		}

		ts, err := g.tsType(f.Type)
		if err != nil {
			return nil, fmt.Errorf("%s.%s: %w", t.Name(), f.Name, err)
		}

		// A pointer, an omitempty or an omitzero field may be absent from the
		// JSON, and the console must be made to handle that rather than
		// trusting it. omitzero is how a time.Time is left out: omitempty
		// never omits a struct.
		optional := ""
		if strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero") || f.Type.Kind() == reflect.Pointer {
			optional = "?"
		}

		out = append(out, fmt.Sprintf("  %s%s: %s;\n", name, optional, ts))
	}
	return out, nil
}

func (g *generator) tsType(t reflect.Type) (string, error) {
	// Time is RFC 3339 on the wire (design 00 §3.5), so it is a string here and
	// the console parses it deliberately rather than assuming a Date.
	if deref(t) == reflect.TypeOf(time.Time{}) {
		return "string", nil
	}

	switch t.Kind() {
	case reflect.Pointer:
		return g.tsType(t.Elem())

	case reflect.String:
		return "string", nil

	case reflect.Bool:
		return "boolean", nil

	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return "number", nil

	case reflect.Slice, reflect.Array:
		// []byte is base64 in JSON; json.RawMessage is arbitrary JSON. Both are
		// opaque to the console, and `unknown` forces it to say so.
		if t.Elem().Kind() == reflect.Uint8 {
			return "unknown", nil
		}
		inner, err := g.tsType(t.Elem())
		if err != nil {
			return "", err
		}
		// A nil slice marshals as null, not [].
		return "(" + inner + "[] | null)", nil

	case reflect.Map:
		key, err := g.tsType(t.Key())
		if err != nil {
			return "", err
		}
		value, err := g.tsType(t.Elem())
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("(Record<%s, %s> | null)", key, value), nil

	case reflect.Struct:
		g.enqueue(t)
		return tsName(t), nil

	case reflect.Interface:
		return "unknown", nil

	default:
		return "", fmt.Errorf("no TypeScript equivalent for %s", t.Kind())
	}
}

func deref(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t
}

var _ = sort.Strings
