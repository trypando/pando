package openapi

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"slices"

	"golang.org/x/tools/go/types/typeutil"
)

// shape is the schema of a value about to be serialized. Where the value is a
// map literal, its keys are the object's properties, read from the literal;
// otherwise its type says.
func (a *analysis) shape(e ast.Expr, f *frame, op *operation, top bool) *Schema {
	e, f = a.resolve(e, f)
	e = unparen(e)
	t := a.info.TypeOf(e)

	if s := a.shapeOfExpr(e, t, f, op, top); s != nil {
		return s
	}
	if t == nil {
		op.problemf("answers with %s, whose type is unknown", exprString(e))
		return anySchema()
	}
	if !a.isField(e, top) && (loose(t) || looseList(t)) {
		msg := fmt.Sprintf("answers with %s, a %s whose shape is not in the code this generator reads", exprString(e), t)
		if top {
			op.unread = msg
		} else {
			op.problemf("%s", msg)
		}
	}
	return a.sch.forType(t)
}

// isField reports a struct field below the top of a body. One declared as
// free-form — an adapter's own details, say — is described as its
// declaration says, as it would be inside the struct.
func (a *analysis) isField(e ast.Expr, top bool) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || top {
		return false
	}
	s := a.info.Selections[sel]
	return s != nil && s.Kind() == types.FieldVal
}

// shapeOfExpr is the shape read from the expression itself, or nil when its
// type has to say.
func (a *analysis) shapeOfExpr(e ast.Expr, t types.Type, f *frame, op *operation, top bool) *Schema {
	switch x := e.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return a.shape(x.X, f, op, top)
		}
	case *ast.CompositeLit:
		return a.literal(x, t, f, op)
	case *ast.Ident:
		return a.variable(x, t, f, op, top)
	case *ast.CallExpr:
		if loose(t) {
			return a.helperResult(x, f, op, top)
		}
	}
	return nil
}

// literal is a map literal's object, key by key, or an empty list's.
func (a *analysis) literal(x *ast.CompositeLit, t types.Type, f *frame, op *operation) *Schema {
	switch u := t.Underlying().(type) {
	case *types.Map:
		if isString(u.Key()) {
			return a.mapLiteral(x, f, op)
		}
	case *types.Slice:
		if len(x.Elts) == 0 && loose(u.Elem()) {
			// []any{}: an empty list says nothing about what else the
			// handler might send in its place.
			return &Schema{Type: "array", empty: true}
		}
	}
	return nil
}

func (a *analysis) mapLiteral(x *ast.CompositeLit, f *frame, op *operation) *Schema {
	obj := &Schema{Type: "object", Properties: Props{}}
	for _, el := range x.Elts {
		kv, ok := el.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		k, ok := a.constString(kv.Key)
		if !ok {
			op.problemf("answers with a map whose key %s is not a constant", exprString(kv.Key))
			continue
		}
		obj.Properties = append(obj.Properties, Prop{Name: k, Schema: a.shape(kv.Value, f, op, false)})
		obj.Required = append(obj.Required, k)
	}
	return obj
}

// variable is a local variable's shape: the lists appended to it, or the
// values it is given and the keys set on it afterwards.
func (a *analysis) variable(x *ast.Ident, t types.Type, f *frame, op *operation, top bool) *Schema {
	if items := a.appended(x, t, f, op); items != nil {
		return items
	}
	obj := a.info.Uses[x]
	if !loose(t) || obj == nil || obj.Parent() == a.pkg.Types.Scope() {
		return nil
	}
	var shapes []*Schema
	for _, d := range a.assignments(x, f) {
		shapes = append(shapes, a.shape(d, f, op, top))
	}
	s := union(shapes)
	if s == nil || s.Type != "object" {
		return s
	}
	c := *s
	c.Properties = slices.Clone(s.Properties)
	c.Required = slices.Clone(s.Required)
	a.addAssignedKeys(&c, obj, f, op)
	return &c
}

// helperResult is what a helper in this package returns, read from its
// return statements with its parameters bound to the arguments.
func (a *analysis) helperResult(x *ast.CallExpr, f *frame, op *operation, top bool) *Schema {
	fn, _ := typeutil.Callee(a.info, x).(*types.Func)
	decl := a.funcs[fn]
	if decl == nil {
		return nil
	}
	callee := newFrame(decl, decl.Body, f)
	a.bindParams(decl, x, f, callee)
	var shapes []*Schema
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		if _, ok := n.(*ast.FuncLit); ok {
			return false
		}
		if ret, ok := n.(*ast.ReturnStmt); ok && len(ret.Results) > 0 {
			shapes = append(shapes, a.shape(ret.Results[0], callee, op, top))
		}
		return true
	})
	s := union(shapes)
	if s != nil && s.Type == "object" {
		// Keys a helper sets on its parameter before returning it, as
		// withTotal does.
		for obj := range callee.bind {
			a.addAssignedKeys(s, obj, callee, op)
		}
	}
	return s
}

