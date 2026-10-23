package authz

import (
	"context"
	"strings"

	"github.com/trypando/pando/internal/errs"
)

// Install verbs are a boundary between the people who hold them (R-080a).
//
// A custom role (R-082) may hold one install verb and not another, and that is
// only worth anything if no verb can be used to obtain the rest. Three ways one
// could, each closed here:
//
//   - Handing it on. Granting a role, putting someone in a group that holds
//     one, linking a provider's group to such a group, composing a custom role:
//     each confers the role's verbs, so the caller must hold every one of them.
//     Taking one away is the same act in reverse and needs the same.
//   - Becoming someone who holds it. Resetting a password, linking an identity,
//     changing an email or suspending an account is a way into that account, or
//     a way to lock its holder out, so the caller must hold every install verb
//     the account does.
//   - Reaching all of them at once. An identity provider vouches for
//     identities, and Pando believes it, so whoever configures one can sign in
//     as any account it is linked to; a restore replaces every grant with the
//     backup's. Either needs every install verb there is.
//
// App verbs are not part of this: an account manager may reset the password of
// an app's owner, and so reach what that owner holds on their apps. That is
// what managing accounts is, and R-080a says so plainly rather than implying a
// boundary that is not there.

// InstallVerbsHeld returns the install-scoped verbs p's grants carry, directly
// and through groups. It reads grants only: host policy and account status
// decide whether a verb may be used now, not whether it is held, and holding is
// what passing it on is about.
func (a *Authorizer) InstallVerbsHeld(ctx context.Context, p Principal) (map[Verb]bool, error) {
	held := map[Verb]bool{}
	switch p.Kind {
	case KindSystem:
		addInstallVerbs(held, Verbs)
		return held, nil
	case KindAnonymous:
		return held, nil
	}
	grants, err := a.store.InstallGrantsFor(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		role, err := a.roleOf(ctx, nil, g)
		if err != nil {
			return nil, err
		}
		addInstallVerbs(held, role.Verbs)
	}
	return held, nil
}

// addInstallVerbs adds the install-scoped verbs of vs to held.
func addInstallVerbs(held map[Verb]bool, vs []Verb) {
	for _, v := range vs {
		if InstallScoped(v) {
			held[v] = true
		}
	}
}

// lacking returns which install verbs of want p does not hold, in catalog
// order. An app verb in want is a caller mistake: install roles cannot carry
// one (R-080), so it is refused rather than quietly ignored.
func (a *Authorizer) lacking(ctx context.Context, p Principal, want []Verb) ([]Verb, error) {
	wanted := map[Verb]bool{}
	for _, v := range want {
		if !InstallScoped(v) {
			return nil, errs.Newf(errs.Internal,
				"%s is a per-app permission and cannot be checked installation-wide.", v)
		}
		wanted[v] = true
	}
	held, err := a.InstallVerbsHeld(ctx, p)
	if err != nil {
		return nil, err
	}
	var missing []Verb
	for _, v := range Verbs {
		if wanted[v] && !held[v] {
			missing = append(missing, v)
		}
	}
	return missing, nil
}

// CheckConfers refuses p handing on, or taking away, install verbs p does not
// hold itself (R-080a). act completes "Pando refused to …", for example "give
// the group Ops the role Administrator".
func (a *Authorizer) CheckConfers(ctx context.Context, p Principal, verbs []Verb, act string) error {
	missing, err := a.lacking(ctx, p, verbs)
	if err != nil || len(missing) == 0 {
		return err
	}
	return a.deny(ctx, p, "", missing[0], errs.Newf(errs.PermVerbRequired,
		"Pando refused to %s, because that would hand on permissions you do not hold yourself: %s. "+
			"An installation permission can be given or taken away only by someone who holds it.",
		act, joinVerbs(missing)).
		WithRemedy("Ask an administrator, or anyone holding all of those permissions, to make this change.").
		WithDetail("missing_verbs", missing))
}

// CheckConfersRole is CheckConfers for the install verbs of one role. An
// app-scoped role carries none, so there is nothing to check for it here:
// granting one on an app is app.grants.manage's question. act may use %s for
// the role's name, for example "grant the role %s".
func (a *Authorizer) CheckConfersRole(ctx context.Context, p Principal, roleID, act string) error {
	if roleID == "" {
		return nil
	}
	role, err := a.store.Role(ctx, roleID)
	if err != nil {
		return err
	}
	var install []Verb
	for _, v := range role.Verbs {
		if InstallScoped(v) {
			install = append(install, v)
		}
	}
	name := role.Name
	if name == "" {
		name = roleID
	}
	return a.CheckConfers(ctx, p, install, strings.Replace(act, "%s", name, 1))
}

