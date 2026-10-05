package authz

// Verb is a control-plane permission.
//
// Data-plane use is not a verb. Reaching an app through the proxy is binary
// (R-070) and is checked by CheckData, which is a different function taking
// different arguments for exactly that reason.
type Verb string

const (
	// Install-scoped verbs. Held through a grant with no app (O-17), which is
	// what makes "an administrator" a principal with a grant rather than a
	// separate mechanism beside grants and roles.
	//
	// Namespaced `install.*` so the scope is visible at every call site: a verb
	// that reads `install.` cannot be mistaken for one that needs an app, which
	// is the mistake that would reintroduce the hole these close.
	InstallView           Verb = "install.view"
	InstallUsersManage    Verb = "install.users.manage"
	InstallPolicyManage   Verb = "install.policy.manage"
	InstallAdaptersManage Verb = "install.adapters.manage"
	InstallAuditRead      Verb = "install.audit.read"

	// InstallBackupManage covers taking, verifying and restoring backups.
	// Its own verb rather than part of install.policy.manage: a restore
	// replaces the entire install, and folding that into the verb that edits a
	// source allowlist would hand it to everyone who could edit one.
	InstallBackupManage Verb = "install.backup.manage"

	// The install.apps.* verbs are each app verb's install-wide counterpart:
	// install.apps.deploy is app.deploy on every app, including apps made
	// later (issue #81). They are how an install grant bears on an app — the
	// one sanctioned way, read in exactly one place, CheckControl, through the
	// table in everyApp. One verb per app verb rather than bundles, so a
	// custom role can carry "read every app's logs" without "deploy every
	// app", and so a group gets a permission once instead of a grant on each
	// app. The old bundles, install.apps.view as Viewer and
	// install.apps.manage as Owner on every app, are built-in roles now:
	// App viewer and App manager (R-081).
	//
	// Control plane only. None appears in CheckData: using an app still needs
	// a data grant or ownership (R-072, R-087).
	InstallAppsView              Verb = "install.apps.view"
	InstallAppsLogsRead          Verb = "install.apps.logs.read"
	InstallAppsDeploy            Verb = "install.apps.deploy"
	InstallAppsRestart           Verb = "install.apps.restart"
	InstallAppsSpecEdit          Verb = "install.apps.spec.edit"
	InstallAppsSecretsWrite      Verb = "install.apps.secrets.write"
	InstallAppsSecretsRead       Verb = "install.apps.secrets.read"
	InstallAppsExec              Verb = "install.apps.exec"
	InstallAppsGrantsManage      Verb = "install.apps.grants.manage"
	InstallAppsRoutingOverride   Verb = "install.apps.routing.override"
	InstallAppsResourcesOverride Verb = "install.apps.resources.override"
	InstallAppsEgressTighten     Verb = "install.apps.egress.tighten"
	InstallAppsEgressLoosen      Verb = "install.apps.egress.loosen"
	InstallAppsDelete            Verb = "install.apps.delete"

	// InstallTokensManage covers service tokens (R-060): listing, creating and
	// revoking them. Its own verb rather than install.users.manage's, because a
	// service token is a principal an automation acts as — issuing one is a
	// different trust from managing people, and an installation may want
	// somebody who runs its CI to hold one without the other.
	InstallTokensManage Verb = "install.tokens.manage"

	// InstallDeploysApprove approves or rejects any deploy that needs
	// approval, on any app, including the holder's own (R-155). It is
	// app.deploy.approve's install-wide counterpart, older than the
	// install.apps.* verbs and named for what it does rather than renamed to
	// match them. Not in App manager: approval is a trust an installation
	// hands out on purpose, and managing apps does not imply it.
	InstallDeploysApprove Verb = "install.deploys.approve"

	// InstallUpgrade upgrades Pando itself in place (R-356): it replaces the
	// server everything else runs behind, so it is its own trust rather than
	// part of managing policy or adapters.
	InstallUpgrade Verb = "install.upgrade"

	// InstallEventsManage subscribes to events install-wide and manages
	// anybody's subscriptions (R-368). An install-wide subscription hears about
	// every app and about sign-ins, which is why seeing one app is not enough.
	InstallEventsManage Verb = "install.events.manage"

	// AppCreate is install-scoped despite its name: there is no app yet when it
	// is checked. Sequence A step 1 has always called it install-level.
	AppCreate Verb = "app.create"

	AppView             Verb = "app.view"
	AppLogsRead         Verb = "app.logs.read"
	AppDeploy           Verb = "app.deploy"
	AppRestart          Verb = "app.restart"
	AppSpecEdit         Verb = "app.spec.edit"
	AppSecretsWrite     Verb = "app.secrets.write"
	AppSecretsRead      Verb = "app.secrets.read"
	AppExec             Verb = "app.exec"
	AppGrantsManage     Verb = "app.grants.manage"
	AppRoutingOverride  Verb = "app.routing.override"
	AppResourceOverride Verb = "app.resources.override"

	// AppEgressTighten changes an app's egress within the installation's
	// rules (R-182, R-184): its own list, additions to a denylist, removals
	// from an allowlist, private addresses blocked. Never beyond policy
	// (R-272), so Operator holds it as well as Owner.
	AppEgressTighten Verb = "app.egress.tighten"

	// AppEgressLoosen loosens the installation's egress rules for an app,
	// where host policy permits loosening by verb (R-183, R-184). It was
	// app.egress.override until issue #79; the migration renamed it in every
	// role that held it. Holding it covers every egress change, tightening
	// included.
	AppEgressLoosen Verb = "app.egress.loosen"

	// AppDeployApprove approves or rejects this app's deploys that need
	// approval (R-155). In no built-in role: an installation grants it
	// deliberately, through a custom role.
	AppDeployApprove Verb = "app.deploy.approve"

	AppDelete Verb = "app.delete"
)

