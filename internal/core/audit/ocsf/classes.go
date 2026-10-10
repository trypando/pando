package ocsf

import "sort"

// Mapping is the OCSF class and activity one audit action is recorded as.
type Mapping struct {
	ClassUID     int
	ClassName    string
	ActivityID   int
	ActivityName string
}

// CategoryUID is the class's category: OCSF numbers a class category*1000+n.
func (m Mapping) CategoryUID() int { return m.ClassUID / 1000 }

// CategoryName is the category's OCSF caption.
func (m Mapping) CategoryName() string { return categoryNames[m.CategoryUID()] }

// TypeUID is class_uid*100 + activity_id, as OCSF defines it.
func (m Mapping) TypeUID() int { return m.ClassUID*100 + m.ActivityID }

// TypeName is the OCSF type caption, "<Class>: <Activity>".
func (m Mapping) TypeName() string { return m.ClassName + ": " + m.ActivityName }

var categoryNames = map[int]string{
	1: "System Activity",
	3: "Identity & Access Management",
	6: "Application Activity",
}

// The class/activity pairs used, named once so the table below reads as a
// list of decisions rather than a list of numbers. Every value is OCSF 1.3.0.
var (
	// 1007 Process Activity.
	processLaunch    = Mapping{1007, "Process Activity", 1, "Launch"}
	processTerminate = Mapping{1007, "Process Activity", 2, "Terminate"}

	// 3001 Account Change.
	accountCreate         = Mapping{3001, "Account Change", 1, "Create"}
	accountEnable         = Mapping{3001, "Account Change", 2, "Enable"}
	accountPasswordChange = Mapping{3001, "Account Change", 3, "Password Change"}
	accountPasswordReset  = Mapping{3001, "Account Change", 4, "Password Reset"}
	accountDisable        = Mapping{3001, "Account Change", 5, "Disable"}
	accountDelete         = Mapping{3001, "Account Change", 6, "Delete"}
	accountOther          = Mapping{3001, "Account Change", 99, "Other"}

	// 3002 Authentication.
	authLogon  = Mapping{3002, "Authentication", 1, "Logon"}
	authLogoff = Mapping{3002, "Authentication", 2, "Logoff"}

	// 3005 User Access Management.
	accessAssign = Mapping{3005, "User Access Management", 1, "Assign Privileges"}
	accessRevoke = Mapping{3005, "User Access Management", 2, "Revoke Privileges"}
	accessOther  = Mapping{3005, "User Access Management", 99, "Other"}

	// 3006 Group Management.
	groupAddUser    = Mapping{3006, "Group Management", 3, "Add User"}
	groupRemoveUser = Mapping{3006, "Group Management", 4, "Remove User"}
	groupDelete     = Mapping{3006, "Group Management", 5, "Delete"}
	groupCreate     = Mapping{3006, "Group Management", 6, "Create"}
	groupOther      = Mapping{3006, "Group Management", 99, "Other"}

	// 6002 Application Lifecycle.
	appInstall = Mapping{6002, "Application Lifecycle", 1, "Install"}
	appRemove  = Mapping{6002, "Application Lifecycle", 2, "Remove"}
	appStart   = Mapping{6002, "Application Lifecycle", 3, "Start"}
	appStop    = Mapping{6002, "Application Lifecycle", 4, "Stop"}
	appRestart = Mapping{6002, "Application Lifecycle", 5, "Restart"}
	appUpdate  = Mapping{6002, "Application Lifecycle", 8, "Update"}

	// 6003 API Activity.
	apiCreate = Mapping{6003, "API Activity", 1, "Create"}
	apiRead   = Mapping{6003, "API Activity", 2, "Read"}
	apiUpdate = Mapping{6003, "API Activity", 3, "Update"}
	apiDelete = Mapping{6003, "API Activity", 4, "Delete"}
	apiOther  = Mapping{6003, "API Activity", 99, "Other"}

	// 6004 Web Resource Access Activity.
	webAccessGrant = Mapping{6004, "Web Resource Access Activity", 1, "Access Grant"}
	webAccessDeny  = Mapping{6004, "Web Resource Access Activity", 2, "Access Deny"}
)

