package policy

import (
	"context"
	"fmt"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/spec"
	"github.com/trypando/pando/internal/errs"
)

// Document is the host policy (design 02 §2.5).
//
// A single versioned document rather than scattered columns, so applying policy
// to a running install is one transaction and one audit event (R-274).
//
// Policy is a floor, not an override (R-272): it can only deny. Nothing here
// grants anything, which is why it is evaluated before grants rather than mixed
// with them.
type Document struct {
	// SourceAllowlist restricts where apps may be created from (R-092). Empty
	// means no restriction — Pando ships permissive defaults (R-270).
	SourceAllowlist []string `json:"source_allowlist,omitempty"`

	// DisabledVerbs are denied install-wide, to everyone, including an app's
	// owner. R-085's "host policy may disable exec install-wide" is this list
	// containing app.exec.
	DisabledVerbs []string `json:"disabled_verbs,omitempty"`

	// AgentDisabledVerbs are denied to token principals only — the CLI token an
	// agent holds, and the MCP server acting through one (R-262).
	//
	// This is O-12's resolution. The tempting alternative is a list of tools
	// the MCP server refuses to expose, and it does not work: an agent holding
	// a token can call the REST API directly, so an MCP-layer exclusion is a
	// speed bump rather than a boundary. Expressed here, it is evaluated before
	// grants for every surface, which is the only place it means anything.
	//
	// Default-closed: Default() ships the exclusions design 04 §3 lists, and an
	// install can lift them by editing policy like any other rule.
	AgentDisabledVerbs []string `json:"agent_disabled_verbs,omitempty"`

	// AllowAnonymousGrants controls whether an app may be shared with everyone
	// (R-076). Pointer so that "unset" is distinguishable from "explicitly
	// false", which matters when a policy document is partially written.
	//
	// Superseded by PublicSharing, which says more; still read, so a policy
	// file or environment that sets it keeps meaning what it meant — false is
	// PublicSharingNone. PublicSharing wins when both are set.
	AllowAnonymousGrants *bool `json:"allow_anonymous_grants,omitempty"`

	// PublicSharing is how an app may be shared with everyone (R-076, R-075a):
	// at all, only behind a passcode, or not at all.
	PublicSharing PublicSharing `json:"public_sharing,omitempty"`

	// MinBuildIsolation and MinRuntimeIsolation are floors (R-024, R-114).
	// Separate because how isolated a build must be is a different question
	// from how isolated the running app must be.
	MinBuildIsolation   spec.IsolationClass `json:"min_build_isolation,omitempty"`
	MinRuntimeIsolation spec.IsolationClass `json:"min_runtime_isolation,omitempty"`

	// Egress (R-181 – R-185). The install's rules are a floor (R-183): an
	// app's own list narrows them, and only the loosening moves EgressFor
	// names — adding to an allowlist, removing from a denylist, turning
	// private-range blocking off — are subject to EgressLoosening.
	//
	// EgressMode is allow_all (empty), denylist or allowlist, and EgressList
	// is the list it reads. EgressBlockPrivate works with any mode.
	EgressMode         spec.EgressMode `json:"egress_mode,omitempty"`
	EgressList         []string        `json:"egress_list,omitempty"`
	EgressBlockPrivate bool            `json:"egress_block_private,omitempty"`

	// EgressLoosening is whether an app may loosen the rules above: by
	// somebody holding app.egress.loosen (empty, the default, R-270), only
	// after a deploy approval (R-154), or never.
	EgressLoosening EgressLoosening `json:"egress_loosening,omitempty"`

	// EgressAllowlist is the field before issue #79, when there was no mode
	// and an app's list replaced this one. Still read — a non-empty one with
	// no EgressMode is an allowlist — so a policy file that sets it keeps
	// meaning what its author wrote. Never enforced before, so an install that
	// set it starts being held to it; that is what the setting said.
	EgressAllowlist []string `json:"egress_allowlist,omitempty"`

	// Deploy approval (R-154 – R-159). Off unless something here, or the app's
	// own spec, turns it on.
	//
	// DeployApprovalRequired: every app's deploys need approval.
	// DeployApprovalApps: these apps' deploys do, whatever their owners say.
	// DeployApprovalCount: how many approvals a deploy needs; zero is one.
	// DeployApprovalExpiryHours: how long a request waits; zero is forever.
	// Default() ships seven days.
	DeployApprovalRequired    bool     `json:"deploy_approval_required,omitempty"`
	DeployApprovalApps        []string `json:"deploy_approval_apps,omitempty"`
	DeployApprovalCount       int      `json:"deploy_approval_count,omitempty"`
	DeployApprovalExpiryHours int      `json:"deploy_approval_expiry_hours,omitempty"`

	// RequireBackupBeforeDestroy: an admin sets "never destroy without backup"
	// once, and app owners cannot override downward (R-284).
	RequireBackupBeforeDestroy bool `json:"require_backup_before_destroy,omitempty"`

	// MaxTokenLifetimeDays caps how long a token may live; 0 means no cap.
	// R-061 allows policy to forbid non-expiring tokens.
	MaxTokenLifetimeDays int `json:"max_token_lifetime_days,omitempty"`

	// MaxLogDiskBytes is R-224: the total this install will commit to app logs
	// across every app. Zero means no aggregate limit.
	//
	// Enforced at plan time against the **sum of every app's cap**, not against
	// measured usage — which is O-16's resolution. Bounding what is committed
	// is the stronger guarantee: if every app's logs are capped and the caps
	// sum under the budget, the total cannot exceed it. Measuring usage would
	// mean acting after the disk was already filling, and the only remedy then
	// is recreating containers, which the reconciler may not do on a schedule
	// because an unrelated app turned chatty.
	MaxLogDiskBytes int64 `json:"max_log_disk_bytes,omitempty"`

	// Audit retention (R-347, R-348). The live audit log keeps
	// AuditRetentionMonths whole months; an older month is archived, the
	// archive read back and checked, and only then is the month removed.
	//
	// Zero is three months. Fewer than three is refused here, and the
	// database refuses it again: the floor is held by the function that
	// removes a month, so nothing the running server can write lowers it.
	// More is anyone's choice.
	//
	// AuditArchive is where an archive goes: keep (empty, the default) under
	// Pando's own directory, export to a backup destination — the default
	// one, or AuditArchiveDestination — or off, which archives nothing and so
	// removes nothing, the log growing as it did before retention existed.
	AuditRetentionMonths    int               `json:"audit_retention_months,omitempty"`
	AuditArchive            audit.ArchiveMode `json:"audit_archive,omitempty"`
	AuditArchiveDestination string            `json:"audit_archive_destination,omitempty"`

	// DisableAIScreening forbids AI screening of deployment plans install-wide
	// (R-336). Default false, like everything else here: Pando ships permissive
	// and configuration narrows (R-270).
	//
	// Worth an explicit setting rather than leaving it to whether an adapter is
	// configured, because the two are different decisions made by different
	// people. Screening sends repository contents to a provider (R-337), and an
	// administrator who has to say no to that should not have to do it by
	// deleting somebody else's adapter.
	DisableAIScreening bool `json:"disable_ai_screening,omitempty"`

	// DisableAnonymousUseAudit stops recording app.use for visitors who are
	// not signed in (R-227). Default false: anonymous use of a public app is
	// recorded like anyone's, once per browser session, because "who used this
	// app" after a leak includes the people nobody knew by name. An install
	// with a busy public site that does not want those rows turns it off.
	DisableAnonymousUseAudit bool `json:"disable_anonymous_use_audit,omitempty"`

	// The security score (R-314 – R-316, design 09 §5).
	//
	// MinSecurityScore is the floor a deploy has to clear, 0 to 100. Zero is
	// off, which is the shipped posture (R-270) — and so is an installation
	// with no scanner configured, where this is inert and the console says so
	// rather than silently blocking every deploy (R-317).
	MinSecurityScore int `json:"min_security_score,omitempty"`

	// InsecureAction is what happens to an app that is *already running* when
	// it falls below the floor: "warn" or "stop". Empty means warn.
	//
	// A policy change or a newly published CVE is not a reason to take
	// somebody's working service away without notice, so stopping is opt-in
	// and grace comes with it (R-315).
	InsecureAction string `json:"insecure_action,omitempty"`

	// InsecureGraceHours is how long an app has after it is first found below
	// the floor, before "stop" applies. Zero with InsecureAction "stop" means
	// the default below, not "immediately" — an accidental zero must not empty
	// a host.
	InsecureGraceHours int `json:"insecure_grace_hours,omitempty"`

	// IgnoreUnfixableFindings drops findings with no fix available from both
	// the score and the list.
	//
	// Off by default, so the number means "what is wrong with this app" rather
	// than "what could this app's owner do about it today" — and an upgrade
	// does not silently move every score. An installation that has decided it
	// only acts on what it can fix turns it on, and the two agree again: what
	// is counted is what is shown.
	IgnoreUnfixableFindings bool `json:"ignore_unfixable_findings,omitempty"`

	// DisablePasswordSignIn refuses sign-in with a local username and
	// password (issue #51), once people sign in through an external identity
	// provider. Refused while no external provider is on, so it cannot empty
	// the sign-in page. `pando admin enable-password-sign-in` is the way back
	// when every provider is unreachable, and needs shell access on the host.
	DisablePasswordSignIn bool `json:"disable_password_sign_in,omitempty"`

	// DisableJITProvisioning refuses making an account at a first external
	// sign-in, whatever each provider is set to. People then need an account
	// first — pushed by SCIM, or linked by an administrator.
	DisableJITProvisioning bool `json:"disable_jit_provisioning,omitempty"`

	// DisableUpdateCheck stops Pando asking GitHub whether a newer release
	// exists (R-349). Default false — on — so an install learns about a
	// security release without anyone remembering to look; off sends no
	// request at all, for an air-gapped install or one that may not call out.
	DisableUpdateCheck bool `json:"disable_update_check,omitempty"`

	// AllowPrivateWebhooks lets an event subscription's webhook reach a
	// private, loopback or link-local address (R-372). Default false: anybody
	// who can see an app can subscribe to it, and a webhook that may point
	// anywhere is a way to make Pando send requests into its own network — to
	// a cloud metadata address, or to the database it runs on.
	AllowPrivateWebhooks bool `json:"allow_private_webhooks,omitempty"`

	// UpdateChannel is which releases the check offers (R-350): stable, the
	// default, or prerelease, which also offers release candidates.
	UpdateChannel UpdateChannel `json:"update_channel,omitempty"`

	// UpgradeInPlace lets Pando replace its own container with a newer
	// release (R-355). Off by default, against R-270 on purpose: a version
	// named in a Compose file or infrastructure-as-code is put back by its
	// next apply, so whoever owns that configuration turns this on — usually
	// there, as PANDO_POLICY_UPGRADE_IN_PLACE, which also locks it here.
	UpgradeInPlace bool `json:"upgrade_in_place,omitempty"`

	// AutoUpgradePatches upgrades to a new patch release of the running minor
	// line inside MaintenanceWindow, with nobody present (R-361). Needs
	// UpgradeInPlace and a window.
	AutoUpgradePatches bool `json:"auto_upgrade_patches,omitempty"`

	// MaintenanceWindow is when an automatic upgrade may start, in UTC:
	// weekdays, a start time and a length, as "sun,wed 02:00 2h" (R-361).
	// One string so that it reads the same in the config file, an
	// environment variable and the API.
	MaintenanceWindow string `json:"maintenance_window,omitempty"`
}