// addAssignedKeys adds the keys set on a map after it is made, required when
// they are set whatever happens.
func (a *analysis) addAssignedKeys(s *Schema, obj types.Object, f *frame, op *operation) {
	for _, p := range a.indexAssignments(obj, f) {
		s.Properties = setProp(s.Properties, p.name, a.shape(p.value, f, op, false))
		if p.always && !slices.Contains(s.Required, p.name) {
			s.Required = append(s.Required, p.name)
		}
	}
}

type keyAssign struct {
	name   string
	value  ast.Expr
	always bool
}

// indexAssignments are the keys set on a map variable after it is made:
// body["x"] = v. One set in the body itself always happens; one inside an if
// or a loop may not.
func (a *analysis) indexAssignments(obj types.Object, f *frame) []keyAssign {
	var out []keyAssign
	ast.Inspect(f.node, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		always := slices.Contains(f.body.List, ast.Stmt(as))
		for i, l := range as.Lhs {
			if k, ok := a.keyOf(l, obj); ok {
				out = append(out, keyAssign{name: k, value: as.Rhs[i], always: always})
			}
		}
		return true
	})
	return out
}

// keyOf is the constant key of m["key"], where m is obj.
func (a *analysis) keyOf(l ast.Expr, obj types.Object) (string, bool) {
	ix, ok := l.(*ast.IndexExpr)
	if !ok {
		return "", false
	}
	id, ok := unparen(ix.X).(*ast.Ident)
	if !ok || a.info.Uses[id] != obj {
		return "", false
	}
	return a.constString(ix.Index)
}

// appended is a list of maps built up in the handler: its items are the map
// literals appended to it, or listed in it where it is made.
func (a *analysis) appended(id *ast.Ident, t types.Type, f *frame, op *operation) *Schema {
	if t == nil {
		return nil
	}
	sl, ok := types.Unalias(t).Underlying().(*types.Slice)
	if !ok || !loose(sl.Elem()) {
		return nil
	}
	var items []*Schema
	for _, d := range a.assignments(id, f) {
		for _, v := range a.listItems(d) {
			items = append(items, a.shape(v, f, op, false))
		}
	}
	item := union(items)
	if item == nil {
		return nil
	}
	return &Schema{Type: []string{"array", "null"}, Items: item}
}

// listItems are the values append(list, ...) adds or a list literal holds.
func (a *analysis) listItems(d ast.Expr) []ast.Expr {
	switch d := unparen(d).(type) {
	case *ast.CallExpr:
		if b, ok := typeutil.Callee(a.info, d).(*types.Builtin); ok && b.Name() == "append" && !d.Ellipsis.IsValid() {
			return d.Args[1:]
		}
	case *ast.CompositeLit:
		return d.Elts
	}
	return nil
}

// loose is a type that says nothing about its JSON: an interface, or a map of
// interfaces.
func loose(t types.Type) bool {
	if t == nil {
		return false
	}
	switch u := t.Underlying().(type) {
	case *types.Interface:
		return true
	case *types.Map:
		_, ok := u.Elem().Underlying().(*types.Interface)
		return ok
	}
	return false
}

// looseList is a list of loose values.
func looseList(t types.Type) bool {
	var elem types.Type
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Slice:
		elem = u.Elem()
	case *types.Array:
		elem = u.Elem()
	case *types.Pointer:
		elem = u.Elem()
	default:
		return false
	}
	return loose(elem) || looseList(elem)
}

func setProp(props Props, name string, s *Schema) Props {
	for i, p := range props {
		if p.Name == name {
			props[i].Schema = s
			return props
		}
	}
	return append(props, Prop{Name: name, Schema: s})
}

// union is one schema for several: the one when they agree, one merged
// object for several objects, anyOf otherwise.
func union(shapes []*Schema) *Schema {
	out := distinct(shapes)
	switch len(out) {
	case 0:
		return nil
	case 1:
		return out[0]
	}
	if merged := mergeObjects(out); merged != nil {
		return merged
	}
	// anyOf rather than oneOf: the shapes can overlap, and a value matching
	// two of them is still a value the handler sends.
	return &Schema{AnyOf: out}
}

