package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/types"
	"reflect"
	"strings"
	"unicode"
)

// Schema is a JSON Schema as OpenAPI 3.1 has it: the subset this generator
// writes, in the order a reader expects the keys.
type Schema struct {
	// Ref is a component's internal key while the document is assembled, and
	// "#/components/schemas/<name>" once names are settled.
	Ref                  string    `json:"$ref,omitempty"`
	Type                 any       `json:"type,omitempty"`
	Format               string    `json:"format,omitempty"`
	ContentEncoding      string    `json:"contentEncoding,omitempty"`
	Description          string    `json:"description,omitempty"`
	Enum                 []string  `json:"enum,omitempty"`
	Items                *Schema   `json:"items,omitempty"`
	Properties           Props     `json:"properties,omitempty"`
	Required             []string  `json:"required,omitempty"`
	AdditionalProperties *Schema   `json:"additionalProperties,omitempty"`
	AnyOf                []*Schema `json:"anyOf,omitempty"`
	OneOf                []*Schema `json:"oneOf,omitempty"`

	// empty marks an empty list literal, which a union drops when it has
	// anything else to say what the list holds.
	empty bool
}

// Prop is one property, kept in declaration order.
type Prop struct {
	Name   string
	Schema *Schema
}

// Props marshals as a JSON object in the order the fields are declared, which
// is the order the Go struct writes them and the order a person reads them in.
type Props []Prop

