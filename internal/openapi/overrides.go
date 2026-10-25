package openapi

// What the handlers' code does not say on its own.
//
// Everything else in the document is read from the handlers. These are the
// places where the shape is decided somewhere the reading cannot follow — a
// type's own MarshalJSON, a body built in another package, bytes written
// without a type — and each entry says what is written there. The generator
// refuses to write a document with a route it could not read, so an entry here
// is the only way past that, and it is a line somebody wrote on purpose rather
// than a guess the generator made. An entry for a route that no longer exists
// fails generation too.

// marshalers are the types that write their own JSON, keyed by package path
// and name.
func marshalers() map[string]*Schema {
	shown := func(what string) *Schema {
		return &Schema{Type: "string", Description: what}
	}
	return map[string]*Schema{
		"time.Time": {Type: "string", Format: formatDateTime},

		// secret.Value writes "[redacted]" in every marshaler (R-194).
		"github.com/trypando/pando/internal/secret.Value": {
			Type: "string", Description: "Always `[redacted]`: a secret's value is never written to a response.",
		},
		// The two keys a response shows once, on purpose.
		"github.com/trypando/pando/internal/core/autodeploy.Secret":       shown("The webhook secret. Shown in this response only."),
		"github.com/trypando/pando/internal/core/subscription.secretOnce": shown("The signing key. Shown in this response only."),

		// Arbitrary JSON, written as it was stored. json.RawMessage is an
		// alias of jsontext.Value.
		"encoding/json.RawMessage":     anySchema(),
		"encoding/json/jsontext.Value": anySchema(),
	}
}

// override is what a route reads or answers, where its handler does not say.
type override struct {
	// request is the request body.
	request *body
	// noRequestBody says a handler that reads the body for one method (a
	// SAML form post) takes none on this one.
	noRequestBody bool
	// responses replace, by status, what the reading found.
	responses map[int]*body
	// params are parameters the handler passes on whole, so their names are
	// not in its code.
	params []Parameter
}

// body is a request or response body: a schema in one content type, or a Go
// type named by package path and name. No content type is no body.
type body struct {
	contentType string
	schema      *Schema
	goType      [2]string
	description string
}

const (
	pkgReference = "github.com/trypando/pando/internal/reference"
)

