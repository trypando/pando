package authz

import (
	"context"

	"github.com/trypando/pando/internal/errs"
)

// PrincipalKind identifies what is making a request.
type PrincipalKind string

const (
	KindUser      PrincipalKind = "user"
	KindToken     PrincipalKind = "token"
	KindAnonymous PrincipalKind = "anonymous"
	KindSystem    PrincipalKind = "system"
)

// Principal is the resolved identity of a request (design 06 §1).
type Principal struct {
	Kind PrincipalKind
	ID   string

	// UserID is the user authorization runs against. For a user it equals ID.
	// For a delegated token it is the owner (R-058, R-059) — authorization then
	// proceeds exactly as if the owner made the request, and no grant is ever
	// written for the token itself.
	//
	// For an account token it is empty: an account token is its own principal
	// and is looked up in grants under its own ID (R-060).
	UserID string

	TokenID string

	// Groups are resolved live per request (R-079), never denormalized.
	Groups []string

	// Email and DisplayName travel into the assertion's claims. They are
	// display detail, never used for authorization — an email is not an
	// identity here, users.id is (R-054).
	Email       string
	DisplayName string

	AdapterID string

	// Status of the underlying user, checked at step 2 of the evaluation order.
	Status string

	// Passcodes are the unlock tokens the request carried, by app ID (R-075a):
	// proof that whoever is visiting entered a passcode-protected app's
	// passcode. Unverified as carried — CheckData asks the store whether each
	// is live, on every request, as it asks about everything else.
	Passcodes map[string]string
}

// Anonymous is the principal for an unauthenticated request. It is a real
// principal with a real check path, not an absence — R-075's anonymous grant is
// a grant like any other, and R-056's assertion carries sub: "anonymous".
func Anonymous() Principal { return Principal{Kind: KindAnonymous} }

// System is the principal for the reconciler and background jobs. It bypasses
// grant checks but still writes audit events attributed to "system": anything a
// background job does must be as visible as anything a person does.
func System() Principal { return Principal{Kind: KindSystem, ID: "system"} }

// Store is the authorizer's view of persisted state.
//
// Narrow on purpose: the authorizer must not be able to reach anything else, and
// a small interface makes the evaluation order testable without a database.
type Store interface {
	// UserStatus returns the status of a user: active, suspended, or deleted.
	UserStatus(ctx context.Context, userID string) (string, error)

	// ControlGrantsFor returns control-plane grants matching the principal —
	// direct, by group, or by account token.
	ControlGrantsFor(ctx context.Context, appID string, p Principal) ([]Grant, error)

	// InstallGrantsFor returns the principal's installation-wide grants: the
	// ones with no app (O-17). A separate query from ControlGrantsFor rather
	// than a nullable argument, so an app-scoped lookup can never return an
	// install grant by passing the wrong value.
	InstallGrantsFor(ctx context.Context, p Principal) ([]Grant, error)

	// IsOwner reports whether userID owns the app (R-031).
	IsOwner(ctx context.Context, appID, userID string) (bool, error)

	// HasDataGrant reports whether the principal holds a data-plane grant,
	// directly or through a group.
	HasDataGrant(ctx context.Context, appID string, p Principal) (bool, error)

	// AnonymousAccess reports whether the app is shared with everyone (R-075),
	// and whether that sharing asks for a passcode (R-075a).
	AnonymousAccess(ctx context.Context, appID string) (granted, passcode bool, err error)

	// PasscodeUnlocked reports whether token is a live unlock of this app's
	// passcode: entered, not expired, and made under the grant that stands now.
	PasscodeUnlocked(ctx context.Context, appID, token string) (bool, error)

	// Role returns a role by ID.
	Role(ctx context.Context, roleID string) (Role, error)
}

// Grant binds a principal to an app on one plane.
type Grant struct {
	ID            string
	AppID         string
	Plane         string
	PrincipalKind string
	PrincipalID   string
	RoleID        string

	// Role is the grant's role, when the store read it with the grant. Nil
	// means it did not, and the authorizer asks Store.Role for it.
	Role *Role
}

