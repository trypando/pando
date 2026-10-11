package openapi

import (
	"encoding/json"
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	"slices"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/types/typeutil"
)

// The HTTP layer's package, read as source. Its handlers are what the
// document describes, so they are what the document is made from.
const httpapiPath = "github.com/trypando/pando/internal/httpapi"

const (
	jsonType = "application/json"
	scimType = "application/scim+json"
)

// served is one route the router registers, with the code that answers it.
type served struct {
	method, path string
	handler      ast.Expr
}

// response is one answer a handler can give.
type response struct {
	status      int
	contentType string  // empty for no body
	schema      *Schema // nil for no body
	redirect    bool

	// unread says why the body could not be read from the code. An
	// override for the status answers it; nothing else does.
	unread string
}

// param is a query or header parameter a handler reads.
type param struct {
	name, in string
	schema   *Schema
}

// operation is what reading one handler found.
type operation struct {
	handlerName string

	request            *Schema
	requestContentType string
	rawBody            bool // reads r.Body other than as JSON
	bodyReplaced       bool // sets r.Body to one it made, for a handler it calls

	responses []response
	params    map[string]*param // keyed in+name

	errors     bool // answers with the error envelope
	scimErrors bool // answers with a SCIM error

	problems []string
	unread   string // set by shape for the response being read
}

func (op *operation) problemf(format string, args ...any) {
	op.problems = append(op.problems, fmt.Sprintf(format, args...))
}

// analysis reads the httpapi package.
type analysis struct {
	pkg   *packages.Package
	info  *types.Info
	funcs map[*types.Func]*ast.FuncDecl
	sch   *schemas
}

func load(path string) (*analysis, error) {
	cfg := &packages.Config{
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax |
			packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
	}
	pkgs, err := packages.Load(cfg, path)
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}
	if len(pkgs) != 1 {
		return nil, fmt.Errorf("loading %s found %d packages", path, len(pkgs))
	}
	pkg := pkgs[0]
	if len(pkg.Errors) > 0 {
		return nil, fmt.Errorf("loading %s: %w", path, pkg.Errors[0])
	}

	a := &analysis{pkg: pkg, info: pkg.TypesInfo, funcs: map[*types.Func]*ast.FuncDecl{}, sch: newSchemas()}
	for _, f := range pkg.Syntax {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil {
				if fn, ok := a.info.Defs[fd.Name].(*types.Func); ok {
					a.funcs[fn] = fd
				}
			}
		}
	}
	return a, nil
}

// lookupType finds a named type in httpapi or anything it imports.
func (a *analysis) lookupType(pkgPath, name string) types.Type {
	seen := map[*types.Package]bool{}
	var find func(p *types.Package) types.Type
	find = func(p *types.Package) types.Type {
		if seen[p] {
			return nil
		}
		seen[p] = true
		if p.Path() == pkgPath {
			if obj := p.Scope().Lookup(name); obj != nil {
				return obj.Type()
			}
			return nil
		}
		for _, imp := range p.Imports() {
			if t := find(imp); t != nil {
				return t
			}
		}
		return nil
	}
	return find(a.pkg.Types)
}

// --- the router ---------------------------------------------------------------

// routes walks Server.Routes the way chi will: each Route call adds its prefix
// to everything registered inside it.
func (a *analysis) routes() ([]served, error) {
	var root *ast.FuncDecl
	for fn, decl := range a.funcs {
		if fn.Name() == "Routes" && isServerMethod(fn) {
			root = decl
		}
	}
	if root == nil {
		return nil, fmt.Errorf("%s has no (*Server).Routes", a.pkg.PkgPath)
	}
	var out []served
	a.walkRouter(root.Body, "", &out)
	return out, nil
}

func isServerMethod(fn *types.Func) bool {
	sig, ok := fn.Type().(*types.Signature)
	if !ok || sig.Recv() == nil {
		return false
	}
	return strings.HasSuffix(types.TypeString(sig.Recv().Type(), nil), ".Server")
}