func overrides() map[string]override {
	binary := func(contentType, what string) *body {
		return &body{contentType: contentType, schema: &Schema{Type: "string", ContentEncoding: "binary"}, description: what}
	}
	text := func(contentType, what string) *body {
		return &body{contentType: contentType, schema: &Schema{Type: "string"}, description: what}
	}
	query := func(name, what string) Parameter {
		return Parameter{Name: name, In: "query", Description: what, Schema: &Schema{Type: "string"}}
	}

	out := map[string]override{
		// RFC 6749 §4.1.3: the token request is a form, read with ParseForm.
		"POST /api/v1/oauth/token": {request: &body{contentType: "application/x-www-form-urlencoded", schema: &Schema{
			Type: "object", Required: []string{"grant_type", "code", "code_verifier"},
			Properties: Props{
				{Name: "grant_type", Schema: &Schema{Type: "string", Enum: []string{"authorization_code"}}},
				{Name: "code", Schema: &Schema{Type: "string"}},
				{Name: "code_verifier", Schema: &Schema{Type: "string"}},
				{Name: "client_id", Schema: &Schema{Type: "string"}},
				{Name: "redirect_uri", Schema: &Schema{Type: "string"}},
			},
		}}},
		// MCP's own JSON-RPC messages, answered by internal/mcp (R-413).
		"POST /api/v1/mcp": {
			request: &body{contentType: jsonType, schema: anySchema(), description: "A JSON-RPC 2.0 MCP message, or a batch of them."},
			responses: map[int]*body{
				200: {contentType: jsonType, schema: anySchema(), description: "The message's JSON-RPC answer."},
				202: {description: "A notification or a response, accepted, with nothing to answer."},
			},
		},
		// The image's bytes, in whichever of the accepted types it is: the
		// handler sets Content-Type from the stored image.
		"GET /api/v1/apps/{appID}/icon": {responses: map[int]*body{
			200: binary("image/*", "The app's image: PNG, JPEG, WebP or GIF."),
		}},
		// The image file itself as the body; the bytes decide its type.
		"PUT /api/v1/apps/{appID}/icon": {request: binary("image/*", "The image file: PNG, JPEG, WebP or GIF.")},

		// The upload is a gzipped tar of the source (design 04 §4).
		"POST /api/v1/apps/{appID}/source": {request: binary("application/gzip", "The app's source as a gzipped tar.")},

		// Served through a viewer interface the reading does not follow;
		// the events are described in the summary.
		"GET /api/v1/apps/{appID}/logs/stream": {responses: map[int]*body{
			200: text("text/event-stream", "Server-sent events: each `data:` line is a line of the app's output."),
		}},

		// The git host's push or release event, read raw so its signature can
		// be checked over the exact bytes.
		"POST /api/v1/apps/{appID}/auto-deploy/webhook": {request: &body{
			contentType: jsonType,
			schema:      &Schema{Type: "object", Description: "The event as GitHub sends it, signed in `X-Hub-Signature-256`."},
		}},

		// A GitHub App's event, read raw for the same reason.
		"POST /api/v1/sources/{sourceID}/webhook": {request: &body{
			contentType: jsonType,
			schema:      &Schema{Type: "object", Description: "The event as GitHub sends it, signed in `X-Hub-Signature-256`."},
		}},

		// The provider's answer is handed to core whole.
		"GET /api/v1/auth/providers/{providerID}/callback": {
			noRequestBody: true,
			params: []Parameter{
				query("code", "The authorization code an OpenID Connect provider returns."),
				query("state", "The state Pando sent at the start of the sign-in."),
				query("error", "Set by the provider when the sign-in failed."),
			},
		},
		"POST /api/v1/auth/providers/{providerID}/callback": {request: &body{
			contentType: "application/x-www-form-urlencoded",
			schema: &Schema{Type: "object", Properties: Props{
				{Name: "SAMLResponse", Schema: &Schema{Type: "string", Description: "The provider's signed SAML response, base64."}},
				{Name: "RelayState", Schema: &Schema{Type: "string"}},
			}},
		}},
		"GET /api/v1/auth/providers/{providerID}/metadata": {responses: map[int]*body{
			200: text("application/samlmetadata+xml", "Pando's SAML service provider metadata."),
		}},

		// Encoded once and served as bytes.
		"GET /api/v1/reference": {responses: map[int]*body{
			200: {contentType: jsonType, goType: [2]string{pkgReference, "Document"}},
		}},
	}

	// SCIM (RFC 7643, RFC 7644). The resources are built in core/idp as maps,
	// from what the provider sent and what Pando keeps of it.
	user := &body{contentType: scimType, schema: scimRef("ScimUser")}
	group := &body{contentType: scimType, schema: scimRef("ScimGroup")}
	list := &body{contentType: scimType, schema: scimRef("ScimListResponse")}
	patch := &body{contentType: scimType, schema: scimRef("ScimPatchOp")}
	discovery := &body{contentType: scimType, schema: &Schema{Type: "object", Description: "SCIM discovery (RFC 7644 §4)."}}
	scim := "/api/v1/scim/v2"
	scimUser, scimGroup := scim+"/Users/{id}", scim+"/Groups/{id}"
	for key, o := range map[string]override{
		"GET " + scim + "/ServiceProviderConfig": {responses: map[int]*body{200: discovery}},
		"GET " + scim + "/ResourceTypes":         {responses: map[int]*body{200: discovery}},
		"GET " + scim + "/Schemas":               {responses: map[int]*body{200: discovery}},
		"GET " + scim + "/Users":                 {responses: map[int]*body{200: list}},
		"POST " + scim + "/Users":                {request: user, responses: map[int]*body{201: user}},
		"GET " + scimUser:                        {responses: map[int]*body{200: user}},
		"PUT " + scimUser:                        {request: user, responses: map[int]*body{200: user}},
		"PATCH " + scimUser:                      {request: patch, responses: map[int]*body{200: user}},
		"GET " + scim + "/Groups":                {responses: map[int]*body{200: list}},
		"POST " + scim + "/Groups":               {request: group, responses: map[int]*body{201: group}},
		"GET " + scimGroup:                       {responses: map[int]*body{200: group}},
		"PUT " + scimGroup:                       {request: group, responses: map[int]*body{200: group}},
		"PATCH " + scimGroup:                     {request: patch},
	} {
		out[key] = o
	}
	return out
}