// Policy is host policy, evaluated before grants.
type Policy interface {
	// Allows reports whether host policy permits the verb at all. Policy is a
	// floor, not an override (R-272): a policy disabling exec install-wide
	// denies the owner too.
	//
	// The principal is passed because O-12's resolution needs it. "Agents may
	// not exec here" is expressed as a policy scoped to token principals rather
	// than as an MCP-layer exclusion list, because an agent holding a token can
	// call the REST API directly — an MCP-specific block is a speed bump, not a
	// boundary. Enforcement has to live where every surface passes through it,
	// and this is that place.
	Allows(ctx context.Context, p Principal, verb Verb, appID string) error
}

// Snapshotter is a Policy that can answer several questions from one read of
// its document. AppVerbs asks policy about every app verb for one screen, and
// reading the document once per verb was most of what that cost.
//
// The snapshot lives for one call and is never kept: policy applies to a
// running install the moment it changes (R-274), so a snapshot held across
// requests would be the cache R-274 rules out.
type Snapshotter interface {
	Snapshot(ctx context.Context) (Policy, error)
}

// DenialAuditPolicy is a Policy that says whether anonymous denials on the
// data plane are audited. A policy that does not implement it audits them.
type DenialAuditPolicy interface {
	AuditsAnonymousDenials(ctx context.Context) bool
}

// Auditor records authorization outcomes.
type Auditor interface {
	Denied(ctx context.Context, p Principal, appID string, verb Verb, reason errs.Code)

	// ThroughInstall records an app verb allowed by an install-wide grant
	// rather than one on the app (issue #81), so the audit log shows that
	// the access came from everywhere and not from here: which install verb,
	// through which grant. Not called for app.view and app.logs.read, which
	// the console asks on every screen and every few seconds (see
	// auditsReach).
	ThroughInstall(ctx context.Context, p Principal, appID string, verb, through Verb, grant Grant)
}

// Authorizer evaluates both planes.
type Authorizer struct {
	store   Store
	policy  Policy
	auditor Auditor
}

func New(store Store, policy Policy, auditor Auditor) *Authorizer {
	return &Authorizer{store: store, policy: policy, auditor: auditor}
}

// CheckControl authorizes a control-plane action: managing an app.
//
// The order is fixed and each step can only deny; none can restore access denied
// by an earlier step (design 06 §2).
func (a *Authorizer) CheckControl(ctx context.Context, p Principal, appID string, verb Verb) error {
	// An install verb has no app to be held on, and evaluating one here would
	// search for a grant that cannot exist and deny — safe, but it would hide a
	// call site that meant CheckInstall. Refused loudly instead.
	if InstallScoped(verb) {
		return errs.Newf(errs.Internal,
			"%s is an installation-wide permission and cannot be checked against an app.", verb)
	}

	if p.Kind == KindSystem {
		// The reconciler and background jobs. Grant checks are bypassed; audit
		// is not.
		return nil
	}

	via, denial, err := a.control(ctx, p, appID, verb)
	if err != nil {
		return err
	}
	if denial != nil {
		return a.deny(ctx, p, appID, verb, denial)
	}
	if via != nil && a.auditor != nil && auditsReach(verb) {
		a.auditor.ThroughInstall(ctx, p, appID, verb, via.verb, via.grant)
	}
	return nil
}

// auditsReach reports whether an app verb allowed through an install grant
// is written to the audit log. Every verb but the two reads: app.view is
// asked on every app screen and app.logs.read every few seconds while the
// logs are open, and a row for each would bury the changes the log is for.
// Neither read is audited for anybody else either. [P] design 06 §6.
func auditsReach(verb Verb) bool {
	return verb != AppView && verb != AppLogsRead
}

// AppVerbs returns the app verbs the principal may use on an app, by asking
// the same question CheckControl asks for each — so what the console shows as
// editable is what the API will allow, and cannot drift from it. Denials are
// not audited: this is somebody looking at a screen, not trying anything.
//
// The question is asked once per app verb, with the same rules as control,
// but the principal's status, the policy document, the grants and their roles
// are read once for the whole call rather than once per verb (lookups). None
// of it outlives the call (R-274).
func (a *Authorizer) AppVerbs(ctx context.Context, p Principal, appID string) ([]Verb, error) {
	out := []Verb{}
	m := &lookups{}
	for _, verb := range AppVerbs() {
		if p.Kind == KindSystem {
			out = append(out, verb)
			continue
		}
		_, denial, err := a.controlWith(ctx, m, p, appID, verb)
		if err != nil {
			return nil, err
		}
		if denial == nil {
			out = append(out, verb)
		}
	}
	return out, nil
}

