package openapi

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/types"
	"slices"
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

	pkgHTTP = "net/http"
	pkgURL  = "net/url"
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

	scimErrors bool // answers with a SCIM error

	problems []string
	unread   string // set by shape for the response being read
}

func (op *operation) problemf(format string, args ...any) {
	op.problems = append(op.problems, fmt.Sprintf(format, args...))
}

func (op *operation) addParam(name, in string, s *Schema) {
	key := in + " " + name
	if _, ok := op.params[key]; !ok {
		op.params[key] = &param{name: name, in: in, schema: s}
	}
}

// analysis reads the httpapi package.
type analysis struct {
	pkg   *packages.Package
	info  *types.Info
	funcs map[*types.Func]*ast.FuncDecl
	sch   *schemas

	errorName string // the error envelope's component, once names are settled
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
			a.index(d)
		}
	}
	return a, nil
}

// index remembers a function declaration, so a call to it can be followed.
func (a *analysis) index(d ast.Decl) {
	fd, ok := d.(*ast.FuncDecl)
	if !ok || fd.Body == nil {
		return
	}
	if fn, ok := a.info.Defs[fd.Name].(*types.Func); ok {
		a.funcs[fn] = fd
	}
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
			return typeIn(p, name)
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

func typeIn(p *types.Package, name string) types.Type {
	if obj := p.Scope().Lookup(name); obj != nil {
		return obj.Type()
	}
	return nil
}

// funcOf is the function a call or a function value names, when its body is
// in the package being read.
func (a *analysis) funcOf(id *ast.Ident) (*types.Func, *ast.FuncDecl) {
	fn, ok := a.info.Uses[id].(*types.Func)
	if !ok {
		return nil, nil
	}
	return fn, a.funcs[fn]
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

var routeMethods = map[string]bool{"Get": true, "Post": true, "Put": true, "Patch": true, "Delete": true}

func (a *analysis) walkRouter(body ast.Node, prefix string, out *[]served) {
	ast.Inspect(body, func(n ast.Node) bool {
		call, sel := a.routerCall(n)
		if call == nil {
			return true
		}
		p, ok := a.constString(call.Args[0])
		switch {
		case !ok:
		case sel == "Route":
			a.walkSubrouter(call.Args[1], prefix+p, out)
		case routeMethods[sel]:
			*out = append(*out, served{method: strings.ToUpper(sel), path: normalize(prefix + p), handler: call.Args[1]})
		default:
			return true
		}
		return false
	})
}

// routerCall is a call of a chi router's method, and the method's name.
func (a *analysis) routerCall(n ast.Node) (*ast.CallExpr, string) {
	call, ok := n.(*ast.CallExpr)
	if !ok || len(call.Args) < 2 {
		return nil, ""
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || !strings.Contains(types.TypeString(a.info.TypeOf(sel.X), nil), "github.com/go-chi/chi") {
		return nil, ""
	}
	return call, sel.Sel.Name
}

func (a *analysis) walkSubrouter(next ast.Expr, prefix string, out *[]served) {
	switch next := next.(type) {
	case *ast.FuncLit:
		a.walkRouter(next.Body, prefix, out)
	case *ast.SelectorExpr:
		if _, decl := a.funcOf(next.Sel); decl != nil {
			a.walkRouter(decl.Body, prefix, out)
		}
	}
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
	var id *ast.Ident
	switch h := h.(type) {
	case *ast.SelectorExpr:
		id = h.Sel
	case *ast.Ident:
		id = h
	case *ast.FuncLit:
		return []*frame{newFrame(h, h.Body, nil)}, ""
	case *ast.CallExpr:
		// A wrapper given the handler, as s.scim(func ...) is.
		var out []*frame
		for _, arg := range h.Args {
			if lit, ok := arg.(*ast.FuncLit); ok {
				out = append(out, newFrame(lit, lit.Body, nil))
			}
		}
		return out, ""
	}
	if fn, decl := a.funcOf(id); decl != nil {
		return []*frame{newFrame(decl, decl.Body, nil)}, fn.Name()
	}
	return nil, ""
}

func newFrame(node ast.Node, body *ast.BlockStmt, parent *frame) *frame {
	f := &frame{node: node, body: body, bind: map[types.Object]bound{}, stack: map[ast.Node]bool{node: true}}
	if parent != nil {
		for n := range parent.stack {
			f.stack[n] = true
		}
	}
	return f
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

// visit reads one function body.
func (a *analysis) visit(f *frame, op *operation) {
	consumed := map[ast.Node]bool{} // r.Body read as JSON

	ast.Inspect(f.body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.CallExpr:
			return a.call(n, f, op, consumed)
		case *ast.IndexExpr:
			a.queryIndex(n, f, op)
		case *ast.AssignStmt:
			a.bodyAssignment(n, op, consumed)
		case *ast.SelectorExpr:
			if a.isBody(n) && !consumed[n] && !op.bodyReplaced {
				op.rawBody = true
			}
		}
		return true
	})
}

// queryIndex reads q["id"], where q is the request's query.
func (a *analysis) queryIndex(n *ast.IndexExpr, f *frame, op *operation) {
	if !isNamed(a.info.TypeOf(n.X), pkgURL, "Values") || !a.fromQuery(n.X, f) {
		return
	}
	if k, ok := a.constString(n.Index); ok {
		op.addParam(k, "query", a.queryParamSchema(n, f, true))
	}
}

// bodyAssignment reads r.Body = ...: a body the handler made itself, for a
// handler it calls (rollback is a deploy), unless it wraps the one sent.
func (a *analysis) bodyAssignment(n *ast.AssignStmt, op *operation, consumed map[ast.Node]bool) {
	for i, l := range n.Lhs {
		sel, ok := l.(*ast.SelectorExpr)
		if !ok || !a.isBody(sel) {
			continue
		}
		consumed[sel] = true
		if i < len(n.Rhs) && !a.readsBody(n.Rhs[i]) {
			op.bodyReplaced = true
		}
	}
}

// isBody reports whether a selector is a request's Body.
func (a *analysis) isBody(sel *ast.SelectorExpr) bool {
	return sel.Sel.Name == "Body" && isRequest(a.info.TypeOf(sel.X))
}

// readsBody reports whether an expression mentions the request's body.
func (a *analysis) readsBody(e ast.Expr) bool {
	found := false
	ast.Inspect(e, func(n ast.Node) bool {
		if sel, ok := n.(*ast.SelectorExpr); ok && a.isBody(sel) {
			found = true
		}
		return !found
	})
	return found
}

// callSite is one call being read.
type callSite struct {
	call     *ast.CallExpr
	fn       *types.Func
	f        *frame
	op       *operation
	consumed map[ast.Node]bool
}

// A reader handles a call to one function, and says whether to read the
// call's arguments too.
type reader func(a *analysis, c callSite) bool

// readers are the calls that say something about a request or a response,
// by the function called. The API package's own functions are named as
// "httpapi.<name>" whatever package is being read.
var readers = map[string]reader{
	"httpapi.JSON":     func(a *analysis, c callSite) bool { return a.respondCall(c, jsonType) },
	"httpapi.scimJSON": func(a *analysis, c callSite) bool { return a.respondCall(c, scimType) },
	// The error envelope is described once, for every operation.
	"httpapi.Error":    func(*analysis, callSite) bool { return false },
	"httpapi.scimFail": func(_ *analysis, c callSite) bool { c.op.scimErrors = true; return false },
	// A replayed idempotent response is the response the handler wrote the
	// first time.
	"(*httpapi.Server).replayed": func(*analysis, callSite) bool { return false },
	"(*httpapi.Server).remember": func(*analysis, callSite) bool { return false },

	"httpapi.decodeWithKey":                 (*analysis).decodeWithKey,
	"(*encoding/json.Decoder).Decode":       (*analysis).decode,
	"net/http.Redirect":                     (*analysis).redirect,
	"github.com/coder/websocket.Accept":     (*analysis).websocket,
	"(net/url.Values).Get":                  (*analysis).queryGet,
	"(net/url.Values).Has":                  (*analysis).queryGet,
	"(*net/http.Request).FormValue":         (*analysis).formValue,
	"(net/http.Header).Get":                 (*analysis).headerGet,
	"(net/http.Header).Values":              (*analysis).headerGet,
	"(net/http.Header).Set":                 (*analysis).headerSet,
	"(net/http.ResponseWriter).WriteHeader": (*analysis).writeHeader,
}

func (a *analysis) call(call *ast.CallExpr, f *frame, op *operation, consumed map[ast.Node]bool) bool {
	fn, _ := typeutil.Callee(a.info, call).(*types.Func)
	if fn == nil {
		return true
	}
	full := fn.FullName()
	if fn.Pkg() != nil && fn.Pkg().Path() == a.pkg.PkgPath {
		full = strings.Replace(full, a.pkg.PkgPath, "httpapi", 1)
	}
	c := callSite{call: call, fn: fn, f: f, op: op, consumed: consumed}
	if read, ok := readers[full]; ok {
		return read(a, c)
	}
	a.follow(c)
	return true
}

// follow reads a helper in this package that is given the request or the
// response writer, as part of the handler, with its parameters bound to what
// the handler passed.
func (a *analysis) follow(c callSite) {
	decl := a.funcs[c.fn]
	if decl == nil || c.f.stack[decl] || len(c.f.stack) > 8 || !a.passesRequest(c.call) {
		return
	}
	callee := newFrame(decl, decl.Body, c.f)
	a.bindParams(decl, c.call, c.f, callee)
	a.visit(callee, c.op)
}

func (a *analysis) passesRequest(call *ast.CallExpr) bool {
	return slices.ContainsFunc(call.Args, func(arg ast.Expr) bool {
		t := a.info.TypeOf(arg)
		return isRequest(t) || isWriter(t)
	})
}

func (a *analysis) bindParams(decl *ast.FuncDecl, call *ast.CallExpr, caller, callee *frame) {
	var names []*ast.Ident
	for _, field := range decl.Type.Params.List {
		if len(field.Names) == 0 {
			names = append(names, nil)
		}
		names = append(names, field.Names...)
	}
	for i, name := range names {
		if i >= len(call.Args) || name == nil {
			continue
		}
		if obj := a.info.Defs[name]; obj != nil {
			callee.bind[obj] = bound{e: call.Args[i], f: caller}
		}
	}
}

func (a *analysis) respondCall(c callSite, contentType string) bool {
	a.respond(c.call.Args[1], c.call.Args[2], contentType, c.f, c.op)
	return true // the body may read the query too
}

// decodeWithKey reads the body as JSON into its second argument, and takes
// an idempotency key from the header or the body.
func (a *analysis) decodeWithKey(c callSite) bool {
	a.requestBody(c.call.Args[1], c.f, c.op)
	c.op.addParam("Idempotency-Key", "header", &Schema{
		Type: "string",
		Description: "Makes a retry of this request return the first response rather than act twice. " +
			"Also accepted as `idempotency_key` in the body.",
	})
	return false
}

// decode reads json.NewDecoder(r.Body).Decode(&v).
func (a *analysis) decode(c callSite) bool {
	sel, ok := c.call.Fun.(*ast.SelectorExpr)
	if !ok {
		return true
	}
	dec, ok := unparen(sel.X).(*ast.CallExpr)
	if !ok || len(dec.Args) != 1 {
		return true
	}
	if body := a.requestBodyExpr(dec.Args[0], c.f); body != nil {
		c.consumed[body] = true
		a.requestBody(c.call.Args[0], c.f, c.op)
	}
	return true
}

func (a *analysis) redirect(c callSite) bool {
	for _, st := range a.constInts(c.call.Args[3], c.f, c.op) {
		c.op.responses = append(c.op.responses, response{status: st, redirect: true})
	}
	return true
}

func (a *analysis) websocket(c callSite) bool {
	c.op.responses = append(c.op.responses, response{status: 101})
	return true
}

func (a *analysis) queryGet(c callSite) bool {
	sel, ok := c.call.Fun.(*ast.SelectorExpr)
	if !ok || !a.fromQuery(sel.X, c.f) {
		return true
	}
	if k, ok := a.constString(c.call.Args[0]); ok {
		c.op.addParam(k, "query", a.queryParamSchema(c.call, c.f, c.fn.Name() == "Has"))
	}
	return true
}

func (a *analysis) formValue(c callSite) bool {
	if k, ok := a.constString(c.call.Args[0]); ok {
		c.op.addParam(k, "query", a.queryParamSchema(c.call, c.f, false))
	}
	return true
}

// headerGet reads r.Header.Get("X-Name").
func (a *analysis) headerGet(c callSite) bool {
	sel, ok := c.call.Fun.(*ast.SelectorExpr)
	if !ok {
		return true
	}
	hs, ok := unparen(sel.X).(*ast.SelectorExpr)
	if !ok || hs.Sel.Name != "Header" || !isRequest(a.info.TypeOf(hs.X)) {
		return true
	}
	if k, ok := a.constString(c.call.Args[0]); ok && !ignoredHeaders[strings.ToLower(k)] {
		c.op.addParam(k, "header", &Schema{Type: "string"})
	}
	return true
}

// headerSet reads w.Header().Set("Content-Type", ...).
func (a *analysis) headerSet(c callSite) bool {
	sel, ok := c.call.Fun.(*ast.SelectorExpr)
	if !ok || !a.responseHeader(sel.X, c.f) {
		return true
	}
	if k, ok := a.constString(c.call.Args[0]); !ok || !strings.EqualFold(k, "Content-Type") {
		return true
	}
	c.f.contentType = "?"
	if v, ok := a.constString(c.call.Args[1]); ok {
		c.f.contentType = v
	}
	return true
}

// writeHeader reads w.WriteHeader(status): no body, or the bytes of the
// Content-Type set before it.
func (a *analysis) writeHeader(c callSite) bool {
	ct := c.f.contentType
	for _, st := range a.constInts(c.call.Args[0], c.f, c.op) {
		r := response{status: st}
		switch {
		case ct == "":
		case ct == "?" || strings.HasPrefix(ct, jsonType):
			r.unread = fmt.Sprintf("writes a %d response whose body this generator cannot read (Content-Type %s)", st, ct)
		default:
			r.contentType, r.schema = ct, rawSchema(ct)
		}
		c.op.responses = append(c.op.responses, r)
	}
	return true
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
	args := []ast.Expr{e}
	if call, ok := unparen(e).(*ast.CallExpr); ok {
		args = call.Args
	}
	for _, arg := range args {
		if s, ok := unparen(arg).(*ast.SelectorExpr); ok && a.isBody(s) {
			return s
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
	switch {
	case t == nil:
		op.problemf("decodes the body into something whose type is unknown")
		return
	case isUntypedNil(t):
		return
	}
	t = deref(t)
	if _, ok := t.Underlying().(*types.Interface); ok {
		op.problemf("decodes the body into an interface, which says nothing about its shape")
		return
	}
	s := a.sch.forType(t)
	if op.request != nil {
		s = union([]*Schema{op.request, s})
	}
	op.request = s
	op.requestContentType = jsonType
}

func isUntypedNil(t types.Type) bool {
	b, ok := t.(*types.Basic)
	return ok && b.Kind() == types.UntypedNil
}

// respond records one JSON response.
func (a *analysis) respond(statusExpr, bodyExpr ast.Expr, contentType string, f *frame, op *operation) {
	statuses := a.constInts(statusExpr, f, op)
	body, bf := a.resolve(bodyExpr, f)
	if id, ok := unparen(body).(*ast.Ident); ok && a.info.Uses[id] == types.Universe.Lookup("nil") {
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
		b, ok := f.bind[a.info.Uses[id]]
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
	var out []int
	if id, ok := unparen(e).(*ast.Ident); ok {
		for _, def := range a.assignments(id, f) {
			out = append(out, a.constInts(def, f, op)...)
		}
	}
	if len(out) == 0 {
		op.problemf("answers with a status code that is not a constant: %s", exprString(e))
		return nil
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func (a *analysis) objectOf(id *ast.Ident) types.Object {
	if obj := a.info.Uses[id]; obj != nil {
		return obj
	}
	return a.info.Defs[id]
}

// assignments are the expressions a local variable is given in its function.
func (a *analysis) assignments(id *ast.Ident, f *frame) []ast.Expr {
	obj := a.objectOf(id)
	if obj == nil {
		return nil
	}
	is := func(e ast.Expr) bool {
		lid, ok := e.(*ast.Ident)
		return ok && a.objectOf(lid) == obj
	}
	var out []ast.Expr
	ast.Inspect(f.node, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			out = append(out, pairsWhere(n.Lhs, n.Rhs, is)...)
		case *ast.ValueSpec:
			out = append(out, pairsWhere(identExprs(n.Names), n.Values, is)...)
		}
		return true
	})
	return out
}

// pairsWhere is the right-hand sides whose left-hand side matches, in an
// assignment of as many values as names.
func pairsWhere(lhs, rhs []ast.Expr, match func(ast.Expr) bool) []ast.Expr {
	if len(lhs) != len(rhs) {
		return nil
	}
	var out []ast.Expr
	for i, l := range lhs {
		if match(l) {
			out = append(out, rhs[i])
		}
	}
	return out
}

func identExprs(ids []*ast.Ident) []ast.Expr {
	out := make([]ast.Expr, len(ids))
	for i, id := range ids {
		out[i] = id
	}
	return out
}

func (a *analysis) constString(e ast.Expr) (string, bool) {
	tv, ok := a.info.Types[e]
	if !ok || tv.Value == nil || tv.Value.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(tv.Value), true
}

// derivesFrom reports whether an expression is, or is a variable given, a
// call that match accepts.
func (a *analysis) derivesFrom(e ast.Expr, f *frame, match func(*ast.CallExpr) bool) bool {
	e, f = a.resolve(e, f)
	switch x := unparen(e).(type) {
	case *ast.CallExpr:
		return match(x)
	case *ast.Ident:
		return slices.ContainsFunc(a.assignments(x, f), func(d ast.Expr) bool { return a.derivesFrom(d, f, match) })
	}
	return false
}

// fromQuery reports whether a url.Values is the request's query string.
func (a *analysis) fromQuery(e ast.Expr, f *frame) bool {
	return a.derivesFrom(e, f, func(x *ast.CallExpr) bool {
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Query" && isNamed(deref(a.info.TypeOf(sel.X)), pkgURL, "URL")
	})
}

// responseHeader reports whether an http.Header is the response's.
func (a *analysis) responseHeader(e ast.Expr, f *frame) bool {
	return a.derivesFrom(e, f, func(x *ast.CallExpr) bool {
		sel, ok := x.Fun.(*ast.SelectorExpr)
		return ok && sel.Sel.Name == "Header" && isWriter(a.info.TypeOf(sel.X))
	})
}

func isRequest(t types.Type) bool {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return isNamed(p.Elem(), pkgHTTP, "Request")
	}
	return false
}

func isWriter(t types.Type) bool { return isNamed(t, pkgHTTP, "ResponseWriter") }

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

func deref(t types.Type) types.Type {
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		return p.Elem()
	}
	return t
}
