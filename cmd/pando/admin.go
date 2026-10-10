package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"
	"golang.org/x/term"

	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/audit"
	"github.com/trypando/pando/internal/core/bootstrap"
	corepolicy "github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/core/state"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/hash"
	"github.com/trypando/pando/internal/log"
	"github.com/trypando/pando/internal/secret"
)

// Server-side administration: the commands that work when nobody can sign in.
//
// These live here and not in internal/cli, and the difference is load-bearing.
// internal/cli is a client of the API and nothing more (R-261) — it imports no
// core package, so a command that needed something the API cannot do would fail
// to compile rather than quietly grow a shortcut. A password reset is exactly
// such a command: it exists for the case where there is no account to
// authenticate as, so there is no request it could make.
//
// Host shell access is the authorization, and it is the right boundary rather
// than an absence of one. Whoever can run this can already read the config file
// that holds the database credentials, and could change the row by hand. The
// command exists so that doing it correctly — a real argon2id digest, sessions
// ended, an audit event written — is easier than doing it by hand.

func adminCmd(configPath *string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Administer this installation from the host",
		Long: "Commands that work against the database directly, for when nobody can sign in.\n\n" +
			"Running these needs shell access on the host and the configuration that names the\n" +
			"database. That is the whole of the authorization, and it is the same access that\n" +
			"could change the row by hand.",
	}
	cmd.AddCommand(resetPasswordCmd(configPath))
	cmd.AddCommand(enablePasswordSignInCmd(configPath))
	cmd.AddCommand(setupTokenCmd(configPath))
	return cmd
}

// setupTokenCmd makes a new setup token for an installation nobody has set up
// yet (R-046, issue #130), for the operator who no longer has the log line the
// first one was printed on: the container was recreated, or the log rotated.
//
// The token alone goes to stdout, so a script can take it with $(…); what to do
// with it goes to stderr.
func setupTokenCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "setup-token",
		Short: "Make a new setup token for an installation that is not set up yet",
		Long: "Prints a new one-time setup token and retires the one Pando printed to its log at startup.\n\n" +
			"A new installation is set up in the console, and the setup form asks for this token, so that\n" +
			"reaching the console first is not enough to become its administrator. Refused once the\n" +
			"installation has an account.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := setup(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()
			ctx := log.Into(cmd.Context(), logger)

			db, err := state.Connect(ctx, state.ConnectOptions{
				OwnerURL:       cfg.Database.URL,
				ConnectTimeout: cfg.Database.ConnectTimeout,
			})
			if err != nil {
				return err
			}
			defer db.Close()

			token, err := state.NewUsers(db).ReplaceSetupToken(ctx)
			if err != nil {
				return err
			}
			// Audited, and fatal if it is not: this hands a way to become the
			// administrator to whoever holds the host shell.
			if err := audit.New(db.Pool).Write(ctx, audit.Event{
				PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "setup.token.replace",
				TargetKind: "installation", TargetID: "setup",
				Detail: map[string]any{"via": "pando admin setup-token",
					"reason": "run from the host shell, outside any session"},
			}); err != nil {
				return errs.Wrap(errs.Internal,
					"A setup token was made and Pando could not record it in the audit log. Run the command again.", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), token)
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Open the console and set up the administrator with this setup token. It works once, and any earlier one no longer works.")
			return nil
		},
	}
}

func resetPasswordCmd(configPath *string) *cobra.Command {
	var supplied string

	cmd := &cobra.Command{
		Use:   "reset-password [username]",
		Short: "Set a local account's password when nobody can sign in",
		Long: "Sets a new password for a local account and ends every session it has.\n\n" +
			"The account must change the password again at its next sign-in: this hands a way\n" +
			"back in to somebody, which is not the same as choosing their credential.\n\n" +
			"Accounts that sign in through an external identity provider are changed where they\n" +
			"live, not here.",
		Args:         cobra.MaximumNArgs(1),
		SilenceUsage: true,

		RunE: func(cmd *cobra.Command, args []string) error {
			username := bootstrap.AdminUsername
			if len(args) == 1 {
				username = args[0]
			}

			password, generated, err := passwordFor(cmd, supplied)
			if err != nil {
				return err
			}
			if password.Len() < hash.MinPasswordLength {
				return errs.Newf(errs.ValidInvalid,
					"A password needs at least %d characters.", hash.MinPasswordLength)
			}

			cfg, logger, err := setup(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()

			ctx := log.Into(cmd.Context(), logger)
			if err := resetPassword(ctx, cfg, username, password, logger); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Password reset for %s.\n", username)
			if generated {
				// To stdout, not the log. The log is the thing that loses it,
				// which is how this command came to exist.
				fmt.Fprintf(cmd.OutOrStdout(), "New password: %s\n", password.Reveal())
			}
			fmt.Fprintln(cmd.OutOrStdout(),
				"Every session it had has ended, and it must set a new password at the next sign-in.")
			return nil
		},
	}

	cmd.Flags().StringVar(&supplied, "password", "",
		"the new password; prompts on a terminal, and generates one otherwise")
	return cmd
}

// passwordFor resolves the new password: the flag, a prompt, or a generated one.
//
// Prompting is preferred over the flag because a password in argv is in the
// host's process list and in the operator's shell history. The flag stays for
// the unattended case, where there is no terminal to prompt on.
func passwordFor(cmd *cobra.Command, supplied string) (secret.Value, bool, error) {
	if supplied != "" {
		return secret.New(supplied), false, nil
	}

	if f, ok := cmd.InOrStdin().(interface{ Fd() uintptr }); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), "New password (leave empty to generate one): ")
		typed, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		if err != nil {
			return secret.Value{}, false, fmt.Errorf("reading the password: %w", err)
		}
		if len(typed) > 0 {
			return secret.New(string(typed)), false, nil
		}
	}

	password, err := hash.Generate()
	return password, true, err
}

