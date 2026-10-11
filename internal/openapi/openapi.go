// Package openapi writes Pando's HTTP API as an OpenAPI 3.1 document, read from
// the handlers that serve it.
//
// R-261 makes the API the product, and a description of it written by hand is
// a second opinion about it that drifts the first time a field is added. So
// nothing here is written by hand except what the code cannot say (see
// overrides.go). The routes are the router's: (*httpapi.Server).Routes is read
// as source, so a route is described because it is registered. Each route's
// request body is the type its handler decodes into; each response is what its
// handler passes to JSON, with the status it passes — a map literal's keys are
// read from the literal, and a struct is described field by field the way
// encoding/json writes it, as cmd/gen-api-types describes it for the console.
// The summaries, groups and verbs are httpapi's routeDocs, the table
// TestR261_EveryRouteIsDocumented holds to the router.
//
// It reads source, so it runs where the source is: `make reference` writes
// docs/openapi.json with it, and the binary does not carry it.
package openapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/types"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/reference"
)

// Document is an OpenAPI 3.1 document.
type Document struct {
	OpenAPI    string                           `json:"openapi"`
	Info       Info                             `json:"info"`
	Servers    []Server                         `json:"servers"`
	Security   []map[string][]string            `json:"security"`
	Tags       []Tag                            `json:"tags"`
	Paths      map[string]map[string]*Operation `json:"paths"`
	Components Components                       `json:"components"`
}

// Info is the document's title and version.
type Info struct {
	Title       string `json:"title"`
	Version     string `json:"version"`
	Description string `json:"description"`
}

// Server is where the API is reached.
type Server struct {
	URL         string                    `json:"url"`
	Description string                    `json:"description"`
	Variables   map[string]ServerVariable `json:"variables"`
}

// ServerVariable is a part of a server's URL the reader fills in.
type ServerVariable struct {
	Default     string `json:"default"`
	Description string `json:"description"`
}

// Tag is a group of operations.
type Tag struct {
	Name string `json:"name"`
}

// Operation is one method on one path.
type Operation struct {
	OperationID string                 `json:"operationId"`
	Summary     string                 `json:"summary"`
	Description string                 `json:"description,omitempty"`
	Tags        []string               `json:"tags"`
	Parameters  []Parameter            `json:"parameters,omitempty"`
	RequestBody *RequestBody           `json:"requestBody,omitempty"`
	Responses   map[string]*Response   `json:"responses"`
	Security    *[]map[string][]string `json:"security,omitempty"`
	Verb        string                 `json:"x-pando-verb,omitempty"`
}

// Parameter is a path, query or header parameter.
type Parameter struct {
	Name        string  `json:"name"`
	In          string  `json:"in"`
	Required    bool    `json:"required,omitempty"`
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema"`
}

// RequestBody is what an operation reads.
type RequestBody struct {
	Description string               `json:"description,omitempty"`
	Required    bool                 `json:"required,omitempty"`
	Content     map[string]MediaType `json:"content"`
}

// Response is one answer.
type Response struct {
	Ref         string               `json:"$ref,omitempty"`
	Description string               `json:"description,omitempty"`
	Headers     map[string]Header    `json:"headers,omitempty"`
	Content     map[string]MediaType `json:"content,omitempty"`
}

// Header is a response header.
type Header struct {
	Description string  `json:"description,omitempty"`
	Schema      *Schema `json:"schema"`
}

// MediaType is a body in one content type.
type MediaType struct {
	Schema *Schema `json:"schema"`
}

// Components are the document's shared parts.
type Components struct {
	Schemas         map[string]*Schema        `json:"schemas"`
	Responses       map[string]*Response      `json:"responses"`
	SecuritySchemes map[string]SecurityScheme `json:"securitySchemes"`
}

