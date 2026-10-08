# 12 — Audit Streaming and Export

Issue #129. Requirements R-379 – R-392, with R-228 implemented as written and R-252 amended.

An installation's security team investigates from its SIEM, not from each product's console, and keeps
evidence longer than Pando does. Before this, the audit log could be read newest first through
`GET /audit` and downloaded a month at a time once a month had aged out (R-347). Neither is a feed.
This document designs a **commit-ordered cursor** over the live log, a pull endpoint that serves it,
**audit sinks** that push it to syslog and HTTPS collectors, **OCSF** as the one SIEM schema shipped,
**on-demand export** of any range, and the **fields and coverage** a stream needs to be worth reading.

---

## 1. What a SIEM gets, and what it does not

| | `GET /audit` | Audit stream (this document) | Event subscriptions (design 11) | Archives (R-347) |
|---|---|---|---|---|
| What | Any slice of the live log | **Every** audit event | The catalog's events, the catalog's fields | One month past retention |
| Order | Newest first | Commit order | Outbox `seq` | `id` |
| Resumable | No | Yes, from a cursor | Per delivery | n/a |
| Detail | Whole | Whole | Trimmed to the `Def` (design 11 §2 [D]) | Whole |
| Kept | Until archived | Until archived | 30 days | As policy says |

**[D]** The stream is not a subscription kind. Design 11 trims an event to the fields its `Def` names,
on purpose, and drops an outbox row after 30 days; a SIEM fed from it would get a curated subset,
silently. The stream reads `audit_events` itself.

**[D]** Pando ships events out. It does not index, search or analyze them beyond what `GET /audit` and
the audit search already do (R-010, R-016).

## 2. The cursor

