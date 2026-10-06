# Reference

Pando's external interfaces — everything that goes in and everything that comes out. Start here when
you need to know what a surface accepts rather than how it is built.

R-261 makes one of these authoritative: **the HTTP API is the product.** The console, the CLI and the
MCP server are clients of it, and none has a capability the API lacks. When two documents here
disagree, the API reference is the one that is right.

The first three are **generated from the code that serves them** by `make reference`, and CI fails
when they are out of date. They describe the binary rather than the intent — when one of them
disagrees with a design document, the design document is the one that is behind.

| Interface | Reference |
|---|---|
| HTTP API — every endpoint, verb and error code | [`api.md`](api.md) (generated) |
| CLI — every command and flag | [`cli.md`](cli.md) (generated) |
| MCP — every tool and its arguments | [`mcp.md`](mcp.md) (generated) |
| HTTP API, as designed | [`design/04-api.md`](design/04-api.md) |
| App spec — the object almost everything operates on | [`design/01-spec-schema.md`](design/01-spec-schema.md) |
| Adapter interfaces — for contributing one | [`design/03-adapter-interfaces.md`](design/03-adapter-interfaces.md) |
| Authorization verbs and the proxy's request contract | [`design/06-authorization-and-proxy.md`](design/06-authorization-and-proxy.md) |

---

## HTTP API

Base path `/api/v1`. JSON in and out. Every response carries an `X-Request-Id`.

Authentication, in precedence order:

1. `Authorization: Bearer tok_…` — a token principal, from `pando token`.
2. `Cookie: pando_session=ses_…` — a browser session, from `POST /api/v1/sessions`.

Resources, in full, are in [`design/04-api.md`](design/04-api.md) §2: apps, deploys, specs, slots,
secrets, environment, volumes, grants, users, groups, roles, host policy, backups, tokens and the
audit log.

**Errors** are one envelope everywhere. The `code` is stable and machine-readable; the `message` and
`remedy` are written to be pasted into an assistant and acted on without further context (R-105).

```json
{
  "code": "PLAN_SLOT_UNFILLED",
  "message": "This app needs a PostgreSQL database, and one hasn't been chosen yet.",
  "remedy": "Choose how to fill the database slot: provision one inside this app, connect to an existing one, or paste a connection string.",
  "details": { "slots": [{ "key": "database", "type": "postgres" }] },
  "request_id": "req_01HQ8…"
}
```

Code prefixes and what they mean: `AUTH_*` the caller is not who they need to be, `PERM_*` they are
but may not do this, `POLICY_*` host policy forbids it for everyone, `PLAN_*` the app cannot be
deployed as configured, `VALID_*` the request is malformed, `STATE_*` the object is in the wrong
state for this, `CAPACITY_*` a resource is exhausted, `BACKUP_*` and `ADAPTER_*` name their subsystem.
A `WARN_*` is never a blocker — anything that blocks is a `PLAN_*` error.

**IDs** are prefixed, sortable and opaque: `app_`, `spec_`, `usr_`, `tok_`, `vol_`, `gr_`, `ses_`,
`req_`, with a ULID body. Treat them as strings; the prefix is for reading, not parsing.

**Time** is RFC 3339, UTC, everywhere on the wire.

**Pagination** is cursor-based: `?limit=50&cursor=…`, and the response carries `next_cursor`.

**Idempotency**: `POST` endpoints that create infrastructure accept an `Idempotency-Key` header, and
a retry replays rather than repeats.

## What an app receives

An app deployed on Pando is handed three things, and the distinction between the first and the second
is the whole security model.

**`X-Pando-Assertion`** — a signed Ed25519 JWT describing the caller. This is the only statement about
identity an app should trust. Verify it against the JWKS Pando publishes at `/.well-known/jwks.json`.
Claims: `sub` (stable per user, independent of email and of the identity adapter), `email`, `name`,
`groups`, `aud` (the app's ID — this is what stops an assertion minted for one app being replayed
against another), `iat`, `exp`, `iss`. Valid for 120 seconds.

**Other `X-Pando-*` headers** — the same information, unsigned, for convenience. Explicitly not
trustworthy on their own (R-053). Inbound `X-Pando-*` headers from a client are stripped
unconditionally before a request reaches an app, so they cannot be forged; but an app that reads them
instead of verifying the assertion is trusting Pando's proxy rather than a signature, and there is no
way for it to tell the difference if the proxy is ever bypassed.

**Environment variables** — the app's own configuration, plus anything injected by a filled slot
(database credentials and connection strings) and any secret bound to it. Nothing is read from the
repository at deploy time (R-020): the spec is the sole record of how an app runs.

An app never receives a `pando_*` cookie. Those are stripped on the way out (R-173).

## Configuration