// UpdateChannel is which releases the update check offers (R-350).
type UpdateChannel string

const (
	// UpdateChannelStable offers releases only.
	UpdateChannelStable UpdateChannel = "stable"
	// UpdateChannelPrerelease also offers release candidates.
	UpdateChannelPrerelease UpdateChannel = "prerelease"
)

// Valid refuses a value that is neither. Empty is unset, which is stable.
func (c UpdateChannel) Valid() error {
	switch c {
	case "", UpdateChannelStable, UpdateChannelPrerelease:
		return nil
	}
	return fmt.Errorf("%q is not an update_channel setting; use stable or prerelease", string(c))
}

// Channel is the update channel in force, stable when unset.
func (d Document) Channel() UpdateChannel {
	if d.UpdateChannel == "" {
		return UpdateChannelStable
	}
	return d.UpdateChannel
}

// What InsecureAction may say.
const (
	InsecureWarn = "warn"
	InsecureStop = "stop"
)

// DefaultInsecureGraceHours is the grace an install gets when it turns stopping
// on without saying how long. A day: long enough for somebody to see the
// warning in a working week's morning, short enough to mean something.
const DefaultInsecureGraceHours = 24

// GraceHours is the grace in force, with the default applied.
func (d Document) GraceHours() int {
	if d.InsecureGraceHours > 0 {
		return d.InsecureGraceHours
	}
	return DefaultInsecureGraceHours
}