func (a *analysis) walkRouter(body ast.Node, prefix string, out *[]served) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !strings.Contains(types.TypeString(a.info.TypeOf(sel.X), nil), "github.com/go-chi/chi") {
			return true
		}
		switch sel.Sel.Name {
		case "Route":
			p, ok := a.constString(call.Args[0])
			if !ok {
				return false
			}
			switch next := call.Args[1].(type) {
			case *ast.FuncLit:
				a.walkRouter(next.Body, prefix+p, out)
			case *ast.SelectorExpr:
				if fn, ok := a.info.Uses[next.Sel].(*types.Func); ok && a.funcs[fn] != nil {
					a.walkRouter(a.funcs[fn].Body, prefix+p, out)
				}
			}
			return false
		case "Get", "Post", "Put", "Patch", "Delete":
			p, ok := a.constString(call.Args[0])
			if !ok {
				return false
			}
			*out = append(*out, served{
				method:  strings.ToUpper(sel.Sel.Name),
				path:    normalize(prefix + p),
				handler: call.Args[1],
			})
			return false
		}
		return true
	})
}

func normalize(path string) string {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		return strings.TrimSuffix(path, "/")
	}
	return path
}

// --- handlers -----------------------------------------------------------------

// frame is one function body being read, with what its parameters were given
// by the call that led here.
type frame struct {
	node  ast.Node // *ast.FuncDecl or *ast.FuncLit
	body  *ast.BlockStmt
	bind  map[types.Object]bound
	stack map[ast.Node]bool

	contentType string // the response Content-Type set so far, "?" when not a constant
}

type bound struct {
	e ast.Expr
	f *frame
}

// handlerFrames are the bodies that answer a route, and the name to call it by.
func (a *analysis) handlerFrames(h ast.Expr) ([]*frame, string) {
	switch h := h.(type) {
	case *ast.SelectorExpr:
		if fn, ok := a.info.Uses[h.Sel].(*types.Func); ok && a.funcs[fn] != nil {
			return []*frame{a.newFrame(a.funcs[fn], a.funcs[fn].Body, nil)}, fn.Name()
		}
	case *ast.Ident:
		if fn, ok := a.info.Uses[h].(*types.Func); ok && a.funcs[fn] != nil {
			return []*frame{a.newFrame(a.funcs[fn], a.funcs[fn].Body, nil)}, fn.Name()
		}
	case *ast.FuncLit:
		return []*frame{a.newFrame(h, h.Body, nil)}, ""
	case *ast.CallExpr:
		// A wrapper given the handler, as s.scim(func ...) is.
		var out []*frame
		for _, arg := range h.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				out = append(out, a.newFrame(lit, lit.Body, nil))
			}
		}
		return out, ""
	}
	return nil, ""
}

func (a *analysis) newFrame(node ast.Node, body *ast.BlockStmt, parent *frame) *frame {
	f := &frame{node: node, body: body, bind: map[types.Object]bound{}, stack: map[ast.Node]bool{node: true}}
	if parent != nil {
		for n := range parent.stack {
			f.stack[n] = true
		}
	}
	return f
}

// helpers the reading does not follow: the error envelope is described once,
// and a replayed idempotent response is the response the handler wrote the
// first time.
var notFollowed = map[string]bool{
	"httpapi.Error":              true,
	"httpapi.scimFail":           true,
	"httpapi.JSON":               true,
	"httpapi.scimJSON":           true,
	"(*httpapi.Server).replayed": true,
	"(*httpapi.Server).remember": true,
}

func (a *analysis) analyze(h ast.Expr) *operation {
	op := &operation{params: map[string]*param{}}
	frames, name := a.handlerFrames(h)
	op.handlerName = name
	if len(frames) == 0 {
		op.problemf("the handler is not a function this generator can read")
		return op
	}
	for _, f := range frames {
		a.visit(f, op)
	}
	return op
}

func (a *analysis) visit(f *frame, op *operation) {
	consumed := map[ast.Node]bool{} // r.Body read as JSON

	ast.Inspect(f.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			return a.call(n, f, op, consumed)

		case *ast.IndexExpr:
			// q["id"], where q is the request's query.
			if isNamed(a.info.TypeOf(n.X), "net/url", "Values") && a.fromQuery(n.X, f) {
				if k, ok := a.constString(n.Index); ok {
					op.addParam(k, "query", a.queryParamSchema(n, f, true))
				}
			}

		case *ast.AssignStmt:
			// r.Body = ...: a body the handler made itself, for a handler
			// it calls (rollback is a deploy), unless it wraps the one sent.
			for i, l := range n.Lhs {
				sel, ok := l.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Body" || !isRequest(a.info.TypeOf(sel.X)) {
					continue
				}
				consumed[sel] = true
				if i < len(n.Rhs) && !a.readsBody(n.Rhs[i]) {
					op.bodyReplaced = true
				}
			}

		case *ast.SelectorExpr:
			if n.Sel.Name == "Body" && isRequest(a.info.TypeOf(n.X)) && !consumed[n] && !op.bodyReplaced {
				op.rawBody = true
			}
		}
		return true
	})
}

