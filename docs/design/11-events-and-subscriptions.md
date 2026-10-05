# 11 — Events and Subscriptions

Issue #50. Requirements R-364 – R-374, with R-231, R-232, R-266 and R-362 amended.

Before this, Pando told people about four things through one adapter that reached only someone already
looking at the console, and two of the four were never sent. This document designs an **event catalog**,
an **outbox** fed from what Pando already records, **subscriptions** that send events to signed
**webhooks** or to **notification channels**, and per-person **preferences** for the notifications Pando
sends on its own.

---

## 1. Two kinds of message

| | Pando's own notifications | Subscriptions |
|---|---|---|
| Who decides it is sent | Pando: a deploy needs *your* approval | A person who subscribed |
| Addressed to | Named people | A webhook, or a channel, or the subscriber |
| Vocabulary | `NotificationKind`: `app_failed`, `deploy_approval`, … | The event catalog: `deploy.failed`, … |
| Reaches | Every adapter whose audience is **people**, per preferences | The one destination the subscription names |
| Managed under | Settings → Notifications | Events |

**[D]** The two are kept apart because their audiences are. A notification that a deploy needs *your*
approval must never be posted to a Slack channel because an administrator configured one, and an event
subscription must not need a person to exist for each message. R-373 makes the split structural: a notify
adapter says, as capabilities data (R-254), whether it reaches **people** or posts to a **channel**, and
the router that sends Pando's own notifications skips channels.

## 2. The catalog

`internal/core/events` — a leaf package, imported by the audit writer, the state store and the delivery
engine. One `Def` per event: name, scope (app or install), source (audit, state, core), summary, fields,
and for an audit-sourced event the audit actions it is copied from. `GET /events`, `docs/events.md`
(written by `make reference`), the CLI, MCP and the console all read it (R-364).

**[D]** An event's `data` holds the fields its `Def` lists and nothing else. The audit writer copies those
keys from the audit detail and drops the rest, so a field somebody later adds to an audit detail does not
reach a subscriber's endpoint without being written down here first. This is also R-194's second line:
`token.create`'s detail is never forwarded, only `name` and `kind`.

**[D]** Names are `area.what_happened`, past tense. A filter is a name, a prefix (`deploy.*`, whole
segments only) or `*`. A filter that matches nothing is refused at save time (`VALID_UNKNOWN_EVENT`).

**[P]** Health is not its own event. An app's health is its state (`running` ↔ `degraded`, design 05 §1),
so `app.state_changed` carries it. A separate `app.health_changed` would announce one transition twice.

## 3. The outbox

```sql
events (id evt_…, seq, name, app_id, actor_kind, actor_id, on_behalf_of, request_id, data, occurred_at, routed_at)
```

Three writers, one table (R-365):

1. **The audit writer.** `audit.Writer.Write` now writes the audit row and, when the action is
   catalogued, the event, in one transaction. Both or neither.
2. **Triggers**, for state that changes without an audited action: `apps.state`
   (`app.state_changed`), `deployments.status` reaching `succeeded`/`failed` (`deploy.*` — the deploy runner
   audits the start, not the end), `backups` insert (`backup.created`, however it was taken) and
   `backup_attempts` with outcome `failed` (`backup.failed` for the scheduled backup nobody is waiting on).
   A trigger is the one place every writer of a column passes through; a call in Go would be the one place
   somebody forgets. Triggers mint IDs with `pando_ulid()`, the same shape `internal/id` makes, so an event
   from either source parses, sorts and reads alike.
3. **Core**, for the two events that are neither: `adapter.unhealthy` / `adapter.recovered`, from a health
   check of every adapter every five minutes, and `subscription.test`.

**[D]** No foreign key from `events.app_id`. `app.deleted` outlives the app.

**[P]** An event with nothing left to deliver is removed after 30 days. The audit log is the history
(R-224, R-347); this is a queue.

## 4. Subscriptions

```sql
subscriptions (id sub_…, owner_user_id → users | owner_token_id → tokens, app_id → apps NULL, events text[],
               destination webhook|notify, url, adapter_id, method, content_type, payload_template,
               header_names text[], description, enabled, disabled_reason, failing_since, consecutive_failures, …)
subscription_secrets (subscription_id, field signing_key|header:<Name>, adapter_ref, ciphertext, external_ref, …)
event_deliveries (id dlv_…, subscription_id, event_id, status pending|succeeded|failed, attempts,
                  next_attempt_at, last_*, redelivered_by, round_base, UNIQUE (subscription_id, event_id))
delivery_attempts (delivery_id, attempt, attempted_at, status_code, error, duration_ms)
notification_preferences (user_id, kind, channel, enabled)
```