// BatchStore is a Store that can read the principal's control grants on
// several apps in one call. AllowsEach uses it when the store offers it; a
// store without it is asked app by app, with the same answers.
type BatchStore interface {
	// ControlGrantsForApps returns ControlGrantsFor's answer for each app,
	// with an entry, possibly empty, for every app asked about.
	ControlGrantsForApps(ctx context.Context, appIDs []string, p Principal) (map[string][]Grant, error)
}

// AllowsEach is Allows for several apps and verbs at once: for each app, the
// verbs CheckControl would allow on it, without auditing a denial. For a list
// that shows what the caller may do on each of its rows (issue #72).
//
// Each answer is the one Allows gives, by the same rules in the same order:
// the principal, the policy document, the install grants and their roles are
// read once for the call, as AppVerbs reads them, and the control grants for
// every app in one query when the store is a BatchStore. None of it outlives
// the call (R-274).
func (a *Authorizer) AllowsEach(ctx context.Context, p Principal, appIDs []string, verbs ...Verb) (map[string]map[Verb]bool, error) {
	for _, verb := range verbs {
		if InstallScoped(verb) {
			return nil, errs.Newf(errs.Internal,
				"%s is an installation-wide permission and cannot be checked against an app.", verb)
		}
	}
	out := make(map[string]map[Verb]bool, len(appIDs))
	m := &lookups{}
	if p.Kind != KindSystem && len(appIDs) > 0 {
		if b, ok := a.store.(BatchStore); ok {
			grants, err := b.ControlGrantsForApps(ctx, appIDs, p)
			if err != nil {
				return nil, err
			}
			m.control = grants
		}
	}
	for _, appID := range appIDs {
		allowed := make(map[Verb]bool, len(verbs))
		for _, verb := range verbs {
			if p.Kind == KindSystem {
				allowed[verb] = true
				continue
			}
			_, denial, err := a.controlWith(ctx, m, p, appID, verb)
			if err != nil {
				return nil, err
			}
			allowed[verb] = denial == nil
		}
		out[appID] = allowed
	}
	return out, nil
}

// lookups holds what control reads, for one call that asks it about several
// verbs. Each is read the first time a verb needs it, so a call asks the store
// for nothing a per-verb check would not have asked for. A nil *lookups reads
// everything fresh, which is what every single-verb check does.
type lookups struct {
	principalDone bool
	principalErr  error

	policyDone bool
	policy     Policy
	policyErr  error

	// control holds each app's grants once read; an app missing from it
	// has not been read yet.
	control map[string][]Grant

	installDone bool
	install     []Grant

	roles map[string]Role
}

// checkPrincipalWith runs steps 1–4, once per lookups.
func (a *Authorizer) checkPrincipalWith(ctx context.Context, m *lookups, p Principal) error {
	if m == nil {
		return a.checkPrincipal(ctx, p)
	}
	if !m.principalDone {
		m.principalErr = a.checkPrincipal(ctx, p)
		m.principalDone = true
	}
	return m.principalErr
}

// policyAllows runs step 5. With lookups, the document is read once: through
// a snapshot when the policy offers one, and a failure to read it denies every
// verb, as Allows would have for each.
func (a *Authorizer) policyAllows(ctx context.Context, m *lookups, p Principal, verb Verb, appID string) error {
	if a.policy == nil {
		return nil
	}
	if m == nil {
		return a.policy.Allows(ctx, p, verb, appID)
	}
	if !m.policyDone {
		m.policy = a.policy
		if s, ok := a.policy.(Snapshotter); ok {
			m.policy, m.policyErr = s.Snapshot(ctx)
		}
		m.policyDone = true
	}
	if m.policyErr != nil {
		return m.policyErr
	}
	return m.policy.Allows(ctx, p, verb, appID)
}

func (a *Authorizer) controlGrants(ctx context.Context, m *lookups, p Principal, appID string) ([]Grant, error) {
	if m == nil {
		return a.store.ControlGrantsFor(ctx, appID, p)
	}
	if grants, ok := m.control[appID]; ok {
		return grants, nil
	}
	grants, err := a.store.ControlGrantsFor(ctx, appID, p)
	if err != nil {
		return nil, err
	}
	if m.control == nil {
		m.control = map[string][]Grant{}
	}
	m.control[appID] = grants
	return grants, nil
}