func (op *operation) addParam(name, in string, s *Schema) {
	key := in + " " + name
	if _, ok := op.params[key]; !ok {
		op.params[key] = &param{name: name, in: in, schema: s}
	}
}

func (a *analysis) call(call *ast.CallExpr, f *frame, op *operation, consumed map[ast.Node]bool) bool {
	fn, _ := typeutil.Callee(a.info, call).(*types.Func)
	if fn == nil {
		return true
	}
	full := fn.FullName()
	if fn.Pkg() != nil && fn.Pkg().Path() == a.pkg.PkgPath {
		// The API package's own functions, by the names its source gives them.
		full = strings.Replace(full, a.pkg.PkgPath, "httpapi", 1)
	}

	switch full {
	case "httpapi.JSON":
		a.respond(call.Args[1], call.Args[2], jsonType, f, op)
		return true // the body may read the query too
	case "httpapi.scimJSON":
		a.respond(call.Args[1], call.Args[2], scimType, f, op)
		return true
	case "httpapi.Error":
		op.errors = true
		return false
	case "httpapi.scimFail":
		op.scimErrors = true
		return false
	case "httpapi.decodeWithKey":
		// Reads the body as JSON into its second argument, and takes an
		// idempotency key from the header or the body.
		a.requestBody(call.Args[1], f, op)
		op.addParam("Idempotency-Key", "header", &Schema{
			Type: "string",
			Description: "Makes a retry of this request return the first response rather than act twice. " +
				"Also accepted as `idempotency_key` in the body.",
		})
		return false

	case "(*encoding/json.Decoder).Decode":
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if dec, ok := unparen(sel.X).(*ast.CallExpr); ok && len(dec.Args) == 1 {
			if body := a.requestBodyExpr(dec.Args[0], f); body != nil {
				consumed[body] = true
				a.requestBody(call.Args[0], f, op)
			}
		}
		return true

	case "net/http.Redirect":
		for _, st := range a.constInts(call.Args[3], f, op) {
			op.responses = append(op.responses, response{status: st, redirect: true})
		}
		return true

	case "github.com/coder/websocket.Accept":
		op.responses = append(op.responses, response{status: 101})
		return true

	case "(net/url.Values).Get", "(net/url.Values).Has":
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && a.fromQuery(sel.X, f) {
			if k, ok := a.constString(call.Args[0]); ok {
				op.addParam(k, "query", a.queryParamSchema(call, f, fn.Name() == "Has"))
			}
		}
		return true

	case "(*net/http.Request).FormValue":
		if k, ok := a.constString(call.Args[0]); ok {
			op.addParam(k, "query", a.queryParamSchema(call, f, false))
		}
		return true

	case "(net/http.Header).Get", "(net/http.Header).Values":
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if hs, ok := unparen(sel.X).(*ast.SelectorExpr); ok && hs.Sel.Name == "Header" && isRequest(a.info.TypeOf(hs.X)) {
			if k, ok := a.constString(call.Args[0]); ok && !ignoredHeaders[strings.ToLower(k)] {
				op.addParam(k, "header", &Schema{Type: "string"})
			}
		}
		return true

	case "(net/http.Header).Set":
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !a.responseHeader(sel.X, f) {
			return true
		}
		if k, ok := a.constString(call.Args[0]); ok && strings.EqualFold(k, "Content-Type") {
			if v, ok := a.constString(call.Args[1]); ok {
				f.contentType = v
			} else {
				f.contentType = "?"
			}
		}
		return true

	case "(net/http.ResponseWriter).WriteHeader":
		ct := f.contentType
		for _, st := range a.constInts(call.Args[0], f, op) {
			switch {
			case ct == "":
				op.responses = append(op.responses, response{status: st})
			case ct == "?" || strings.HasPrefix(ct, jsonType):
				op.responses = append(op.responses, response{status: st, unread: fmt.Sprintf("writes a %d response whose body this generator cannot read (Content-Type %s)", st, ct)})
			default:
				op.responses = append(op.responses, response{status: st, contentType: ct, schema: rawSchema(ct)})
			}
		}
		return true
	}

	if notFollowed[full] {
		return false
	}

	// A helper in this package given the request or the response writer is
	// part of the handler: follow it, with its parameters bound to what the
	// handler passed.
	decl := a.funcs[fn]
	if decl == nil || f.stack[decl] || len(f.stack) > 8 {
		return true
	}
	passes := false
	for _, arg := range call.Args {
		if t := a.info.TypeOf(arg); isRequest(t) || isWriter(t) {
			passes = true
		}
	}
	if !passes {
		return true
	}
	callee := a.newFrame(decl, decl.Body, f)
	a.bindParams(decl, call, f, callee)
	a.visit(callee, op)
	return true
}