// Verbs is the catalog (design 06 §5, R-080).
//
// There is no implication graph: holding app.delete does not imply app.view.
// Implication graphs are where authorization bugs live, because the graph is
// consulted in one place and forgotten in another. The console suggests
// sensible combinations instead, which is a UI concern and cannot go wrong
// silently.
var Verbs = []Verb{
	InstallView,
	InstallUsersManage,
	InstallPolicyManage,
	InstallAdaptersManage,
	InstallAuditRead,
	InstallBackupManage,
	InstallAppsView,
	InstallAppsLogsRead,
	InstallAppsDeploy,
	InstallAppsRestart,
	InstallAppsSpecEdit,
	InstallAppsSecretsWrite,
	InstallAppsSecretsRead,
	InstallAppsExec,
	InstallAppsGrantsManage,
	InstallAppsRoutingOverride,
	InstallAppsResourcesOverride,
	InstallAppsEgressTighten,
	InstallAppsEgressLoosen,
	InstallAppsDelete,
	InstallTokensManage,
	InstallDeploysApprove,
	InstallUpgrade,
	InstallEventsManage,
	AppCreate,

	AppView,
	AppLogsRead,
	AppDeploy,
	AppRestart,
	AppSpecEdit,
	AppSecretsWrite,
	AppSecretsRead,
	AppExec,
	AppGrantsManage,
	AppRoutingOverride,
	AppResourceOverride,
	AppEgressTighten,
	AppEgressLoosen,
	AppDeployApprove,
	AppDelete,
}

var verbSet = func() map[Verb]struct{} {
	m := make(map[Verb]struct{}, len(Verbs))
	for _, v := range Verbs {
		m[v] = struct{}{}
	}
	return m
}()

// IsVerb reports whether v is in the catalog. Custom roles are arbitrary
// subsets of it (R-082), so this is what validates one.
func IsVerb(v Verb) bool {
	_, ok := verbSet[v]
	return ok
}

// Built-in role IDs, seeded by migration and protected by trigger (R-081).
const (
	RoleViewer   = "role_viewer"
	RoleOperator = "role_operator"
	RoleOwner    = "role_owner"

	// RoleAdministrator is install-scoped and holds every install verb and no
	// app verb — but the install.apps.* verbs among them are every app verb
	// on every app (R-081), so an administrator can look after any app
	// without a grant on it. They are still not its owner: R-031's owner of
	// record is unchanged, and using the app still needs a data grant (R-087).
	RoleAdministrator = "role_administrator"

	// RoleCreator is install-scoped and holds one verb, app.create. A creator
	// manages the apps they make because making one writes them its owner
	// (R-073) — not because this role says anything about apps — so they
	// manage those and nothing else in the installation.
	RoleCreator = "role_creator"

	// RoleAppViewer and RoleAppManager are the Viewer and the Owner on every
	// app, install-scoped: the counterparts of each of their app verbs. They
	// replace the install.apps.view and install.apps.manage bundles, so a
	// grant of either reads the same as one of those did (issue #81).
	// App manager does not approve deploys, as Owner does not (R-155).
	RoleAppViewer  = "role_app_viewer"
	RoleAppManager = "role_app_manager"

	// RoleAuditor sees every app, reads its logs and reads the audit log, and
	// changes nothing: the security group's role (issue #81). Not
	// install.view — the accounts, adapters and policy are not what it audits.
	RoleAuditor = "role_auditor"
)