// SecurityScheme is one way to authenticate.
type SecurityScheme struct {
	Type        string `json:"type"`
	Scheme      string `json:"scheme,omitempty"`
	In          string `json:"in,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
}

// The security schemes, by name.
const (
	schemeToken   = "bearerToken"
	schemeSession = "sessionCookie"
	schemeSCIM    = "scimToken"
)

const scimPrefix = "/api/v1/scim/v2/"

const formatDateTime = "date-time"

// errorsPage describes every error code, each at an anchor named for the code.
const errorsPage = "https://trypando.ai/docs/errors"

// Generate reads the handlers and writes the document. It fails, naming each
// one, when a route has an answer it could not read: a document that guessed
// would be the hand-written second opinion this exists to avoid.
func Generate(doc reference.Document, version string) ([]byte, error) {
	a, err := load(httpapiPath)
	if err != nil {
		return nil, err
	}
	d, problems, err := a.document(doc, version, overrides())
	if err != nil {
		return nil, err
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("the OpenAPI document cannot be written: %d answer(s) could not be read from the code:\n  %s\n"+
			"Make the handler send a type or a map literal the generator can read, or say what it sends in internal/openapi/overrides.go",
			len(problems), strings.Join(problems, "\n  "))
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (a *analysis) document(ref reference.Document, version string, ov map[string]override) (*Document, []string, error) {
	registered, err := a.routes()
	if err != nil {
		return nil, nil, err
	}
	b := &assembly{
		a:        a,
		d:        newDocument(ref, version),
		ov:       ov,
		handlers: map[string]served{},
		used:     map[string]bool{},
		ids:      map[string]int{},
		tags:     map[string]bool{},
	}
	for _, s := range registered {
		b.handlers[s.method+" "+s.path] = s
	}
	b.d.Components.Responses["Error"] = &Response{
		Description: "The error envelope: what went wrong, a stable code to branch on, and often what to do about it.",
		Content:     map[string]MediaType{jsonType: {Schema: a.sch.forType(a.errorType())}},
	}
	for name, s := range scimSchemas() {
		b.d.Components.Schemas[name] = s
	}
	b.d.Components.Responses["ScimError"] = &Response{
		Description: "A SCIM error.",
		Content:     map[string]MediaType{scimType: {Schema: &Schema{Ref: "#/components/schemas/ScimError"}}},
	}

	for _, route := range ref.API.Routes {
		b.add(route)
	}
	b.settleIDs()
	b.unusedOverrides()

	problems := append(b.problems, a.sch.problems...)
	problems = append(problems, a.settleNames(b.d)...)
	sort.Strings(problems)
	problems = slices.Compact(problems)

	prune(b.d)
	if es := b.d.Components.Schemas[a.errorName]; es != nil {
		describeCodes(es, ref.Errors)
	}
	return b.d, problems, nil
}

const errsPath = "github.com/trypando/pando/internal/errs"

func (a *analysis) errorType() types.Type { return a.lookupType(errsPath, "Error") }

// newDocument is the document before any route is in it.
func newDocument(ref reference.Document, version string) *Document {
	return &Document{
		OpenAPI: "3.1.0",
		Info:    Info{Title: "Pando API", Version: version, Description: intro(ref.API.BasePath)},
		Servers: []Server{{
			URL:         "https://{host}",
			Description: "Your Pando installation",
			Variables: map[string]ServerVariable{
				"host": {Default: "pando.example.com", Description: "The address Pando's console is reached at."},
			},
		}},
		Security: []map[string][]string{{schemeToken: {}}, {schemeSession: {}}},
		Paths:    map[string]map[string]*Operation{},
		Components: Components{
			Schemas:         map[string]*Schema{},
			Responses:       map[string]*Response{},
			SecuritySchemes: securitySchemes(ref.API.Auth),
		},
	}
}

// intro is the document's description: how to reach the API, then how it
// answers.
func intro(base string) string {
	return "Send requests to your installation's address followed by `" + base + "`, " +
		"for example `https://pando.example.com" + base + "`.\n\n" +
		"Authenticate with a token. Create one in the console under **API and tools** › **Your tokens** › **New token**; " +
		"it is shown once. Send it in the `Authorization` header:\n\n" +
		"```\n" +
		"export PANDO_TOKEN=tok_…\n" +
		"curl -H \"Authorization: Bearer $PANDO_TOKEN\" https://pando.example.com" + base + "/apps\n" +
		"```\n\n" +
		"A failed request answers with a JSON envelope: `code`, `message` and often `remedy`. " +
		"Each code is described at `" + errorsPage + "#<CODE>`, for example " +
		"[PLAN_SLOT_UNFILLED](" + errorsPage + "#PLAN_SLOT_UNFILLED).\n\n" +
		"A field listed as required is always present in a response. A request may leave out any field it does " +
		"not need; Pando answers `VALID_INVALID` naming what is missing."
}

// assembly is the document being put together, route by route.
type assembly struct {
	a        *analysis
	d        *Document
	ov       map[string]override
	handlers map[string]served
	used     map[string]bool // overrides used
	ids      map[string]int  // operationIds, and how many operations have each
	tags     map[string]bool
	ops      []builtOp
	problems []string
}