func (a *analysis) bindParams(decl *ast.FuncDecl, call *ast.CallExpr, caller, callee *frame) {
	i := 0
	for _, field := range decl.Type.Params.List {
		for _, name := range field.Names {
			if i < len(call.Args) {
				if obj := a.info.Defs[name]; obj != nil {
					callee.bind[obj] = bound{e: call.Args[i], f: caller}
				}
			}
			i++
		}
		if len(field.Names) == 0 {
			i++
		}
	}
}

// Headers that are not the API's own parameters: credentials, which the
// security schemes describe, and transport.
var ignoredHeaders = map[string]bool{
	"authorization": true, "cookie": true, "content-type": true, "origin": true,
	"referer": true, "user-agent": true, "host": true, "x-forwarded-for": true,
	"x-forwarded-proto": true, "x-forwarded-host": true, "sec-fetch-site": true,

	// One replica passing a request to another (relay.go). Not for clients.
	"pando-replica-relay": true,
}

func rawSchema(contentType string) *Schema {
	if strings.HasPrefix(contentType, "text/") {
		return &Schema{Type: "string"}
	}
	return &Schema{Type: "string", ContentEncoding: "binary"}
}

// requestBodyExpr is the r.Body a decoder reads, directly or through a size
// limit, or nil when it reads something else.
func (a *analysis) requestBodyExpr(e ast.Expr, f *frame) ast.Node {
	e, _ = a.resolve(e, f)
	switch e := unparen(e).(type) {
	case *ast.SelectorExpr:
		if e.Sel.Name == "Body" && isRequest(a.info.TypeOf(e.X)) {
			return e
		}
	case *ast.CallExpr:
		for _, arg := range e.Args {
			if s, ok := unparen(arg).(*ast.SelectorExpr); ok && s.Sel.Name == "Body" && isRequest(a.info.TypeOf(s.X)) {
				return s
			}
		}
	}
	return nil
}

// requestBody records the type a handler decodes the body into.
func (a *analysis) requestBody(target ast.Expr, f *frame, op *operation) {
	if op.bodyReplaced {
		return
	}
	target, _ = a.resolve(target, f)
	t := a.info.TypeOf(target)
	if t == nil {
		op.problemf("decodes the body into something whose type is unknown")
		return
	}
	if b, ok := t.(*types.Basic); ok && b.Kind() == types.UntypedNil {
		return
	}
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	if _, ok := t.Underlying().(*types.Interface); ok {
		op.problemf("decodes the body into an interface, which says nothing about its shape")
		return
	}
	s := a.sch.forType(t)
	if op.request != nil {
		if mustJSON(op.request) == mustJSON(s) {
			return
		}
		s = union([]*Schema{op.request, s})
	}
	op.request = s
	op.requestContentType = jsonType
}

// respond records one JSON response.
func (a *analysis) respond(statusExpr, bodyExpr ast.Expr, contentType string, f *frame, op *operation) {
	statuses := a.constInts(statusExpr, f, op)
	body, bf := a.resolve(bodyExpr, f)
	if id, ok := unparen(body).(*ast.Ident); ok && id.Name == "nil" && a.info.Uses[id] == types.Universe.Lookup("nil") {
		for _, st := range statuses {
			op.responses = append(op.responses, response{status: st})
		}
		return
	}
	op.unread = ""
	s := a.shape(body, bf, op, true)
	unread := op.unread
	for _, st := range statuses {
		op.responses = append(op.responses, response{status: st, contentType: contentType, schema: s, unread: unread})
	}
}

