package subscription

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
)

// How a webhook is sent, for a receiver that expects a particular request
// (R-375): its method, content type, headers of its own, and a body template.

// Limits on what a subscription may ask for [P].
const (
	maxHeaders      = 20
	maxHeaderValue  = 4096
	maxTemplate     = 16 << 10
	maxRenderedBody = 256 << 10
)

var methods = map[string]bool{"POST": true, "PUT": true, "PATCH": true}

// headerName is RFC 9110's token: what an HTTP field name may be.
var headerName = regexp.MustCompile("^[A-Za-z0-9!#$%&'*+.^_`|~-]+$")

// reservedHeaders are set by Pando or by the transport, and a subscription
// may not set them: a forged Pando-Signature would defeat the signature, and
// the rest describe the request the transport is making.
var reservedHeaders = map[string]bool{
	"host": true, "content-length": true, "content-type": true, "transfer-encoding": true,
	"connection": true, "keep-alive": true, "te": true, "trailer": true, "upgrade": true,
	"proxy-authorization": true, "proxy-connection": true, "user-agent": true,
}

// checkHeaders validates custom headers and returns their names, sorted, in
// their canonical spelling.
func checkHeaders(headers map[string]string) ([]string, map[string]string, error) {
	if len(headers) > maxHeaders {
		return nil, nil, errs.Newf(errs.ValidInvalid, "A webhook may have at most %d headers of its own.", maxHeaders)
	}
	out := make(map[string]string, len(headers))
	names := make([]string, 0, len(headers))
	for name, value := range headers {
		name = strings.TrimSpace(name)
		if !headerName.MatchString(name) {
			return nil, nil, errs.Newf(errs.ValidInvalid,
				"%q is not a header name. Use letters, digits and hyphens, such as Authorization or X-Api-Key.", name)
		}
		lower := strings.ToLower(name)
		if reservedHeaders[lower] || strings.HasPrefix(lower, "pando-") {
			return nil, nil, errs.Newf(errs.ValidInvalid,
				"Pando sets the %s header itself, so a webhook cannot. Choose the content type with content_type; Pando-* headers carry the event and its signature.", name)
		}
		if strings.ContainsAny(value, "\r\n") || len(value) > maxHeaderValue {
			return nil, nil, errs.Newf(errs.ValidInvalid,
				"The value of the %s header must be one line of at most %d characters.", name, maxHeaderValue)
		}
		canonical := httpCanonical(name)
		if _, dup := out[canonical]; dup {
			return nil, nil, errs.Newf(errs.ValidInvalid, "The %s header is given twice.", canonical)
		}
		out[canonical] = value
		names = append(names, canonical)
	}
	sort.Strings(names)
	return names, out, nil
}

