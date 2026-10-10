// Package bootstrap creates the first administrative account on a fresh install.
//
// R-046: a fresh installation has no account until somebody claims it. The
// person who sets the administrator's username and password up in the console
// (POST /setup, Claim) must present the one-time setup token Pando printed to
// its log at startup (issue #130); no password is ever printed. An operator who
// wants the account made unattended supplies PANDO_ADMIN_PASSWORD, and Run
// makes it at startup instead.
package bootstrap

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/authz"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

// AdminUsername is the account created on first run.
const AdminUsername = "admin"

// Result reports what first run produced.
type Result struct {
	// Created says Run made the first administrator, from PANDO_ADMIN_PASSWORD.
	Created bool
	User    state.User

	// Unclaimed says the installation has no account and is waiting for
	// somebody to set one up in the console, with the setup token.
	Unclaimed bool

	// SetupToken is the token Run just made, for the caller to print. Zero
	// when the installation already had one, which cannot be read back:
	// SetupTokenMade then says when it was made, so the log can point at it.
	SetupToken     secret.Value
	SetupTokenMade time.Time
}

// Run seeds the local identity adapter and, if no account exists and the
// operator supplied PANDO_ADMIN_PASSWORD, the first administrator.
//
// Idempotent: on every start after the first it finds a user and does nothing.
// The check is "any user at all" rather than "the admin user" so that deleting
// the seeded admin after creating a real one does not make it reappear on the
// next restart.
//
// Without a supplied password it creates no account and reports Unclaimed: the
// administrator is set up in the console, by whoever has the setup token (R-046).
// Run makes that token the first time and returns it to be printed. It is not a
// password: it works once, only while there is no account, and only on the
// setup form, so the log line holding it stops mattering the moment it is used.
//
// A supplied password still must be changed at first sign-in, because an
// environment variable is not a safe place for one — it is in the Compose
// file, in `docker inspect`, and inherited by every child process.
//
// Under state.FirstAccountLock. Replicas started together each run this, and
// without the lock both found no account and both made one; the second failed
// on the username and that replica did not start (issue #72).
func Run(ctx context.Context, users *state.Users, grants *state.Grants, db *state.DB, auditor *audit.Writer, supplied secret.Value) (Result, error) {
	var res Result
	err := db.Exclusive(ctx, state.FirstAccountLock, func() error {
		var err error
		res, err = run(ctx, users, grants, db, auditor, supplied)
		return err
	})
	return res, err
}