Set as environment variables, or in a config file. The environment prefix is `PANDO_`, and a nested
setting is joined with an underscore: `server.base_domain` is `PANDO_SERVER_BASE_DOMAIN`.

| Variable | Default | Purpose |
|---|---|---|
| `PANDO_DATABASE_URL` | the bundled Postgres | Point Pando at an existing database. |
| `PANDO_SERVER_ADDR` | `:8080` | Address the console and API listen on. |
| `PANDO_SERVER_BASE_DOMAIN` | `localtest.me` | Domain per-app subdomains are taken from, under hostname routing. |
| `PANDO_SERVER_ROUTING_MODE` | port | How apps are addressed: port, subdomain or path. |
| `PANDO_SERVER_ISSUER` | derived | The `iss` claim in identity assertions. |
| `PANDO_SERVER_EXTERNAL_URL` | — | The address browsers reach this installation on, such as `https://pando.example.com`. Set it when something other than Pando terminates TLS: it is what marks the session cookie `Secure`. Unset means "use the request", which is right on a localhost install and when Pando serves TLS itself. |
| `PANDO_SERVER_PROXY_UPSTREAM` | — | Where the proxy sends traffic it has authorized. |
| `PANDO_SERVER_WORK_DIR` | `/var/lib/pando` | Build contexts, uploads and adapter state. |
| `PANDO_APP_PORT_START` / `_END` | `9000` / `9019` | Range of host ports apps are allocated. |
| `PANDO_ADMIN_PASSWORD` | generated | Initial admin password. Read on first run only. |
| `PANDO_LOG_LEVEL` | `info` | Log verbosity. |
| `PANDO_APPS_DOCKER_CREDENTIALS` | `false` | Pull private images with the Docker login on the Pando server (`docker login`, read from `DOCKER_CONFIG` or `~/.docker/config.json`, credential helpers included) for apps that have no registry credential of their own. Every app on the install can then pull whatever that login can read. When Pando runs in the shipped Compose stack, mount a `config.json` into the container and set `DOCKER_CONFIG` to its directory; a credential helper such as the macOS keychain is not reachable from inside the container. |
| `PANDO_RECONCILER_BACKOFF` | see R-149 | Retry schedule. Compressing it is for tests; `pando` warns when it is set faster than the shipped default. |

`PANDO_PORT` is not read by Pando. It is a variable in the shipped `docker-compose.yml`, which uses
it to choose the host port published in front of the container's fixed `8080`.

A config file is read only when one is named with `pando serve --config <path>`. The environment
wins over the file.

### Host policy at startup

Any host policy setting can also be fixed at startup, in a `policy:` section of the config file or
as `PANDO_POLICY_<SETTING>`:

```yaml
policy:
  min_security_score: 70
  disabled_verbs: [app.exec]
  public_sharing: passcode_only   # allowed, passcode_only or none
```

```sh
PANDO_POLICY_MIN_SECURITY_SCORE=70
PANDO_POLICY_DISABLED_VERBS=app.exec,app.secrets.read   # lists are comma-separated
PANDO_POLICY_PUBLIC_SHARING=passcode_only
```

A setting fixed this way overrides what is saved in the console, applies everywhere policy is
checked, and cannot be changed from the console, the API or the CLI while it is set: the console
shows it disabled, with where it is set, and `PUT /api/v1/policy` refuses a change to it. It is
never written into the saved policy, so removing it and restarting brings back what was saved. A
name that is not a policy setting, or a value that does not read as one, stops Pando at startup
rather than being ignored.

The settings are `source_allowlist`, `disabled_verbs`, `agent_disabled_verbs`,
`public_sharing`, `allow_anonymous_grants`, `min_build_isolation`, `min_runtime_isolation`,
`egress_mode`, `egress_list`, `egress_block_private`, `egress_loosening`, `egress_allowlist`,
`deploy_approval_required`, `deploy_approval_apps`, `deploy_approval_count`,
`deploy_approval_expiry_hours`, `require_backup_before_destroy`, `max_token_lifetime_days`,
`max_log_disk_bytes`, `disable_ai_screening`, `disable_anonymous_use_audit`,
`disable_password_sign_in`, `disable_jit_provisioning`, `min_security_score`, `insecure_action`,
`insecure_grace_hours` and `ignore_unfixable_findings`.

`public_sharing` is how an app may be shared with everyone: `allowed` (with or without a passcode),
`passcode_only`, or `none`. The older `allow_anonymous_grants: false` still means `none` when
`public_sharing` is unset.