// AppVerbsHeld returns the app verbs p's grants carry on one app: its grants
// on the app, directly and through groups, and the install verbs that stand
// for an app verb on every app (R-080, R-081). Like InstallVerbsHeld it reads
// grants only; host policy and account status decide use, not holding.
func (a *Authorizer) AppVerbsHeld(ctx context.Context, p Principal, appID string) (map[Verb]bool, error) {
	held := map[Verb]bool{}
	switch p.Kind {
	case KindSystem:
		for _, v := range AppVerbs() {
			held[v] = true
		}
		return held, nil
	case KindAnonymous:
		return held, nil
	}
	grants, err := a.store.ControlGrantsFor(ctx, appID, p)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		role, err := a.roleOf(ctx, nil, g)
		if err != nil {
			return nil, err
		}
		for _, v := range role.Verbs {
			if !InstallScoped(v) {
				held[v] = true
			}
		}
	}
	install, err := a.store.InstallGrantsFor(ctx, p)
	if err != nil {
		return nil, err
	}
	for _, g := range install {
		role, err := a.roleOf(ctx, nil, g)
		if err != nil {
			return nil, err
		}
		for _, v := range role.Verbs {
			if app, ok := everyApp[v]; ok {
				held[app] = true
			}
		}
	}
	return held, nil
}

// CheckConfersOnApp refuses p granting a role on an app, or giving a grant on
// it a new role, when the role carries an app verb p does not hold on that app
// itself (R-080a, R-155). Without it app.grants.manage would be every app
// verb: an Owner could give themselves app.deploy.approve through a custom
// role and approve their own deploys, which is the two-person rule an
// installation keeps by withholding that verb. act may use %s for the role's
// name, for example "grant the role %s".
func (a *Authorizer) CheckConfersOnApp(ctx context.Context, p Principal, appID, roleID, act string) error {
	if roleID == "" {
		return nil
	}
	role, err := a.store.Role(ctx, roleID)
	if err != nil {
		return err
	}
	held, err := a.AppVerbsHeld(ctx, p, appID)
	if err != nil {
		return err
	}
	var missing []Verb
	for _, v := range Verbs {
		if !InstallScoped(v) && role.Has(v) && !held[v] {
			missing = append(missing, v)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	name := role.Name
	if name == "" {
		name = roleID
	}
	return a.deny(ctx, p, appID, missing[0], errs.Newf(errs.PermVerbRequired,
		"Pando refused to %s, because that would hand on permissions you do not hold on this app yourself: %s. "+
			"A permission on an app can be given only by someone who holds it there.",
		strings.Replace(act, "%s", name, 1), joinVerbs(missing)).
		WithRemedy("Choose a role with only permissions you hold on this app, or ask someone who holds the rest to make this change.").
		WithDetail("missing_verbs", missing))
}

// CheckMayChangeAccount refuses p changing an account that holds install
// verbs p does not (R-080a): a password reset, a linked identity or a changed
// email is a way into that account, and a suspension or deletion is a way to
// take its permissions from it. act completes "Pando refused to …". Your own
// account is always yours to change.
func (a *Authorizer) CheckMayChangeAccount(ctx context.Context, p Principal, userID, act string) error {
	if userID == "" || userID == p.UserID {
		return nil
	}
	held, err := a.InstallVerbsHeld(ctx, Principal{Kind: KindUser, ID: userID, UserID: userID})
	if err != nil {
		return err
	}
	theirs := make([]Verb, 0, len(held))
	for _, v := range Verbs {
		if held[v] {
			theirs = append(theirs, v)
		}
	}
	missing, err := a.lacking(ctx, p, theirs)
	if err != nil || len(missing) == 0 {
		return err
	}
	return a.deny(ctx, p, "", missing[0], errs.Newf(errs.PermVerbRequired,
		"Pando refused to %s, because that account holds installation permissions you do not: %s. "+
			"Changing it would let you act with them, or take them from someone who holds them.",
		act, joinVerbs(missing)).
		WithRemedy("Ask an administrator, or anyone holding all of those permissions, to make this change.").
		WithDetail("missing_verbs", missing))
}

// CheckHoldsEveryInstallVerb refuses p unless p holds every install verb
// there is (R-080a), for an act that reaches all of them: configuring an
// identity provider, which decides who signs in as which account, or
// restoring the installation, which replaces every grant. act completes
// "Pando refused to …"; because completes "…, because …".
func (a *Authorizer) CheckHoldsEveryInstallVerb(ctx context.Context, p Principal, act, because string) error {
	every := make([]Verb, 0, len(Verbs))
	for _, v := range Verbs {
		if InstallScoped(v) {
			every = append(every, v)
		}
	}
	missing, err := a.lacking(ctx, p, every)
	if err != nil || len(missing) == 0 {
		return err
	}
	return a.deny(ctx, p, "", missing[0], errs.Newf(errs.PermVerbRequired,
		"Pando refused to %s, because %s. That needs every installation permission, and you do not hold: %s.",
		act, because, joinVerbs(missing)).
		WithRemedy("Ask an administrator to make this change.").
		WithDetail("missing_verbs", missing))
}

func joinVerbs(vs []Verb) string {
	out := make([]string, len(vs))
	for i, v := range vs {
		out[i] = string(v)
	}
	return strings.Join(out, ", ")
}
