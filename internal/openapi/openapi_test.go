package openapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/httpapi"
	"github.com/trypando/pando/internal/reference"
)

// generated is the document as `make reference` writes it, parsed.
func generated(t *testing.T) (map[string]any, reference.Document) {
	t.Helper()
	ref := httpapi.Reference()
	body, err := Generate(ref, "test")
	require.NoError(t, err)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	return doc, ref
}

// TestR261_TheOpenAPIDocumentCoversEveryRoute asserts R-261: the OpenAPI
// document describes every route the API serves, each with what it answers,
// read from its handler.
//
// The routes come from routeDocs, which TestR261_EveryRouteIsDocumented holds
// to the router; this holds the document to routeDocs. Generation itself fails
// on any answer it cannot read, so a new endpoint that sends something the
// generator cannot describe fails here, in `make check`, rather than shipping
// a document that is quietly wrong about it.
func TestR261_TheOpenAPIDocumentCoversEveryRoute(t *testing.T) {
	doc, ref := generated(t)
	paths := doc["paths"].(map[string]any)

	tags := map[string]bool{}
	for _, tag := range doc["tags"].([]any) {
		tags[tag.(map[string]any)["name"].(string)] = true
	}

	described := 0
	ids := map[string]string{}
	for _, route := range ref.API.Routes {
		key := route.Method + " " + route.Path
		item, ok := paths[route.Path].(map[string]any)
		require.True(t, ok, "%s is documented and has no path in the OpenAPI document", key)
		op, ok := item[strings.ToLower(route.Method)].(map[string]any)
		require.True(t, ok, "%s is documented and has no operation in the OpenAPI document", key)
		described++

		id, _ := op["operationId"].(string)
		require.NotEmpty(t, id, "%s has no operationId", key)
		require.Empty(t, ids[id], "%s and %s share the operationId %s", key, ids[id], id)
		ids[id] = key

		require.NotEmpty(t, op["summary"], "%s has no summary", key)
		for _, tag := range op["tags"].([]any) {
			require.True(t, tags[tag.(string)], "%s is tagged %q, which the document does not declare", key, tag)
		}

		answers := 0
		for status := range op["responses"].(map[string]any) {
			if status != "default" {
				answers++
			}
		}
		require.Positive(t, answers, "%s has no response besides the error envelope", key)
		require.Contains(t, op["responses"], "default", "%s does not say how it fails", key)

		if route.Verb != "" {
			require.Equal(t, route.Verb, op["x-pando-verb"], "%s", key)
		}
		if route.Public {
			require.Equal(t, []any{}, op["security"], "%s is public and asks for a credential", key)
		}
	}

	operations := 0
	for _, item := range paths {
		operations += len(item.(map[string]any))
	}
	require.Equal(t, described, operations, "the document has operations routeDocs does not")
}