`egress_mode` is where apps may connect out to: `allow_all` (the default), `denylist` (anywhere except
`egress_list`) or `allowlist` (only `egress_list`). `egress_block_private` blocks private addresses
under any mode. `egress_loosening` is whether an app may loosen those rules — add to the allowlist,
remove from the denylist, unblock private addresses: `verb` (the default; whoever holds
`app.egress.loosen`), `approval` (its deploy needs approval) or `forbidden`. Apps may always narrow
them. The older `egress_allowlist` still means `allowlist` mode when `egress_mode` is unset.

`deploy_approval_required` makes every app's deploys wait for approval, and `deploy_approval_apps`
does so for the listed app IDs. `deploy_approval_count` is how many approvals a deploy needs (0 means
one) and `deploy_approval_expiry_hours` how long a request waits (default 168; 0 means until somebody
answers).

`GET /api/v1/config`, `pando config` and the Policy screen list every setting Pando started with,
its value, and where it came from. Secrets are never shown.

### Adapters at startup

Adapters, and the AI functions each one handles, can be declared in an `adapters:` section of the
config file instead of being added in the console. There is no environment-variable form.

```yaml
adapters:
  ai_anthropic:                  # the adapter's ID
    category: ai
    kind: anthropic
    name: Anthropic
    config:
      model: claude-opus-5-5
    credentials:
      api_key: {env: ANTHROPIC_API_KEY}   # or {file: /run/secrets/anthropic_key}
    functions:                   # a list of names, or names with a model of their own
      repair_plan: {}
      answer_questions: {}
      revise_plan: {}
      search_audit: {model: claude-haiku-4-5}
  ai_local:                      # a model on this machine, served by Ollama
    category: ai
    kind: local
    config:
      base_url: http://host.docker.internal:11434/v1
      model: qwen2.5:7b
    functions: [answer_reference, draft_access]
```

The AI adapter kinds are `anthropic`, `openai` (with `api_key` from `OPENAI_API_KEY` by default; it uses
OpenAI's Responses API, which stores each conversation on OpenAI's servers, not Pando's) and `local`, for any server that speaks the OpenAI API: Ollama, LM Studio, llama.cpp's server or vLLM. A
local server needs no key, and `model` is required because no model is on every server.

`category` and `kind` are required. `name`, `default`, `enabled`, `config`, `credentials` and
`functions` are optional; `functions` is for AI adapters only. The AI functions are `repair_plan`,
`answer_questions`, `revise_plan`, `draft_access`, `draft_policy`, `search_audit` and
`answer_reference`. A function no adapter handles is off, and Pando works without it.

Credentials are never written in the file. Each names an environment variable or a file to read at
startup, and a value written inline, or a key under `config` that looks like a credential, stops
Pando at startup. A declared adapter whose variable is unset or whose file cannot be read is logged
and skipped, like any adapter that fails to start.

A declared adapter overrides a saved adapter with the same ID, a saved AI adapter of the same kind,
and a saved default in its category; a declared function overrides the function's saved assignment.
While declared, they cannot be changed from the console, the API, the CLI or MCP, and each refusal
names the file and key. Nothing saved is deleted: the Adapters screen and `GET /api/v1/adapters` show
an overridden adapter as overridden, and removing the declaration and restarting brings it back.
Declared adapters and adapters added in the console can be used together.

A declaration that contradicts itself stops Pando at startup with an error naming both keys: two
default adapters in one category, two AI adapters of one kind (an installation has one per
provider), one AI function under two adapters, or two services adapters that provide the same kind
of service.

## Guarantees worth relying on

These are requirements, not implementation details, and they will not be changed without a major
version:

- A user's `sub` is stable across email changes and across a change of identity adapter (R-054). An
  app may key its own data on it.
- The audit log is append-only, at the database grant level (R-027). Nothing that happened stops
  having happened.
- Spec revisions are append-only, enforced by a database trigger, so a rollback target cannot be
  rewritten under you. The last ten pinned specs are retained (R-152).
- Deleting an app decides what happens to its data rather than assuming. Interactively Pando asks
  whether to keep a final backup (R-204); non-interactively, through the CLI, API or MCP, it backs up
  by default and `--force` is what skips it (R-205).
- A failed app stays failed until a person acts (R-151). Nothing retries it back into existence.
- Warnings never block. Anything that blocks is a `PLAN_*` error, and that split is deliberate: a
  warning shaped like an error teaches people to ignore both.

## Other documents

- [`../SECURITY.md`](../SECURITY.md) — the security model, the cryptography in use, and how to report
  a vulnerability.
- [`releasing.md`](releasing.md) — version numbering, and how to verify a download's signature.
- [`../CONTRIBUTING.md`](../CONTRIBUTING.md) — building it, and what a change needs before it merges.
- [`requirements.md`](requirements.md) — what Pando is, as 210 numbered requirements. The authority
  behind everything above.
- [`traceability/requirements-index.md`](traceability/requirements-index.md) — generated: which
  requirement is implemented where, and which have acceptance tests.
