package reference

import (
	"encoding/json"
	"fmt"
	"strings"
)

// EventsGuide is what a person needs to subscribe to events and receive them:
// how to subscribe, how events are chosen, and what a webhook receives and how
// to check it came from Pando (R-364, R-369).
//
// One copy of the text. EventsMarkdown renders it as docs/events.md and Events
// puts it in docs/events.json, so the page and the site cannot say different
// things. Plain text: paragraphs separated by a blank line, `backticks` for
// code, and nothing else of Markdown.
type EventsGuide struct {
	Subscribing string       `json:"subscribing"`
	Matching    string       `json:"matching"`
	Webhook     WebhookGuide `json:"webhook"`

	// DataNote is what every audit-sourced event's data also carries.
	DataNote string `json:"data_note"`
}

// WebhookGuide is what a webhook endpoint receives.
type WebhookGuide struct {
	Intro            string          `json:"intro"`
	BodyExample      json.RawMessage `json:"body_example"`
	Notes            string          `json:"notes"`
	Headers          []WebhookHeader `json:"headers"`
	Verifying        string          `json:"verifying"`
	Examples         []CodeExample   `json:"examples"`
	Retries          string          `json:"retries"`
	PrivateAddresses string          `json:"private_addresses"`
	Shaping          string          `json:"shaping"`
	TemplateExample  string          `json:"template_example"`
	TemplateNotes    string          `json:"template_notes"`
}

// WebhookHeader is one header Pando sends with a delivery.
type WebhookHeader struct {
	Name    string `json:"name"`
	Meaning string `json:"meaning"`
}

// CodeExample is a code sample in one language.
type CodeExample struct {
	Language string `json:"language"`
	Code     string `json:"code"`
}

func eventsGuide() EventsGuide {
	return EventsGuide{
		Subscribing: "A subscription sends the events it names to a webhook, or through a notification adapter — " +
			"Slack, Microsoft Teams, Discord, email or ntfy. Make one in the console under Events, with " +
			"`pando subscriptions create`, with `POST /api/v1/subscriptions`, or with the " +
			"`pando_create_subscription` tool.\n\n" +
			"A subscription is about one app, which needs `app.view` on it, or the whole installation, " +
			"which needs `install.events.manage`. Every delivery is checked against its owner's access at " +
			"the moment it is sent, so a subscription stops delivering when its owner loses sight of " +
			"what it is about.",
		Matching: "A subscription's `events` is a list of names (`deploy.failed`), prefixes (`deploy.*`), or " +
			"`*` for everything. A name that matches no event is refused when the subscription is saved. " +
			"An app event reaches subscriptions on its app and install-wide ones; an install event " +
			"reaches install-wide subscriptions only.",
		Webhook: WebhookGuide{
			Intro: "Pando sends a `POST` with a JSON body to the subscription's URL:",
			BodyExample: json.RawMessage(`{
  "id": "evt_01J…",
  "type": "deploy.failed",
  "occurred_at": "2026-10-04T12:00:00Z",
  "app": { "id": "app_01HQ8…", "name": "Billing", "slug": "billing" },
  "actor": { "kind": "system" },
  "data": { "deployment_id": "dep_…", "error_code": "BUILD_FAILED", "message": "…" }
}`),
			Notes: "`app` is absent for an install event. `actor.kind` is `user`, `token`, `system` or " +
				"`anonymous`. `data` holds the event's fields and nothing else; no event carries a " +
				"secret value.",
			Headers: []WebhookHeader{
				{"Pando-Event", "The event's name."},
				{"Pando-Event-Id", "The event's ID. A delivery can arrive more than once; drop one whose ID you have seen."},
				{"Pando-Delivery-Id", "This delivery's ID, as the delivery log shows it."},
				{"Pando-Timestamp", "When this attempt was signed, in Unix seconds."},
				{"Pando-Signature", "`v1=` and the hex HMAC-SHA256 of the timestamp, a full stop, and the body, keyed by the subscription's signing key."},
			},
			Verifying: "The signing key is shown once, when the subscription is made or its key is rotated. To " +
				"check a delivery, compute the HMAC over the raw body exactly as received and compare it in " +
				"constant time; refuse a timestamp more than five minutes from your clock, so a recorded " +
				"delivery cannot be replayed:",
			Examples: []CodeExample{{Language: "python", Code: "import hashlib, hmac, time\n\n" +
				"def from_pando(key: str, headers, body: bytes) -> bool:\n" +
				"    ts = headers[\"Pando-Timestamp\"]\n" +
				"    if abs(time.time() - int(ts)) > 300:\n" +
				"        return False\n" +
				"    mac = hmac.new(key.encode(), ts.encode() + b\".\" + body, hashlib.sha256).hexdigest()\n" +
				"    return hmac.compare_digest(\"v1=\" + mac, headers[\"Pando-Signature\"])\n"}},
			Retries: "A `2xx` answer is a delivery. Anything else — another status, a redirect, a timeout after 15 " +
				"seconds — is retried after 1 minute, 5 minutes, 30 minutes, 2 hours, 6 hours and 12 hours, " +
				"then marked failed. Every attempt is recorded and shown in the delivery log, and any " +
				"delivery can be sent again from it. An endpoint that has failed every attempt for a day is " +
				"turned off, and its owner is told. Turning it back on clears the record.\n\n" +
				"Delivery is at least once and survives a restart: events are written to an outbox in " +
				"the same transaction as what they describe.",
			PrivateAddresses: "A webhook may not reach a private, loopback or link-local address — the local network, " +
				"the host itself, a cloud metadata service — unless host policy's `allow_private_webhooks` is " +
				"on. The check is made on the address actually connected to, and redirects are not " +
				"followed.",
			Shaping: "For a receiver that expects a particular request, a webhook may set `method` (POST, PUT or " +
				"PATCH), `content_type`, `headers` of its own — for an API key the receiver needs; values are " +
				"stored encrypted and never shown again — and `payload_template`, the body as a Go template. " +
				"Pando's `Pando-*` headers are always sent and cannot be set, and the signature covers the body " +
				"actually sent. A template is given `.ID`, `.Type`, `.OccurredAt`, `.App` (`.ID`, `.Name`, " +
				"`.Slug`), `.Actor`, `.Data`, `.Subject`, `.Body`, `.Fields`, `.Link` and `.Envelope`, and the " +
				"functions `json` (a value as JSON) and `default`. For a chat tool that takes a `text` field:",
			TemplateExample: `{"text": {{json .Subject}}, "url": {{json .Link}}}`,
			TemplateNotes: "A template is checked against a sample event when it is saved, and a JSON content type needs " +
				"it to produce JSON. `link` points into the console when Pando's `external_url` is set.",
		},
		DataNote: "Audit-sourced events also carry `target_kind` and `target_id` in `data` when the action had " +
			"a target.",
	}
}