func run(ctx context.Context, users *state.Users, grants *state.Grants, db *state.DB, auditor *audit.Writer, supplied secret.Value) (Result, error) {
	if err := users.EnsureLocalAdapter(ctx); err != nil {
		return Result{}, err
	}

	var count int
	if err := db.QueryRow(ctx, `SELECT count(*) FROM users WHERE deleted_at IS NULL`).Scan(&count); err != nil {
		return Result{}, errs.Wrap(errs.Internal, "Could not check for existing accounts.", err)
	}
	if count > 0 {
		// Set up already, so no token: one left behind would be a second way
		// in that nothing needs.
		return Result{}, users.ClearSetupToken(ctx)
	}
	if supplied.IsZero() {
		token, made, err := users.EnsureSetupToken(ctx)
		if err != nil {
			return Result{}, err
		}
		return Result{Unclaimed: true, SetupToken: secret.New(token), SetupTokenMade: made}, nil
	}

	if supplied.Len() < hash.MinPasswordLength {
		return Result{}, errs.Newf(errs.ValidInvalid,
			"The administrator password supplied for this installation is shorter than %d characters.",
			hash.MinPasswordLength).
			WithRemedy("Set PANDO_ADMIN_PASSWORD to something longer, or unset it and set up the administrator in the console.")
	}
	password := supplied
	digest, err := hash.New(password)
	if err != nil {
		return Result{}, errs.Wrap(errs.Internal, "Could not secure the initial password.", err)
	}

	user, err := users.Create(ctx, state.LocalAdapterID, AdminUsername, "", "Administrator", digest, true)
	if err != nil {
		return Result{}, err
	}

	// The account is administrative because of a grant, not because of a column
	// on the user (O-17). There is no `is_admin` anywhere: administration is a
	// role with verbs, revocable like any other, and this row is the only thing
	// that separates the first account from every later one. Without it a fresh
	// install has a user who can sign in and do nothing — which is how the
	// endpoints were gated before this existed, and why any logged-in account
	// could suspend this one.
	grant, err := grants.GrantInstall(ctx, "user", user.ID, authz.RoleAdministrator, "system")
	if err != nil {
		return Result{}, err
	}

	if err := auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "grant.create",
		TargetKind:    "grant",
		TargetID:      grant.ID,
		Detail: map[string]any{
			"reason": "first run",
			"scope":  "install",
			"role":   authz.RoleAdministrator,
			"user":   user.ID,
		},
	}); err != nil {
		return Result{}, err
	}

	if err := auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "user.create",
		TargetKind:    "user",
		TargetID:      user.ID,
		Detail:        map[string]any{"reason": "first run", "username": AdminUsername},
	}); err != nil {
		return Result{}, err
	}

	// A token from a start before PANDO_ADMIN_PASSWORD was set would otherwise
	// outlive the setup it was for.
	if err := users.ClearSetupToken(ctx); err != nil {
		return Result{}, err
	}

	log.From(ctx).Info("created the first administrator", zap.String("user_id", user.ID))
	return Result{Created: true, User: user}, nil
}

// Claim sets up an unclaimed installation: the first account, as its
// administrator, with the username and password the person chose (R-046).
// Refused with state.ErrAlreadySetUp once any account exists.
//
// They chose the password themselves, so it need not be changed at the next
// sign-in — unlike one supplied through the environment or handed over.
//
// They must also present the setup token (issue #130), which the claim uses up.
func Claim(ctx context.Context, users *state.Users, auditor *audit.Writer, setupToken secret.Value, username, displayName string, password secret.Value) (state.User, error) {
	if username == "" {
		return state.User{}, errs.New(errs.ValidInvalid, "The administrator needs a username.")
	}
	if password.Len() < hash.MinPasswordLength {
		return state.User{}, errs.Newf(errs.ValidInvalid, "A password needs at least %d characters.", hash.MinPasswordLength)
	}
	digest, err := hash.New(password)
	if err != nil {
		return state.User{}, errs.Wrap(errs.Internal, "Could not secure the password.", err)
	}
	// Run seeds this at startup; ensured here too so a claim never depends
	// on the order things happened in.
	if err := users.EnsureLocalAdapter(ctx); err != nil {
		return state.User{}, err
	}
	user, grantID, err := users.ClaimFirst(ctx, setupToken, username, displayName, digest, authz.RoleAdministrator)
	if err != nil {
		return state.User{}, err
	}

	// Attributed to the account that did it: nobody was signed in, and
	// "system" would say Pando chose this administrator, which it did not.
	for _, e := range []audit.Event{
		{PrincipalKind: audit.KindUser, PrincipalID: user.ID, Action: "user.create",
			TargetKind: "user", TargetID: user.ID,
			Detail: map[string]any{"reason": "first run setup", "username": username}},
		{PrincipalKind: audit.KindUser, PrincipalID: user.ID, Action: "grant.create",
			TargetKind: "grant", TargetID: grantID,
			Detail: map[string]any{"reason": "first run setup", "scope": "install", "role": authz.RoleAdministrator, "user": user.ID}},
	} {
		if auditor == nil {
			break
		}
		if err := auditor.Write(ctx, e); err != nil {
			return state.User{}, err
		}
	}
	log.From(ctx).Info("the installation was set up", zap.String("user_id", user.ID))
	return user, nil
}
