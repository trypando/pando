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
	handlers := map[string]served{}
	for _, s := range registered {
		handlers[s.method+" "+s.path] = s
	}

	d := &Document{
		OpenAPI: "3.1.0",
		Info: Info{
			Title:   "Pando API",
			Version: version,
			Description: "The HTTP API of a Pando installation.\n\n" +
				"Authenticate with a token in the `Authorization: Bearer <token>` header, or with the session cookie " +
				"`POST /api/v1/sessions` sets.\n\n" +
				"A failed request answers with the error envelope: a stable `code` to branch on, a `message` to show, " +
				"and often a `remedy` saying what to do.\n\n" +
				"A field listed as required is always present in a response. A request may leave out any field it does " +
				"not need; Pando answers `VALID_INVALID` naming what is missing.",
		},
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

	errorRef := a.sch.forType(a.lookupType("github.com/trypando/pando/internal/errs", "Error"))
	d.Components.Responses["Error"] = &Response{
		Description: "The error envelope: what went wrong, a stable code to branch on, and often what to do about it.",
		Content:     map[string]MediaType{jsonType: {Schema: errorRef}},
	}
	for name, s := range scimSchemas() {
		d.Components.Schemas[name] = s
	}
	d.Components.Responses["ScimError"] = &Response{
		Description: "A SCIM error.",
		Content:     map[string]MediaType{scimType: {Schema: &Schema{Ref: "#/components/schemas/ScimError"}}},
	}

	usedOverrides := map[string]bool{}
	var problems []string
	opIDs := map[string]int{}
	seenTags := map[string]bool{}

	type built struct {
		key string
		op  *Operation
		id  string
	}
	var ops []built

	for _, route := range ref.API.Routes {
		key := route.Method + " " + normalize(route.Path)
		s, ok := handlers[key]
		if !ok {
			problems = append(problems, key+": documented in routeDocs, and no handler for it was found in (*Server).Routes")
			continue
		}
		res := a.analyze(s.handler)
		o, hasOverride := ov[key]
		if hasOverride {
			usedOverrides[key] = true
		}
		for _, p := range res.problems {
			problems = append(problems, key+": "+p)
		}

		op, opProblems := a.operation(route, res, o, hasOverride)
		for _, p := range opProblems {
			problems = append(problems, key+": "+p)
		}

		if !seenTags[route.Group] {
			seenTags[route.Group] = true
			d.Tags = append(d.Tags, Tag{Name: route.Group})
		}

		id := operationID(res.handlerName, route.Method, route.Path)
		opIDs[id]++
		ops = append(ops, built{key: key, op: op, id: id})

		path := normalize(route.Path)
		if d.Paths[path] == nil {
			d.Paths[path] = map[string]*Operation{}
		}
		d.Paths[path][strings.ToLower(route.Method)] = op
	}

	// One handler on two methods: the method tells them apart.
	for _, b := range ops {
		b.op.OperationID = b.id
		if opIDs[b.id] > 1 {
			b.op.OperationID = b.id + pascal(strings.ToLower(b.key[:strings.IndexByte(b.key, ' ')]))
		}
	}

	for key := range ov {
		if !usedOverrides[key] {
			problems = append(problems, key+": has an entry in overrides.go and is not a documented route")
		}
	}

	problems = append(problems, a.sch.problems...)
	sort.Strings(problems)
	problems = slices.Compact(problems)

	// Settle component names, and point every reference at them.
	names := a.sch.names()
	for key, c := range a.sch.byKey {
		if d.Components.Schemas[names[key]] != nil {
			problems = append(problems, key+" is named "+names[key]+", which a SCIM schema in overrides.go already is")
		}
		d.Components.Schemas[names[key]] = c.schema
	}
	fix := func(s *Schema) {
		walk(s, func(s *Schema) {
			if s.Ref != "" && !strings.HasPrefix(s.Ref, "#/") {
				s.Ref = "#/components/schemas/" + names[s.Ref]
			}
		})
	}
	for _, s := range d.Components.Schemas {
		fix(s)
	}
	for _, r := range d.Components.Responses {
		for _, m := range r.Content {
			fix(m.Schema)
		}
	}
	for _, methods := range d.Paths {
		for _, op := range methods {
			if op.RequestBody != nil {
				for _, m := range op.RequestBody.Content {
					fix(m.Schema)
				}
			}
			for _, r := range op.Responses {
				for _, m := range r.Content {
					fix(m.Schema)
				}
			}
			for _, p := range op.Parameters {
				fix(p.Schema)
			}
		}
	}
	prune(d)

	// The error codes, from the catalog the server is tested against.
	if es := d.Components.Schemas[names[typeKey(namedOf(a.lookupType("github.com/trypando/pando/internal/errs", "Error")))]]; es != nil {
		describeCodes(es, ref.Errors)
	}

	return d, problems, nil
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
	for _, r := range d.Components.Responses {
		for _, m := range r.Content {
			add(m.Schema)
		}
	}
	for _, methods := range d.Paths {
		for _, op := range methods {
			if op.RequestBody != nil {
				for _, m := range op.RequestBody.Content {
					add(m.Schema)
				}
			}
			for _, r := range op.Responses {
				for _, m := range r.Content {
					add(m.Schema)
				}
			}
			for _, p := range op.Parameters {
				add(p.Schema)
			}
		}
	}
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
	for i, p := range es.Properties {
		if p.Name != "code" {
			continue
		}
		var b strings.Builder
		b.WriteString("A stable code to branch on. Renaming one is a breaking change.\n\n| Code | Status | Meaning |\n|---|---|---|\n")
		var enum []string
		for _, c := range codes {
			enum = append(enum, string(c.Code))
			fmt.Fprintf(&b, "| `%s` | %d | %s |\n", c.Code, c.Status, strings.ReplaceAll(forReaders(c.Meaning), "|", "\\|"))
		}
		c := *p.Schema
		c.Enum = enum
		c.Description = strings.TrimSuffix(b.String(), "\n")
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

// operation assembles one operation from what the reading found.
func (a *analysis) operation(route reference.Route, res *operation, o override, hasOverride bool) (*Operation, []string) {
	var problems []string
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

	for _, m := range pathParam.FindAllStringSubmatch(path, -1) {
		op.Parameters = append(op.Parameters, Parameter{Name: m[1], In: "path", Required: true, Schema: &Schema{Type: "string"}})
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
		op.Parameters = append(op.Parameters, Parameter{Name: p.name, In: p.in, Schema: p.schema})
	}
	op.Parameters = append(op.Parameters, o.params...)

	// The request body.
	switch {
	case hasOverride && o.request != nil:
		mt, err := a.media(o.request)
		if err != nil {
			problems = append(problems, err.Error())
		}
		op.RequestBody = &RequestBody{Description: o.request.description, Content: map[string]MediaType{o.request.contentType: mt}}
	case res.request != nil:
		op.RequestBody = &RequestBody{Content: map[string]MediaType{res.requestContentType: {Schema: res.request}}}
	case res.rawBody && !o.noRequestBody:
		problems = append(problems, "reads the request body other than as JSON; say what it takes in overrides.go")
	}

	// The responses, by status.
	byStatus := map[int][]response{}
	for _, r := range res.responses {
		byStatus[r.status] = append(byStatus[r.status], r)
	}
	if hasOverride {
		for st, b := range o.responses {
			mt, err := a.media(b)
			if err != nil {
				problems = append(problems, err.Error())
			}
			r := response{status: st, contentType: b.contentType, schema: mt.Schema}
			if b.contentType == "" {
				r.schema = nil
			}
			byStatus[st] = []response{r}
			if b.description != "" {
				op.Responses[strconv.Itoa(st)] = &Response{Description: b.description}
			}
		}
	}
	if len(byStatus) == 0 {
		problems = append(problems, "no response was found in its handler")
	}
	for st, rs := range byStatus {
		code := strconv.Itoa(st)
		resp := op.Responses[code]
		if resp == nil {
			resp = &Response{Description: statusText(st)}
			op.Responses[code] = resp
		}
		byType := map[string][]*Schema{}
		for _, r := range rs {
			if r.unread != "" {
				problems = append(problems, r.unread+"; say what it sends in overrides.go")
				continue
			}
			if r.redirect {
				resp.Headers = map[string]Header{"Location": {Description: "Where the browser is sent.", Schema: &Schema{Type: "string"}}}
				continue
			}
			if r.contentType == "" {
				continue
			}
			byType[r.contentType] = append(byType[r.contentType], r.schema)
		}
		for ct, shapes := range byType {
			if resp.Content == nil {
				resp.Content = map[string]MediaType{}
			}
			resp.Content[ct] = MediaType{Schema: union(shapes)}
		}
	}

	switch {
	case scim || res.scimErrors:
		op.Responses["default"] = &Response{Ref: "#/components/responses/ScimError"}
	default:
		// Every route can answer with the envelope: an unauthenticated
		// request is refused before the handler, and anything can fail.
		op.Responses["default"] = &Response{Ref: "#/components/responses/Error"}
	}
	return op, problems
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
