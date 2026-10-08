package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/reference"
)

// The API's own description (R-261).
//
// chi knows every route's method and pattern and nothing about what any of them
// is for, so the summaries live here, beside the router, and
// `TestEveryRouteIsDocumented` walks the real router and fails when this table
// and that router disagree in either direction. Adding an endpoint without
// describing it fails the build; so does describing one that no longer exists.
// That is the whole mechanism, and it is the reason this file is worth
// maintaining: the reference cannot rot quietly, because rot is a red test.
//
// The verb on each row is the authorization verb the handler checks (design 06
// §5). Empty means the endpoint asks for nothing beyond being signed in — which
// is an answer somebody needs, not an omission.
var routeDocs = []reference.Route{
	// --- session and account ---------------------------------------------
	{Method: "POST", Path: "/api/v1/sessions", Group: "Session", Summary: "Sign in with a username and password. Sets the session cookie. Refused when host policy has `disable_password_sign_in` on."},
	{Method: "GET", Path: "/api/v1/auth/options", Group: "Session", Summary: "How people can sign in here: whether password sign-in is on, and the identity providers that are (`id`, `name`, `kind`). Public."},
	{Method: "GET", Path: "/api/v1/auth/providers/{providerID}/start", Group: "Session", Summary: "Start signing in through an identity provider: a browser navigation, redirected to the provider. `next` is the path to land on afterwards. Works under `/.pando` on an app's own hostname too. Public."},
	{Method: "GET", Path: "/api/v1/auth/providers/{providerID}/callback", Group: "Session", Summary: "Where an OpenID Connect provider returns the browser: the redirect URI to register with it. Public."},
	{Method: "POST", Path: "/api/v1/auth/providers/{providerID}/callback", Group: "Session", Summary: "Where a SAML provider posts its response: the Assertion Consumer Service URL to register with it. Public."},
	{Method: "GET", Path: "/api/v1/auth/providers/{providerID}/metadata", Group: "Session", Summary: "Pando's SAML service provider metadata for this provider. Its URL is also the entity ID. Public."},
	{Method: "GET", Path: "/api/v1/auth/complete", Group: "Session", Summary: "Finish a provider sign-in in the browser that started it, with the one-time `code` the callback issued. Sets the session cookie and redirects to where the sign-in began. Public."},
	{Method: "GET", Path: "/api/v1/auth/failures/{flowID}", Group: "Session", Summary: "Why a provider sign-in failed (`message`, `remedy`), for the sign-in page. Public."},
	{Method: "DELETE", Path: "/api/v1/sessions", Group: "Session", Summary: "Sign out, ending this session."},
	{Method: "GET", Path: "/api/v1/setup", Group: "Session", Summary: "Whether this installation is waiting for its first administrator (`needed`). Public."},
	{Method: "POST", Path: "/api/v1/setup", Group: "Session", Summary: "Set up a new installation: the first account (`username`, `display_name`, `password`), made an administrator, and signed in. Public, and refused once any account exists (R-046)."},
	{Method: "GET", Path: "/api/v1/me", Group: "Session", Summary: "Who the caller is, and the install-level verbs they hold."},
	{Method: "POST", Path: "/api/v1/me/password", Group: "Session", Summary: "Change your own password. Yours only, whatever verbs you hold."},
	{Method: "GET", Path: "/api/v1/me/apps", Group: "Session", Summary: "The apps you can open, which is a different list from the apps you can administer (R-070, R-071). `favorite` marks the ones you have pinned, `section_id` the section you filed each under, and `sections` lists your sections. `can_manage` marks the ones you can also administer — those GET /apps lists for you. Favorites first, then apps filed in a section, then the rest, each by name, a page at a time: `limit` (default 100, at most 500), `cursor` (the previous page's `next_cursor`, empty after the last page) and `q` to match the name or slug."},
	{Method: "PUT", Path: "/api/v1/me/favorites/{appID}", Group: "Session", Summary: "Mark an app you can open as a favorite, pinning it to the top of your launcher. Yours only; it grants nothing (R-341)."},
	{Method: "DELETE", Path: "/api/v1/me/favorites/{appID}", Group: "Session", Summary: "Unpin an app from your favorites."},
	{Method: "POST", Path: "/api/v1/me/sections", Group: "Session", Summary: "Make a section in your launcher: a named, collapsible grouping of apps. Yours only; it grants nothing (R-342)."},
	{Method: "PATCH", Path: "/api/v1/me/sections/{sectionID}", Group: "Session", Summary: "Rename one of your sections."},
	{Method: "DELETE", Path: "/api/v1/me/sections/{sectionID}", Group: "Session", Summary: "Delete one of your sections. Its apps go back to Your apps."},
	{Method: "PUT", Path: "/api/v1/me/sections/{sectionID}/apps/{appID}", Group: "Session", Summary: "File an app you can open into one of your sections, moving it out of any other."},
	{Method: "DELETE", Path: "/api/v1/me/sections/{sectionID}/apps/{appID}", Group: "Session", Summary: "Take an app out of a section, back to Your apps."},

	// --- tokens -----------------------------------------------------------
	{Method: "GET", Path: "/api/v1/tokens", Group: "Tokens", Summary: "Your own tokens. Never anyone else's."},
	{Method: "POST", Path: "/api/v1/tokens", Group: "Tokens", Summary: "Mint a delegated token. It acts as you, is bounded by your live grants, and dies with your account (R-058, R-059). The secret is shown once."},
	{Method: "GET", Path: "/api/v1/tokens/service", Group: "Tokens", Summary: "The installation's service tokens.", Verb: string(authz.InstallTokensManage)},
	{Method: "POST", Path: "/api/v1/tokens/service", Group: "Tokens", Summary: "Mint a service token: its own principal, holding only what is shared with it, outliving whoever created it (R-060). The secret is shown once.", Verb: string(authz.InstallTokensManage)},
	{Method: "DELETE", Path: "/api/v1/tokens/{tokenID}", Group: "Tokens", Summary: "Revoke a token. Yours; a service token with install.tokens.manage; anyone else's with install.users.manage."},

	// --- apps -------------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps", Group: "Apps", Summary: "The apps you can administer. Each carries `detection` — its status, and its stage while running — once it has been through detection. Newest first, a page at a time: `limit` (default 100, at most 500), `cursor` (the previous page's `next_cursor`, which is empty after the last page), `q` to match the name or slug, and `id`, repeatable, to read only those apps. `total` counts the matches exactly up to 10,000; past that it is 10,000 and `total_is_lower_bound` is true (O-53)."},
	{Method: "POST", Path: "/api/v1/apps", Group: "Apps", Summary: "Create an app. `source` is `{type: git, url, ref}` for a repository, `{type: image, image, credential}` for an image that is already built (`credential` optional, for a private one: see PUT /registry-credential), or `{type: upload}` for files sent next with POST /source. Checked against the source allowlist before anything is fetched (R-092). Returns immediately in draft while detection runs; follow it with GET /detection and `wait`.", Verb: string(authz.AppCreate)},
	{Method: "GET", Path: "/api/v1/apps/{appID}", Group: "Apps", Summary: "One app: name, source, state and pinned spec; `detection` — its status, and its stage while running — once it has been through detection; and `last_backup`, its last daily backup attempt.", Verb: string(authz.AppView)},
	{Method: "PATCH", Path: "/api/v1/apps/{appID}", Group: "Apps", Summary: "Rename an app or change its source.", Verb: string(authz.AppSpecEdit)},
	{Method: "DELETE", Path: "/api/v1/apps/{appID}", Group: "Apps", Summary: "Delete an app. With storage, `backup=true` keeps a final copy and `force=true` discards it; without either, the request is refused so the decision is taken rather than assumed (R-204, R-205).", Verb: string(authz.AppDelete)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/icon", Group: "Apps", Summary: "The image on the app's launcher tile. Anyone who can open the app can load it; `icon_updated_at` on the app says whether there is one and when it changed (R-340)."},
	{Method: "PUT", Path: "/api/v1/apps/{appID}/icon", Group: "Apps", Summary: "Set the app's tile image. The body is the image itself — PNG, JPEG, WebP or GIF, at most 256 KB. SVG is refused (R-340).", Verb: string(authz.AppSpecEdit)},
	{Method: "DELETE", Path: "/api/v1/apps/{appID}/icon", Group: "Apps", Summary: "Remove the app's tile image, so the tile goes back to the map generated for it.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/usage", Group: "Apps", Summary: "What each part of the app is using now — CPU in thousandths of a core, memory and disk in bytes, and each mounted volume's size — beside its limits (0 is none; `host_cpu_millis` and `host_memory_bytes` say what none means). A reading, not a history (R-245, R-016). `supported: false` when the runtime cannot report it.", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/status", Group: "Apps", Summary: "What the app is doing now: its state, and each part separately — running, restarting and how often, health, exit code — so a single crash-looping part is visible rather than averaged into one word. `auto_deploy_paused` is true when the spec asks for auto-deploy and approval now stops it (R-158).", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/start", Group: "Apps", Summary: "Set the app's desired state to running. The reconciler converges to it, so it survives a restart.", Verb: string(authz.AppRestart)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/stop", Group: "Apps", Summary: "Set the app's desired state to stopped.", Verb: string(authz.AppRestart)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/restart", Group: "Apps", Summary: "Restart the running workloads without changing anything.", Verb: string(authz.AppRestart)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/logs", Group: "Apps", Summary: "The app's own output, from the runtime. `tail` sets how many lines; `workload` picks which part of the app, defaulting to the primary one.", Verb: string(authz.AppLogsRead)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/exec", Group: "Apps", Summary: "A terminal in the running app, over a websocket. Refused when host policy has turned exec off, including for the owner (R-085).", Verb: string(authz.AppExec)},

	// --- detection --------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/detection", Group: "Detection", Summary: "What Pando worked out about the repository: the winning bid, its evidence, the runners-up and any outstanding questions. While `status` is `running`, `stage` is one of fetching, detecting, trying, scanning or screening, and `elapsed_seconds` how long it has been going. `wait` (seconds, up to 60) holds the answer until detection reaches a new stage or finishes.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/detection/rerun", Group: "Detection", Summary: "Run detection again, against the current commit.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/detection/diff", Group: "Detection", Summary: "What accepting the proposal would change about the running app.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/detection/answers", Group: "Detection", Summary: "Answer detection's questions. Each answer is a fact detection could not find, not a preference.", Verb: string(authz.AppSpecEdit)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/detection/revise", Group: "Detection", Summary: "Tell the AI adapter what is wrong with the plan (`message`). It checks the repository, changes what it can show, replies, and records the exchange on the proposal. Needs an AI adapter.", Verb: string(authz.AppSpecEdit)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/detection/accept", Group: "Detection", Summary: "Accept the proposal, writing a spec revision and pinning it. `values` sets variables in the same step — `{key, value, secret?, workload?}` each; a secret goes to the secrets adapter and needs app.secrets.write. Accepting over a configured app needs `confirm`.", Verb: string(authz.AppSpecEdit)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/source", Group: "Detection", Summary: "Send an app's files as its source: a gzipped tar of the app's directory (Content-Type `application/gzip`, at most 256 MB), paths relative to its root. A single index.html is enough for a static site. Replaces any files sent before; follow it with POST /detection/rerun. Refused under a source allowlist that does not include `upload` (R-092).", Verb: string(authz.AppSpecEdit)},

	// --- specs ------------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/specs", Group: "Configuration", Summary: "Every spec revision, and which one is pinned. Revisions are append-only (R-152).", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/specs", Group: "Configuration", Summary: "Write a new spec revision. It does not deploy and does not become pinned. A routing mode other than its adapter's default, where the app did not already have one, also needs app.routing.override (R-163). A change to `egress` needs app.egress.tighten or app.egress.loosen; one that newly loosens the installation's egress rules needs app.egress.loosen where policy gates loosening by verb, is refused with PLAN_EGRESS_LOOSENING_FORBIDDEN where policy forbids it, and makes the next deploy need approval where policy says so (R-183, R-184). Turning on automatic deploys while the app's deploys need approval is refused (R-158). With `?dry_run=true` nothing is written: the same checks run and refuse with the same errors, and an accepted spec answers 200 with the egress rules it would run with, what it newly loosens, and whether deploying it would need approval and why.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/routing", Group: "Configuration", Summary: "Where the app is reached, where it will be after the next deploy when a change is saved (`next_address`), and the routing adapters it could move to, with the modes each serves and defaults to.", Verb: string(authz.AppView)},
	{Method: "PUT", Path: "/api/v1/apps/{appID}/routing", Group: "Configuration", Summary: "Change where a configured app is reached: `adapter_ref`, `mode`, and `hostname` or `path_prefix` (such as `/team/notes`), each defaulting to the adapter's own. Writes a revision the next deploy ships. A change needs `confirm: true`, since the old address stops working; a mode the adapter does not default to also needs app.routing.override (R-163). An address another app holds, or a path inside or around another app's, is refused with STATE_ADDRESS_TAKEN.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/specs/{rev}", Group: "Configuration", Summary: "One revision, in full.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/specs/{rev}/pin", Group: "Configuration", Summary: "Pin a revision: what the reconciler converges to, and what the next deploy ships.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/specs/{a}/diff/{b}", Group: "Configuration", Summary: "The classified difference between two revisions — what a deploy of it would restart, rebuild or leave alone.", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/export", Group: "Configuration", Summary: "The app's configuration as a document, with every secret redacted (R-194).", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/plan", Group: "Configuration", Summary: "What a deploy of the pinned spec would do, and every reason it would refuse — before anything is created. Carries the egress rules the app would run with, merged, with where each part came from and what every loosening needs (`egress`, R-188), `notes`, and whether the deploy would need approval and why (`approval`, R-154).", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/egress", Group: "Configuration", Summary: "The installation's egress rules (`install`: mode, list, private-address blocking, and what loosening them needs), the rules the pinned spec runs with, merged, with every loosening and what it needs (`effective`), the pinned spec's own egress settings (`spec`), and which revision those are (`revision`). `?revision=N`, or `?revision=latest` for the newest, reads that revision instead — what a saved change not yet deployed would run with. Change them by writing a spec (R-182, R-188).", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/slots", Group: "Configuration", Summary: "The things the app says it needs, and what fills each one (R-130).", Verb: string(authz.AppView)},
	{Method: "PUT", Path: "/api/v1/apps/{appID}/slots/{key}", Group: "Configuration", Summary: "Fill a slot: provision one, bind to something already running, or set a value. Takes effect at the next deploy.", Verb: string(authz.AppSpecEdit)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/volumes", Group: "Storage", Summary: "The storage this app keeps. It outlives the app (R-204).", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/volumes", Group: "Storage", Summary: "Declare a path the app keeps between deploys. Anything written outside one is discarded at the next deploy (R-201).", Verb: string(authz.AppSpecEdit)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/restore", Group: "Storage", Summary: "Restore this app's storage from one of its backups.", Verb: string(authz.AppDeploy)},

	// --- deploys ----------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/deployments", Group: "Deploys", Summary: "Every deploy of this app, newest first.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/deployments", Group: "Deploys", Summary: "Deploy. Returns 202 with the deployment; the build runs behind it. When the deploy needs approval (R-154) it comes back `awaiting_approval` instead, the app is left as it is, the people who can approve are told, and any older request for the app still waiting is superseded. Retrying with the same idempotency key replays the first answer rather than deploying twice (R-262).", Verb: string(authz.AppDeploy)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/deployments/{depID}", Group: "Deploys", Summary: "One deploy: what it shipped, and how it ended. One that needed approval carries `approvals_required`, `approval_expires_at`, `approval_reasons`, the `approvals` so far and, while it waits, `can_decide`.", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/deployments/{depID}/logs", Group: "Deploys", Summary: "The deploy's output as server-sent events, flushed per line while it runs (R-170).", Verb: string(authz.AppLogsRead)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/deployments/rollback", Group: "Deploys", Summary: "Deploy the last revision that ran successfully, or the revision `to` names. Rolling back to a revision that ran successfully before never needs approval (R-157).", Verb: string(authz.AppDeploy)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/deployments/{depID}/approve", Group: "Deploys", Summary: "Approve a deploy that is waiting for approval, with an optional `comment`. Takes `install.deploys.approve`, or `app.deploy.approve` on this app; either may approve its holder's own request (R-155). The last approval it needs plans it again and starts it: 200 with the deployment, `pending` once started and still `awaiting_approval` while it needs more. Refused while another deploy of the app is running; the request keeps waiting.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/deployments/{depID}/reject", Group: "Deploys", Summary: "Reject a deploy that is waiting for approval, with an optional `comment`. One rejection ends the request (R-156). The same permissions as approving.", Verb: string(authz.AppView)},
	// --- events and subscriptions (issue #50) ------------------------------
	{Method: "GET", Path: "/api/v1/events", Group: "Events", Summary: "The event catalog: every event a subscription can name — `name`, `scope` (app or install), `source`, `summary` and its data `fields`. The same list as docs/events.md (R-364). `destinations` lists the notification adapters a subscription can send through (`id`, `kind`, and `audience`: people or channel)."},
	{Method: "GET", Path: "/api/v1/subscriptions", Group: "Events", Summary: "Your event subscriptions, newest first. `app_id` narrows to one app; `everyone=true` lists every person's, which needs `install.events.manage`. `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues."},
	{Method: "POST", Path: "/api/v1/subscriptions", Group: "Events", Summary: "Subscribe to events (`events`: names, prefixes such as `deploy.*`, or `*`) on one app (`app_id`, which needs `app.view`) or install-wide (no `app_id`, which needs `install.events.manage`), sent to a `webhook` (`url`) or through a notification adapter (`notify`, `adapter_id`). A webhook's `signing_key` is in this response and never again (R-367, R-371)."},
	{Method: "GET", Path: "/api/v1/subscriptions/{subscriptionID}", Group: "Events", Summary: "One subscription: its filter, destination, whether it is on, and why Pando turned it off if it did. Yours, or anybody's with `install.events.manage`."},
	{Method: "PATCH", Path: "/api/v1/subscriptions/{subscriptionID}", Group: "Events", Summary: "Change a subscription's `events`, `url`, `adapter_id` or `description`, or turn it on or off (`enabled`). Turning one on clears its record of failures."},
	{Method: "DELETE", Path: "/api/v1/subscriptions/{subscriptionID}", Group: "Events", Summary: "Delete a subscription, its signing key and its delivery log."},
	{Method: "POST", Path: "/api/v1/subscriptions/{subscriptionID}/signing-key", Group: "Events", Summary: "Replace a webhook's signing key. The new `signing_key` is in this response and never again; deliveries not yet sent are signed with it."},
	{Method: "POST", Path: "/api/v1/subscriptions/{subscriptionID}/test", Group: "Events", Summary: "Send a `subscription.test` event to this subscription alone, whatever its filter says. Returns the queued delivery."},
	{Method: "GET", Path: "/api/v1/subscriptions/{subscriptionID}/deliveries", Group: "Events", Summary: "A subscription's deliveries, newest first: each event, its `status` (pending, succeeded, failed), attempts and last error. `before` and `limit` page; `next_before` continues (R-369)."},
	{Method: "GET", Path: "/api/v1/subscriptions/{subscriptionID}/deliveries/{deliveryID}", Group: "Events", Summary: "One delivery: every attempt at it (`attempt_log`) and the `payload` sent."},
	{Method: "POST", Path: "/api/v1/subscriptions/{subscriptionID}/deliveries/{deliveryID}/redeliver", Group: "Events", Summary: "Send a delivery again now, with the whole retry schedule ahead of it."},
	{Method: "GET", Path: "/api/v1/me/notifications", Group: "Events", Summary: "Your notifications inbox, newest first: what Pando told you on the console, each with `kind`, `subject`, `body`, `app_name`, `link` and `read_at`, and `unread`, how many are unread in all. `unread=true` lists only those; `before` and `limit` page (R-377)."},
	{Method: "POST", Path: "/api/v1/me/notifications/{notificationID}/read", Group: "Events", Summary: "Mark one of your notifications read."},
	{Method: "POST", Path: "/api/v1/me/notifications/read", Group: "Events", Summary: "Mark every one of your notifications read."},
	{Method: "GET", Path: "/api/v1/notification-preferences", Group: "Events", Summary: "Which of Pando's own notifications reach you, on which channel: every `kind`, every `channel` that reaches people (the console, email), and your `choices` with defaults filled in (R-373)."},
	{Method: "PUT", Path: "/api/v1/notification-preferences", Group: "Events", Summary: "Set your notification preferences: `choices`, each a `kind`, a `channel` and `enabled`. Choices not sent keep their current value."},

	{Method: "GET", Path: "/api/v1/approvals", Group: "Deploys", Summary: "Deploys waiting for approval on every app you can see, oldest first: each deployment with `app_name`, `app_slug`, its `approval_reasons`, the `approvals` so far, and `can_decide` — whether you may approve or reject it. `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues, and a page can hold fewer than `limit` and still have one."},

	// --- secrets ----------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/secrets", Group: "Secrets", Summary: "Which secrets this app has, and where each came from. Never their values.", Verb: string(authz.AppView)},
	{Method: "PUT", Path: "/api/v1/apps/{appID}/secrets/{key}", Group: "Secrets", Summary: "Set a secret. Rotating one recreates the workload rather than leaving it running with the old value (R-193).", Verb: string(authz.AppSecretsWrite)},
	{Method: "DELETE", Path: "/api/v1/apps/{appID}/secrets/{key}", Group: "Secrets", Summary: "Remove a secret.", Verb: string(authz.AppSecretsWrite)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/secrets/{key}/value", Group: "Secrets", Summary: "Read one secret's value. Its own verb, separate from managing the app, and audited every time (R-083).", Verb: string(authz.AppSecretsRead)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/registry-credential", Group: "Secrets", Summary: "Whether the app has a credential for pulling its image, and of which `kind` (`basic` or `ecr`), with its `username` or `access_key_id` and `region`. Never the password or secret key.", Verb: string(authz.AppView)},
	{Method: "PUT", Path: "/api/v1/apps/{appID}/registry-credential", Group: "Secrets", Summary: "Set the credential the app's image is pulled with: `kind` `basic` with `username` and `password` (a token), or `kind` `ecr` with `access_key_id`, `secret_access_key` and optionally `region`, from which a registry password is minted for every pull. Replaces any credential the app had. It belongs to the app and is never given to it.", Verb: string(authz.AppSecretsWrite)},
	{Method: "DELETE", Path: "/api/v1/apps/{appID}/registry-credential", Group: "Secrets", Summary: "Remove the app's registry credential, so its image is pulled anonymously.", Verb: string(authz.AppSecretsWrite)},

	// --- security ---------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/security", Group: "Security", Summary: "The app's security score, what it was taken from, and the findings behind it (R-310).", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/security/scan", Group: "Security", Summary: "Scan the app now. A write, not a refresh: the score decides whether the next deploy is allowed (R-312, R-314).", Verb: string(authz.AppDeploy)},

	// --- sharing ----------------------------------------------------------
	{Method: "GET", Path: "/api/v1/apps/{appID}/events", Group: "Events", Summary: "The app's recent events, newest first: each as a webhook receives it (`id`, `type`, `occurred_at`, `actor`, `data`, `link`) and as a person reads it (`subject`, `body`, `fields`). `before` and `limit` page. Kept as long as the event outbox keeps them, 30 days (R-378).", Verb: string(authz.AppView)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/grants", Group: "Sharing", Summary: "Who can reach this app, and who can administer it — two planes, listed separately (R-070, R-071). By principal, the grant to everyone first, with each principal's grants on both planes on the same page: `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues.", Verb: string(authz.AppView)},
	{Method: "POST", Path: "/api/v1/apps/{appID}/grants", Group: "Sharing", Summary: "Share the app with a user, a group, a token, or with everyone. The anonymous grant is a real row, refused where host policy forbids it (R-075, R-076).", Verb: string(authz.AppGrantsManage)},
	{Method: "PATCH", Path: "/api/v1/apps/{appID}/grants/{grantID}", Group: "Sharing", Summary: "Change the role a grant for managing the app carries (`role_id`), one update so the person is never left with nothing in between; or, on the grant to everyone, set `passcode` (\"\" removes it). A new passcode asks everyone let in by the old one again (R-075a).", Verb: string(authz.AppGrantsManage)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/principals", Group: "Sharing", Summary: "People and groups to share the app with, matching `q` (username, name or email; group name), at most 20 of each.", Verb: string(authz.AppGrantsManage)},
	{Method: "GET", Path: "/api/v1/apps/{appID}/passcode", Group: "Sharing", Summary: "The name of an app that asks for a passcode, for its passcode page. Public; not-found for any other app (R-075a)."},
	{Method: "POST", Path: "/api/v1/apps/{appID}/passcode", Group: "Sharing", Summary: "Enter an app's passcode (`passcode`). Right, and the browser is let in for a day by a cookie the app never sees; ten wrong tries in fifteen minutes and it waits. Public (R-075a)."},
	{Method: "DELETE", Path: "/api/v1/apps/{appID}/grants/{grantID}", Group: "Sharing", Summary: "Take a grant away.", Verb: string(authz.AppGrantsManage)},

	// --- accounts, groups, roles -----------------------------------------
	{Method: "GET", Path: "/api/v1/users", Group: "Identity", Summary: "The accounts on this installation, newest first, each with its `install_role_id`. `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues; `q` matches the username, display name or email; `id`, repeatable, reads only those accounts; `total` counts the matches exactly up to 10,000; past that it is 10,000 and `total_is_lower_bound` is true (O-53).", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/users", Group: "Identity", Summary: "Create an account.", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/users/{userID}", Group: "Identity", Summary: "One account. Your own needs no verb.", Verb: string(authz.InstallView)},
	{Method: "PATCH", Path: "/api/v1/users/{userID}", Group: "Identity", Summary: "Change an account: any of `username`, `display_name`, `email` and `status`. Username and email only on a local account; a username only with this verb, even your own. Suspension is not deletion (R-049). Your own name and email need no verb.", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/users/{userID}", Group: "Identity", Summary: "Delete an account, with the destruction rules that follow from it (R-282).", Verb: string(authz.InstallUsersManage)},
	{Method: "PUT", Path: "/api/v1/users/{userID}/role", Group: "Identity", Summary: "Give an account an installation role. Deliberately not a field on PATCH: changing someone's status and changing their power are different acts.", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/users/{userID}/role", Group: "Identity", Summary: "Take an installation role away. The last administrator cannot be demoted.", Verb: string(authz.InstallUsersManage)},
	{Method: "POST", Path: "/api/v1/users/{userID}/password", Group: "Identity", Summary: "Reset another local account's password (`password`), ending every session it holds. `must_change_password` defaults to true: whoever set it hands it over, and its holder chooses their own at the next sign-in. Your own is `POST /me/password`.", Verb: string(authz.InstallUsersManage)},
	{Method: "POST", Path: "/api/v1/passwords/generate", Group: "Identity", Summary: "A strong random password, 18 to 22 characters with upper and lower case, digits and symbols, for creating or resetting an account. Stores nothing.", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/users/{userID}/apps", Group: "Identity", Summary: "The apps an account has something on: its role for managing each, directly or through a group, whether it can use each, and whether you can change that (`can_manage`). Only apps you can see are listed. Your own needs nothing. By app name, a page of apps at a time: `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues, and a page can hold fewer apps than the limit when some are ones you cannot see.", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/groups", Group: "Identity", Summary: "Groups, whether Pando's own or an identity adapter's (R-078), Pando's first and then by name, each with `member_count`; GET /groups/{groupID} has the members. `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues; `q` matches the name; `member` keeps the groups an account is directly in; `total` counts the matches exactly up to 10,000; past that it is 10,000 and `total_is_lower_bound` is true (O-53).", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/groups", Group: "Identity", Summary: "Create a group.", Verb: string(authz.InstallUsersManage)},
	{Method: "PUT", Path: "/api/v1/groups/{groupID}/members", Group: "Identity", Summary: "Set a group's members.", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/groups/{groupID}/members", Group: "Identity", Summary: "The accounts directly in a group, oldest first, each as GET /users shows it. `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues; `q` matches the username, display name or email; `id`, repeatable, keeps only those accounts, which answers whether they are members. `total` counts the matches exactly up to 10,000; past that it is 10,000 and `total_is_lower_bound` is true (O-53).", Verb: string(authz.InstallView)},
	{Method: "PUT", Path: "/api/v1/groups/{groupID}/members/{userID}", Group: "Identity", Summary: "Add one account to a group. It then holds everything the group holds.", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/groups/{groupID}/members/{userID}", Group: "Identity", Summary: "Remove one account from a group. Refused when it would leave nobody who can manage accounts (R-088).", Verb: string(authz.InstallUsersManage)},
	{Method: "PUT", Path: "/api/v1/groups/{groupID}/role", Group: "Identity", Summary: "Give a group an installation role (`role_id`), which everyone in it holds.", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/groups/{groupID}/role", Group: "Identity", Summary: "Take a group's installation role away. Refused when it would leave nobody who can manage accounts (R-088).", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/users/{userID}/identities", Group: "Identity", Summary: "The external identities that sign in to an account: which provider, its ID for the person, whether it is where the account came from, and whether SCIM manages it. Your own, or anyone's with install.view."},
	{Method: "POST", Path: "/api/v1/users/{userID}/identities", Group: "Identity", Summary: "Link a provider's identity (`adapter_id`, `external_id`) to an account, so it signs in there. Adds an alias and never merges accounts (O-1). If it already signs in to another account, `replace_account: true` moves it, and that account is kept, suspended, as an alias.", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/users/{userID}/identities", Group: "Identity", Summary: "Unlink an identity (`adapter_id`, `external_id` in the query) from an account.", Verb: string(authz.InstallUsersManage)},
	{Method: "PUT", Path: "/api/v1/groups/{groupID}/links/{syncedGroupID}", Group: "Identity", Summary: "Make everyone in a group an identity provider syncs count as a member of a group made in Pando, live (R-078, R-079).", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/groups/{groupID}/links/{syncedGroupID}", Group: "Identity", Summary: "Remove such a link. Refused when it would leave nobody who can manage accounts (R-088).", Verb: string(authz.InstallUsersManage)},

	// --- identity providers -------------------------------------------------
	{Method: "GET", Path: "/api/v1/identity-providers", Group: "Identity providers", Summary: "Every identity provider, local accounts included: settings, which credentials are set (never their values), the callback URL and entity ID to register with the provider, the SCIM base URL, and its revocation — session lifetime, mode, and the window that results (R-047, R-050). `kinds` describes the kinds that can be added, with presets for known providers.", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/identity-providers", Group: "Identity providers", Summary: "Add an identity provider: `kind` (oidc or saml), `name`, `config`, write-only `credentials`, `jit_provisioning`, `link_by_email`. Off until `enabled` is set, so it can be tested first.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "GET", Path: "/api/v1/identity-providers/{providerID}", Group: "Identity providers", Summary: "One identity provider.", Verb: string(authz.InstallView)},
	{Method: "PATCH", Path: "/api/v1/identity-providers/{providerID}", Group: "Identity providers", Summary: "Change an identity provider. `config` replaces the settings whole; each `credentials` field replaces that one, and an empty value removes it. Takes effect on the next sign-in, without a restart.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "DELETE", Path: "/api/v1/identity-providers/{providerID}", Group: "Identity providers", Summary: "Remove an identity provider nobody has signed in through. One that has is turned off instead.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "POST", Path: "/api/v1/identity-providers/{providerID}/check", Group: "Identity providers", Summary: "Whether the provider answers: its discovery document or metadata can be read (`ok`, `message`).", Verb: string(authz.InstallAdaptersManage)},
	{Method: "POST", Path: "/api/v1/identity-providers/{providerID}/scim-token", Group: "Identity providers", Summary: "Turn SCIM on for a provider, or replace its token. The token is in this response and never again. With SCIM on, the provider's pushes create and suspend accounts and set its groups' members at once (R-048).", Verb: string(authz.InstallAdaptersManage)},
	{Method: "DELETE", Path: "/api/v1/identity-providers/{providerID}/scim-token", Group: "Identity providers", Summary: "Turn SCIM off for a provider. What it pushed stays.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "GET", Path: "/api/v1/identity-providers/{providerID}/test", Group: "Identity providers", Summary: "Start a test sign-in: a browser navigation to the provider, returning to the console with a report of the claims it sent and what Pando would do with them. Signs nobody in, and works while the provider is off.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "GET", Path: "/api/v1/identity-providers/{providerID}/tests/{flowID}", Group: "Identity providers", Summary: "A test sign-in's report, to the administrator who ran it.", Verb: string(authz.InstallAdaptersManage)},

	// --- SCIM ---------------------------------------------------------------
	{Method: "GET", Path: "/api/v1/scim/v2/ServiceProviderConfig", Group: "SCIM", Summary: "SCIM 2.0 service provider configuration. Every SCIM endpoint takes the provider's SCIM token as a bearer token, not a Pando token."},
	{Method: "GET", Path: "/api/v1/scim/v2/ResourceTypes", Group: "SCIM", Summary: "SCIM resource types: User and Group."},
	{Method: "GET", Path: "/api/v1/scim/v2/Schemas", Group: "SCIM", Summary: "The SCIM attributes Pando reads."},
	{Method: "GET", Path: "/api/v1/scim/v2/Users", Group: "SCIM", Summary: "The accounts this provider provisioned. `filter` takes `userName`, `externalId` or `emails` with `eq`; `startIndex` and `count` page."},
	{Method: "POST", Path: "/api/v1/scim/v2/Users", Group: "SCIM", Summary: "Provision an account. An account the provider's sign-in already made is adopted rather than refused."},
	{Method: "GET", Path: "/api/v1/scim/v2/Users/{id}", Group: "SCIM", Summary: "One provisioned account."},
	{Method: "PUT", Path: "/api/v1/scim/v2/Users/{id}", Group: "SCIM", Summary: "Replace an account's profile. `active: false` suspends it and ends its sessions at once; `active: true` lifts only a suspension this provider made (R-049)."},
	{Method: "PATCH", Path: "/api/v1/scim/v2/Users/{id}", Group: "SCIM", Summary: "Change an account's profile or `active` with SCIM PATCH operations."},
	{Method: "DELETE", Path: "/api/v1/scim/v2/Users/{id}", Group: "SCIM", Summary: "Deprovision an account: it is suspended, not deleted (R-049), leaves the provider's groups, and its sessions end."},
	{Method: "GET", Path: "/api/v1/scim/v2/Groups", Group: "SCIM", Summary: "This provider's groups. `filter` takes `displayName` or `externalId` with `eq`; `excludedAttributes=members` leaves members out."},
	{Method: "POST", Path: "/api/v1/scim/v2/Groups", Group: "SCIM", Summary: "Push a group. It holds no access until an administrator gives it some or links it to a Pando group (R-078)."},
	{Method: "GET", Path: "/api/v1/scim/v2/Groups/{id}", Group: "SCIM", Summary: "One pushed group, with its members."},
	{Method: "PUT", Path: "/api/v1/scim/v2/Groups/{id}", Group: "SCIM", Summary: "Replace a pushed group's name and members."},
	{Method: "PATCH", Path: "/api/v1/scim/v2/Groups/{id}", Group: "SCIM", Summary: "Add or remove members, or rename, with SCIM PATCH operations. Takes effect on the next request (R-079)."},
	{Method: "DELETE", Path: "/api/v1/scim/v2/Groups/{id}", Group: "SCIM", Summary: "Delete a pushed group, and every grant made to it."},
	{Method: "GET", Path: "/api/v1/groups/{groupID}/apps", Group: "Identity", Summary: "A group's app grants: the role everyone in it has on each app, whether they can open it, and whether you can change that (`can_manage`). Only apps you can see. Share an app with a group through `POST /apps/{appID}/grants` with `principal_kind: group`. By app name, a page of apps at a time: `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues.", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/groups/{groupID}", Group: "Identity", Summary: "One group with its `member_count` (direct members), its `linked_from` or `links_to`, and where it comes from. The members are GET /groups/{groupID}/members.", Verb: string(authz.InstallView)},
	{Method: "DELETE", Path: "/api/v1/groups/{groupID}", Group: "Identity", Summary: "Delete a group. Everything shared with it goes with it: its members lose that access and keep anything given to them another way. Refused if it would leave nobody who can manage accounts (R-088).", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/roles", Group: "Identity", Summary: "Roles, built in and custom. By default the ones granted across the installation; `scope=app` gives the ones granted on an app, and `scope=all` both. Built-in roles are immutable (R-081).", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/roles", Group: "Identity", Summary: "Compose a custom role from verbs (R-082).", Verb: string(authz.InstallUsersManage)},
	{Method: "DELETE", Path: "/api/v1/roles/{roleID}", Group: "Identity", Summary: "Delete a custom role, and every grant of it: whoever held it loses what it allowed. Built-in roles cannot be deleted (R-081). Refused if it would leave nobody who can manage accounts (R-088).", Verb: string(authz.InstallUsersManage)},
	{Method: "GET", Path: "/api/v1/verbs", Group: "Identity", Summary: "Every verb, by scope, for composing a role. There is no implication graph: holding one says nothing about another (R-082).", Verb: string(authz.InstallView)},

	// --- installation -----------------------------------------------------
	{Method: "GET", Path: "/api/v1/adapters", Group: "Installation", Summary: "The adapters configured here and what they can currently do — live capabilities, not stored configuration. Names which credentials are set, never their values. `pending_restart` marks one saved since Pando started, which is not yet what runs; `restart_needed` says any is. A routing adapter Pando runs a process in front of itself for — Traefik, cloudflared — has `edge`: whether it is running, and why not to whoever may change it (R-174).", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/adapters/kinds", Group: "Installation", Summary: "The kinds of adapter this build of Pando can run, and the settings each takes — which are credentials (write-only, stored encrypted), which are required, the default each takes when left empty or an example, and which are `advanced`: less common, never required, always with a default, and asked for apart from the rest.", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/restart", Group: "Installation", Summary: "Restart Pando: finish the requests in flight, then start again, loading the adapters and the configuration file afresh. Apps behind Pando are unreachable for the seconds it takes. Environment variables are not re-read. Returns before the restart; `started_at` on GET /api/v1/adapters changes once it is back.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "POST", Path: "/api/v1/adapters", Group: "Installation", Summary: "Configure an adapter. Settings go in config; credentials such as an API key go in credentials, which is write-only and stored encrypted.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "GET", Path: "/api/v1/capacity", Group: "Installation", Summary: "What the host has, and what is committed to apps (R-242).", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/image-registry", Group: "Installation", Summary: "The image registry builds are pushed to when a runtime pulls rather than imports: `url`, `username`, `kind` (`basic` or `ecr`), `layout` (`per_app` or `single`), `insecure`, `always`, and `password_set` — never the password. `fixed` lists the fields set in the startup configuration (PANDO_REGISTRY_*), each with where; those win over what is stored here.", Verb: string(authz.InstallView)},
	{Method: "PUT", Path: "/api/v1/image-registry", Group: "Installation", Summary: "Change the stored image registry. Every field is optional and one left out is unchanged; `password` is sealed by the secrets adapter, never shown again, and `\"\"` removes it. A field fixed at startup is refused unless it is sent with its startup value. Every replica uses the change at its next push or pull, without a restart.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "DELETE", Path: "/api/v1/image-registry", Group: "Installation", Summary: "Remove the stored image registry and its password. Fields set in the startup configuration still apply.", Verb: string(authz.InstallAdaptersManage)},
	{Method: "GET", Path: "/api/v1/policy", Group: "Installation", Summary: "Host policy. Reading the rules you work under is not the same privilege as changing them (R-274).", Verb: string(authz.InstallView)},
	{Method: "PUT", Path: "/api/v1/policy", Group: "Installation", Summary: "Replace host policy. Policy is a floor, never an override (R-272).", Verb: string(authz.InstallPolicyManage)},
	{Method: "GET", Path: "/api/v1/ai/functions", Group: "Installation", Summary: "List every AI function, the adapter that handles it and on which model, whether it is on, and whether the startup configuration assigns it (R-259, R-271).", Verb: string(authz.InstallView)},
	{Method: "PUT", Path: "/api/v1/ai/functions/{function}", Group: "Installation", Summary: "Assign an AI function to an adapter, optionally on a model of its own. Refused while another adapter handles it (R-259).", Verb: string(authz.InstallAdaptersManage)},
	{Method: "DELETE", Path: "/api/v1/ai/functions/{function}", Group: "Installation", Summary: "Turn an AI function off by removing its assignment (R-259).", Verb: string(authz.InstallAdaptersManage)},
	{Method: "POST", Path: "/api/v1/ai/access/draft", Group: "Installation", Summary: "Draft a custom role and a group from a description, with verbs from the catalog only. Body: `description`, and `current` with the draft so far to refine it. A draft: create it with POST /roles and POST /groups (R-343).", Verb: string(authz.InstallUsersManage)},
	{Method: "POST", Path: "/api/v1/ai/policy/draft", Group: "Installation", Summary: "Propose host policy from a description: the document as it would be saved, and each change. Fields fixed in the startup configuration are declined, naming where. Body: `description`, and `proposed` with the document so far to refine it. Save it with PUT /policy (R-344).", Verb: string(authz.InstallPolicyManage)},
	{Method: "POST", Path: "/api/v1/ai/audit/search", Group: "Installation", Summary: "Turn a question into audit filters, run them, and summarize what they found. Body: `question`. The filters are ordinary GET /audit parameters (R-345).", Verb: string(authz.InstallAuditRead)},
	{Method: "POST", Path: "/api/v1/ai/reference/answer", Group: "Reference", Summary: "Answer a \"How can I…\" question from this reference, citing the endpoints, commands and tools it relies on. Body: `question`. Describes; does nothing (R-346)."},
	{Method: "POST", Path: "/api/v1/policy/preview", Group: "Installation", Summary: "Which apps a candidate policy would block, before it is saved.", Verb: string(authz.InstallPolicyManage)},
	{Method: "GET", Path: "/api/v1/config", Group: "Installation", Summary: "The configuration Pando started with (R-271): every non-secret setting, its value and where it came from — an environment variable, the config file, or the default — and the host policy fields fixed there, which cannot be changed through the API while they are set. Secrets are never listed.", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/audit", Group: "Installation", Summary: "The audit log, newest first. Filters combine: `action` (a prefix), `principal_id` (who did it, including through a token), `principal_kind` (user, token, system or anonymous), `app_id`, `target_kind` and `target_id` (what it was done to), `involving` (an ID that is the actor or the target — everything to do with one account), and `since`/`until` (RFC 3339; since inclusive, until exclusive). Pages with `before`. Append-only: no endpoint edits or deletes an event, and the database refuses it too (R-027).", Verb: string(authz.InstallAuditRead)},
	{Method: "GET", Path: "/api/v1/audit/archives", Group: "Installation", Summary: "The months of the audit log past retention, archived and removed from the live log (R-347). Each with its manifest: `month`, `row_count`, `first_id` and `last_id`, `first_at` and `last_at`, `size_bytes`, `sha256`, and where it is kept — `adapter_ref` when exported to a backup destination, absent when Pando keeps it. Retention is host policy's `audit_retention_months` (at least 3) and `audit_archive` (keep, export or off).", Verb: string(authz.InstallAuditRead)},
	{Method: "GET", Path: "/api/v1/updates", Group: "Installation", Summary: "Whether a newer Pando is released (R-351): `current`, `latest`, `available`, and `releases`, each version after the running one, newest first, with its changelog section as `notes` and `security` and `breaking` marked. `upgrade` says how to move this installation to `latest`. `enabled` and `channel` are host policy's `disable_update_check` and `update_channel`; while the check is off Pando sends no request and reports nothing. `error` says why the last check failed. Every response to a signed-in caller carries the server's version as `Pando-Version`.", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/upgrade", Group: "Installation", Summary: "What an in-place upgrade to `?version=` would do (R-355, R-360): `possible`, and when not, `reasons`, each saying what to change; `image`, how Pando's deployment names its image, and `tag`, the moving tag the upgrade points at the new image; `breaking`, the versions in between that may break what works now, with their notes, which make `confirm_breaking` necessary; and `note`, that every app is unreachable while Pando restarts.", Verb: string(authz.InstallView)},
	{Method: "GET", Path: "/api/v1/upgrade/last", Group: "Installation", Summary: "The most recent in-place upgrade, as `upgrade`, or null: `from`, `to`, `state` (`running`, `succeeded`, `rolled_back` or `failed`), `reason`, the new version's last log lines when it was rolled back, `backup_id` or `skip_backup`, and whether the rollback copy of the database is still kept (`snapshot_gone`, after 24 hours healthy).", Verb: string(authz.InstallView)},
	{Method: "POST", Path: "/api/v1/upgrade", Group: "Installation", Summary: "Upgrade Pando in place (R-355 – R-360). Body: `version`; `passphrase` for the full backup taken first, or `skip_backup: true` to go without one, which is audited; and `confirm_breaking`, the version again, when the plan lists `breaking` versions. The image's signature is verified before anything else, and Pando refuses one that does not verify. Answers 202 with the upgrade: a helper then stops Pando, copies its database, starts the new version and waits for it to be ready, and puts the previous version and database back if it is not. Every app is unreachable while Pando restarts. `GET /upgrade/last` has the outcome once Pando is back.", Verb: string(authz.InstallUpgrade)},
	{Method: "GET", Path: "/api/v1/audit/archives/{archiveID}", Group: "Installation", Summary: "Download one archived month: gzipped JSON lines, one audit event per line with every field the live log held. `Repr-Digest` carries its SHA-256, the same as the manifest's `sha256`.", Verb: string(authz.InstallAuditRead)},
	{Method: "GET", Path: "/api/v1/backups", Group: "Installation", Summary: "The backups this installation holds, newest first, a page at a time: `limit` (default 100, at most 500) and `cursor` page; `next_cursor` continues; `app_id` narrows to one app. The first page also carries `attempts`: the most recent apps' last daily backup attempt, with why it was skipped or failed, at most a page of them.", Verb: string(authz.InstallBackupManage)},
	{Method: "POST", Path: "/api/v1/backups", Group: "Installation", Summary: "Take a backup now.", Verb: string(authz.InstallBackupManage)},
	{Method: "POST", Path: "/api/v1/backups/{backupID}/verify", Group: "Installation", Summary: "Check a backup before it is needed, rather than at the moment of disaster (R-216).", Verb: string(authz.InstallBackupManage)},
	{Method: "POST", Path: "/api/v1/backups/{backupID}/restore", Group: "Installation", Summary: "Restore from a backup. Verified first: an incomplete one is refused rather than half-applied (R-215).", Verb: string(authz.InstallBackupManage)},

	// --- the reference itself --------------------------------------------
	{Method: "GET", Path: "/api/v1/reference", Group: "Reference", Summary: "This document: every endpoint, every CLI command, every MCP tool and every error code, built from the running binary."},
}

// handleReference serves the API's own description.
//
// It asks for nothing, not even a session. The document holds no data about
// this installation — no app, no account, no policy, nothing an anonymous
// visitor could not read in the repository, where `docs/api.md` is the same
// document. What it holds is the shape of the API, and R-261 makes that the
// product: a client that has to authenticate before it can learn how to
// authenticate is a product with one client.
//
// Nothing in it is a credential and nothing in it is a capability. The verbs it
// names are the ones every endpoint already returns in its own error envelope
// when a caller lacks them.
//
// Built once, the first time it is asked for: everything in it is fixed when
// the binary is, and it is the largest document the API serves.
func (s *Server) handleReference(w http.ResponseWriter, r *http.Request) {
	body, err := referenceJSON()
	if err != nil {
		Error(w, r, errs.Wrap(errs.Internal, "Pando could not assemble its API reference.", err))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// referenceJSON is the reference document, encoded as JSON writes it, built
// on first use and kept.
var referenceJSON = sync.OnceValues(func() ([]byte, error) {
	referenceBuilds.Add(1)
	var buf bytes.Buffer
	err := json.NewEncoder(&buf).Encode(reference.Build(routeDocs))
	return buf.Bytes(), err
})

// referenceBuilds counts how many times the reference was built, for the
// test that it is built once.
var referenceBuilds atomic.Int64

// Reference returns the assembled document, for the generator that writes
// `docs/` from it.
func Reference() reference.Document { return reference.Build(routeDocs) }

// normalizeRoutePath makes a chi pattern comparable with a documented path.
//
// chi reports a subrouter's index as "/api/v1/apps/" and a documented path says
// "/api/v1/apps", because that is what a person types.
func normalizeRoutePath(path string) string {
	if len(path) > 1 && strings.HasSuffix(path, "/") {
		return strings.TrimSuffix(path, "/")
	}
	return path
}