`audit_events.id` comes from a sequence. A sequence value is taken when a row is inserted, not when its
transaction commits, so two concurrent writers — two requests on one replica, or two replicas (#72) —
can commit 41 after 42. A reader that has seen 42 and asks for "after 42" never sees 41. Paging by `id`
is fine for `GET /audit`, which reads a settled past; it is wrong for a tail.

**[D]** Each row records the transaction that wrote it: `txid xid8 NOT NULL DEFAULT pg_current_xact_id()`
(migration 000067). The stream reads, in order of `(txid, id)`, only rows whose `txid` is below
`pg_snapshot_xmin(pg_current_snapshot())` — the oldest transaction still running. Every transaction
below that horizon has committed or rolled back, so no row can later appear behind the cursor. A
transaction still in flight holds the horizon back, which delays delivery but never skips a row.

- `xid8` is 64-bit and does not wrap. It is assigned per cluster, so every replica writing to the one
  database shares it (R-256).
- Rows written before 000067 have `txid` 0 and sort by `id`. All of them committed long ago.
- A row moved from the default partition into a month's partition (`audit_ensure_partition`) keeps its
  `txid` and `id`, so a move never makes an event reappear or disappear.
- What holds the horizon back is a long-running transaction that has *written*, anywhere in the
  cluster. Pando's own writes are short. A migration or a manual `psql` session left open delays the
  stream for as long as it stays open; the destination's backlog shows it. On a Postgres server that
  hosts other databases too, their transactions count: a busy neighbor adds latency, never a gap.
  The test suites share one server between many databases, which is why their stream tests wait for
  events rather than expecting them at once.

The cursor on the wire is opaque: `c1.<txid>.<id>`. An empty cursor starts at the oldest event in the
live log. `now` starts after the newest event the horizon has passed, for a consumer that wants only
what happens next.

**[D]** Delivery is at least once. A consumer that loses its acknowledgment re-reads from its last
stored cursor and sees events twice; each event's `id` is its idempotency key.

## 3. What every event carries

Migration 000067 adds, as nullable columns so existing rows keep NULL:

| Column | Meaning | Filled by |
|---|---|---|
| `schema_version` | `2` for rows from 000067 on; NULL (read as 1) before | Default |
| `outcome` | `success`, `denied` or `failed` | `Event.Outcome`, else derived from the action (§3.1) |
| `source_ip` | The client's address (R-380) | Request context |
| `peer_ip` | The TCP peer that connected to Pando | Request context |
| `user_agent` | The client's `User-Agent`, at most 512 bytes | Request context |
| `actor_name`, `actor_email` | The acting user's display name and email at the time | Looked up in the insert |
| `txid` | §2 | Default |

**[D]** `target_kind` is always set (R-379). An event naming an app and no target is about the app
(`app` / `app_id`); one naming neither is about the installation (`install` / `install`). The writer
fills these in, so the 104 places an `audit.Event` is built did not each have to change, and a new one
cannot forget.

**[D]** Source, peer and user agent come from the request context, set by the API's middleware and the
proxy, so the writer fills them in for every in-request event and leaves them NULL for system events.

**[D]** The actor's name and email are looked up in the same `INSERT` (`SELECT … FROM users WHERE id =
coalesce(on_behalf_of, principal_id)`), so a SIEM that cannot join on Pando's user IDs still has a
human name, and a renamed user's past events keep the name they acted under.

### 3.1 Outcome

`Event.Outcome` wins when set — a finished deploy sets `success` or `failed`. Otherwise:

- an action ending `.denied` or `.refused` is `denied`;
- an action ending `.failed`, or `app.failed`, `app.delete.backup_failed`, `backup.verify.failed`,
  `upgrade.rolled_back`, is `failed`;
- everything else is `success`.

`TestR379_EveryActionHasAnOutcome` holds every catalogued action against this, so a new
`something.denied` cannot be recorded as a success.

### 3.2 Client address (R-380)

**[D]** Pando records the TCP peer unless the peer is in `server.trusted_proxies`, a list of CIDRs. From a
trusted peer it reads `X-Forwarded-For` **from the right** and takes the first address that is not itself
trusted: the left of that header is whatever the client wrote, and the right is what each trusted hop
appended. An empty list — the default — is today's behavior.

- A catch-all entry (`0.0.0.0/0`, `::/0`, or any prefix shorter than /8 for IPv4 or /16 for IPv6) is
  refused at startup. It would make the header the client's to choose.
- A list that is too narrow records the proxy's address, which is what Pando recorded before. It is
  not a hole.
- `peer_ip` is always recorded beside `source_ip`, so the hop the address came from is visible.

This is a trusted-proxy list, which O-19 decided against for the session cookie. The cases differ: the
scheme browsers use is one value an operator can state once (`PANDO_SERVER_EXTERNAL_URL`), and a client
address is different on every request and exists behind a proxy only in the header. O-19 carries a note
saying so. The list is used for the audit address and nothing else: it does not decide `Secure`,
rate limits, or anything an attacker would gain from a forged address beyond a wrong line in the log.

Rate limiting (passcodes, sign-in) keeps keying on the TCP peer. Behind a proxy that peer is the proxy,
which is the existing behavior; moving rate limits onto a forwarded address is a separate decision.

## 4. Reading the stream

```
GET /api/v1/audit/stream?after=<cursor>&limit=<n>&wait=<seconds>&action=<prefix>&exclude=<prefix>
→ 200 { "events": [ <native line>, … ], "cursor": "c1.…", "caught_up": true }
```

- `install.audit.read`, like `GET /audit`. Reading the log in order is reading the log.
- Events are the native line format (§6.1), oldest first. `limit` defaults to 500, capped at 1000.
- `wait` (0–60, default 0) long-polls: when nothing is past the cursor, the request holds until an event
  is, or the wait runs out, and returns an empty page with the same cursor.
- `action` and `exclude` are repeatable prefixes. The cursor always advances past events a filter
  dropped, so a filtered reader never re-reads them.
- `format=ocsf` returns OCSF objects instead (§6.2).
- **CLI:** `pando audit tail [--follow] [--after c1.…] [--format native|ocsf]` prints one line per event
  and, with `--follow`, keeps long-polling. The last cursor is written to stderr on exit so a script can
  resume.
- **MCP:** `pando_read_audit_stream` returns one page. An agent asked "what happened since I last
  looked" passes back the cursor it was given.

## 5. Audit sinks

### 5.1 A category

**[D]** `audit_sink` is the thirteenth adapter category (R-252 amended, R-382). It passes design 03
§8.1's test on vocabulary: Splunk HEC wants `Authorization: Splunk <token>` and an `event` envelope,
Datadog a `DD-API-KEY` header and `ddsource`, Elastic `_bulk` action lines, Azure Monitor a data
collection rule, a syslog collector RFC 5424 framing and a client certificate. Core should not know
any of those. Building them as a subscription destination would be the adapter-shaped thing in core
that §8.1 says not to build.

```go
type AuditSinkAdapter interface {
    Adapter
    AuditSinkCapabilities() AuditSinkCapabilities
    // Send delivers a batch, in order. A nil error means every event in it was
    // accepted; anything else means none should be counted, and core retries
    // the batch. An adapter never sees the cursor and never reads the log.
    Send(ctx context.Context, b AuditBatch) error
}

type AuditSinkCapabilities struct {
    MaxBatch  int    // events per Send; core never exceeds it
    Format    string // "native" or "ocsf": what core encodes events as
    Actions   []string // prefixes to send; empty is every action
    Exclude   []string // prefixes not to send, such as app.use
    StartAtNow bool    // a new destination starts after the newest event
    Transport string // "syslog" or "https", for the console's disclosure line
    Endpoint  string // host[:port] it sends to, never with a credential
}

type AuditBatch struct {
    Events []json.RawMessage // encoded in Capabilities().Format, oldest first
    IDs    []int64           // the events' ids, for an adapter that wants an idempotency key
}
```

An adapter receives bytes core has already encoded. It cannot read the log, cannot see another event,
and cannot write audit (R-027, R-226): `internal/adapter/auditsink` is under the depguard rule like every
other adapter.

Two kinds ship:

- **`audit_sink/syslog`** — RFC 5424 over TCP with TLS (RFC 5425 octet-counted framing), or plain TCP
  when the operator says so. Settings: `address`, `tls` (`on`, `off`), `ca_certificate`,
  `server_name`, `app_name`, `facility`. Credentials: `client_certificate`, `client_key` for mutual TLS.
  The message is the event, encoded, as MSG; `MSGID` is the action; structured data carries
  `[pando@32473 id="…" outcome="…"]`.
- **`audit_sink/https`** — a batch `POST` of newline-delimited JSON (or a JSON array, per `body`).
  Settings: `url`, `body` (`ndjson`, `json_array`, `splunk_hec`, `elastic_bulk`), `auth_header` (the header name),
  `auth_scheme` (`Bearer`, `Splunk`, none), `format`, `extra_headers` (non-secret). Credential:
  `token`. Presets fill these in for Splunk HEC (`splunk_hec` wraps each event as `{"event": …,
  "sourcetype": "pando:audit"}`), Datadog Logs, Elastic (`elastic_bulk` puts `{"create":{}}` before
  each event, for a data stream's `_bulk` endpoint), Sumo Logic HTTP source, and the Azure Monitor Logs
  ingestion API for Sentinel (`json_array`, to a data collection rule's stream). Google SecOps and anything else that takes NDJSON with a token are the generic
  preset.

Both refuse a credential in plain configuration, as every adapter does (R-190), and an `https` URL
carrying `user:pass@` is refused by a CHECK, as 000066 does for registries.

### 5.2 Delivery

`internal/core/auditstream` is the engine. It runs as the leader job `audit-stream` (R-256: one
delivering replica, so a destination never receives two interleaved copies), every 2 seconds, and on
each pass, for every enabled `audit_sink` adapter:

1. Builds the adapter from its row and sealed credentials, as `core/imageregistry` does, so a rotated
   token is used on the next pass on whichever replica leads.
2. Reads up to `MaxBatch` events past the destination's cursor (§2), filtered by its `actions` and
   `exclude` settings (§5.3), and encodes them.
3. Calls `Send`. On success, stores the new cursor, `delivered_at`, and clears the error, in one
   statement. On failure, stores the error and schedules the next attempt.

State lives in `audit_sink_state` (one row per adapter): `cursor_txid`, `cursor_id`, `delivered_at`,
`delivered_count`, `last_error`, `last_error_at`, `failing_since`, `attempts`, `next_attempt_at`,
`disabled_at`, `disabled_reason`, `gap_from`, `gap_to`. Backlog is computed on read — events past the
cursor — and capped at 100 000 in the count, since counting a large backlog exactly is the cost of the
backlog again.

**[D]** Retry and disable reuse design 11's schedule: immediately, 1 m, 5 m, 30 m, 2 h, 6 h, 12 h, then
every 12 h. A destination failing for 24 hours and at least 5 attempts in a row is **disabled**:
`adapter_configs.enabled` set false, `audit.sink.disable` written, and every holder of
`install.audit.export` notified (`audit_sink_disabled`). Turning it back on is enabling the adapter.

**[D]** Its first failure writes `audit.sink.fail`, and its first success after failing writes
`audit.sink.recover`. Not one per attempt: a destination down for a day would otherwise write a hundred
events about itself into the stream it cannot deliver.

A new destination starts at the oldest event in the live log unless created with `start: now`.

### 5.3 What a destination receives

`actions` (prefixes, default all) and `exclude` (prefixes, default none) narrow it. **[D]** `app.use` is
included by default (R-384): a security team asking who used an app expects the answer to be in the
SIEM, and leaving it out by default would be the silent subset §1 refuses. A team whose SIEM bills by
ingest sets `exclude: app.use`. The console says, beside the setting, that `app.use` is usually most of
the volume.

### 5.4 Retention (R-386)

**[D]** The archiver neither archives nor removes a month while an **enabled** destination has events
in it past its cursor. It writes `audit.archive.held` once per month held, naming the destinations, and
tries again at its next pass. Archiving first and holding only the drop would record an archive for
rows still in the live log, which is what a later gap check reads as removed. A destination Pando **disabled**
stops holding: once its month is dropped, its state records the range it missed (`gap_from`,
`gap_to`), the console shows it, and the month's archive is offered as the backfill. A destination
that is merely behind holds; one that is broken past §5.2's threshold does not, so a dead collector
cannot grow the live log without bound (R-224).

## 6. Formats

### 6.1 Native

One JSON object per event: `row_to_json` of the `audit_events` row, as the archiver writes it
(R-347). Live, streamed, exported and archived events are therefore one shape by construction, and a
column added later appears in all four at once. `schema_version` says which columns to expect.

### 6.2 OCSF

OCSF 1.3.0. One object per event. The mapping is a table in code
(`internal/core/audit/ocsf`), keyed by every catalogued action, and `TestR384_EveryActionMapsToOCSF`
fails when an action has no row — there is no fallthrough. `docs/audit-formats.md` is generated from
that table by `make reference`; it is the field-by-field reference.

Base fields:

| OCSF | From |
|---|---|
| `class_uid`, `category_uid`, `activity_id`, `type_uid` | The action's row; `type_uid = class_uid × 100 + activity_id` |
| `time` | `occurred_at`, epoch milliseconds |
| `severity_id` | 1 Informational for success, 3 Medium for denied, 4 High for failed |
| `status_id` / `status` | 1 Success, 2 Failure; `status_detail` is `denied` or `failed` |
| `message` | The action, as `<action> by <actor> on <target>` |
| `metadata.uid` | `id` |
| `metadata.product` | `{name: "Pando", vendor_name: "Pando", version}` |
| `metadata.version` | `1.3.0` |
| `metadata.correlation_uid` | `request_id` |
| `actor.user` | `{uid, type, name, email_addr}` from principal and `actor_*`; `type` is `User`, `Token`, `System` or unset for anonymous |
| `actor.invoked_by` | `on_behalf_of`, for a token acting for a person (R-229, R-262) |
| `src_endpoint.ip` | `source_ip` |
| `http_request.user_agent` | `user_agent` |
| `api.operation` | `action` |
| `resources[0]` | `{type: target_kind, uid: target_id}`; plus `{type: "app", uid: app_id}` when both differ |
| `unmapped` | `detail`, whole (it never holds a secret, R-194), plus `peer_ip` and `schema_version` |

Classes used, by family:

| Family | OCSF class | Activities |
|---|---|---|
| `session.*` | 3002 Authentication | 1 Logon (`create`, `denied` as a failed logon), 2 Logoff (`revoke`) |
| `user.*`, `user.password.*`, `user.scim.*` | 3001 Account Change | 1 Create, 3 Password Change, 4 Password Reset, 5 Disable (`suspend`), 2 Enable (`activate`), 6 Delete, 99 Other |
| `grant.*`, `authz.install_wide` | 3005 User Access Management | 1 Assign Privileges, 2 Revoke Privileges, 99 Other (`authz.install_wide`) |
| `group.*` | 3006 Group Management | 6 Create, 5 Delete, 3 Add User, 4 Remove User, 99 Other |
| `app.use`, `app.use.denied`, `app.passcode.*`, `authz.denied` | 6004 Web Resource Access Activity | 1 Access Grant, 2 Access Deny |
| `app.create`, `app.delete`, `app.start`, `app.stop`, `app.restart`, `app.deploy`, `deploy.finish` | 6002 Application Lifecycle | 1 Install, 2 Remove, 3 Start, 4 Stop, 5 Restart, 8 Update |
| `app.exec`, `app.exec.end` | 1007 Process Activity | 1 Launch, 2 Terminate |
| Everything else | 6003 API Activity | 1 Create, 2 Read, 3 Update, 4 Delete, 99 Other |

`role.*` is API Activity rather than User Access Management: creating a role defines a set of verbs and
grants them to nobody. `actor.user.type_id` is 1 User, 3 System, or 99 Other for a token. Process
Activity's `process` object is not filled — the audit log records that a session ran, not a process — so
a validator that requires it will flag `app.exec` and `app.exec.end`.

CEF and ECS are not shipped (R-384). A destination that wants them is served by a collector that maps
OCSF or native JSON; a third format is a follow-up issue, added as a row in the same table.

## 7. On-demand export (R-387)

```
GET /api/v1/audit/export?since=<RFC 3339>&until=<RFC 3339>&format=native|ocsf&action=…
→ 200 application/gzip, Content-Disposition: attachment; filename="audit-<since>-<until>.jsonl.gz"
```

Gzipped JSON lines, in commit order, from the live log only: a range reaching past retention is
answered with what the live log holds and a `Pando-Audit-Live-From` header naming the oldest
event's time, so the caller knows to fetch archives for the rest. Streamed from a cursor, so an export of a
year is not held in memory. `install.audit.read`. CLI: `pando audit export --since … --until …
[--format ocsf] > file.jsonl.gz`. MCP has no export tool, for the reason it has no archive download
(design 04): a gzip of the log is not something an agent's context can use, and the stream tool serves
the same events a page at a time.

## 8. Coverage

Fixed by this change, each with a test named for its requirement:

| Gap | Now |
|---|---|
| Exec has no end (R-228) | `app.exec.end`: `duration_ms`, `reason` (`client_closed`, `server_closed`, `timeout`, `error`) |
| A deploy's outcome is not an audit event | `deploy.finish`, `system`, `outcome` `success`/`failed`, `error_code`, `deployment_id`, `spec_revision` (R-389) |
| A rollback does not say what it rolled back from | `app.deploy` with `trigger: rollback` carries `rolled_back_from` (R-389) |
| Reading the audit log leaves no trace | `audit.read` (list, stream), `audit.export`, `audit.archive.download`; the AI search already writes `ai.search_audit` (R-388) |
| Policy and spec edits do not say what changed | `policy.update` and `spec.create` carry `changed` — field paths — and `before`/`after` for non-secret values (R-390) |
| Log reads | `app.logs.read` when logs are opened or streamed (R-392) |

**[P]** `audit.read` for the stream is written once per principal per hour per replica, not once per
long-poll: a collector polling every two seconds would otherwise put 43 000 events a day about itself
into the log. Export and archive download are written every time.

**[D]** `TestR391_EveryMutatingRouteAudits` walks the router, as `TestR261_EveryRouteIsDocumented` does,
and fails for any non-`GET` route under `/api/v1` that is neither in `auditedRoutes` (with the actions
it writes) nor in `auditExempt` (with the reason). A new route has to be in one or the other.

## 9. Authorization and disclosure (R-385)

**[D]** `install.audit.export` is a new install verb: sending the audit log off the installation. It is
the Administrator's alone among built-in roles (migration 000067, R-081). Creating, changing,
enabling or deleting an `audit_sink` adapter needs it **and** `install.adapters.manage`; an auditor
can read the stream and export ranges with `install.audit.read`, and cannot point the log somewhere
new. `adapter.configure` already records every change, with the fields changed and never a value;
`audit.sink.fail`, `audit.sink.recover` and `audit.sink.disable` record the rest.

**[D]** The console shows every enabled destination on the Audit log screen as what it is — "Every
audit event is sent to splunk.example.com:8088 over HTTPS" — with its cursor, backlog, last error and
any gap, beside the log itself, as R-337 does for AI calls. Nobody reading the audit log should be
able to miss that a copy is leaving.

## 10. Not decided here

- Hash-chaining events, so a consumer can prove it holds a complete stream, stays with **O-27**. The
  cursor gives completeness to a consumer that trusts Pando; a chain would give it to one that does
  not, for the stream and archives alike, and is one mechanism for both.
- OTLP as a sink, if #126 adopts OpenTelemetry: it would be a third `audit_sink` kind.