// resolve follows a parameter to the expression its caller passed.
func (a *analysis) resolve(e ast.Expr, f *frame) (ast.Expr, *frame) {
	for range 16 {
		id, ok := unparen(e).(*ast.Ident)
		if !ok {
			return e, f
		}
		obj := a.info.Uses[id]
		b, ok := f.bind[obj]
		if !ok {
			return e, f
		}
		e, f = b.e, b.f
	}
	return e, f
}

func unparen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

// constInts is every constant value an int expression can have.
func (a *analysis) constInts(e ast.Expr, f *frame, op *operation) []int {
	e, f = a.resolve(e, f)
	if tv, ok := a.info.Types[e]; ok && tv.Value != nil {
		if v, ok := constant.Int64Val(tv.Value); ok {
			return []int{int(v)}
		}
	}
	if id, ok := unparen(e).(*ast.Ident); ok {
		var out []int
		for _, def := range a.assignments(id, f) {
			out = append(out, a.constInts(def, f, op)...)
		}
		if len(out) > 0 {
			sort.Ints(out)
			return dedupeInts(out)
		}
	}
	op.problemf("answers with a status code that is not a constant: %s", exprString(e))
	return nil
}

func dedupeInts(in []int) []int {
	out := in[:0]
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}

// assignments are the expressions a local variable is given in its function.
func (a *analysis) assignments(id *ast.Ident, f *frame) []ast.Expr {
	obj := a.info.Uses[id]
	if obj == nil {
		obj = a.info.Defs[id]
	}
	if obj == nil {
		return nil
	}
	var out []ast.Expr
	ast.Inspect(f.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			if len(n.Lhs) != len(n.Rhs) {
				return true
			}
			for i, l := range n.Lhs {
				if lid, ok := l.(*ast.Ident); ok && (a.info.Defs[lid] == obj || a.info.Uses[lid] == obj) {
					out = append(out, n.Rhs[i])
				}
			}
		case *ast.ValueSpec:
			for i, name := range n.Names {
				if a.info.Defs[name] == obj && i < len(n.Values) && len(n.Names) == len(n.Values) {
					out = append(out, n.Values[i])
				}
			}
		}
		return true
	})
	return out
}

// indexAssignments are the keys set on a map variable after it is made:
// body["x"] = v.
func (a *analysis) indexAssignments(obj types.Object, f *frame) []keyAssign {
	var out []keyAssign
	// An assignment in the body itself always happens; one inside an if or
	// a loop may not, and its key is optional.
	always := map[ast.Stmt]bool{}
	for _, st := range f.body.List {
		always[st] = true
	}
	ast.Inspect(f.node, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != len(as.Rhs) {
			return true
		}
		for i, l := range as.Lhs {
			ix, ok := l.(*ast.IndexExpr)
			if !ok {
				continue
			}
			if id, ok := unparen(ix.X).(*ast.Ident); ok && a.info.Uses[id] == obj {
				if k, ok := a.constString(ix.Index); ok {
					out = append(out, keyAssign{name: k, value: as.Rhs[i], always: always[as]})
				}
			}
		}
		return true
	})
	return out
}

type keyAssign struct {
	name   string
	value  ast.Expr
	always bool
}