type builtOp struct {
	method string
	op     *Operation
	id     string
}

func (b *assembly) problem(key string, ps ...string) {
	for _, p := range ps {
		b.problems = append(b.problems, key+": "+p)
	}
}

// add reads one documented route's handler and puts its operation in.
func (b *assembly) add(route reference.Route) {
	path := normalize(route.Path)
	key := route.Method + " " + path
	s, ok := b.handlers[key]
	if !ok {
		b.problem(key, "documented in routeDocs, and no handler for it was found in (*Server).Routes")
		return
	}
	res := b.a.analyze(s.handler)
	o, hasOverride := b.ov[key]
	b.used[key] = hasOverride
	b.problem(key, res.problems...)

	op, problems := b.a.operation(route, res, o)
	b.problem(key, problems...)

	if !b.tags[route.Group] {
		b.tags[route.Group] = true
		b.d.Tags = append(b.d.Tags, Tag{Name: route.Group})
	}
	id := operationID(res.handlerName, route.Method, route.Path)
	b.ids[id]++
	b.ops = append(b.ops, builtOp{method: route.Method, op: op, id: id})

	if b.d.Paths[path] == nil {
		b.d.Paths[path] = map[string]*Operation{}
	}
	b.d.Paths[path][strings.ToLower(route.Method)] = op
}

// settleIDs names each operation: one handler on two methods is told apart by
// the method.
func (b *assembly) settleIDs() {
	for _, o := range b.ops {
		o.op.OperationID = o.id
		if b.ids[o.id] > 1 {
			o.op.OperationID = o.id + pascal(strings.ToLower(o.method))
		}
	}
}

func (b *assembly) unusedOverrides() {
	for key := range b.ov {
		if !b.used[key] {
			b.problem(key, "has an entry in overrides.go and is not a documented route")
		}
	}
}

// settleNames names each component and points every reference at it.
func (a *analysis) settleNames(d *Document) []string {
	var problems []string
	names := a.sch.names()
	for key, c := range a.sch.byKey {
		if d.Components.Schemas[names[key]] != nil {
			problems = append(problems, key+" is named "+names[key]+", which a SCIM schema in overrides.go already is")
		}
		d.Components.Schemas[names[key]] = c.schema
	}
	a.errorName = names[typeKey(namedOf(a.errorType()))]
	roots := d.schemaRoots()
	for _, s := range d.Components.Schemas {
		roots = append(roots, s)
	}
	for _, s := range roots {
		walk(s, func(s *Schema) {
			if s.Ref != "" && !strings.HasPrefix(s.Ref, "#/") {
				s.Ref = "#/components/schemas/" + names[s.Ref]
			}
		})
	}
	return problems
}

// schemaRoots are the schemas the paths and the shared responses use.
func (d *Document) schemaRoots() []*Schema {
	var out []*Schema
	content := func(c map[string]MediaType) {
		for _, m := range c {
			out = append(out, m.Schema)
		}
	}
	for _, r := range d.Components.Responses {
		content(r.Content)
	}
	for _, methods := range d.Paths {
		for _, op := range methods {
			if op.RequestBody != nil {
				content(op.RequestBody.Content)
			}
			for _, r := range op.Responses {
				content(r.Content)
			}
			for _, p := range op.Parameters {
				out = append(out, p.Schema)
			}
		}
	}
	return out
}

// prune drops the component schemas nothing refers to: a type met while
// reading a shape that was then described another way.
func prune(d *Document) {
	used := map[string]bool{}
	var queue []*Schema
	add := func(s *Schema) {
		walk(s, func(s *Schema) {
			name, ok := strings.CutPrefix(s.Ref, "#/components/schemas/")
			if ok && !used[name] {
				used[name] = true
				queue = append(queue, d.Components.Schemas[name])
			}
		})
	}
	queue = d.schemaRoots()
	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		add(s)
	}
	for name := range d.Components.Schemas {
		if !used[name] {
			delete(d.Components.Schemas, name)
		}
	}
}

func securitySchemes(auth []reference.Auth) map[string]SecurityScheme {
	desc := map[string]string{}
	for _, a := range auth {
		desc[a.Name] = forReaders(a.Description)
	}
	return map[string]SecurityScheme{
		schemeToken:   {Type: "http", Scheme: "bearer", Description: desc["Token"]},
		schemeSession: {Type: "apiKey", In: "cookie", Name: httpapi.SessionCookie, Description: desc["Session"]},
		schemeSCIM: {Type: "http", Scheme: "bearer",
			Description: "An identity provider's SCIM token, from `POST /api/v1/identity-providers/{providerID}/scim-token`. " +
				"Accepted by the SCIM endpoints only, and not a Pando token."},
	}
}