func (a *Authorizer) installGrants(ctx context.Context, m *lookups, p Principal) ([]Grant, error) {
	if m == nil {
		return a.store.InstallGrantsFor(ctx, p)
	}
	if !m.installDone {
		grants, err := a.store.InstallGrantsFor(ctx, p)
		if err != nil {
			return nil, err
		}
		m.install, m.installDone = grants, true
	}
	return m.install, nil
}

// roleOf returns a grant's role: the one read with the grant when the store
// joined it, else the store's answer, kept for the rest of the call.
func (a *Authorizer) roleOf(ctx context.Context, m *lookups, g Grant) (Role, error) {
	if g.Role != nil {
		return *g.Role, nil
	}
	if m != nil {
		if r, ok := m.roles[g.RoleID]; ok {
			return r, nil
		}
	}
	r, err := a.store.Role(ctx, g.RoleID)
	if err != nil {
		return Role{}, err
	}
	if m != nil {
		if m.roles == nil {
			m.roles = map[string]Role{}
		}
		m.roles[g.RoleID] = r
	}
	return r, nil
}

// Allows reports whether CheckControl would allow the verb, without auditing a
// denial — for deciding what to show, as AppVerbs does, one verb at a time.
func (a *Authorizer) Allows(ctx context.Context, p Principal, appID string, verb Verb) (bool, error) {
	if InstallScoped(verb) {
		return false, errs.Newf(errs.Internal,
			"%s is an installation-wide permission and cannot be checked against an app.", verb)
	}
	if p.Kind == KindSystem {
		return true, nil
	}
	_, denial, err := a.control(ctx, p, appID, verb)
	if err != nil {
		return false, err
	}
	return denial == nil, nil
}

// PreviewControl is CheckControl's answer without auditing a denial: the same
// refusal, with the same reason, for a dry run that asks what would happen
// and changes nothing. A denial nobody suffered is not an event (design 06 §6).
func (a *Authorizer) PreviewControl(ctx context.Context, p Principal, appID string, verb Verb) error {
	if InstallScoped(verb) {
		return errs.Newf(errs.Internal,
			"%s is an installation-wide permission and cannot be checked against an app.", verb)
	}
	if p.Kind == KindSystem {
		return nil
	}
	_, denial, err := a.control(ctx, p, appID, verb)
	if err != nil {
		return err
	}
	return denial
}

// AllowsInstall reports whether CheckInstall would allow the verb, without
// auditing a denial. For a decision with two ways to be allowed — deploy
// approval is install.deploys.approve or app.deploy.approve (R-155) — where
// failing the first is not a denial at all if the second succeeds, and an
// audit log full of denials nobody suffered would bury the ones that matter.
func (a *Authorizer) AllowsInstall(ctx context.Context, p Principal, verb Verb) (bool, error) {
	if !InstallScoped(verb) {
		return false, errs.Newf(errs.Internal,
			"%s is a per-app permission and cannot be checked installation-wide.", verb)
	}
	if p.Kind == KindSystem {
		return true, nil
	}
	if !a.passesInstallFloor(ctx, p, verb) {
		return false, nil
	}
	grants, err := a.store.InstallGrantsFor(ctx, p)
	if err != nil {
		return false, err
	}
	for _, g := range grants {
		role, err := a.roleOf(ctx, nil, g)
		if err != nil {
			return false, err
		}
		if role.Has(verb) {
			return true, nil
		}
	}
	return false, nil
}

// passesInstallFloor reports whether the principal and host policy steps
// allow an install verb. Each of them can only deny, so a "no" from either is
// the answer, not a failure to give one.
func (a *Authorizer) passesInstallFloor(ctx context.Context, p Principal, verb Verb) bool {
	if a.checkPrincipal(ctx, p) != nil {
		return false
	}
	return a.policy == nil || a.policy.Allows(ctx, p, verb, "") == nil
}

// reach is how an app verb was allowed through an install grant: the install
// verb that stands for it on every app, and the grant that carried it.
type reach struct {
	verb  Verb
	grant Grant
}

// control evaluates steps 1–7 for one app verb. It returns the reason for a
// denial, or a failure to evaluate at all, and audits neither. When an
// install grant allowed the verb rather than one on the app, via says which.
func (a *Authorizer) control(ctx context.Context, p Principal, appID string, verb Verb) (via *reach, denial, failure error) {
	return a.controlWith(ctx, nil, p, appID, verb)
}