// TestTheOpenAPIDocumentIsWellFormed holds the document to the parts of
// OpenAPI 3.1 a renderer depends on: every reference resolves, every path
// parameter is declared and every declared one is in the path, every status
// is a status, and every security requirement names a scheme.
func TestTheOpenAPIDocumentIsWellFormed(t *testing.T) {
	doc, _ := generated(t)

	require.Regexp(t, `^3\.1\.\d+$`, doc["openapi"])
	info := doc["info"].(map[string]any)
	require.NotEmpty(t, info["title"])
	require.Equal(t, "test", info["version"])

	components := doc["components"].(map[string]any)
	schemes := components["securitySchemes"].(map[string]any)
	checkSecurity := func(where string, reqs any) {
		for _, req := range reqs.([]any) {
			for name := range req.(map[string]any) {
				require.Contains(t, schemes, name, "%s names the security scheme %q, which is not declared", where, name)
			}
		}
	}
	checkSecurity("the document", doc["security"])

	// Every $ref resolves.
	refs := 0
	var walk func(where string, v any)
	walk = func(where string, v any) {
		switch v := v.(type) {
		case map[string]any:
			if ref, ok := v["$ref"].(string); ok {
				refs++
				require.NotNil(t, resolve(doc, ref), "%s refers to %s, which is not in the document", where, ref)
			}
			for k, child := range v {
				walk(where+"/"+k, child)
			}
		case []any:
			for i, child := range v {
				walk(where+"/"+strconv.Itoa(i), child)
			}
		}
	}
	walk("#", doc)
	require.Positive(t, refs)

	template := regexp.MustCompile(`\{([^}]+)\}`)
	status := regexp.MustCompile(`^[1-5]\d\d$`)
	for path, item := range doc["paths"].(map[string]any) {
		require.True(t, strings.HasPrefix(path, "/"), "%s is not a path", path)
		var inPath []string
		for _, m := range template.FindAllStringSubmatch(path, -1) {
			inPath = append(inPath, m[1])
		}
		for method, raw := range item.(map[string]any) {
			op := raw.(map[string]any)
			where := strings.ToUpper(method) + " " + path
			require.Contains(t, []string{"get", "put", "post", "delete", "patch"}, method, where)

			var declared []string
			for _, p := range asList(op["parameters"]) {
				param := p.(map[string]any)
				require.Contains(t, []string{"path", "query", "header", "cookie"}, param["in"], where)
				require.NotNil(t, param["schema"], "%s: parameter %v has no schema", where, param["name"])
				if param["in"] == "path" {
					require.Equal(t, true, param["required"], "%s: a path parameter is required", where)
					declared = append(declared, param["name"].(string))
				}
			}
			slices.Sort(declared)
			want := slices.Clone(inPath)
			slices.Sort(want)
			require.Equal(t, want, declared, "%s: path parameters", where)

			for code, raw := range op["responses"].(map[string]any) {
				require.True(t, code == "default" || status.MatchString(code), "%s answers %q, which is not a status", where, code)
				resp := raw.(map[string]any)
				if _, isRef := resp["$ref"]; !isRef {
					require.NotEmpty(t, resp["description"], "%s %s has no description", where, code)
				}
				for ct, mt := range asMap(resp["content"]) {
					require.NotEmpty(t, ct, where)
					require.NotNil(t, mt.(map[string]any)["schema"], "%s %s %s has no schema", where, code, ct)
				}
			}
			if body, ok := op["requestBody"].(map[string]any); ok {
				require.NotEmpty(t, body["content"], "%s has a request body with no content", where)
				require.NotEqual(t, http.MethodGet, strings.ToUpper(method), "%s takes a body", where)
			}
			if sec, ok := op["security"]; ok {
				checkSecurity(where, sec)
			}
		}
	}
}

// TestTheErrorEnvelopeListsEveryCode asserts the error schema's code is the
// catalog the server is tested against.
func TestTheErrorEnvelopeListsEveryCode(t *testing.T) {
	doc, ref := generated(t)
	envelope := resolve(doc, "#/components/schemas/Error").(map[string]any)
	code := envelope["properties"].(map[string]any)["code"].(map[string]any)
	enum := code["enum"].([]any)
	require.Len(t, enum, len(ref.Errors))
	for i, c := range ref.Errors {
		require.Equal(t, string(c.Code), enum[i])
	}
	require.Equal(t, []any{"code", "message"}, envelope["required"])
}

func resolve(doc map[string]any, ref string) any {
	if !strings.HasPrefix(ref, "#/") {
		return nil
	}
	var cur any = doc
	for _, part := range strings.Split(strings.TrimPrefix(ref, "#/"), "/") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[strings.ReplaceAll(strings.ReplaceAll(part, "~1", "/"), "~0", "~")]
	}
	return cur
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func asMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// --- the reading, against a package written for it --------------------------

const fakeAPI = "github.com/trypando/pando/internal/openapi/testdata/fakeapi"

func fakeRoutes() []reference.Route {
	route := func(method, path string) reference.Route {
		return reference.Route{Method: method, Path: path, Group: "Things", Summary: "Does a thing. Then more."}
	}
	return []reference.Route{
		route("GET", "/api/v1/things"),
		route("POST", "/api/v1/things"),
		route("GET", "/api/v1/things/{thingID}"),
		route("DELETE", "/api/v1/things/{thingID}"),
		route("GET", "/api/v1/loose"),
		route("GET", "/api/v1/go"),
		route("GET", "/api/v1/text"),
	}
}

func readFake(t *testing.T, routes []reference.Route, ov map[string]override) (map[string]any, []string) {
	t.Helper()
	a, err := load(fakeAPI)
	require.NoError(t, err)
	d, problems, err := a.document(reference.Document{API: reference.API{Routes: routes}}, "v", ov)
	require.NoError(t, err)
	body, err := json.Marshal(d)
	require.NoError(t, err)
	var doc map[string]any
	require.NoError(t, json.Unmarshal(body, &doc))
	return doc, problems
}