// MarshalJSON writes the properties as an object, in order.
func (p Props) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, prop := range p {
		if i > 0 {
			buf.WriteByte(',')
		}
		k, err := marshalPlain(prop.Name)
		if err != nil {
			return nil, err
		}
		buf.Write(k)
		buf.WriteByte(':')
		v, err := marshalPlain(prop.Schema)
		if err != nil {
			return nil, err
		}
		buf.Write(v)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalPlain is json.Marshal without HTML escaping, as the document itself
// is written: a description's `<CODE>` stays readable.
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// anySchema is the schema that accepts anything: arbitrary JSON.
func anySchema() *Schema { return &Schema{} }

// nullable widens a schema to also accept null, which is what encoding/json
// writes for a nil pointer, slice or map.
func nullable(s *Schema) *Schema {
	switch t := s.Type.(type) {
	case string:
		c := *s
		c.Type = []string{t, "null"}
		return &c
	case []string:
		return s
	}
	if s.Ref == "" && s.Type == nil && len(s.AnyOf) == 0 && len(s.OneOf) == 0 {
		return s // already anything
	}
	return &Schema{AnyOf: []*Schema{s, {Type: "null"}}}
}

// component is a named Go type that became a reusable schema.
type component struct {
	tn     *types.TypeName
	key    string
	schema *Schema
}

// schemas turns Go types into JSON Schema the way encoding/json serializes
// them: the same rules cmd/gen-api-types applies to make the console's
// TypeScript, read from the compiler's view of the types rather than from
// reflection, because most of what the handlers send is a map literal or an
// anonymous struct that reflection never sees.
type schemas struct {
	byKey    map[string]*component
	problems []string

	// marshalers are the types that write their own JSON, and what they
	// write. A type with a MarshalJSON this table does not name is a problem:
	// its shape is in a function body, not in its fields.
	marshalers map[string]*Schema
}

func newSchemas() *schemas {
	return &schemas{byKey: map[string]*component{}, marshalers: marshalers()}
}

// typeKey is a named type's identity, with its package and type arguments.
func typeKey(n *types.Named) string {
	obj := n.Obj()
	key := obj.Name()
	if obj.Pkg() != nil {
		key = obj.Pkg().Path() + "." + key
	}
	if args := n.TypeArgs(); args != nil && args.Len() > 0 {
		parts := make([]string, args.Len())
		for i := range args.Len() {
			parts[i] = types.TypeString(args.At(i), nil)
		}
		key += "[" + strings.Join(parts, ",") + "]"
	}
	return key
}

func (s *schemas) problemf(format string, args ...any) {
	s.problems = append(s.problems, fmt.Sprintf(format, args...))
}

// forType is the schema for a value of type t.
func (s *schemas) forType(t types.Type) *Schema {
	if sc, ok := s.marshaled(t); ok {
		return sc
	}

	switch t := t.(type) {
	case *types.Alias:
		return s.forType(types.Unalias(t))

	case *types.Pointer:
		return nullable(s.forType(t.Elem()))

	case *types.Named:
		if _, isStruct := t.Underlying().(*types.Struct); isStruct {
			return s.named(t)
		}
		// A named string, number or list is its underlying type on the wire.
		return s.forType(t.Underlying())

	case *types.Basic:
		return basic(t)

	case *types.Slice:
		if isByte(t.Elem()) {
			return &Schema{Type: []string{"string", "null"}, ContentEncoding: "base64"}
		}
		return &Schema{Type: []string{"array", "null"}, Items: s.forType(t.Elem())}

	case *types.Array:
		if isByte(t.Elem()) {
			return &Schema{Type: "string", ContentEncoding: "base64"}
		}
		return &Schema{Type: "array", Items: s.forType(t.Elem())}

	case *types.Map:
		return &Schema{Type: []string{"object", "null"}, AdditionalProperties: s.forType(t.Elem())}

	case *types.Struct:
		return s.object(t)

	case *types.Interface:
		return anySchema()

	case *types.TypeParam:
		return anySchema()
	}

	s.problemf("%s has no JSON form", t)
	return anySchema()
}

func isByte(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Kind() == types.Byte
}

func basic(t *types.Basic) *Schema {
	switch {
	case t.Info()&types.IsString != 0:
		return &Schema{Type: "string"}
	case t.Info()&types.IsBoolean != 0:
		return &Schema{Type: "boolean"}
	case t.Info()&types.IsInteger != 0:
		switch t.Kind() {
		case types.Int64, types.Uint64, types.Int, types.Uint, types.Uintptr:
			return &Schema{Type: "integer", Format: "int64"}
		}
		return &Schema{Type: "integer", Format: "int32"}
	case t.Info()&types.IsFloat != 0:
		return &Schema{Type: "number"}
	case t.Kind() == types.UntypedNil:
		return &Schema{Type: "null"}
	}
	return anySchema()
}

// marshaled answers for a type that writes its own JSON.
func (s *schemas) marshaled(t types.Type) (*Schema, bool) {
	n := namedOf(t)
	if n == nil {
		return nil, false
	}
	key := typeKey(n)
	if sc, ok := s.marshalers[key]; ok {
		return sc, true
	}
	if hasMethod(t, "MarshalJSON") {
		s.problemf("%s writes its own JSON (MarshalJSON), so its shape is not in its fields. "+
			"Say what it writes in marshalers() in internal/openapi/overrides.go", key)
		return anySchema(), true
	}
	if hasMethod(t, "MarshalText") {
		return &Schema{Type: "string"}, true
	}
	return nil, false
}

func namedOf(t types.Type) *types.Named {
	t = types.Unalias(t)
	if p, ok := t.(*types.Pointer); ok {
		t = types.Unalias(p.Elem())
	}
	n, _ := t.(*types.Named)
	return n
}

// hasMethod reports whether t, or a pointer to it, has the method. encoding/json
// calls a pointer method on an addressable value, which every field of a
// struct it is given by pointer is.
func hasMethod(t types.Type, name string) bool {
	for _, c := range []types.Type{t, types.NewPointer(t)} {
		ms := types.NewMethodSet(c)
		for i := range ms.Len() {
			if ms.At(i).Obj().Name() == name {
				return true
			}
		}
	}
	return false
}

// named is a reference to a struct type's component, made the first time it
// is seen.
func (s *schemas) named(n *types.Named) *Schema {
	key := typeKey(n)
	if key == "time.Time" {
		return &Schema{Type: "string", Format: formatDateTime}
	}
	// A type declared inside a function is that function's shape, not a
	// name anyone else can use: it is described where it is used.
	if obj := n.Obj(); obj.Pkg() != nil && obj.Parent() != nil && obj.Parent() != obj.Pkg().Scope() {
		return s.object(n.Underlying().(*types.Struct))
	}
	if _, ok := s.byKey[key]; !ok {
		c := &component{tn: n.Obj(), key: key}
		s.byKey[key] = c // before the fields, so a recursive type ends
		c.schema = s.object(n.Underlying().(*types.Struct))
	}
	return &Schema{Ref: key}
}

// object is a struct's JSON object, field by field as encoding/json writes it.
func (s *schemas) object(st *types.Struct) *Schema {
	out := &Schema{Type: "object"}
	s.fields(st, out, map[string]bool{})
	if out.Properties == nil {
		out.Properties = Props{}
	}
	return out
}

func (s *schemas) fields(st *types.Struct, out *Schema, seen map[string]bool) {
	for i := range st.NumFields() {
		f := st.Field(i)
		tag := reflect.StructTag(st.Tag(i)).Get("json")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if est := s.flattened(f, name); est != nil {
			s.fields(est, out, seen)
			continue
		}
		if name == "" {
			name = f.Name()
		}
		if !f.Exported() || seen[name] {
			continue // the shallower field wins, as in encoding/json
		}
		seen[name] = true

		omit := strings.Contains(opts, "omitempty") || strings.Contains(opts, "omitzero")
		out.Properties = append(out.Properties, Prop{Name: name, Schema: s.field(f.Type(), opts, omit)})
		if !omit && !isPointer(f.Type()) {
			out.Required = append(out.Required, name)
		}
	}
}

// flattened is the struct an embedded field's fields are flattened from, as
// encoding/json flattens them into the outer object, exported or not; nil
// for any other field.
func (s *schemas) flattened(f *types.Var, name string) *types.Struct {
	if !f.Embedded() || name != "" {
		return nil
	}
	inner := types.Unalias(stripPointer(f.Type()))
	est, ok := inner.Underlying().(*types.Struct)
	if !ok {
		return nil
	}
	if _, own := s.marshaled(inner); own {
		return nil
	}
	return est
}

// field is one field's schema, by its type and its tag's options.
func (s *schemas) field(t types.Type, opts string, omit bool) *Schema {
	switch {
	case strings.Contains(opts, "string"):
		return &Schema{Type: "string"}
	case omit:
		// Left out when empty, so present means not null.
		return notNull(s.forType(stripPointer(t)))
	}
	return s.forType(t)
}

func stripPointer(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}

func isPointer(t types.Type) bool {
	_, ok := types.Unalias(t).(*types.Pointer)
	return ok
}

// notNull takes null back out of a schema: omitempty leaves a nil slice or
// map out rather than writing null.
func notNull(s *Schema) *Schema {
	if ts, ok := s.Type.([]string); ok && len(ts) == 2 && ts[1] == "null" {
		c := *s
		c.Type = ts[0]
		return &c
	}
	if len(s.AnyOf) == 2 && s.AnyOf[1].Type == "null" {
		return s.AnyOf[0]
	}
	return s
}

// names settles each component's name: the Go type's own, unless two types
// share it, in which case each is prefixed with its package's — so a name does
// not depend on which of the two happened to be reached first.
func (s *schemas) names() map[string]string {
	byName := map[string][]*component{}
	for _, c := range s.byKey {
		byName[baseName(c)] = append(byName[baseName(c)], c)
	}
	out := map[string]string{}
	for name, cs := range byName {
		for _, c := range cs {
			if len(cs) == 1 {
				out[c.key] = name
				continue
			}
			out[c.key] = pascal(c.tn.Pkg().Name()) + name
		}
	}
	return out
}

func baseName(c *component) string {
	name := upperFirst(c.tn.Name())
	if i := strings.IndexByte(c.key, '['); i >= 0 {
		// A generic type's name carries its arguments: Page[App] is PageApp.
		for _, part := range strings.FieldsFunc(c.key[i:], func(r rune) bool {
			return r == '[' || r == ']' || r == ','
		}) {
			name += pascal(part[strings.LastIndexByte(part, '.')+1:])
		}
	}
	return name
}

// upperFirst capitalizes an unexported type's name, so every component name
// reads as a type.
func upperFirst(s string) string {
	r := []rune(s)
	if len(r) > 0 {
		r[0] = unicode.ToUpper(r[0])
	}
	return string(r)
}

// lowerInitial lowers a name's leading capital, or its leading acronym:
// SSOComplete is ssoComplete, ListApps is listApps.
func lowerInitial(name string) string {
	r := []rune(name)
	n := 0
	for n < len(r) && unicode.IsUpper(r[n]) {
		n++
	}
	if n > 1 && n < len(r) {
		n-- // the last capital starts the next word
	}
	for i := range n {
		r[i] = unicode.ToLower(r[i])
	}
	return string(r)
}

func pascal(s string) string {
	var b strings.Builder
	up := true
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			up = true
			continue
		}
		if up {
			b.WriteRune(unicode.ToUpper(r))
			up = false
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// walk visits every schema under s, s included.
func walk(s *Schema, visit func(*Schema)) {
	if s == nil {
		return
	}
	visit(s)
	walk(s.Items, visit)
	walk(s.AdditionalProperties, visit)
	for _, p := range s.Properties {
		walk(p.Schema, visit)
	}
	for _, c := range s.AnyOf {
		walk(c, visit)
	}
	for _, c := range s.OneOf {
		walk(c, visit)
	}
}