// Role is a named set of verbs.
//
// Tagged because this type is serialized directly by the API, and every other
// type on the wire is lower_snake_case. An untagged struct would put Go field
// names into the API surface — where they would then be a compatibility
// promise, and renaming a field would be a breaking change to a client.
type Role struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Builtin bool   `json:"builtin"`
	Verbs   []Verb `json:"verbs"`
}

// Has reports whether the role grants v.
func (r Role) Has(v Verb) bool {
	for _, got := range r.Verbs {
		if got == v {
			return true
		}
	}
	return false
}

// InstallScoped reports whether a verb is held install-wide rather than on one
// app.
//
// Checked at the boundary between CheckControl and CheckInstall so that a verb
// cannot be authorized in the wrong scope. Passing an install verb to
// CheckControl, or an app verb to CheckInstall, is a programming error and is
// refused rather than evaluated — an install verb evaluated against an app
// would look for a grant that cannot exist and deny, which is safe; an app verb
// evaluated install-wide would look for a grant that *can* exist and allow,
// which is not.
func InstallScoped(v Verb) bool {
	switch v {
	case InstallView, InstallUsersManage, InstallPolicyManage,
		InstallAdaptersManage, InstallAuditRead, InstallBackupManage,
		InstallTokensManage, InstallDeploysApprove, InstallUpgrade, InstallEventsManage, AppCreate:
		return true
	default:
		_, ok := everyApp[v]
		return ok
	}
}

// AppVerbs is every app-scoped verb, in catalog order.
func AppVerbs() []Verb {
	var out []Verb
	for _, v := range Verbs {
		if !InstallScoped(v) {
			out = append(out, v)
		}
	}
	return out
}

// everyApp is the whole of how an install grant bears on an app: each app
// verb's install-wide counterpart, which CheckControl reads as that app verb
// on every app (R-081, issue #81).
//
// This is the one place an install verb implies an app verb, and it is a
// table rather than a rule — not "strip install.apps. and prepend app." — so
// that it can be read in one look, and so a verb added to the catalog has no
// counterpart until somebody writes one here on purpose. One install verb per
// app verb, never a bundle: a bundle is a role (App viewer, App manager).
//
// Keyed by the install verb. TestR080_EveryAppVerbHasOneInstallCounterpart
// holds it to exactly one entry per app verb.
var everyApp = map[Verb]Verb{
	InstallAppsView:              AppView,
	InstallAppsLogsRead:          AppLogsRead,
	InstallAppsDeploy:            AppDeploy,
	InstallAppsRestart:           AppRestart,
	InstallAppsSpecEdit:          AppSpecEdit,
	InstallAppsSecretsWrite:      AppSecretsWrite,
	InstallAppsSecretsRead:       AppSecretsRead,
	InstallAppsExec:              AppExec,
	InstallAppsGrantsManage:      AppGrantsManage,
	InstallAppsRoutingOverride:   AppRoutingOverride,
	InstallAppsResourcesOverride: AppResourceOverride,
	InstallAppsEgressTighten:     AppEgressTighten,
	InstallAppsEgressLoosen:      AppEgressLoosen,
	InstallDeploysApprove:        AppDeployApprove,
	InstallAppsDelete:            AppDelete,
}

// EveryApp returns the app verb an install verb stands for on every app, and
// whether it stands for one. For listing and explaining access; deciding it
// is CheckControl's.
func EveryApp(install Verb) (Verb, bool) {
	v, ok := everyApp[install]
	return v, ok
}

// InstallCounterpart returns the install verb that stands for an app verb on
// every app, and whether there is one.
func InstallCounterpart(app Verb) (Verb, bool) {
	for install, v := range everyApp {
		if v == app {
			return install, true
		}
	}
	return "", false
}