func op(t *testing.T, doc map[string]any, method, path string) map[string]any {
	t.Helper()
	o, ok := doc["paths"].(map[string]any)[path].(map[string]any)[method].(map[string]any)
	require.True(t, ok, "%s %s", method, path)
	return o
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	require.NoError(t, err)
	return string(b)
}

func TestTheReadingFollowsTheHandlers(t *testing.T) {
	doc, problems := readFake(t, fakeRoutes(), map[string]override{})

	// The one handler that answers with something whose shape is nowhere in
	// the code is the one problem.
	require.Len(t, problems, 1, "%v", problems)
	require.Contains(t, problems[0], "GET /api/v1/loose: answers with v")

	// A list built from map literals, a helper adding a key, and the query.
	list := op(t, doc, "get", "/api/v1/things")
	require.Equal(t, "listThings", list["operationId"])
	require.Equal(t, "Does a thing.", list["summary"])
	require.JSONEq(t, `[
		{"name":"all","in":"query","schema":{"type":"boolean"}},
		{"name":"id","in":"query","schema":{"type":"array","items":{"type":"string"}}},
		{"name":"limit","in":"query","schema":{"type":"integer"}}
	]`, jsonOf(t, list["parameters"]))
	listed := list["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]
	require.JSONEq(t, `{
		"type":"object",
		"properties":{
			"things":{"type":["array","null"],"items":{"type":"object","properties":{"id":{"type":"string"},"all":{"type":"boolean"}},"required":["id","all"]}},
			"ids":{"type":["array","null"],"items":{"type":"string"}},
			"total":{"type":"integer","format":"int64"}
		},
		"required":["things","ids","total"]
	}`, jsonOf(t, listed))

	// A decoded body, and a status chosen on a branch.
	create := op(t, doc, "post", "/api/v1/things")
	require.JSONEq(t, `{"content":{"application/json":{"schema":{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}}}}`,
		jsonOf(t, create["requestBody"]))
	responses := create["responses"].(map[string]any)
	require.Contains(t, responses, "200")
	require.Contains(t, responses, "201")
	require.Equal(t, map[string]any{"$ref": "#/components/responses/Error"}, responses["default"])

	// Two literals on two branches, one through a helper: one object.
	get := op(t, doc, "get", "/api/v1/things/{thingID}")
	require.JSONEq(t, `{
		"type":"object",
		"properties":{"ok":{"type":"boolean"},"thing":{"$ref":"#/components/schemas/Thing"}},
		"required":["ok"]
	}`, jsonOf(t, get["responses"].(map[string]any)["200"].(map[string]any)["content"].(map[string]any)["application/json"].(map[string]any)["schema"]))

	// A struct, as encoding/json writes it.
	require.JSONEq(t, `{
		"type":"object",
		"properties":{
			"id":{"type":"string"},
			"name":{"type":"string"},
			"tags":{"type":["array","null"],"items":{"type":"string"}},
			"at":{"type":"string","format":"date-time"},
			"parent":{"anyOf":[{"$ref":"#/components/schemas/Thing"},{"type":"null"}]},
			"extra":{"type":"integer","format":"int64"}
		},
		"required":["id","tags","at","extra"]
	}`, jsonOf(t, resolve(doc, "#/components/schemas/Thing")))

	del := op(t, doc, "delete", "/api/v1/things/{thingID}")
	require.JSONEq(t, `{"description":"Done. No body."}`, jsonOf(t, del["responses"].(map[string]any)["204"]))

	redirect := op(t, doc, "get", "/api/v1/go")
	require.Contains(t, redirect["responses"].(map[string]any)["302"].(map[string]any)["headers"], "Location")
	require.JSONEq(t, `[{"name":"X-Thing","in":"header","schema":{"type":"string"}}]`, jsonOf(t, redirect["parameters"]))

	text := op(t, doc, "get", "/api/v1/text")
	require.Equal(t, "getText", text["operationId"], "an inline handler is named for its method and path")
	require.Contains(t, text["responses"].(map[string]any)["200"].(map[string]any)["content"], "text/plain")
}

func TestAnOverrideAnswersWhatTheReadingCannot(t *testing.T) {
	_, problems := readFake(t, fakeRoutes(), map[string]override{
		"GET /api/v1/loose": {responses: map[int]*body{200: {contentType: jsonType, schema: &Schema{Type: "integer"}}}},
	})
	require.Empty(t, problems)
}

func TestTheReadingRefusesWhatItCannotMatch(t *testing.T) {
	routes := append(fakeRoutes(), reference.Route{Method: "GET", Path: "/api/v1/nowhere", Group: "Things", Summary: "Nothing."})
	_, problems := readFake(t, routes, map[string]override{
		"GET /api/v1/loose": {responses: map[int]*body{200: {contentType: jsonType, schema: &Schema{Type: "integer"}}}},
		"GET /api/v1/gone":  {},
		"GET /api/v1/go":    {responses: map[int]*body{200: {contentType: jsonType, goType: [2]string{"example.com/none", "T"}}}},
	})
	require.Len(t, problems, 3, "%v", problems)
	joined := strings.Join(problems, "\n")
	require.Contains(t, joined, "GET /api/v1/nowhere: documented in routeDocs, and no handler")
	require.Contains(t, joined, "GET /api/v1/gone: has an entry in overrides.go and is not a documented route")
	require.Contains(t, joined, "example.com/none.T, which httpapi does not reach")
}

func TestATypeWithItsOwnJSONMustBeNamed(t *testing.T) {
	a, err := load(httpapiPath)
	require.NoError(t, err)

	value := a.lookupType("github.com/trypando/pando/internal/secret", "Value")
	require.NotNil(t, value)
	require.Equal(t, "string", a.sch.forType(value).Type)
	require.Empty(t, a.sch.problems)

	delete(a.sch.marshalers, "github.com/trypando/pando/internal/secret.Value")
	a.sch.forType(value)
	require.Len(t, a.sch.problems, 1)
	require.Contains(t, a.sch.problems[0], "secret.Value writes its own JSON")
}

func TestNames(t *testing.T) {
	for in, want := range map[string]string{
		"Does a thing. Then more.":                 "Does a thing.",
		"One sentence only":                        "One sentence only",
		"Uses `a.b` and (see c. d) first. Second.": "Uses `a.b` and (see c. d) first.",
		"Ends with a period.":                      "Ends with a period.",
	} {
		require.Equal(t, want, firstSentence(in), in)
	}
	for in, want := range map[string]string{
		"ListApps": "listApps", "SSOComplete": "ssoComplete", "Me": "me", "SAMLMetadata": "samlMetadata", "API": "api",
	} {
		require.Equal(t, want, lowerInitial(in), in)
	}
	require.Equal(t, "listApps", operationID("handleListApps", "GET", "/api/v1/apps"))
	require.Equal(t, "getScimV2UsersById", operationID("", "GET", "/api/v1/scim/v2/Users/{id}"))
	require.Equal(t, "PageApp", pascal("page")+"App")
	require.Equal(t, "Status 418", statusText(418))
}

func TestNullability(t *testing.T) {
	str := &Schema{Type: "string"}
	require.Equal(t, []string{"string", "null"}, nullable(str).Type)
	require.Equal(t, "string", notNull(nullable(str)).Type)
	ref := &Schema{Ref: "#/x"}
	require.Equal(t, ref, notNull(nullable(ref)))
	require.Equal(t, anySchema(), nullable(anySchema()))
	already := &Schema{Type: []string{"array", "null"}}
	require.Equal(t, already, nullable(already))
}

func TestUnion(t *testing.T) {
	require.Nil(t, union(nil))
	a := &Schema{Ref: "#/a"}
	b := &Schema{Ref: "#/b"}
	require.Equal(t, a, union([]*Schema{a, a}))
	require.Equal(t, &Schema{AnyOf: []*Schema{a, b}}, union([]*Schema{a, b}))

	one := &Schema{Type: "object", Properties: Props{{Name: "x", Schema: a}}, Required: []string{"x"}}
	two := &Schema{Type: "object", Properties: Props{{Name: "x", Schema: b}, {Name: "y", Schema: a}}, Required: []string{"x", "y"}}
	require.JSONEq(t, `{"type":"object","properties":{"x":{"anyOf":[{"$ref":"#/a"},{"$ref":"#/b"}]},"y":{"$ref":"#/a"}},"required":["x"]}`,
		mustJSON(union([]*Schema{one, two})))
}