// controlWith is control reading through m, which AppVerbs shares across
// every verb it asks about. The rules are the same whether m is nil or not;
// only how often the store is asked differs.
func (a *Authorizer) controlWith(ctx context.Context, m *lookups, p Principal, appID string, verb Verb) (via *reach, denial, failure error) {
	// Steps 1–4: the principal itself.
	if err := a.checkPrincipalWith(ctx, m, p); err != nil {
		return nil, err, nil
	}

	// Step 5: host policy, before grants. A policy that disables a verb
	// install-wide denies the owner too (R-272) — and anyone holding it
	// install-wide, because policy is asked about the app verb, never about
	// the install verb standing for it.
	if err := a.policyAllows(ctx, m, p, verb, appID); err != nil {
		return nil, err, nil
	}

	// Steps 6–7: grants, then the verb.
	grants, err := a.controlGrants(ctx, m, p, appID)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range grants {
		role, err := a.roleOf(ctx, m, g)
		if err != nil {
			return nil, nil, err
		}
		if role.Has(verb) {
			return nil, nil, nil
		}
	}

	// Step 6b: an install grant that reaches every app (R-081). The only
	// install verbs that bear on an app are each app verb's counterpart in
	// everyApp — nothing else in an install role is read here, and only the
	// one counterpart of this verb is looked for.
	install, err := a.installGrants(ctx, m, p)
	if err != nil {
		return nil, nil, err
	}
	for _, g := range install {
		role, err := a.roleOf(ctx, m, g)
		if err != nil {
			return nil, nil, err
		}
		for _, held := range role.Verbs {
			if v, ok := everyApp[held]; ok && v == verb {
				return &reach{verb: held, grant: g}, nil, nil
			}
		}
	}

	return nil, errs.Newf(errs.PermVerbRequired, "You do not have permission to do this. It requires %s on this app.", verb), nil
}

// CheckInstall authorizes an installation-wide action (O-17, R-265).
//
// A separate function from CheckControl, taking different arguments, for the
// same reason CheckData is separate: the scopes are different questions and a
// single function with an optional appID is one missed argument away from
// authorizing an app verb install-wide. That mistake has already been made once
// in this package's history, in the other direction (design 06 §2).
//
// Host policy still runs first (R-272) and still denies the holder: an install
// that has disabled a verb has disabled it for administrators too.
func (a *Authorizer) CheckInstall(ctx context.Context, p Principal, verb Verb) error {
	// The mirror of the guard in CheckControl, and the more important half. An
	// app verb evaluated install-wide would look for a grant that *can* exist
	// and could allow.
	if !InstallScoped(verb) {
		return errs.Newf(errs.Internal,
			"%s is a per-app permission and cannot be checked installation-wide.", verb)
	}

	if p.Kind == KindSystem {
		// The reconciler and background jobs, as in CheckControl. Audit is not
		// bypassed.
		return nil
	}

	if err := a.checkPrincipal(ctx, p); err != nil {
		return a.deny(ctx, p, "", verb, err)
	}

	if a.policy != nil {
		if err := a.policy.Allows(ctx, p, verb, ""); err != nil {
			return a.deny(ctx, p, "", verb, err)
		}
	}

	grants, err := a.store.InstallGrantsFor(ctx, p)
	if err != nil {
		return err
	}
	for _, g := range grants {
		role, err := a.roleOf(ctx, nil, g)
		if err != nil {
			return err
		}
		if role.Has(verb) {
			return nil
		}
	}

	return a.deny(ctx, p, "", verb, errs.Newf(errs.PermVerbRequired,
		"You do not have permission to do this. It requires %s for this installation.", verb))
}