func scimRef(name string) *Schema { return &Schema{Ref: "#/components/schemas/" + name} }

// scimSchemas are the SCIM resources, as core/idp's userResource,
// groupResource and listResponse write them.
func scimSchemas() map[string]*Schema {
	str := func(d string) *Schema { return &Schema{Type: "string", Description: d} }
	strs := &Schema{Type: "array", Items: &Schema{Type: "string"}}
	member := &Schema{Type: "object", Properties: Props{
		{Name: "value", Schema: str("The ID.")},
		{Name: "display", Schema: str("")},
		{Name: "$ref", Schema: str("The resource's URL.")},
	}}
	meta := &Schema{Type: "object", Properties: Props{
		{Name: "resourceType", Schema: str("")},
		{Name: "location", Schema: str("")},
		{Name: "created", Schema: &Schema{Type: "string", Format: formatDateTime}},
		{Name: "lastModified", Schema: &Schema{Type: "string", Format: formatDateTime}},
	}}
	open := anySchema()
	return map[string]*Schema{
		"ScimUser": {
			Type:        "object",
			Description: "A SCIM user (RFC 7643 §4.1). Attributes the provider sent that Pando does not read are kept and returned.",
			Properties: Props{
				{Name: "schemas", Schema: strs},
				{Name: "id", Schema: str("The Pando account's ID.")},
				{Name: "userName", Schema: str("")},
				{Name: "externalId", Schema: str("The provider's ID for the person.")},
				{Name: "active", Schema: &Schema{Type: "boolean", Description: "False suspends the account."}},
				{Name: "groups", Schema: &Schema{Type: "array", Items: member}},
				{Name: "meta", Schema: meta},
			},
			AdditionalProperties: open,
		},
		"ScimGroup": {
			Type:        "object",
			Description: "A SCIM group (RFC 7643 §4.2).",
			Properties: Props{
				{Name: "schemas", Schema: strs},
				{Name: "id", Schema: str("")},
				{Name: "displayName", Schema: str("")},
				{Name: "externalId", Schema: str("")},
				{Name: "members", Schema: &Schema{Type: "array", Items: member}},
				{Name: "meta", Schema: meta},
			},
			AdditionalProperties: open,
		},
		"ScimListResponse": {
			Type:        "object",
			Description: "A page of SCIM resources (RFC 7644 §3.4.2).",
			Properties: Props{
				{Name: "schemas", Schema: strs},
				{Name: "totalResults", Schema: &Schema{Type: "integer"}},
				{Name: "startIndex", Schema: &Schema{Type: "integer"}},
				{Name: "itemsPerPage", Schema: &Schema{Type: "integer"}},
				{Name: "Resources", Schema: &Schema{Type: "array", Items: open}},
			},
			Required: []string{"schemas", "totalResults", "startIndex", "itemsPerPage", "Resources"},
		},
		"ScimPatchOp": {
			Type:        "object",
			Description: "SCIM PATCH operations (RFC 7644 §3.5.2).",
			Properties: Props{
				{Name: "schemas", Schema: strs},
				{Name: "Operations", Schema: &Schema{Type: "array", Items: &Schema{Type: "object", Properties: Props{
					{Name: "op", Schema: str("add, remove or replace, in any case.")},
					{Name: "path", Schema: str("")},
					{Name: "value", Schema: open},
				}}}},
			},
		},
		"ScimError": {
			Type:        "object",
			Description: "A SCIM error (RFC 7644 §3.12).",
			Properties: Props{
				{Name: "schemas", Schema: strs},
				{Name: "status", Schema: str("The HTTP status, as a string.")},
				{Name: "detail", Schema: str("")},
				{Name: "scimType", Schema: str("")},
			},
			Required: []string{"schemas", "status", "detail"},
		},
	}
}