// EventsMarkdown is docs/events.md: every event a subscription can name, what a
// webhook receives, and how to check it came from Pando. Written from the
// catalog in internal/core/events and from eventsGuide, like the rest of the
// reference.
func EventsMarkdown(doc Document) string {
	g := eventsGuide()
	w := g.Webhook
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("# Pando events\n\n")
	b.WriteString(g.Subscribing + "\n\n")

	b.WriteString("## Choosing events\n\n")
	b.WriteString(g.Matching + "\n\n")

	b.WriteString("## Webhooks\n\n")
	b.WriteString(w.Intro + "\n\n")
	b.WriteString("```json\n" + string(w.BodyExample) + "\n```\n\n")
	b.WriteString(w.Notes + "\n\n")
	b.WriteString("| Header | Meaning |\n| --- | --- |\n")
	for _, h := range w.Headers {
		fmt.Fprintf(&b, "| `%s` | %s |\n", h.Name, h.Meaning)
	}
	b.WriteString("\n### Checking a delivery came from Pando\n\n")
	b.WriteString(w.Verifying + "\n\n")
	for _, e := range w.Examples {
		b.WriteString("```" + e.Language + "\n" + e.Code + "```\n\n")
	}
	b.WriteString("### Retries and turning off\n\n")
	b.WriteString(w.Retries + "\n\n")
	b.WriteString("### Private addresses\n\n")
	b.WriteString(w.PrivateAddresses + "\n\n")
	b.WriteString("### Shaping the request\n\n")
	b.WriteString(w.Shaping + "\n\n")
	b.WriteString("```\n" + w.TemplateExample + "\n```\n\n")
	b.WriteString(w.TemplateNotes + "\n\n")

	b.WriteString("## Events\n\n")
	b.WriteString("| Event | About | What happened | Data |\n| --- | --- | --- | --- |\n")
	for _, e := range doc.Events {
		fields := make([]string, 0, len(e.Fields))
		for _, f := range e.Fields {
			fields = append(fields, "`"+f.Name+"` "+f.Description)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", e.Name, e.Scope, e.Summary, strings.Join(fields, "<br>"))
	}
	b.WriteString("\n" + g.DataNote + "\n")
	return b.String()
}