**[D] A subscription belongs to a person or to an account token.** Deliveries are authorized as the owner
when they are sent (R-368). A delegated token makes one for the person it acts for (R-058); an account
token owns its own (R-060), bounded by its own grants like any principal, and authorized at send time as
itself — refused once it is revoked or expired (`Tokens.Active`). A token is not a person: it has no inbox
and no email address, so its subscription may send to a webhook or a channel adapter and never to an
adapter whose audience is people.

**[D] Authorization (R-368):**

| | Needs |
|---|---|
| Subscribe to an app | `app.view` on it — anyone who can see an app may hear about it |
| Subscribe install-wide | `install.events.manage` |
| See, change, delete a subscription | Its owner, or `install.events.manage` |
| Each delivery | The owner, live: active, and still holding `app.view` (app) or `install.events.manage` (install) |

`install.events.manage` is a new install verb, in Administrator by migration 000043 (R-081). It is not an
`everyApp` counterpart: there is no app verb it stands for. An install-wide subscription hears sign-ins and
policy changes, which seeing every app does not cover. An app subscription the owner can no longer see is
dropped at send time with the reason in the delivery log, not deleted: the grant may come back.

**[D]** A subscription hears what happens after it is made. An event still waiting to be routed when a
subscription is created is older than it and is not routed to it.

## 5. Delivery

The dispatcher (`internal/core/subscription`) runs one loop: **route**, then **send**.

- **Route.** Take up to 100 unrouted events in `seq` order `FOR UPDATE SKIP LOCKED`, match them against
  enabled subscriptions, insert a delivery per match, mark each event routed — one transaction. An event
  route cannot decide about stays unrouted for the next pass.
- **Send.** Claim up to 32 due deliveries by pushing `next_attempt_at` out by a two-minute lease, send up to
  eight at once, record each attempt. A Pando that dies mid-send leaves the lease to expire and the delivery
  to be sent again: at least once (R-366).

**Webhook (R-369).** `POST` the envelope (`id`, `type`, `occurred_at`, `app`, `actor`, `data`) with
`Pando-Event`, `Pando-Event-Id`, `Pando-Delivery-Id`, `Pando-Timestamp` and
`Pando-Signature: v1=<hex HMAC-SHA256(key, timestamp + "." + body)>`. Signed at send time, so a rotated key
applies to deliveries not yet sent. 2xx is delivered; anything else, a 3xx included, is a failure. 15-second
timeout.

**[P] Retry schedule:** immediately, then after 1 minute, 5 minutes, 30 minutes, 2 hours, 6 hours and
12 hours — about a day — then `failed`. Redelivering starts the schedule again (`round_base`).

**[P] Turning off (R-370):** every attempt failed for 24 hours **and** at least five in a row. Both, so a
quiet installation's one slow failure and a busy one's burst during a short outage each do not. The owner
gets `subscription_disabled` through the router; `subscription.disable` is audited and so is itself an
event.

**Notify destination.** The event is described as a notification (`Describe`: subject naming the app and
what happened, the message or reason as body, the remaining fields as labeled values) and handed to the
adapter. An adapter that reaches people gets the owner as recipient; a channel gets none.

## 6. SSRF (R-372)

Anyone who can see an app can subscribe to it, so a webhook URL is input from a person who may hold
nothing else. Unchecked, it is a way to make Pando send requests into its own network.

**[D]** Refused by default: private (RFC 1918, ULA), loopback, link-local (169.254.169.254 among them),
CGNAT, unspecified and multicast addresses, IPv4 carried in IPv6 judged as the IPv4 it carries —
`egress.Private` plus unspecified and multicast. Host policy's `allow_private_webhooks` lifts it.

Enforced twice. `CheckURL` resolves the host when the subscription is saved, for a refusal a person can
read (`POLICY_WEBHOOK_PRIVATE_ADDRESS`). The enforcing check is in the dialer's `Control` hook, on the
address actually connected to after DNS, so a name that resolves somewhere public at save time and private
at send time is still refused. Redirects are not followed and no proxy is read from the environment, so a
public endpoint cannot bounce a delivery inward. Which client is used — guarded or not — is decided from
policy on every send.

**[D]** Adapter endpoints (a Slack webhook URL, an SMTP host, an ntfy server) are not guarded. They are set
by an administrator holding `install.adapters.manage`, the same trust as every other adapter's endpoint, and
an ntfy server on the local network is an ordinary thing to want.

## 7. Notification adapters (R-374)

| Kind | Audience | Credential | Package |
|---|---|---|---|
| `console` | people | — | `notify/console` |
| `smtp` | people | `password` | `notify/smtp` |
| `slack` | channel | `webhook_url` (hooks.slack.com) | `notify/chat` |
| `teams` | channel | `webhook_url` (a Workflows webhook) | `notify/chat` |
| `discord` | channel | `webhook_url` (discord.com) | `notify/chat` |
| `ntfy` | channel | `topic`, `access_token` | `notify/ntfy` |