// StopsInsecureApps reports whether policy says to stop them (R-316).
func (d Document) StopsInsecureApps() bool {
	return d.InsecureAction == InsecureStop
}

// PublicSharing is the host's rule for sharing an app with everyone.
type PublicSharing string

const (
	// PublicSharingAllowed: public, with or without a passcode.
	PublicSharingAllowed PublicSharing = "allowed"
	// PublicSharingPasscodeOnly: public only behind a passcode.
	PublicSharingPasscodeOnly PublicSharing = "passcode_only"
	// PublicSharingNone: no app may be shared with everyone.
	PublicSharingNone PublicSharing = "none"
)

// Valid refuses a value that is none of the three. Empty is unset.
func (p PublicSharing) Valid() error {
	switch p {
	case "", PublicSharingAllowed, PublicSharingPasscodeOnly, PublicSharingNone:
		return nil
	}
	return fmt.Errorf("%q is not a public_sharing setting; use allowed, passcode_only or none", string(p))
}

// PublicSharingMode is the rule in force, reading the older boolean when the
// new field is unset.
func (d Document) PublicSharingMode() PublicSharing {
	if d.PublicSharing != "" {
		return d.PublicSharing
	}
	if d.AllowAnonymousGrants != nil && !*d.AllowAnonymousGrants {
		return PublicSharingNone
	}
	return PublicSharingAllowed
}