func describeCodes(es *Schema, codes []errs.CodeDoc) {
	about := map[string]string{
		"code": "A stable code to branch on. Each is described at `" + errorsPage + "#<CODE>`, " +
			"for example [PLAN_SLOT_UNFILLED](" + errorsPage + "#PLAN_SLOT_UNFILLED).",
		"message":    "What went wrong, written to be shown to a person.",
		"remedy":     "What to do about it, when there is something to do.",
		"details":    "Facts about this failure, such as the field that was refused.",
		"request_id": "The request's ID, for finding it in Pando's log.",
	}
	for i, p := range es.Properties {
		c := *p.Schema
		c.Description = about[p.Name]
		if p.Name == "code" {
			for _, code := range codes {
				c.Enum = append(c.Enum, string(code.Code))
			}
		}
		es.Properties[i].Schema = &c
	}
}

// The references a summary carries for the people building Pando: requirement
// and open-decision IDs, design sections and issues. They mean nothing to
// someone calling the API, so the document leaves them out.
var (
	internalRef  = `(?:R-\d+[a-z]?|O-\d+|issue #\d+|design \d+(?: §[\d.]+)?)`
	internalRefs = internalRef + `(?:\s*(?:,|;|–|-|and)\s*` + internalRef + `)*`
	refAside     = regexp.MustCompile(`\s*\(([^()]*)\)`)
	refInAside   = regexp.MustCompile(`(?:[,;]\s*)?` + internalRefs + `(?:[,;]\s*)?`)
	refBare      = regexp.MustCompile(`\s*\b` + internalRefs)
	refAny       = regexp.MustCompile(internalRef)
)

// forReaders takes the internal references out of text an API reader sees:
// "(R-092)" goes, and "(`egress`, R-188)" becomes "(`egress`)".
func forReaders(s string) string {
	s = refAside.ReplaceAllStringFunc(s, func(aside string) string {
		m := refAside.FindStringSubmatch(aside)
		if !refAny.MatchString(m[1]) {
			return aside
		}
		inner := strings.TrimSpace(refInAside.ReplaceAllString(m[1], ", "))
		inner = strings.TrimSpace(strings.Trim(inner, ", "))
		if inner == "" {
			return ""
		}
		lead := aside[:strings.IndexByte(aside, '(')]
		return lead + "(" + inner + ")"
	})
	return refBare.ReplaceAllString(s, "")
}

var pathParam = regexp.MustCompile(`\{([^}:]+)(:[^}]*)?\}`)

// operation assembles one operation from what the reading found and what
// overrides.go says.
func (a *analysis) operation(route reference.Route, res *operation, o override) (*Operation, []string) {
	path := normalize(route.Path)
	op := &Operation{
		Summary:     route.Title,
		Description: forReaders(route.Summary),
		Tags:        []string{route.Group},
		Responses:   map[string]*Response{},
		Verb:        route.Verb,
	}
	if route.Verb != "" {
		op.Description = strings.TrimSpace(op.Description + "\n\nNeeds `" + route.Verb + "`.")
	}

	scim := strings.HasPrefix(path+"/", scimPrefix)
	switch {
	case scim:
		op.Security = &[]map[string][]string{{schemeSCIM: {}}}
	case route.Public:
		op.Security = &[]map[string][]string{}
	}
	op.Parameters = append(parameters(path, res), o.params...)

	var problems []string
	op.RequestBody, problems = a.requestBodyOf(res, o)
	problems = append(problems, a.responsesOf(op, res, o)...)

	// Every route can answer with the envelope: an unauthenticated request
	// is refused before the handler, and anything can fail.
	op.Responses["default"] = &Response{Ref: "#/components/responses/Error"}
	if scim || res.scimErrors {
		op.Responses["default"] = &Response{Ref: "#/components/responses/ScimError"}
	}
	return op, problems
}