// CheckData authorizes data-plane access: using an app through the proxy.
//
// This function contains exactly ONE cross-plane implication — ownership
// (R-072). No other control-plane role appears in it, and being a Pando admin
// does not appear in it at all (R-087: an admin has root and can reach a
// container outside Pando, but the supported path requires a grant).
//
// This was reversed once during design. If a change adds a control-plane check
// here, reject it — that is R-029, and the test asserting an operator on someone
// else's app is denied *use* exists to catch exactly that change.
func (a *Authorizer) CheckData(ctx context.Context, p Principal, appID string) error {
	if err := a.checkPrincipal(ctx, p); err != nil {
		return a.deny(ctx, p, appID, "app.use", err)
	}

	if p.UserID != "" {
		owner, err := a.store.IsOwner(ctx, appID, p.UserID)
		if err != nil {
			return err
		}
		if owner {
			return nil // R-072: the sole implication between planes.
		}
	}

	granted, err := a.store.HasDataGrant(ctx, appID, p)
	if err != nil {
		return err
	}
	if granted {
		return nil
	}

	anon, passcode, err := a.store.AnonymousAccess(ctx, appID)
	if err != nil {
		return err
	}
	if anon && !passcode {
		return nil // R-075.
	}
	if anon && passcode {
		// Shared with everyone who knows the passcode (R-075a). Still the
		// data plane alone — a passcode is a key to using the app, and says
		// nothing about managing it.
		if token := p.Passcodes[appID]; token != "" {
			unlocked, err := a.store.PasscodeUnlocked(ctx, appID, token)
			if err != nil {
				return err
			}
			if unlocked {
				return nil
			}
		}
		// Not audited as a denial: every first visit to a passcode app lands
		// here, and the proxy answers it with the passcode page.
		return errs.New(errs.PermPasscodeRequired, "This app asks for a passcode.")
	}

	denied := errs.New(errs.PermDenied, "You do not have access to this app.")
	if p.Kind == KindAnonymous && !a.auditsAnonymousDenials(ctx) {
		// Anyone can cause this one, once per request, without an account:
		// every crawler and stray link to a private app does. Host policy may
		// stop recording it (disable_anonymous_denial_audit). A denial to
		// anyone signed in is always recorded.
		return denied
	}
	return a.deny(ctx, p, appID, "app.use", denied)
}

// auditsAnonymousDenials reports whether host policy wants an anonymous
// data-plane denial audited. Yes unless the policy says otherwise: when it
// cannot say, keep the record.
func (a *Authorizer) auditsAnonymousDenials(ctx context.Context) bool {
	if d, ok := a.policy.(DenialAuditPolicy); ok {
		return d.AuditsAnonymousDenials(ctx)
	}
	return true
}

// checkPrincipal runs steps 1–4: status, token validity, and token derivation.
func (a *Authorizer) checkPrincipal(ctx context.Context, p Principal) error {
	switch p.Kind {
	case KindAnonymous:
		// An anonymous principal is always "valid"; whether it may proceed is
		// entirely a question of grants.
		return nil

	case KindSystem:
		return nil

	case KindUser:
		return statusError(p.Status)

	case KindToken:
		// An account token is its own principal (R-060) and has no owner to
		// derive from. Its own validity was established at authentication.
		if p.UserID == "" {
			return nil
		}

		// Step 4 — token derivation. A delegated token is only as alive as its
		// owner (R-059). This is a live lookup on every request rather than a
		// cascade run at revocation time: slower, and correct, because a missed
		// cascade is a permanent security hole and a live lookup cannot be
		// missed.
		status, err := a.store.UserStatus(ctx, p.UserID)
		if err != nil {
			return err
		}
		if err := statusError(status); err != nil {
			return errs.New(errs.AuthTokenOrphaned,
				"This token no longer works because the account that created it is no longer active.")
		}
		return nil

	default:
		return errs.New(errs.AuthInvalid, "Unrecognized credential.")
	}
}

func statusError(status string) error {
	switch status {
	case "active":
		return nil
	case "suspended":
		// Suspended is not deleted (R-049). The distinction matters because
		// destruction rules fire on deletion and must not fire here.
		return errs.New(errs.AuthInvalid, "This account is suspended.")
	case "deleted":
		return errs.New(errs.AuthInvalid, "This account no longer exists.")
	default:
		return errs.New(errs.AuthInvalid, "This account is not active.")
	}
}

// deny records the denial and returns the error.
//
// Every denial is audited, not only successes. A denial pattern is the signal
// that matters for detecting misuse, and it is the thing most commonly left out.
func (a *Authorizer) deny(ctx context.Context, p Principal, appID string, verb Verb, err error) error {
	if a.auditor != nil {
		a.auditor.Denied(ctx, p, appID, verb, errs.CodeOf(err))
	}
	return err
}