// unknown is what an action missing from the table is encoded as: an action
// written by a newer Pando, read from an old archive. Known actions never reach
// it — TestR384_EveryActionMapsToOCSF holds the table to audit.Actions.
var unknown = apiOther

// classes maps every catalogued action to its OCSF class and activity (design
// 12 §6.2, R-384). Written out per action, with no fallthrough, so each
// mapping is a reviewable line. A denial or failure is the same activity as
// the attempt; status_id says how it ended.
var classes = map[string]Mapping{
	"adapter.configure":              apiUpdate,
	"ai.answer_reference":            apiRead,
	"ai.draft_access":                apiCreate,
	"ai.draft_policy":                apiCreate,
	"ai.function.assign":             apiUpdate,
	"ai.function.unassign":           apiDelete,
	"ai.search_audit":                apiRead,
	"app.auto_deploy":                apiOther,
	"app.auto_deploy.configure":      apiUpdate,
	"app.auto_deploy.webhook_secret": apiUpdate,
	"app.bundle.destroy":             apiDelete,
	"app.corrected":                  apiUpdate,
	"app.create":                     appInstall,
	"app.create.denied":              apiCreate,
	"app.delete":                     appRemove,
	"app.delete.backup_failed":       apiOther,
	"app.deploy":                     appUpdate,
	"app.drift_unreconcilable":       apiOther,
	"app.exec":                       processLaunch,
	"app.exec.end":                   processTerminate,
	"app.failed":                     apiOther,
	"app.favorite":                   apiUpdate,
	"app.icon.clear":                 apiDelete,
	"app.idle.notice":                apiOther,
	"app.idle.stopped":               appStop,
	"app.icon.set":                   apiCreate,
	"app.logs.read":                  apiRead,
	"app.passcode.denied":            webAccessDeny,
	"app.passcode.unlock":            webAccessGrant,
	"app.reconciled":                 apiUpdate,
	"app.registry_credential.delete": apiDelete,
	"app.registry_credential.write":  apiCreate,
	"app.restart":                    appRestart,
	"app.restore":                    apiOther,
	"app.restore.start":              apiCreate,
	"app.scan":                       apiOther,
	"app.scan.failed":                apiOther,
	"app.secret.read":                apiRead,
	"app.security.insecure":          apiOther,
	"app.security.recovered":         apiOther,
	"app.security.stopped":           appStop,
	"app.slot.set":                   apiCreate,
	"app.source.upload":              apiCreate,
	"app.start":                      appStart,
	"app.stop":                       appStop,
	"app.stopped_by_reconciler":      appStop,
	"app.unfavorite":                 apiUpdate,
	"app.update":                     apiUpdate,
	"app.use":                        webAccessGrant,
	"app.use.denied":                 webAccessDeny,
	"app.volume.add":                 apiCreate,
	"audit.archive.download":         apiRead,
	"audit.archive.held":             apiOther,
	"audit.export":                   apiRead,
	"audit.read":                     apiRead,
	"audit.sink.disable":             apiUpdate,
	"audit.sink.fail":                apiOther,
	"audit.sink.recover":             apiOther,
	"authz.denied":                   webAccessDeny,
	"authz.install_wide":             accessOther,
	"backup.create":                  apiCreate,
	"backup.expire":                  apiDelete,
	"backup.failed":                  apiOther,
	"backup.restore":                 apiOther,
	"backup.restore.refused":         apiOther,
	"backup.restore.start":           apiCreate,
	"backup.verify":                  apiRead,
	"backup.verify.failed":           apiRead,
	"deploy.approve":                 apiUpdate,
	"deploy.expire":                  apiDelete,
	"deploy.finish":                  appUpdate,
	"deploy.reject":                  apiUpdate,
	"deploy.request":                 apiCreate,
	"deploy.supersede":               apiOther,
	"detection.rerun":                apiOther,
	"detection.revise":               apiUpdate,
	"detection.answer":               apiUpdate,
	"detection.screen":               apiOther,
	"grant.create":                   accessAssign,
	"grant.delete":                   accessRevoke,
	"grant.update":                   accessAssign,
	"group.create":                   groupCreate,
	"group.delete":                   groupDelete,
	"group.link":                     groupOther,
	"group.member.add":               groupAddUser,
	"group.member.remove":            groupRemoveUser,
	"group.members.change":           groupOther,
	"group.members.set":              groupOther,
	"group.rename":                   groupOther,
	"group.sync.refused":             groupOther,
	"group.unlink":                   groupOther,
	"group.update":                   groupOther,
	"identity_provider.create":       apiCreate,
	"identity_provider.delete":       apiDelete,
	"identity_provider.scim.disable": apiUpdate,
	"identity_provider.scim.enable":  apiUpdate,
	"identity_provider.scim.rotate":  apiUpdate,
	"identity_provider.test":         apiRead,
	"identity_provider.update":       apiUpdate,
	"install.restart":                apiOther,
	"launcher.section.create":        apiCreate,
	"launcher.section.delete":        apiDelete,
	"launcher.section.place":         apiUpdate,
	"launcher.section.rename":        apiUpdate,
	"launcher.section.unplace":       apiUpdate,
	"policy.update":                  apiUpdate,
	"role.create":                    apiCreate,
	"role.delete":                    apiDelete,
	"routing.change":                 apiUpdate,
	"secret.delete":                  apiDelete,
	"secret.write":                   apiCreate,
	"server.start":                   apiOther,
	"session.create":                 authLogon,
	"session.denied":                 authLogon,
	"session.revoke":                 authLogoff,
	"setup.token.replace":            apiCreate,
	"source.connection.authorize":    apiCreate,
	"source.connection.authorized":   apiCreate,
	"source.connection.delete":       apiDelete,
	"source.connection.use":          apiRead,
	"spec.create":                    apiCreate,
	"spec.export":                    apiRead,
	"spec.pin":                       apiUpdate,
	"subscription.create":            apiCreate,
	"subscription.delete":            apiDelete,
	"subscription.disable":           apiUpdate,
	"subscription.key.rotate":        apiUpdate,
	"subscription.redeliver":         apiOther,
	"subscription.test":              apiRead,
	"subscription.update":            apiUpdate,
	"token.create":                   apiCreate,
	"token.revoke":                   apiDelete,
	"upgrade.backup_skipped":         apiOther,
	"upgrade.failed":                 apiOther,
	"upgrade.refused":                apiOther,
	"upgrade.rolled_back":            apiOther,
	"upgrade.start":                  apiCreate,
	"upgrade.succeeded":              apiOther,
	"user.activate":                  accountEnable,
	"user.create":                    accountCreate,
	"user.delete":                    accountDelete,
	"user.identity.link":             accountOther,
	"user.identity.unlink":           accountOther,
	"user.password.change":           accountPasswordChange,
	"user.password.denied":           accountPasswordChange,
	"user.password.reset":            accountPasswordReset,
	"user.scim.adopt":                accountOther,
	"user.scim.delete":               accountDelete,
	"user.scim.update":               accountOther,
	"user.suspend":                   accountDisable,
	"user.update":                    accountOther,
	"volume.destroy":                 apiDelete,
}

// Row is one line of the mapping table.
type Row struct {
	Action string
	Mapping
}

// Table is the mapping, one row per catalogued action, sorted by action. What
// docs/audit-formats.md is generated from.
func Table() []Row {
	rows := make([]Row, 0, len(classes))
	for action, m := range classes {
		rows = append(rows, Row{Action: action, Mapping: m})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Action < rows[j].Action })
	return rows
}

// Lookup is the mapping for an action, and whether the table has one. An
// action it does not have is encoded as 6003 API Activity, 99 Other.
func Lookup(action string) (Mapping, bool) {
	m, ok := classes[action]
	if !ok {
		return unknown, false
	}
	return m, true
}
