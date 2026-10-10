// Package events is the catalog of things Pando tells subscribers about
// (R-364, issue #50, design 11).
//
// One list, in code, of every event a subscription can name: its stable name,
// whether it is about one app or the whole installation, where it comes from,
// and the fields it carries. The API serves it, `make reference` writes
// docs/events.md from it, and the console's subscription form offers it, so
// none of those can describe an event the others do not.
//
// This package is a leaf: it imports nothing from core, so the state package,
// the audit writer and the delivery engine can all depend on it.
package events

import (
	"strings"
)

// Scope is what an event is about.
type Scope string

const (
	// ScopeApp events name one app, and reach subscriptions on that app as
	// well as install-wide ones.
	ScopeApp Scope = "app"
	// ScopeInstall events are about the installation, and reach install-wide
	// subscriptions only.
	ScopeInstall Scope = "install"
)

// Source is where Pando learns an event happened.
type Source string

const (
	// SourceAudit: copied from the audit log as the audit row is written, in
	// the same transaction.
	SourceAudit Source = "audit"
	// SourceState: written by a database trigger when a row changes.
	SourceState Source = "state"
	// SourceCore: written by core directly.
	SourceCore Source = "core"
)

// Field is one entry in an event's data.
type Field struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// Def is one catalogued event.
type Def struct {
	Name    string  `json:"name"`
	Scope   Scope   `json:"scope"`
	Source  Source  `json:"source"`
	Summary string  `json:"summary"`
	Fields  []Field `json:"fields"`

	// Actions are the audit actions this event is copied from, when Source
	// is SourceAudit.
	Actions []string `json:"-"`
}

// Event names core writes directly, and the ones a trigger writes, as
// constants so a typo is a compile error rather than an event nobody can
// subscribe to.
const (
	AppStateChanged     = "app.state_changed"
	DeploySucceeded     = "deploy.succeeded"
	DeployFailed        = "deploy.failed"
	BackupCreated       = "backup.created"
	BackupFailed        = "backup.failed"
	AdapterUnhealthy    = "adapter.unhealthy"
	AdapterRecovered    = "adapter.recovered"
	SubscriptionTest    = "subscription.test"
	SubscriptionDisable = "subscription.disabled"
)

func f(name, description string) Field { return Field{Name: name, Description: description} }

var (
	deploymentID = f("deployment_id", "The deploy's ID, dep_….")
	reason       = f("reason", "Why, in words a person can act on.")
	backupID     = f("backup_id", "The backup's ID, bkp_….")
	userID       = f("user_id", "The account's ID.")
	scoreField   = f("score", "The security score, 0 to 100.")
)

