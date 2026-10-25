package audit

import "strings"

// What an event says in plain words (R-379): "Deployed crewmate" for
// app.deploy on crewmate, so a person reading the log reads sentences and an
// investigator still has the action beside it.
//
// {app} is the app's name, {target} the target's. A sentence whose name is
// not known — an event about nothing narrower than the installation, or one
// written before names were kept — is not guessed at: the event reads as its
// action, which is never wrong.
var summaries = map[string]string{
	"adapter.configure":    "Configured {target}",
	"ai.answer_reference":  "Asked AI about the reference",
	"ai.draft_access":      "Drafted access with AI",
	"ai.draft_policy":      "Drafted policy with AI",
	"ai.function.assign":   "Assigned an AI function",
	"ai.function.unassign": "Unassigned an AI function",
	"ai.search_audit":      "Searched the audit log with AI",

	"app.auto_deploy":                "Deployed {app} automatically",
	"app.auto_deploy.configure":      "Changed auto-deploy on {app}",
	"app.auto_deploy.webhook_secret": "Replaced the webhook secret of {app}",
	"app.bundle.destroy":             "Destroyed the bundle of {app}",
	"app.corrected":                  "Corrected {app}",
	"app.create":                     "Created {app}",
	"app.create.denied":              "Refused to create an app",
	"app.delete":                     "Deleted {app}",
	"app.delete.backup_failed":       "Backup before deleting {app} failed",
	"app.deploy":                     "Deployed {app}",
	"app.disk.stopped":               "Stopped {app} for disk use",
	"app.disk.warning":               "Warned about disk use of {app}",
	"app.drift_unreconcilable":       "Could not reconcile {app}",
	"app.exec":                       "Opened a terminal in {app}",
	"app.exec.end":                   "Closed a terminal in {app}",
	"app.failed":                     "{app} failed",
	"app.favorite":                   "Added {app} to favorites",
	"app.icon.clear":                 "Removed the icon of {app}",
	"app.icon.set":                   "Changed the icon of {app}",
	"app.idle.notice":                "Warned that {app} is idle",
	"app.idle.stopped":               "Stopped {app} for inactivity",
	"app.logs.read":                  "Read the logs of {app}",
	"app.notice.ignore":              "Ignored a notice on {app}",
	"app.notice.restore":             "Restored a notice on {app}",
	"app.orphan.stopped":             "Stopped {app}, which has no owner",
	"app.orphaned":                   "{app} lost its owner",
	"app.passcode.denied":            "Wrong passcode on {app}",
	"app.passcode.unlock":            "Unlocked {app} with its passcode",
	"app.reconciled":                 "Reconciled {app}",
	"app.registry_credential.delete": "Removed the registry credential of {app}",
	"app.registry_credential.write":  "Changed the registry credential of {app}",
	"app.restart":                    "Restarted {app}",
	"app.restore":                    "Restored {app}",
	"app.restore.start":              "Started restoring {app}",
	"app.scan":                       "Scanned {app}",
	"app.scan.failed":                "Scan of {app} failed",
	"app.secret.read":                "Read a secret of {app}",
	"app.security.insecure":          "{app} fell below the security minimum",
	"app.security.recovered":         "{app} met the security minimum again",
	"app.security.stopped":           "Stopped {app} below the security minimum",
	"app.slot.set":                   "Filled a slot on {app}",
	"app.source.upload":              "Uploaded source for {app}",
	"app.start":                      "Started {app}",
	"app.stop":                       "Stopped {app}",
	"app.stopped_by_reconciler":      "Pando stopped {app}",
	"app.unfavorite":                 "Removed {app} from favorites",
	"app.update":                     "Changed {app}",
	"app.use":                        "Opened {app}",
	"app.use.denied":                 "Refused a visit to {app}",
	"app.volume.add":                 "Added storage to {app}",

	"audit.archive.download": "Downloaded an audit archive",
	"audit.archive.held":     "Held an audit month for a sink",
	"audit.export":           "Exported the audit log",
	"audit.read":             "Read the audit log",
	"audit.sink.disable":     "Turned off audit sink {target}",
	"audit.sink.fail":        "Audit sink {target} failed",
	"audit.sink.recover":     "Audit sink {target} recovered",
	"authz.denied":           "Refused access to {target}",

	"backup.create":          "Took a backup",
	"backup.expire":          "Removed an expired backup",
	"backup.failed":          "Backup failed",
	"backup.restore":         "Restored a backup",
	"backup.restore.refused": "Refused to restore a backup",
	"backup.restore.start":   "Started restoring a backup",
	"backup.verify":          "Checked a backup",
	"backup.verify.failed":   "Backup check failed",

	"cli_sign_in.approve": "Approved a CLI sign-in",
	"cli_sign_in.denied":  "Refused a CLI sign-in",
	"oauth.approve":       "Approved an MCP client sign-in",
	"oauth.denied":        "Refused an MCP client sign-in",

	"deploy.approve":   "Approved a deployment of {app}",
	"deploy.expire":    "A deployment request for {app} expired",
	"deploy.finish":    "Finished deploying {app}",
	"deploy.reject":    "Rejected a deployment of {app}",
	"deploy.request":   "Requested a deployment of {app}",
	"deploy.supersede": "Replaced a deployment request for {app}",

	"detection.answer": "Answered a detection question on {app}",
	"detection.rerun":  "Ran detection again on {app}",
	"detection.revise": "Revised detection on {app}",
	"detection.screen": "Screened the plan of {app}",

	"grant.create": "Shared {app}",
	"grant.delete": "Removed access to {app}",
	"grant.update": "Changed access to {app}",

	"group.create":         "Created group {target}",
	"group.delete":         "Deleted group {target}",
	"group.link":           "Linked group {target}",
	"group.member.add":     "Added a member to {target}",
	"group.member.remove":  "Removed a member from {target}",
	"group.members.change": "Changed the members of {target}",
	"group.members.set":    "Set the members of {target}",
	"group.rename":         "Renamed group {target}",
	"group.sync.refused":   "Refused to sync group {target}",
	"group.unlink":         "Unlinked group {target}",
	"group.update":         "Changed group {target}",

	"identity_provider.create":       "Added identity provider {target}",
	"identity_provider.delete":       "Removed identity provider {target}",
	"identity_provider.scim.disable": "Turned off SCIM for {target}",
	"identity_provider.scim.enable":  "Turned on SCIM for {target}",
	"identity_provider.scim.rotate":  "Replaced the SCIM token of {target}",
	"identity_provider.test":         "Tested sign-in with {target}",
	"identity_provider.update":       "Changed identity provider {target}",

	"install.restart": "Restarted Pando",

	"launcher.section.create":  "Created a launcher section",
	"launcher.section.delete":  "Deleted a launcher section",
	"launcher.section.place":   "Added {app} to a launcher section",
	"launcher.section.rename":  "Renamed a launcher section",
	"launcher.section.unplace": "Removed {app} from a launcher section",

	"notify.connection.authorize":  "Connected {target}",
	"notify.connection.authorized": "{target} is connected",

	"policy.update": "Changed policy",

	"role.create": "Created role {target}",
	"role.delete": "Deleted role {target}",

	"routing.change": "Changed the address of {app}",

	"secret.delete": "Deleted a secret of {app}",
	"secret.write":  "Changed secrets on {app}",

	"server.start": "Pando started",

	"session.create": "Signed in",
	"session.denied": "Sign-in refused",
	"session.revoke": "Signed out",

	"setup.token.replace": "Replaced the setup token",

	"source.connection.app_created": "Created a GitHub App for {target}",
	"source.connection.authorize":   "Authorized {target}",
	"source.connection.authorized":  "{target} is authorized",
	"source.connection.delete":      "Disconnected {target}",
	"source.connection.installed":   "Installed {target}",
	"source.connection.use":         "Used {target}",

	"spec.create": "Changed the spec of {app}",
	"spec.export": "Exported the spec of {app}",
	"spec.pin":    "Pinned a revision of {app}",

	"subscription.create":     "Created subscription {target}",
	"subscription.delete":     "Deleted subscription {target}",
	"subscription.disable":    "Turned off subscription {target}",
	"subscription.key.rotate": "Replaced the signing key of {target}",
	"subscription.redeliver":  "Redelivered an event to {target}",
	"subscription.test":       "Sent a test to {target}",
	"subscription.update":     "Changed subscription {target}",

	"token.create": "Created a token",
	"token.revoke": "Revoked a token",

	"upgrade.backup_skipped": "Upgraded without a backup",
	"upgrade.failed":         "Upgrade failed",
	"upgrade.refused":        "Refused an upgrade",
	"upgrade.rolled_back":    "Rolled back an upgrade",
	"upgrade.start":          "Started an upgrade",
	"upgrade.succeeded":      "Upgraded Pando",

	"user.activate":                      "Reactivated {target}",
	"user.create":                        "Created account {target}",
	"user.delete":                        "Deleted account {target}",
	"user.identity.link":                 "Linked a sign-in to {target}",
	"user.identity.unlink":               "Unlinked a sign-in from {target}",
	"user.password.change":               "Changed a password",
	"user.password.denied":               "Wrong password for {target}",
	"user.password.reset":                "Reset the password of {target}",
	"user.scim.adopt":                    "SCIM took over {target}",
	"user.scim.delete":                   "SCIM suspended {target}",
	"user.scim.update":                   "SCIM changed {target}",
	"user.suspend":                       "Suspended {target}",
	"user.two_factor.decline":            "Declined two-factor setup",
	"user.two_factor.denied":             "Wrong two-factor code for {target}",
	"user.two_factor.disable":            "Turned off two-factor for {target}",
	"user.two_factor.enable":             "Turned on two-factor for {target}",
	"user.two_factor.recovery_code_used": "Used a recovery code",
	"user.two_factor.recovery_codes":     "Made new recovery codes",
	"user.two_factor.reset":              "Reset two-factor for {target}",
	"user.update":                        "Changed account {target}",

	"volume.destroy": "Destroyed storage of {app}",
}

// Summarize is an event's sentence: the wording for its action with its app
// and target named, or the action itself when there is no wording or a name
// it needs is not known.
func Summarize(r Record) string {
	text, ok := summaries[r.Action]
	if !ok {
		return r.Action
	}
	app := r.AppName
	if app == "" && r.TargetKind == "app" {
		app = r.TargetName
	}
	target := r.TargetName
	if target == "" {
		target = r.TargetID
	}
	if (strings.Contains(text, "{app}") && app == "") || (strings.Contains(text, "{target}") && target == "") {
		return r.Action
	}
	// A sentence that starts with a name keeps the name as it is spelled.
	return strings.NewReplacer("{app}", app, "{target}", target).Replace(text)
}