// resetPassword does the work, against the database rather than the API.
func resetPassword(ctx context.Context, cfg *config.Config, username string, password secret.Value, logger *zap.Logger) error {
	db, err := state.Connect(ctx, state.ConnectOptions{
		OwnerURL:       cfg.Database.URL,
		ConnectTimeout: cfg.Database.ConnectTimeout,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	digest, err := hash.New(password)
	if err != nil {
		return errs.Wrap(errs.Internal, "Could not secure the new password.", err)
	}

	users := state.NewUsers(db)
	userID, err := users.ResetPassword(ctx, username, digest)
	if err != nil {
		return err
	}

	// Ending the sessions is not tidying up. A reset that leaves a live session
	// alone has not reset anything: whoever is holding that cookie keeps the
	// access the reset was meant to take back (R-048).
	sessions := state.NewSessions(db)
	if err := sessions.RevokeAllForUser(ctx, userID); err != nil {
		return err
	}

	// Audited, and its failure is fatal to the command.
	//
	// A credential reset that leaves no trace is a backdoor, and this one runs
	// outside every check the API makes — no verb, no session, no principal but
	// whoever holds the host. That is precisely the event the log exists for
	// (R-227), and reporting success without it would be reporting the wrong
	// thing.
	auditor := audit.New(db.Pool)
	if err := auditor.Write(ctx, audit.Event{
		PrincipalKind: audit.KindSystem,
		PrincipalID:   "system",
		Action:        "user.password.reset",
		TargetKind:    "user",
		TargetID:      userID,
		Detail: map[string]any{
			"username": username,
			"via":      "pando admin reset-password",
			"reason":   "run from the host shell, outside any session",
		},
	}); err != nil {
		return errs.Wrap(errs.Internal,
			"The password was reset and Pando could not record it in the audit log.", err)
	}

	logger.Info("password reset from the host shell",
		zap.String("username", username), zap.String("user_id", userID))
	return nil
}

// enablePasswordSignInCmd is the break-glass path when password sign-in is
// off and every identity provider is out of reach (issue #51): the provider is
// down, its certificate rolled over unannounced, or its settings were saved
// wrong. With password sign-in back on, `reset-password` gives someone a way
// in, and they fix the provider from the console.
func enablePasswordSignInCmd(configPath *string) *cobra.Command {
	return &cobra.Command{
		Use:   "enable-password-sign-in",
		Short: "Turn password sign-in back on when no identity provider works",
		Long: "Clears disable_password_sign_in in host policy, so local accounts can sign in with a password again.\n\n" +
			"For when password sign-in is off and nobody can sign in through an identity provider. Follow it with\n" +
			"`pando admin reset-password` if you also need a password for an account.",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, logger, err := setup(*configPath)
			if err != nil {
				return err
			}
			defer func() { _ = logger.Sync() }()
			ctx := log.Into(cmd.Context(), logger)

			overlay, err := startupPolicy(cfg)
			if err != nil {
				return err
			}
			if doc := overlay.Apply(corepolicy.Default()); doc.DisablePasswordSignIn {
				return errs.New(errs.StateSetAtStartup,
					"disable_password_sign_in is set in Pando's startup configuration, so it cannot be changed here.").
					WithRemedy("Remove it from the config file or the PANDO_POLICY_DISABLE_PASSWORD_SIGN_IN variable, and restart Pando.")
			}

			db, err := state.Connect(ctx, state.ConnectOptions{
				OwnerURL:       cfg.Database.URL,
				ConnectTimeout: cfg.Database.ConnectTimeout,
			})
			if err != nil {
				return err
			}
			defer db.Close()

			store := state.NewPolicy(db)
			doc, err := store.Load(ctx)
			if err != nil {
				return err
			}
			if !doc.DisablePasswordSignIn {
				fmt.Fprintln(cmd.OutOrStdout(), "Password sign-in is already on.")
				return nil
			}
			doc.DisablePasswordSignIn = false
			if err := store.Save(ctx, doc, "system"); err != nil {
				return err
			}
			// Audited, and fatal if it is not: this changes how everyone signs
			// in, from outside every check the API makes.
			if err := audit.New(db.Pool).Write(ctx, audit.Event{
				PrincipalKind: audit.KindSystem, PrincipalID: "system", Action: "policy.update",
				TargetKind: "policy", TargetID: "host",
				Detail: map[string]any{"disable_password_sign_in": false, "via": "pando admin enable-password-sign-in",
					"reason": "run from the host shell, outside any session"},
			}); err != nil {
				return errs.Wrap(errs.Internal,
					"Password sign-in was turned on and Pando could not record it in the audit log.", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Password sign-in is on. Local accounts can sign in with their passwords again.")
			return nil
		},
	}
}