// Default is the permissive starting posture (R-270).
func Default() Document {
	allowAnonymous := true
	return Document{
		AllowAnonymousGrants: &allowAnonymous,
		MinBuildIsolation:    spec.IsolationContainer,
		MinRuntimeIsolation:  spec.IsolationContainer,

		DeployApprovalExpiryHours: DefaultDeployApprovalExpiryHours,

		// Design 04 §3's exclusions, as the shipped default rather than as a
		// hard-coded list in the MCP server (O-12). These are the
		// highest-consequence actions in the system, and R-086 already concedes
		// exec is not bounded by the verb list — an agent should not hold them
		// by default. An install can lift any of them by editing policy.
		AgentDisabledVerbs: []string{
			string(authz.AppExec),
			string(authz.AppSecretsRead),
			string(authz.AppGrantsManage),
			string(authz.InstallPolicyManage),
			string(authz.InstallUsersManage),
			string(authz.InstallBackupManage),

			// Approval is a human sign-off on a change (R-154). An agent
			// approving a deploy — possibly one it asked for itself — is
			// the second pair of eyes being nobody's.
			string(authz.InstallDeploysApprove),
			string(authz.AppDeployApprove),

			// Replacing the server everything runs behind (R-356).
			string(authz.InstallUpgrade),
		},
	}
}

// Evaluator answers policy questions against the current document.
type Evaluator struct {
	load func(ctx context.Context) (Document, error)
}

// New builds an evaluator over a document source.
//
// The document is loaded per evaluation rather than cached, because R-274 says
// policy applies to a running install: a cached policy would keep denying, or
// keep allowing, for however long the cache lived.
func New(load func(ctx context.Context) (Document, error)) *Evaluator {
	return &Evaluator{load: load}
}

