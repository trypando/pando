package policy

// What each policy field does, in the words the Policy screen uses for it.
//
// Handed to the policy drafting function (R-344) so it can find the setting a
// person means. Without it the model sees field names only, and "turn off
// terminal access" became an edit to disabled_verbs described as one — the
// right change, named in a way nobody who uses the Policy screen would
// recognize. TestEveryPolicyFieldIsDescribed keeps this complete.
var descriptions = map[string]string{
	"source_allowlist": "Where apps may be created from: a host (github.com), a host suffix (.corp.example), or a host and path (github.com/acme, ghcr.io/acme) — repositories and image registries alike. Add `upload` to allow files sent from a computer. Empty means anywhere.",
	"disabled_verbs": "Permissions nobody may use, install-wide, including an app's owner. " +
		"app.exec here is the Policy screen's \"Turn off terminal access for the whole installation\".",
	"agent_disabled_verbs": "Verbs an agent's token may not use: denied to CLI and MCP tokens only, not to people.",
	"allow_anonymous_grants": "Older form of public_sharing: false means apps cannot be shared with everyone. " +
		"Prefer public_sharing.",
	"public_sharing": "Whether an app may be shared with anyone on the internet: allowed, passcode_only " +
		"(only behind the app's passcode), or none.",
	"min_build_isolation":   "Minimum isolation for builds, as an isolation class number (10 is a container).",
	"min_runtime_isolation": "Minimum isolation for running apps, as an isolation class number (10 is a container).",
	"egress_allowlist":      "Older form of egress_mode allowlist with egress_list. Prefer those.",
	"egress_mode": "Where apps may connect out to: allow_all (anywhere), denylist (anywhere except egress_list), " +
		"or allowlist (only egress_list). Apps can narrow it; loosening it is governed by egress_loosening.",
	"egress_list": "The installation's egress list: hostnames, *.wildcards, addresses or ranges, each optionally " +
		"with a port. Read by egress_mode.",
	"egress_block_private": "Stop apps connecting to private addresses — the local network, loopback, and the " +
		"cloud metadata address — whatever egress_mode says.",
	"egress_loosening": "Whether an app may loosen the installation's egress rules (add to an allowlist, remove from " +
		"a denylist, unblock private addresses): verb (somebody holding app.egress.loosen may), approval (its " +
		"deploy needs approval), or forbidden.",
	"deploy_approval_required": "Every app's deploys need approval from somebody holding install.deploys.approve " +
		"or app.deploy.approve. Automatic deploys stop while it is on.",
	"deploy_approval_apps":  "Apps, by ID, whose deploys need approval whatever their owners say.",
	"deploy_approval_count": "How many approvals a deploy needs. 0 means one.",
	"deploy_approval_expiry_hours": "How long a deploy waits for approval before it expires, in hours. 0 means " +
		"it waits until somebody answers.",
	"require_backup_before_destroy": "Require a backup before anything is destroyed: an app's storage is " +
		"backed up before the app or its volumes are deleted.",
	"max_token_lifetime_days": "Longest a token may live, in days. 0 means no limit.",
	"max_log_disk_bytes":      "Total disk for app logs across every app, in bytes. 0 means no limit.",
	"allow_cpu_oversubscription": "Let apps together ask for more CPU than the runtime has. Off by default: a " +
		"deploy that would need more CPU than is left is refused. On, busy apps share the CPU and run slower.",
	"allow_memory_oversubscription": "Let apps together ask for more memory than the runtime has. Off by " +
		"default: a deploy that would need more memory than is left is refused. On, the host may run out and " +
		"stop an app to free memory. Disk is never oversubscribed.",
	"audit_retention_months": "How many months the audit log keeps before an older month is archived and removed " +
		"from the live log. 0 means 3, which is also the least it may be.",
	"audit_archive": "Where a month of the audit log goes once it is past retention: keep (archived under Pando's " +
		"own directory), export (to a backup destination), or off (nothing is archived, so nothing is removed).",
	"audit_archive_destination": "The backup destination, by adapter ID, that audit archives are exported to when " +
		"audit_archive is export. Empty means the default backup destination.",
	"disable_ai_screening": "Turn off AI screening of deployment plans: AI is never sent a repository to " +
		"repair a plan or answer detection's questions.",
	"disable_anonymous_use_audit": "Don't record visits from people who aren't signed in: app.use is written " +
		"to the audit log for signed-in people and tokens only, not for anonymous visitors to public apps.",
	"disable_anonymous_denial_audit": "Don't record refusals of people who aren't signed in: authz.denied is " +
		"written for an anonymous visitor to an app not shared with them only while this is off. Refusals of " +
		"signed-in people and tokens are always recorded.",
	"min_security_score": "Minimum security score a deploy must reach, 0 to 100. 0 means off.",
	"insecure_action": "What happens to a running app that falls below the minimum security score: " +
		"warn, or stop (after the grace period).",
	"insecure_grace_hours":      "Grace period, in hours, before stop applies to an app below the minimum score.",
	"ignore_unfixable_findings": "Ignore findings with no fix available, in both the security score and the list.",
	"disable_password_sign_in": "Turn off sign-in with a local username and password, so people sign in through " +
		"an external identity provider. Needs at least one provider turned on.",
	"disable_jit_provisioning": "Don't make an account when someone first signs in through an identity provider, " +
		"whatever the provider is set to. People need an account from SCIM or an administrator first.",
	"disable_update_check": "Don't check GitHub for newer Pando releases. Off sends no request at all, for an " +
		"installation that has no internet access or may not call out.",
	"allow_private_webhooks": "Let event subscription webhooks send to private addresses — the local network, " +
		"loopback, and link-local addresses such as a cloud metadata service. Off by default, because anyone who " +
		"can see an app can subscribe to it.",
	"update_channel": "Which Pando releases the update check offers: stable (releases only) or prerelease " +
		"(release candidates too).",
	"upgrade_in_place": "Let Pando upgrade itself to a newer release from the Updates screen. Needs Pando's image " +
		"set to a moving tag such as trypando/pando:latest; usually set in the deployment's configuration.",
	"auto_upgrade_patches": "Upgrade to new patch releases automatically inside maintenance_window. Takes a " +
		"database copy for rollback but not a full backup. Needs upgrade_in_place.",
	"maintenance_window": `When automatic upgrades may start, in UTC: weekdays, a start time and a length, such as ` +
		`"sun,wed 02:00 2h" or "daily 03:30 1h".`,
}

// Describe says what a policy field does, in the Policy screen's words.
func Describe(key string) string { return descriptions[key] }
