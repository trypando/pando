package reference

import (
	"fmt"
	"strings"
)

// EventsMarkdown is docs/events.md: every event a subscription can name, what a
// webhook receives, and how to check it came from Pando (R-364, R-369). Written
// from the catalog in internal/core/events, like the rest of the reference.
func EventsMarkdown(doc Document) string {
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("# Pando events\n\n")
	b.WriteString("A subscription sends the events it names to a webhook, or through a notification adapter —\n")
	b.WriteString("Slack, Microsoft Teams, Discord, email or ntfy. Make one in the console under **Events**, with\n")
	b.WriteString("`pando subscriptions create`, with `POST /api/v1/subscriptions`, or with the\n")
	b.WriteString("`pando_create_subscription` tool.\n\n")
	b.WriteString("A subscription is about one app, which needs `app.view` on it, or the whole installation,\n")
	b.WriteString("which needs `install.events.manage`. Every delivery is checked against its owner's access at\n")
	b.WriteString("the moment it is sent, so a subscription stops delivering when its owner loses sight of\n")
	b.WriteString("what it is about (R-368).\n\n")

	b.WriteString("## Choosing events\n\n")
	b.WriteString("A subscription's `events` is a list of names (`deploy.failed`), prefixes (`deploy.*`), or\n")
	b.WriteString("`*` for everything. A name that matches no event is refused when the subscription is saved.\n")
	b.WriteString("An **app** event reaches subscriptions on its app and install-wide ones; an **install** event\n")
	b.WriteString("reaches install-wide subscriptions only.\n\n")

	b.WriteString("## Webhooks\n\n")
	b.WriteString("Pando sends a `POST` with a JSON body to the subscription's URL:\n\n")
	b.WriteString("```json\n{\n  \"id\": \"evt_01J…\",\n  \"type\": \"deploy.failed\",\n  \"occurred_at\": \"2026-10-04T12:00:00Z\",\n" +
		"  \"app\": { \"id\": \"app_01HQ8…\", \"name\": \"Billing\", \"slug\": \"billing\" },\n" +
		"  \"actor\": { \"kind\": \"system\" },\n" +
		"  \"data\": { \"deployment_id\": \"dep_…\", \"error_code\": \"BUILD_FAILED\", \"message\": \"…\" }\n}\n```\n\n")
	b.WriteString("`app` is absent for an install event. `actor.kind` is `user`, `token`, `system` or\n")
	b.WriteString("`anonymous`. `data` holds the event's fields below and nothing else; no event carries a\n")
	b.WriteString("secret value (R-194).\n\n")
	b.WriteString("| Header | Meaning |\n| --- | --- |\n")
	b.WriteString("| `Pando-Event` | The event's name. |\n")
	b.WriteString("| `Pando-Event-Id` | The event's ID. A delivery can arrive more than once; drop one whose ID you have seen. |\n")
	b.WriteString("| `Pando-Delivery-Id` | This delivery's ID, as the delivery log shows it. |\n")
	b.WriteString("| `Pando-Timestamp` | When this attempt was signed, in Unix seconds. |\n")
	b.WriteString("| `Pando-Signature` | `v1=` and the hex HMAC-SHA256 of the timestamp, a full stop, and the body, keyed by the subscription's signing key. |\n\n")
	b.WriteString("### Checking a delivery came from Pando\n\n")
	b.WriteString("The signing key is shown once, when the subscription is made or its key is rotated. To\n")
	b.WriteString("check a delivery, compute the HMAC over the raw body exactly as received and compare it in\n")
	b.WriteString("constant time; refuse a timestamp more than five minutes from your clock, so a recorded\n")
	b.WriteString("delivery cannot be replayed:\n\n")
	b.WriteString("```python\nimport hashlib, hmac, time\n\n" +
		"def from_pando(key: str, headers, body: bytes) -> bool:\n" +
		"    ts = headers[\"Pando-Timestamp\"]\n" +
		"    if abs(time.time() - int(ts)) > 300:\n" +
		"        return False\n" +
		"    mac = hmac.new(key.encode(), ts.encode() + b\".\" + body, hashlib.sha256).hexdigest()\n" +
		"    return hmac.compare_digest(\"v1=\" + mac, headers[\"Pando-Signature\"])\n```\n\n")
	b.WriteString("### Retries and turning off\n\n")
	b.WriteString("A `2xx` answer is a delivery. Anything else — another status, a redirect, a timeout after 15\n")
	b.WriteString("seconds — is retried after 1 minute, 5 minutes, 30 minutes, 2 hours, 6 hours and 12 hours,\n")
	b.WriteString("then marked failed. Every attempt is recorded and shown in the delivery log, and any\n")
	b.WriteString("delivery can be sent again from it. An endpoint that has failed every attempt for a day is\n")
	b.WriteString("turned off, and its owner is told (R-370). Turning it back on clears the record.\n\n")
	b.WriteString("Delivery is at least once and survives a restart: events are written to an outbox in\n")
	b.WriteString("the same transaction as what they describe (R-366).\n\n")
	b.WriteString("### Private addresses\n\n")
	b.WriteString("A webhook may not reach a private, loopback or link-local address — the local network,\n")
	b.WriteString("the host itself, a cloud metadata service — unless host policy's `allow_private_webhooks` is\n")
	b.WriteString("on (R-372). The check is made on the address actually connected to, and redirects are not\n")
	b.WriteString("followed.\n\n")

	b.WriteString("## Events\n\n")
	b.WriteString("| Event | About | What happened | Data |\n| --- | --- | --- | --- |\n")
	for _, e := range doc.Events {
		fields := make([]string, 0, len(e.Fields))
		for _, f := range e.Fields {
			fields = append(fields, "`"+f.Name+"` "+f.Description)
		}
		fmt.Fprintf(&b, "| `%s` | %s | %s | %s |\n", e.Name, e.Scope, e.Summary, strings.Join(fields, "<br>"))
	}
	b.WriteString("\nAudit-sourced events also carry `target_kind` and `target_id` in `data` when the action had\n")
	b.WriteString("a target.\n")
	return b.String()
}