// Static builds an evaluator over a fixed document, for tests and for an
// install that has not written one yet.
func Static(d Document) *Evaluator {
	return &Evaluator{load: func(context.Context) (Document, error) { return d, nil }}
}

// Allows implements authz.Policy: it reports whether a verb is permitted at all.
//
// Evaluated before grants (design 06 §2, step 5). A policy that disables exec
// install-wide denies the owner too.
func (e *Evaluator) Allows(ctx context.Context, p authz.Principal, verb authz.Verb, _ string) error {
	doc, err := e.load(ctx)
	if err != nil {
		return err
	}

	// Agents first, so the more specific rule produces the more specific
	// message. A person told "this is turned off for the installation" when it
	// is only turned off for their agent would go looking in the wrong place.
	if p.Kind == authz.KindToken {
		for _, disabled := range doc.AgentDisabledVerbs {
			if authz.Verb(disabled) == verb {
				return errs.Newf(errs.PolicyExecDisabled,
					"Tokens and agents are not allowed to do this on this installation.").
					WithDetail("verb", string(verb)).
					WithRemedy("Do it signed in, or ask an administrator to allow it for agents in the installation's policy settings.")
			}
		}
	}

	for _, disabled := range doc.DisabledVerbs {
		if authz.Verb(disabled) != verb {
			continue
		}
		if verb == authz.AppExec {
			// R-085 has its own code because the console explains this one
			// specifically — exec being off install-wide is a deliberate
			// posture, not a permissions mistake to be escalated.
			return errs.New(errs.PolicyExecDisabled,
				"Running commands inside apps is turned off for this installation.").
				WithRemedy("An administrator can turn it back on in the installation's policy settings.")
		}
		return errs.Newf(errs.PolicyExecDisabled,
			"This action is turned off for this installation.").
			WithDetail("verb", string(verb)).
			WithRemedy("An administrator can change this in the installation's policy settings.")
	}
	return nil
}

// AllowsAnonymousGrant checks R-076: whether an app may be shared with
// everyone, with a passcode or without one.
func (e *Evaluator) AllowsAnonymousGrant(ctx context.Context, withPasscode bool) error {
	doc, err := e.load(ctx)
	if err != nil {
		return err
	}
	switch doc.PublicSharingMode() {
	case PublicSharingNone:
		return errs.New(errs.PolicyAnonymousGrantForbidden,
			"Apps on this installation cannot be shared with anyone on the internet.").
			WithRemedy("Share the app with specific people or groups instead, or ask an administrator whether this can be allowed.")
	case PublicSharingPasscodeOnly:
		if !withPasscode {
			return errs.New(errs.PolicyAnonymousGrantForbidden,
				"Apps on this installation can be shared with everyone only behind a passcode.").
				WithRemedy("Make it public with a passcode instead, or share it with specific people or groups.")
		}
	}
	return nil
}

// PublicSharing is the host's rule for sharing with everyone, for the console
// to offer only what it allows.
func (e *Evaluator) PublicSharing(ctx context.Context) (PublicSharing, error) {
	doc, err := e.load(ctx)
	if err != nil {
		return "", err
	}
	return doc.PublicSharingMode(), nil
}

// IsolationFloors returns the build and runtime floors (R-024, R-114).
func (e *Evaluator) IsolationFloors(ctx context.Context) (build, runtime spec.IsolationClass, err error) {
	doc, err := e.load(ctx)
	if err != nil {
		return 0, 0, err
	}
	return doc.MinBuildIsolation, doc.MinRuntimeIsolation, nil
}

// AllowsScreening is host policy's veto over AI screening (R-336).
//
// A reason rather than an error, and empty when it is allowed. This is not a
// failure — it is a posture, and it is shown in the review as one. An
// unreadable policy document denies: a screening that cannot be checked against
// policy is one that has not been checked, and the safe direction is the one
// that sends nothing anywhere.
func (e *Evaluator) AllowsScreening(ctx context.Context) string {
	doc, err := e.load(ctx)
	if err != nil {
		return "Pando could not read its host policy, so it did not screen this plan."
	}
	if doc.DisableAIScreening {
		return "An administrator has turned off AI screening on this installation."
	}
	return ""
}