// distinct drops nils and repeats, and an empty list when anything else is
// there to say what the list holds.
func distinct(shapes []*Schema) []*Schema {
	var out []*Schema
	seen := map[string]bool{}
	empty := false
	for _, s := range shapes {
		if s == nil || seen[mustJSON(s)] {
			continue
		}
		if s.empty {
			empty = true
			continue
		}
		seen[mustJSON(s)] = true
		out = append(out, s)
	}
	if len(out) == 0 && empty {
		return []*Schema{{Type: "array"}}
	}
	return out
}

// mergeObjects is one object for several map literals a handler answers with
// on different branches: every key any of them has, required where all of
// them have it.
func mergeObjects(shapes []*Schema) *Schema {
	if slices.ContainsFunc(shapes, func(s *Schema) bool { return !isPlainObject(s) }) {
		return nil
	}
	out := &Schema{Type: "object", Properties: Props{}}
	byName := map[string][]*Schema{}
	for _, s := range shapes {
		for _, p := range s.Properties {
			if _, ok := byName[p.Name]; !ok {
				out.Properties = append(out.Properties, Prop{Name: p.Name})
			}
			byName[p.Name] = append(byName[p.Name], p.Schema)
		}
	}
	for i, p := range out.Properties {
		out.Properties[i].Schema = union(byName[p.Name])
	}
	for _, name := range shapes[0].Required {
		if !slices.ContainsFunc(shapes[1:], func(s *Schema) bool { return !slices.Contains(s.Required, name) }) {
			out.Required = append(out.Required, name)
		}
	}
	return out
}

func isPlainObject(s *Schema) bool {
	return s.Ref == "" && s.Type == "object" && s.AdditionalProperties == nil && len(s.AnyOf) == 0 && len(s.OneOf) == 0
}

func mustJSON(s *Schema) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// queryParamSchema is a query parameter's type, from what the handler does
// with the value: a whole number when it is parsed as one, a boolean when it
// is compared with "true" or parsed as one, and a string otherwise.
func (a *analysis) queryParamSchema(read ast.Expr, f *frame, multi bool) *Schema {
	uses := append(a.usesOfResult(read, f), read)
	s := &Schema{Type: "string"}
	if kind := a.parsedAs(uses, f); kind != "" {
		s = &Schema{Type: kind}
	}
	_, indexed := read.(*ast.IndexExpr)
	if indexed || (multi && isNamed(a.info.TypeOf(read), pkgURL, "Values")) {
		return &Schema{Type: "array", Items: s}
	}
	return s
}

// usesOfResult are the uses of the variable an expression is put in.
func (a *analysis) usesOfResult(read ast.Expr, f *frame) []ast.Node {
	var uses []ast.Node
	ast.Inspect(f.node, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, r := range as.Rhs {
			if id, ok := as.Lhs[i].(*ast.Ident); ok && r == read {
				uses = append(uses, a.usesOf(a.objectOf(id), f)...)
			}
		}
		return true
	})
	return uses
}

// parsedAs is what a value is parsed as: "integer", "boolean" or "".
func (a *analysis) parsedAs(uses []ast.Node, f *frame) string {
	kind := ""
	ast.Inspect(f.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			if k := a.parsedBy(n, uses); k != "" {
				kind = k
			}
		case *ast.BinaryExpr:
			if a.comparedWithTrue(n, uses) {
				kind = "boolean"
			}
		}
		return true
	})
	return kind
}

var parsers = map[string]string{
	"strconv.Atoi": "integer", "strconv.ParseInt": "integer", "strconv.ParseUint": "integer",
	"strconv.ParseBool": "boolean",
}

func (a *analysis) parsedBy(n *ast.CallExpr, uses []ast.Node) string {
	fn, _ := typeutil.Callee(a.info, n).(*types.Func)
	if fn == nil || len(n.Args) == 0 || !containsNode(uses, n.Args[0]) {
		return ""
	}
	return parsers[fn.FullName()]
}

func (a *analysis) comparedWithTrue(n *ast.BinaryExpr, uses []ast.Node) bool {
	if n.Op != token.EQL && n.Op != token.NEQ {
		return false
	}
	if !containsNode(uses, n.X) && !containsNode(uses, n.Y) {
		return false
	}
	v, ok := a.constString(n.Y)
	return ok && (v == "true" || v == "1")
}

func (a *analysis) usesOf(obj types.Object, f *frame) []ast.Node {
	if obj == nil {
		return nil
	}
	var out []ast.Node
	ast.Inspect(f.node, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok && a.info.Uses[id] == obj {
			out = append(out, id)
		}
		return true
	})
	return out
}

func containsNode(list []ast.Node, e ast.Expr) bool {
	return slices.Contains(list, ast.Node(unparen(e)))
}