// parameters are the path's, then the query's and the headers the handler
// reads, in order.
func parameters(path string, res *operation) []Parameter {
	var out []Parameter
	for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
		out = append(out, Parameter{Name: m[1], In: "path", Required: true, Schema: &Schema{Type: "string"}})
	}
	var extra []*param
	for _, p := range res.params {
		extra = append(extra, p)
	}
	sort.Slice(extra, func(i, j int) bool {
		if extra[i].in != extra[j].in {
			return extra[i].in > extra[j].in // query before header
		}
		return extra[i].name < extra[j].name
	})
	for _, p := range extra {
		out = append(out, Parameter{Name: p.name, In: p.in, Schema: p.schema})
	}
	return out
}

func (a *analysis) requestBodyOf(res *operation, o override) (*RequestBody, []string) {
	switch {
	case o.request != nil:
		mt, err := a.media(o.request)
		body := &RequestBody{Description: o.request.description, Content: map[string]MediaType{o.request.contentType: mt}}
		if err != nil {
			return body, []string{err.Error()}
		}
		return body, nil
	case res.request != nil:
		return &RequestBody{Content: map[string]MediaType{res.requestContentType: {Schema: res.request}}}, nil
	case res.rawBody && !o.noRequestBody:
		return nil, []string{"reads the request body other than as JSON; say what it takes in overrides.go"}
	}
	return nil, nil
}

// responsesOf fills in an operation's responses, by status.
func (a *analysis) responsesOf(op *Operation, res *operation, o override) []string {
	var problems []string
	byStatus := map[int][]response{}
	for _, r := range res.responses {
		byStatus[r.status] = append(byStatus[r.status], r)
	}
	for st, b := range o.responses {
		mt, err := a.media(b)
		if err != nil {
			problems = append(problems, err.Error())
		}
		byStatus[st] = []response{{status: st, contentType: b.contentType, schema: mt.Schema}}
		if b.description != "" {
			op.Responses[strconv.Itoa(st)] = &Response{Description: b.description}
		}
	}
	if len(byStatus) == 0 {
		problems = append(problems, "no response was found in its handler")
	}
	for st, rs := range byStatus {
		code := strconv.Itoa(st)
		if op.Responses[code] == nil {
			op.Responses[code] = &Response{Description: statusText(st)}
		}
		problems = append(problems, fillResponse(op.Responses[code], rs)...)
	}
	return problems
}

// fillResponse puts one status's answers in its response.
func fillResponse(resp *Response, rs []response) []string {
	var problems []string
	byType := map[string][]*Schema{}
	for _, r := range rs {
		switch {
		case r.unread != "":
			problems = append(problems, r.unread+"; say what it sends in overrides.go")
		case r.redirect:
			resp.Headers = map[string]Header{"Location": {Description: "Where the browser is sent.", Schema: &Schema{Type: "string"}}}
		case r.contentType != "":
			byType[r.contentType] = append(byType[r.contentType], r.schema)
		}
	}
	for ct, shapes := range byType {
		if resp.Content == nil {
			resp.Content = map[string]MediaType{}
		}
		resp.Content[ct] = MediaType{Schema: union(shapes)}
	}
	return problems
}

// media turns an override's body into a media type.
func (a *analysis) media(b *body) (MediaType, error) {
	if b.schema != nil {
		return MediaType{Schema: b.schema}, nil
	}
	if b.goType[0] != "" {
		t := a.lookupType(b.goType[0], b.goType[1])
		if t == nil {
			return MediaType{Schema: anySchema()}, fmt.Errorf("overrides.go names %s.%s, which httpapi does not reach", b.goType[0], b.goType[1])
		}
		return MediaType{Schema: a.sch.forType(t)}, nil
	}
	return MediaType{}, nil
}

func statusText(st int) string {
	switch st {
	case 101:
		return "Switched to a websocket."
	case 200:
		return "OK"
	case 201:
		return "Created"
	case 202:
		return "Accepted: the work continues after the response."
	case 204:
		return "Done. No body."
	case 302, 303:
		return "A redirect, for a browser to follow."
	case 503:
		return "Unavailable"
	}
	return "Status " + strconv.Itoa(st)
}

// operationID names an operation after its handler — handleListApps is
// listApps — or, for a handler written inline, after its method and path.
func operationID(handler, method, path string) string {
	if name, ok := strings.CutPrefix(handler, "handle"); ok && name != "" {
		return lowerInitial(name)
	}
	var b strings.Builder
	b.WriteString(strings.ToLower(method))
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/api/v1/"), "/") {
		if m := pathParam.FindStringSubmatch(seg); m != nil {
			b.WriteString("By" + pascal(m[1]))
			continue
		}
		b.WriteString(pascal(seg))
	}
	return b.String()
}