// RecordsAnonymousUse reports whether a visit by someone not signed in is
// written to the audit log as app.use (R-227). A policy that cannot be read
// records it: when unsure, keep the record.
func (e *Evaluator) RecordsAnonymousUse(ctx context.Context) bool {
	doc, err := e.load(ctx)
	if err != nil {
		return true
	}
	return !doc.DisableAnonymousUseAudit
}

// Document returns the current policy.
func (e *Evaluator) Document(ctx context.Context) (Document, error) { return e.load(ctx) }

var _ authz.Policy = (*Evaluator)(nil)

// DefaultDeployApprovalExpiryHours is how long a deploy request waits for
// approval when policy does not say (R-156): a week, so a request made on a
// Friday survives the weekend.
const DefaultDeployApprovalExpiryHours = 7 * 24

// DeployApprovalFor reports whether host policy requires approval for an
// app's deploys: install-wide, or for this app by name. The app's own spec
// can require it too, which the approval service reads (R-154).
func (d Document) DeployApprovalFor(appID string) bool {
	if d.DeployApprovalRequired {
		return true
	}
	for _, id := range d.DeployApprovalApps {
		if id == appID {
			return true
		}
	}
	return false
}

// ApprovalsNeeded is how many approvals a deploy needs, with the default of
// one applied.
func (d Document) ApprovalsNeeded() int {
	if d.DeployApprovalCount > 0 {
		return d.DeployApprovalCount
	}
	return 1
}

// ValidateRules checks the settings that would otherwise be saved and enforce
// nothing, or enforce something nobody meant: an egress mode or entry that
// does not parse, or an approval count below zero. Written for the
// administrator who typed it (R-105).
func (d Document) ValidateRules() error {
	if err := d.ValidateEgress(); err != nil {
		return err
	}
	if d.DeployApprovalCount < 0 {
		return fmt.Errorf("deploy_approval_count is %d; it is how many approvals a deploy needs, so use 1 or more (0 also means 1)", d.DeployApprovalCount)
	}
	if d.DeployApprovalExpiryHours < 0 {
		return fmt.Errorf("deploy_approval_expiry_hours is %d; use a number of hours, or 0 for requests that wait until somebody answers", d.DeployApprovalExpiryHours)
	}
	if err := d.UpdateChannel.Valid(); err != nil {
		return err
	}
	if err := d.ValidateUpgrades(); err != nil {
		return err
	}
	return d.ValidateAuditRetention()
}

// ValidateAuditRetention refuses retention under the floor and a mode that is
// not one (R-348). Written for the administrator who typed it (R-105).
func (d Document) ValidateAuditRetention() error {
	if d.AuditRetentionMonths != 0 && d.AuditRetentionMonths < audit.MinRetentionMonths {
		return fmt.Errorf("audit_retention_months is %d; the audit log keeps at least %d months, so use %d or more (0 also means %d)",
			d.AuditRetentionMonths, audit.MinRetentionMonths, audit.MinRetentionMonths, audit.DefaultRetentionMonths)
	}
	if err := d.AuditArchive.Valid(); err != nil {
		return err
	}
	if d.AuditArchiveDestination != "" && d.AuditArchive != audit.ArchiveExport {
		return fmt.Errorf("audit_archive_destination is set to %q, but audit_archive is not export, so nothing would be sent there; set audit_archive to export or clear the destination",
			d.AuditArchiveDestination)
	}
	return nil
}

// AuditRetention is what the audit archiver reads (R-347).
func (d Document) AuditRetention() audit.Retention {
	return audit.Retention{
		Months:      d.AuditRetentionMonths,
		Archive:     d.AuditArchive,
		Destination: d.AuditArchiveDestination,
	}
}