Slack, Teams and Discord differ only in layout, so they are one package with a `Platform` value each.
Slack escapes `&`, `<`, `>` so an app named `<!channel>` pings nobody; Discord sends
`allowed_mentions: {parse: []}` for the same reason. A webhook URL is the credential, and an error from a
failed post never includes it. SMTP sends one message per recipient, so nobody sees who else was told, and
its health check connects and signs in without sending.

**[D]** An ntfy topic is a credential: on ntfy.sh anyone who knows its name reads it.

**[P]** Pushover is not built. It needs two credentials and a per-person user key, which is a preferences
feature rather than an adapter; a pull request can add it (R-253).

## 8. Preferences (R-373)

`notification_preferences` holds choices a person made; a missing row is the kind's default. Every kind
is on by default except `app_shared` (R-266): the launcher tile is the notification, and an email is for
someone who asks. `GET /notification-preferences` returns every kind on every people channel with defaults
filled in, so a client shows the answer rather than computing it.

The router replaces `registryNotifier`. It also fixes gaps that predate this work: the reconciler's
`Notifier` was never set in `main`, so `app_failed` was never sent; the security pass wrote
`policy_violation` straight to the console store, past the registry; and nothing sent `deploy_failed` or
`backup_failed` (§10).

## 9. Webhook requests (R-375)

A receiver that expects a particular request — an API key in a header, a body shaped for a chat tool or
an incident service — gets it without a relay in between.

- **Method** POST, PUT or PATCH. **Content type** anything with a `/`; `application/json` by default.
- **Headers** of the subscription's own, at most 20, each one line of at most 4 KB. A header Pando or the
  transport sets is refused: every `Pando-*` (so the signature is always Pando's), and `Host`,
  `Content-Type`, `Content-Length`, `User-Agent`, `Connection` and the other hop-by-hop names. Values are
  sealed in `subscription_secrets` under `header:<Name>`; `header_names` on the row lets a screen say what is
  sent without showing a value. Changing headers replaces the whole set.
- **Body template**, Go `text/template`, given `.ID`, `.Type`, `.OccurredAt`, `.App`, `.Actor`, `.Data`,
  `.Subject`, `.Body`, `.Fields`, `.Link` and `.Envelope`, with `json` (a value as JSON) and `default`.
  Empty sends the envelope. Rendered at send time, capped at 256 KB; the signature covers the rendered
  body. Checked at save time by rendering a sample `deploy.failed`, and with a JSON content type the
  result must be JSON, so a mistake is a 400 rather than a delivery log of failures.

`text/template` runs no code a template author supplies beyond these functions, and a template sees only
the event a subscription's owner may already see.

## 10. Failure notifications (R-376)

`deploy_failed` and `backup_failed` are sent from the outbox, after each routing pass, rather than from the
code that failed, so a deploy that fails anywhere tells the same people the same way:

| Kind | Recipients |
|---|---|
| `deploy_failed` | The app's owner; whoever created the deployment — a person, or the person a delegated token acts for. Not Pando itself, not an account token. |
| `backup_failed` | The app's owner; everyone holding `install.backup.manage`. |

Recipients are de-duplicated and go through the router, so preferences and channel audience apply. A Pando
that stops between routing and sending loses the notification and not the event, which subscriptions still
receive.

## 11. The inbox and an app's events (R-377, R-378)

The console adapter has always recorded notifications; nothing displayed them. `GET /me/notifications`
lists a person's, newest first, with an unread count; one or all are marked read, by that person alone,
with the user ID in the `WHERE` clause. The console shows a bell beside Settings on the launcher and the
admin console, with the count, polling every minute. `notifications` gains `link` and `event_id`.

`GET /apps/{id}/events` (`app.view`) is the app's events from the outbox, newest first, each as the
envelope and as `Describe` reads it. The console's app view has an **Events** tab with the feed and the
app's subscriptions; a subscription made there is about the app without asking.

**Links.** When `server.external_url` is set, a delivery's envelope, a template's `.Link`, a notification
and the inbox carry `<external_url>/admin/apps/<id>/events`, or `/admin/events` for an install event.
Unset, they carry none rather than a guess.

## 12. Surfaces

| Surface | |
|---|---|
| API | `GET /events`, `/subscriptions` CRUD, `/signing-key`, `/test`, `/deliveries`, `/redeliver`, `/notification-preferences`, `/me/notifications`, `/apps/{id}/events` (design 04 §2.8a) |
| CLI | `pando events [--app]`, `pando subscriptions …` (with `--method`, `--content-type`, `--header`, `--template-file`), `pando notifications preferences|list|read` |
| MCP | Every endpoint but rotating a signing key, which returns a credential (O-12) |
| Console | **Events** in the admin sidebar for anyone who administers an app; an **Events** tab on each app; the inbox bell; **Notifications** under Settings; **Event webhooks** on the Policy screen |