func httpCanonical(name string) string {
	parts := strings.Split(strings.ToLower(name), "-")
	for i, p := range parts {
		if p != "" {
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, "-")
}

func checkMethod(method string) (string, error) {
	m := strings.ToUpper(strings.TrimSpace(method))
	if m == "" {
		return "POST", nil
	}
	if !methods[m] {
		return "", errs.Newf(errs.ValidInvalid, "%q is not a method a webhook can use. Valid answers: POST, PUT or PATCH.", method)
	}
	return m, nil
}

func checkContentType(ct string) (string, error) {
	ct = strings.TrimSpace(ct)
	if ct == "" {
		return "application/json", nil
	}
	if strings.ContainsAny(ct, "\r\n") || !strings.Contains(ct, "/") {
		return "", errs.Newf(errs.ValidInvalid,
			"%q is not a content type. Use one such as application/json or application/x-www-form-urlencoded.", ct)
	}
	return ct, nil
}

// TemplateData is what a body template is given: the event as the envelope
// carries it, and the same event described for a person.
//
//	{"text": {{json .Subject}}, "url": {{json .Link}}}
//	{{.Type}} on {{.App.Name}}: {{index .Data "error_code"}}
type TemplateData struct {
	ID         string
	Type       string
	OccurredAt time.Time
	App        EnvelopeApp
	Actor      Actor
	Data       map[string]any

	// Subject, Body and Fields are the event as a notification describes it.
	Subject string
	Body    string
	Fields  []api.NotificationField
	Link    string

	// Envelope is Pando's own body, for a template that wraps it.
	Envelope Envelope
}

var templateFuncs = template.FuncMap{
	// json renders a value as JSON: a string quoted and escaped, which is how
	// a value goes into a JSON body safely.
	"json": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	// default is its second argument unless that is empty.
	"default": func(fallback, v any) any {
		if v == nil || v == "" {
			return fallback
		}
		return v
	},
}

func parseTemplate(text string) (*template.Template, error) {
	return template.New("payload").Funcs(templateFuncs).Option("missingkey=zero").Parse(text)
}

// checkTemplate parses a body template and renders it for a sample event, so
// a mistake is found when the subscription is saved rather than in its
// delivery log. A JSON content type needs the result to be JSON.
func checkTemplate(text, contentType string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	if len(text) > maxTemplate {
		return errs.Newf(errs.ValidInvalid, "A body template may be at most %d characters.", maxTemplate)
	}
	t, err := parseTemplate(text)
	if err != nil {
		return errs.New(errs.ValidInvalid, "The body template could not be read: "+err.Error()).
			WithRemedy("Templates use Go template syntax, such as {\"text\": {{json .Subject}}}. docs/events.md lists what a template is given.")
	}
	sample := state.Event{
		ID: "evt_sample", Name: "deploy.failed", AppID: "app_sample", ActorKind: "system",
		Data:       map[string]any{"error_code": "BUILD_FAILED", "message": "The build exited with status 1."},
		OccurredAt: time.Now().UTC(),
	}
	body, err := render(t, sample, &EnvelopeApp{ID: "app_sample", Name: "Sample", Slug: "sample"}, "")
	if err != nil {
		return errs.New(errs.ValidInvalid, "The body template could not be rendered for a sample event: "+err.Error())
	}
	if isJSON(contentType) && !json.Valid(body) {
		return errs.New(errs.ValidInvalid,
			"The body template does not produce JSON, and the content type is "+contentType+". "+
				"Put a value in with json, such as {\"text\": {{json .Subject}}}, or choose another content type.")
	}
	return nil
}

func isJSON(contentType string) bool {
	ct := strings.ToLower(contentType)
	return strings.HasPrefix(ct, "application/json") || strings.Contains(ct, "+json")
}

// render produces a delivery's body from a template.
func render(t *template.Template, e state.Event, app *EnvelopeApp, link string) ([]byte, error) {
	n := Describe(e, app)
	data := TemplateData{
		ID: e.ID, Type: e.Name, OccurredAt: e.OccurredAt.UTC(),
		Actor: Actor{Kind: e.ActorKind, ID: e.ActorID, OnBehalfOf: e.OnBehalfOf},
		Data:  e.Data, Subject: n.Subject, Body: n.Body, Fields: n.Fields, Link: link,
		Envelope: envelopeOf(e, app, link),
	}
	if app != nil {
		data.App = *app
	}
	if data.Data == nil {
		data.Data = map[string]any{}
	}
	var buf bytes.Buffer
	if err := t.Execute(&limitWriter{w: &buf, left: maxRenderedBody}, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// limitWriter stops a template that would render an unbounded body.
type limitWriter struct {
	w    *bytes.Buffer
	left int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.left {
		return 0, fmt.Errorf("the body would be larger than %d bytes", maxRenderedBody)
	}
	l.left -= len(p)
	return l.w.Write(p)
}

// bodyFor is a delivery's body: the subscription's template, or the envelope.
func bodyFor(sub state.Subscription, e state.Event, app *EnvelopeApp, link string) ([]byte, error) {
	if strings.TrimSpace(sub.PayloadTemplate) == "" {
		return jsonMarshal(envelopeOf(e, app, link))
	}
	t, err := parseTemplate(sub.PayloadTemplate)
	if err != nil {
		return nil, sentence("The body template could not be read: " + err.Error())
	}
	body, err := render(t, e, app, link)
	if err != nil {
		return nil, sentence("The body template could not be rendered for this event: " + err.Error())
	}
	return body, nil
}
