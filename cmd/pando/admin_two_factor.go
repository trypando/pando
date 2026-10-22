package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/log"
)

// resetTwoFactorCmd removes a local account's second factor from the host
// (R-405): for the person who lost their authenticator and their recovery
// codes when nobody else can sign in to reset it for them — the only
// administrator, say. Like reset-password, host shell access is the
// authorization.
func resetTwoFactorCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "reset-two-factor [username]",
		Short: "Remove a local account's two-factor authentication when nobody can sign in",
		Long: "Removes a local account's authenticator and recovery codes, ends every session it has,\n" +
			"and lifts any limit its wrong codes put on signing in. It then signs in with its password\n" +
			"alone, and sets two-factor authentication up again there if host policy requires it.\n\n" +
			"Accounts that sign in through an external identity provider get their second factor\n" +
			"from the provider, not here.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,

		RunE: func(cmd *cobra.Command, args []string) error {
			username := bootstrap.AdminUsername
			if len(args) == 1 {
				username = args[0]
			}
			cfg, logger, err := setup(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			ctx := log.Into(cmd.Context(), logger)
			if err := resetTwoFactor(ctx, cfg, username, logger); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Two-factor authentication removed for %s.\n", username)
			fmt.Fprintln(cmd.OutOrStdout(),
				"Every session it had has ended. It signs in with its password next, and sets two-factor\n"+
					"authentication up again if this installation requires it.")
			return nil
		},
	}
}

// resetTwoFactor does the work, against the database rather than the API.
func resetTwoFactor(ctx context.Context, cfg *config.Config, username string, logger *zap.Logger) error {
	db, err := state.Connect(ctx, state.ConnectOptions{
		OwnerURL:       cfg.Database.URL,
		ConnectTimeout: cfg.Database.ConnectTimeout,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	user, found, err := state.NewUsers(db).ByExternalID(ctx, state.LocalAdapterID, username)
	if err != nil {
		return err
	}
	if !found {
		return errs.Newf(errs.NotFound,
			"This installation has no local account called %q.", username).
			WithRemedy("Accounts from an external identity provider get their second factor from the provider.")
	}

	// Removing needs no secrets adapter: nothing sealed is opened, only
	// thrown away.
	had, err := state.NewTwoFactor(db, nil, "").Disable(ctx, user.ID, true)
	if err != nil {
		return err
	}
	if err := state.NewSessionRevoker(db).RevokeAllForUser(ctx, user.ID); err != nil {
		return err
	}
	if err := state.NewFailedAttempts(db).Clear(ctx, state.SignInCodeKey(user.ID)); err != nil {
		return err
	}

	// Audited, and its failure is fatal to the command, as with
	// reset-password: it runs outside every check the API makes (R-227).
	auditor := audit.New(db.Pool)
	if err := auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "user.two_factor.reset",
		TargetKind:    "user",
		TargetID:      user.ID,
		Detail: map[string]any{
			"username":       username,
			"had_two_factor": had,
			"via":            "pando admin reset-two-factor",
			"reason":         fromTheHostShell,
		},
	}); err != nil {
		return errs.Wrap(errs.Internal,
			"Two-factor authentication was removed and Pando could not record it in the audit log.", err)
	}

	logger.Info("two-factor authentication reset from the host shell",
		zap.String("username", username), zap.String("user_id", user.ID))
	return nil
}