// catalog is every event, in the order docs/events.md lists them.
var catalog = []Def{
	// Apps.
	{Name: "app.created", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.create"},
		Summary: "An app was created.", Fields: []Field{f("name", "The app's name."), f("slug", "The app's slug.")}},
	{Name: "app.deleted", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.delete"},
		Summary: "An app was deleted. Its volumes are kept (R-204).",
		Fields:  []Field{backupID, f("volumes_discarded", "Whether its volumes were discarded rather than kept."), f("reason", "idle when Pando deleted it because nobody used it (R-398).")}},
	{Name: "app.disk_warning", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.disk.warning"},
		Summary: "An app is near its disk limit, or over it and will be stopped if it still is at the next reading (R-403).",
		Fields: []Field{f("used_bytes", "What the app's containers and volumes hold."), f("limit_bytes", "Its disk limit."),
			f("over", "true when it is over the limit, false when it is near it.")}},
	{Name: "app.disk_stopped", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.disk.stopped"},
		Summary: "Pando stopped an app that stayed over its disk limit. It stays stopped until somebody starts it (R-403).",
		Fields:  []Field{f("used_bytes", "What the app's containers and volumes hold."), f("limit_bytes", "Its disk limit.")}},
	{Name: "app.idle_notice", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.idle.notice"},
		Summary: "Nobody has used an app for a while, and its owner was told Pando will stop or delete it (R-395).",
		Fields: []Field{f("action", "stop or delete."), f("days", "The idle setting in force."),
			f("last_activity", "When the app was last used, deployed or started."), f("action_at", "When Pando will act.")}},
	{Name: "app.idle_stopped", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.idle.stopped"},
		Summary: "Pando stopped an app nobody had used. It stays stopped until somebody starts it (R-396).",
		Fields:  []Field{f("days", "The idle setting in force."), f("last_activity", "When the app was last used, deployed or started.")}},
	{Name: AppStateChanged, Scope: ScopeApp, Source: SourceState,
		Summary: "An app moved from one state to another: running, degraded, failed, stopped, deploying and the rest. A health change is a move between running and degraded.",
		Fields:  []Field{f("from", "The state it was in."), f("to", "The state it is in now.")}},
	{Name: "app.restarted", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.restart"},
		Summary: "Somebody restarted an app."},
	{Name: "app.failed", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.failed"},
		Summary: "Pando stopped trying to bring an app back after repeated failures. It stays failed until somebody deploys it (R-151).",
		Fields:  []Field{reason, f("failures", "How many attempts failed."), f("window", "Over how long.")}},
	{Name: "app.restored", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.restore"},
		Summary: "An app's data was restored from a backup.", Fields: []Field{f("volumes", "The volumes restored.")}},
	{Name: "app.exec", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.exec"},
		Summary: "Somebody opened a shell in one of an app's workloads. The command is named; what was typed is not recorded (R-228).",
		Fields:  []Field{f("workload", "The workload."), f("command", "The command the session started.")}},
	{Name: "app.secret_written", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"secret.write"},
		Summary: "An app's secret was set or changed. The value is never in an event (R-194).",
		Fields:  []Field{f("key", "The secret's name.")}},

	// Deploys.
	{Name: "deploy.requested", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"deploy.request"},
		Summary: "A deploy is waiting for approval (R-154).",
		Fields: []Field{f("spec_revision", "The revision to deploy."), f("trigger", "What started it."),
			f("reasons", "Why it needs approval."), f("approvals_required", "How many approvals it needs.")}},
	{Name: "deploy.approved", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"deploy.approve"},
		Summary: "Somebody approved a deploy.", Fields: []Field{f("approvals", "Approvals so far."), f("approvals_required", "How many it needs.")}},
	{Name: "deploy.rejected", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"deploy.reject"},
		Summary: "Somebody rejected a deploy.", Fields: []Field{reason}},
	{Name: "deploy.started", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.deploy"},
		Summary: "A deploy started.", Fields: []Field{f("spec_revision", "The revision being deployed."), f("trigger", "What started it.")}},
	{Name: DeploySucceeded, Scope: ScopeApp, Source: SourceState,
		Summary: "A deploy finished and the app is running the new revision.",
		Fields:  []Field{deploymentID, f("spec_id", "The revision deployed."), f("trigger", "What started it.")}},
	{Name: DeployFailed, Scope: ScopeApp, Source: SourceState,
		Summary: "A deploy failed.",
		Fields: []Field{deploymentID, f("spec_id", "The revision that did not deploy."), f("trigger", "What started it."),
			f("error_code", "The stable error code."), f("message", "What went wrong.")}},

	// Security.
	{Name: "security.scanned", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.scan"},
		Summary: "An app was scanned and has a security score (R-310).",
		Fields: []Field{scoreField, f("critical", "Critical findings."), f("high", "High findings."),
			f("medium", "Medium findings."), f("low", "Low and unrated findings."), f("spec_id", "The revision scanned.")}},
	{Name: "security.scan_failed", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.scan.failed"},
		Summary: "A scan did not finish.", Fields: []Field{reason}},
	{Name: "security.insecure", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.security.insecure"},
		Summary: "An app's score is below the installation's minimum (R-315).",
		Fields:  []Field{f("grace_hours", "Hours before Pando acts."), f("action", "What Pando does then.")}},
	{Name: "security.stopped", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.security.stopped"},
		Summary: "Pando stopped an app that stayed below the installation's minimum score.",
		Fields:  []Field{f("grace_hours", "The grace period that passed."), f("insecure_since", "When it fell below.")}},
	{Name: "security.recovered", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"app.security.recovered"},
		Summary: "An app's score is back at or above the minimum."},

	// Access.
	{Name: "grant.created", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"grant.create"},
		Summary: "Somebody was given access. On an app, this is a share; install-wide, a role.",
		Fields: []Field{f("plane", "control or data."), f("role_id", "The role granted, on the control plane."),
			f("principal_kind", "user, group, token or anonymous."), f("principal_id", "Who was given access."),
			f("passcode", "Whether sharing with everyone needs a passcode.")}},
	{Name: "grant.updated", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"grant.update"},
		Summary: "A grant's role changed.", Fields: []Field{f("role_id", "The new role.")}},
	{Name: "grant.deleted", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"grant.delete"},
		Summary: "Access was taken away.", Fields: []Field{f("scope", "app or install.")}},

	// Backups.
	{Name: BackupCreated, Scope: ScopeApp, Source: SourceState,
		Summary: "A backup was taken. A disaster-recovery bundle has no app, and reaches install-wide subscriptions only.",
		Fields:  []Field{backupID, f("kind", "rolling, on_delete or dr_bundle."), f("size_bytes", "Its size.")}},
	{Name: BackupFailed, Scope: ScopeApp, Source: SourceState, Actions: []string{"backup.failed"},
		Summary: "A backup was not taken.",
		Fields: []Field{f("scheduled", "True for the hourly backup nobody asked for."), f("message", "What went wrong."),
			f("remedy", "What to do about it."), reason}},
	{Name: "backup.restored", Scope: ScopeApp, Source: SourceAudit, Actions: []string{"backup.restore"},
		Summary: "A backup was restored.", Fields: []Field{f("restored", "What was restored.")}},

	// The installation.
	{Name: "policy.updated", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"policy.update"},
		Summary: "Host policy changed. The new policy is read from GET /api/v1/policy; the event says only that it changed."},
	{Name: "user.signed_in", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"session.create"},
		Summary: "Somebody signed in.", Fields: []Field{userID, f("via", "How: password, or an identity provider."), f("adapter_id", "The identity provider.")}},
	{Name: "user.sign_in_denied", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"session.denied"},
		Summary: "A sign-in was refused.", Fields: []Field{reason, f("adapter_id", "The identity provider.")}},
	{Name: "user.created", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"user.create"},
		Summary: "An account was created.", Fields: []Field{f("via", "How it was created."), f("adapter_id", "The identity provider.")}},
	{Name: "user.deleted", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"user.delete"},
		Summary: "An account was deleted.", Fields: []Field{f("username", "Its username.")}},
	{Name: "token.created", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"token.create"},
		Summary: "An API token was created. The token itself is never in an event.", Fields: []Field{f("name", "The token's name."), f("kind", "delegated or account.")}},
	{Name: "token.revoked", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"token.revoke"},
		Summary: "An API token was revoked."},
	{Name: "adapter.configured", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"adapter.configure"},
		Summary: "An adapter was added or changed. It applies after a restart.",
		Fields:  []Field{f("category", "The adapter's category."), f("kind", "The adapter's kind."), f("credentials_changed", "Whether a credential changed.")}},
	{Name: AdapterUnhealthy, Scope: ScopeInstall, Source: SourceCore,
		Summary: "An adapter's health check started failing.", Fields: []Field{f("adapter_id", "The adapter."), f("category", "Its category."), reason}},
	{Name: AdapterRecovered, Scope: ScopeInstall, Source: SourceCore,
		Summary: "An adapter's health check passes again.", Fields: []Field{f("adapter_id", "The adapter."), f("category", "Its category.")}},
	{Name: "pando.upgraded", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"upgrade.succeeded"},
		Summary: "Pando upgraded itself in place (R-359).", Fields: []Field{f("from", "The version before."), f("to", "The version now.")}},
	{Name: "pando.upgrade_failed", Scope: ScopeInstall, Source: SourceAudit, Actions: []string{"upgrade.failed", "upgrade.rolled_back"},
		Summary: "An in-place upgrade did not finish.", Fields: []Field{f("from", "The version running."), f("to", "The version attempted."), reason}},

	// Subscriptions themselves.
	{Name: SubscriptionDisable, Scope: ScopeApp, Source: SourceAudit, Actions: []string{"subscription.disable"},
		Summary: "Pando turned a subscription off because its endpoint failed every delivery for a day (R-370). On an app subscription the event names the app; otherwise it is install-wide.",
		Fields:  []Field{f("subscription_id", "The subscription, sub_…."), reason}},
	{Name: SubscriptionTest, Scope: ScopeInstall, Source: SourceCore,
		Summary: "A test delivery somebody asked for. Sent only to the subscription it tests, whatever its filter says.",
		Fields:  []Field{f("subscription_id", "The subscription being tested.")}},
}