// shape is the schema of a value about to be serialized. Where the value is a
// map literal, its keys are the object's properties, read from the literal;
// otherwise its type says.
func (a *analysis) shape(e ast.Expr, f *frame, op *operation, top bool) *Schema {
	e, f = a.resolve(e, f)
	e = unparen(e)
	t := a.info.TypeOf(e)

	switch x := e.(type) {
	case *ast.UnaryExpr:
		if x.Op == token.AND {
			return a.shape(x.X, f, op, top)
		}

	case *ast.CompositeLit:
		if m, ok := t.Underlying().(*types.Map); ok && isString(m.Key()) {
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

	case *ast.Ident:
		if items := a.appended(x, t, f, op); items != nil {
			return items
		}
		if !loose(t) {
			break
		}
		obj := a.info.Uses[x]
		if obj == nil || obj.Parent() == a.pkg.Types.Scope() {
			break
		}
		defs := a.assignments(x, f)
		var shapes []*Schema
		for _, d := range defs {
			shapes = append(shapes, a.shape(d, f, op, top))
		}
		s := union(shapes)
		if s == nil {
			break
		}
		// Keys set afterwards, which may or may not be: optional.
		if extra := a.indexAssignments(obj, f); len(extra) > 0 && s.Type == "object" {
			c := *s
			c.Properties = append(Props{}, s.Properties...)
			for _, p := range extra {
				c.Properties = setProp(c.Properties, p.name, a.shape(p.value, f, op, false))
				if p.always {
					c.Required = appendOnce(append([]string{}, c.Required...), p.name)
				}
			}
			s = &c
		}
		return s

	case *ast.CallExpr:
		if !loose(t) {
			break
		}
		fn, _ := typeutil.Callee(a.info, x).(*types.Func)
		if fn == nil || a.funcs[fn] == nil {
			break
		}
		decl := a.funcs[fn]
		callee := a.newFrame(decl, decl.Body, f)
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
		if s := union(shapes); s != nil {
			// Keys a helper sets on its parameter before returning it, as
			// withTotal does.
			if s.Type == "object" {
				for obj := range callee.bind {
					for _, p := range a.indexAssignments(obj, callee) {
						s.Properties = setProp(s.Properties, p.name, a.shape(p.value, callee, op, false))
						if p.always {
							s.Required = appendOnce(s.Required, p.name)
						}
					}
				}
			}
			return s
		}
	}

	if t == nil {
		op.problemf("answers with %s, whose type is unknown", exprString(e))
		return anySchema()
	}
	// A struct field declared as free-form — an adapter's own details, say —
	// is described as its declaration says, as it would be inside the struct.
	if sel, ok := e.(*ast.SelectorExpr); ok && !top {
		if s := a.info.Selections[sel]; s != nil && s.Kind() == types.FieldVal {
			return a.sch.forType(t)
		}
	}
	if loose(t) || looseList(t) {
		msg := fmt.Sprintf("answers with %s, a %s whose shape is not in the code this generator reads", exprString(e), t)
		if top {
			op.unread = msg
		} else {
			op.problemf("%s", msg)
		}
		return a.sch.forType(t)
	}
	return a.sch.forType(t)
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

func setProp(props Props, name string, s *Schema) Props {
	for i, p := range props {
		if p.Name == name {
			props[i].Schema = s
			return props
		}
	}
	return append(props, Prop{Name: name, Schema: s})
}

func appendOnce(list []string, s string) []string {
	for _, v := range list {
		if v == s {
			return list
		}
	}
	return append(list, s)
}

// union is one schema for several: the one when they agree, oneOf when not.
func union(shapes []*Schema) *Schema {
	var out []*Schema
	seen := map[string]bool{}
	for _, s := range shapes {
		if s == nil {
			continue
		}
		k := mustJSON(s)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
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

// mergeObjects is one object for several map literals a handler answers with
// on different branches: every key any of them has, required where all of
// them have it.
func mergeObjects(shapes []*Schema) *Schema {
	for _, s := range shapes {
		if s.Ref != "" || s.Type != "object" || s.AdditionalProperties != nil || len(s.AnyOf) > 0 || len(s.OneOf) > 0 {
			return nil
		}
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
		all := true
		for _, s := range shapes[1:] {
			if !slices.Contains(s.Required, name) {
				all = false
			}
		}
		if all {
			out.Required = append(out.Required, name)
		}
	}
	return out
}

func mustJSON(s *Schema) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func (a *analysis) constString(e ast.Expr) (string, bool) {
	tv, ok := a.info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// fromQuery reports whether a url.Values is the request's query string.
func (a *analysis) fromQuery(e ast.Expr, f *frame) bool {
	e, f = a.resolve(e, f)
	switch x := unparen(e).(type) {
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Query" && isNamed(deref(a.info.TypeOf(sel.X)), "net/url", "URL") {
			return true
		}
	case *ast.Ident:
		for _, d := range a.assignments(x, f) {
			if a.fromQuery(d, f) {
				return true
			}
		}
	}
	return false
}

// responseHeader reports whether an http.Header is the response's.
func (a *analysis) responseHeader(e ast.Expr, f *frame) bool {
	e, f = a.resolve(e, f)
	switch x := unparen(e).(type) {
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Header" && isWriter(a.info.TypeOf(sel.X)) {
			return true
		}
	case *ast.Ident:
		for _, d := range a.assignments(x, f) {
			if a.responseHeader(d, f) {
				return true
			}
		}
	}
	return false
}

// queryParamSchema is a query parameter's type, from what the handler does
// with the value: a whole number when it is parsed as one, a boolean when it
// is compared with "true" or parsed as one, and a string otherwise.
func (a *analysis) queryParamSchema(read ast.Expr, f *frame, multi bool) *Schema {
	// The value read, or the variable it is put in.
	var uses []ast.Node
	ast.Inspect(f.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, r := range n.Rhs {
				if r == read && i < len(n.Lhs) && len(n.Lhs) == len(n.Rhs) {
					if id, ok := n.Lhs[i].(*ast.Ident); ok {
						obj := a.info.Defs[id]
						if obj == nil {
							obj = a.info.Uses[id]
						}
						uses = append(uses, a.usesOf(obj, f)...)
					}
				}
			}
		}
		return true
	})
	uses = append(uses, read)

	kind := ""
	ast.Inspect(f.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			fn, _ := typeutil.Callee(a.info, n).(*types.Func)
			if fn == nil || len(n.Args) == 0 || !containsNode(uses, n.Args[0]) {
				return true
			}
			switch fn.FullName() {
			case "strconv.Atoi", "strconv.ParseInt", "strconv.ParseUint":
				kind = "integer"
			case "strconv.ParseBool":
				kind = "boolean"
			}
		case *ast.BinaryExpr:
			if (n.Op == token.EQL || n.Op == token.NEQ) && (containsNode(uses, n.X) || containsNode(uses, n.Y)) {
				if v, ok := a.constString(n.Y); ok && (v == "true" || v == "1") {
					kind = "boolean"
				}
			}
		}
		return true
	})
	s := &Schema{Type: "string"}
	if kind != "" {
		s = &Schema{Type: kind}
	}
	if multi && isNamed(a.info.TypeOf(read), "net/url", "Values") {
		return &Schema{Type: "array", Items: s}
	}
	if _, ok := read.(*ast.IndexExpr); ok {
		return &Schema{Type: "array", Items: s}
	}
	return s
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
	e = unparen(e)
	for _, n := range list {
		if n == e {
			return true
		}
	}
	return false
}