var (
	byName   = map[string]Def{}
	byAction = map[string]Def{}
)

func init() {
	for _, d := range catalog {
		byName[d.Name] = d
		for _, a := range d.Actions {
			byAction[a] = d
		}
	}
}

// Catalog returns every event, in documentation order. The slice is a copy.
func Catalog() []Def {
	out := make([]Def, len(catalog))
	copy(out, catalog)
	return out
}

// Lookup returns one event by name.
func Lookup(name string) (Def, bool) {
	d, ok := byName[name]
	return d, ok
}

// ForAction returns the event an audit action is copied to, if any.
func ForAction(action string) (Def, bool) {
	d, ok := byAction[action]
	return d, ok
}

// ValidPattern reports whether p names an event or matches at least one.
//
// A pattern is an event name, a prefix ending in ".*" (deploy.*), or "*". A
// pattern that matches nothing is refused rather than stored: a subscription
// to "deploy.fialed" would be a subscription that never fires, which is the
// worst way to find out about a typo.
func ValidPattern(p string) bool {
	if p == "*" {
		return true
	}
	for name := range byName {
		if Match(p, name) {
			return true
		}
	}
	return false
}

// Match reports whether pattern matches the event name.
func Match(pattern, name string) bool {
	switch {
	case pattern == "*":
		return true
	case strings.HasSuffix(pattern, ".*"):
		return strings.HasPrefix(name, strings.TrimSuffix(pattern, "*"))
	default:
		return pattern == name
	}
}

// MatchAny reports whether any pattern matches.
func MatchAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if Match(p, name) {
			return true
		}
	}
	return false
}

// Data selects an event's fields from an audit event's detail.
//
// Only catalogued fields are copied, so an event's shape is what the catalog
// says it is, and a field somebody later adds to an audit detail does not
// reach a subscriber's endpoint without first being written down here.
func (d Def) Data(detail map[string]any) map[string]any {
	out := map[string]any{}
	for _, field := range d.Fields {
		if v, ok := detail[field.Name]; ok {
			out[field.Name] = v
		}
	}
	return out
}