func isRequest(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return isNamed(p.Elem(), "net/http", "Request")
	}
	return false
}

func isWriter(t types.Type) bool { return isNamed(t, "net/http", "ResponseWriter") }

func isNamed(t types.Type, pkg, name string) bool {
	if t == nil {
		return false
	}
	n, ok := types.Unalias(t).(*types.Named)
	return ok && n.Obj().Pkg() != nil && n.Obj().Pkg().Path() == pkg && n.Obj().Name() == name
}

func isString(t types.Type) bool {
	b, ok := t.Underlying().(*types.Basic)
	return ok && b.Info()&types.IsString != 0
}

func exprString(e ast.Expr) string {
	return types.ExprString(e)
}

// readsBody reports whether an expression mentions the request's body.
func (a *analysis) readsBody(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "Body" && isRequest(a.info.TypeOf(sel.X)) {
			found = true
		}
		return !found
	})
	return found
}

// looseList is a list of loose values.
func looseList(t types.Type) bool {
	switch u := types.Unalias(t).Underlying().(type) {
	case *types.Slice:
		return loose(u.Elem()) || looseList(u.Elem())
	case *types.Array:
		return loose(u.Elem()) || looseList(u.Elem())
	case *types.Pointer:
		return loose(u.Elem()) || looseList(u.Elem())
	}
	return false
}

func deref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
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
		switch d := unparen(d).(type) {
		case *ast.CallExpr:
			if b, ok := typeutil.Callee(a.info, d).(*types.Builtin); ok && b.Name() == "append" && !d.Ellipsis.IsValid() {
				for _, v := range d.Args[1:] {
					items = append(items, a.shape(v, f, op, false))
				}
			}
		case *ast.CompositeLit:
			for _, v := range d.Elts {
				items = append(items, a.shape(v, f, op, false))
			}
		}
	}
	item := union(items)
	if item == nil {
		return nil
	}
	return &Schema{Type: []string{"array", "null"}, Items: item}
}
