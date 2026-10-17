package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/trypando/pando/internal/qrcode"
)

// codeTwoFactorRequired is the server's answer to a sign-in that needs a
// second factor as well as the password (R-405).
const codeTwoFactorRequired = "AUTH_TWO_FACTOR_REQUIRED"

const twoFactorPath = "/me/two-factor"

const currentPasswordPrompt = "Current password: "

// twoFactorCmd is two-factor authentication on your own local account
// (R-405): the same routes the console's settings use.
func twoFactorCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "two-factor",
		Short: "Set up or change two-factor authentication on your own local account",
		Long: "Two-factor authentication asks for a code from an authenticator app after your password.\n" +
			"Accounts from an identity provider set it up at the provider instead.",
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "status",
		Short: "Show whether two-factor authentication is on",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var me struct {
				TwoFactor *struct {
					Enabled                bool `json:"enabled"`
					Required               bool `json:"required"`
					RecoveryCodesRemaining int  `json:"recovery_codes_remaining"`
				} `json:"two_factor"`
			}
			if err := c.Do("GET", "/me", nil, &me); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			switch tf := me.TwoFactor; {
			case tf == nil:
				fmt.Fprintln(out, "This account signs in through an identity provider, which asks for its second factor.")
			case tf.Enabled:
				fmt.Fprintf(out, "Two-factor authentication is on. %d recovery codes left.\n", tf.RecoveryCodesRemaining)
			default:
				fmt.Fprintln(out, "Two-factor authentication is off. Run `pando two-factor setup` to turn it on.")
			}
			if tf := me.TwoFactor; tf != nil && tf.Required {
				fmt.Fprintln(out, "This installation requires it on local accounts.")
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "setup",
		Short: "Set up an authenticator app, or replace the one in use",
		Long: "Shows a QR code to scan with an authenticator app, asks for the code it then shows, and prints\n" +
			"recovery codes. An authenticator already in use keeps working until the new one is confirmed.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			password, err := promptSecret(cmd, currentPasswordPrompt)
			if err != nil {
				return err
			}
			var enrollment struct {
				Secret string `json:"secret"`
				URI    string `json:"uri"`
			}
			if err := c.Do("POST", twoFactorPath+"/totp",
				map[string]string{"current_password": password}, &enrollment); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "Scan this QR code with your authenticator app:")
			fmt.Fprintln(out)
			if picture, err := qrcode.Text(enrollment.URI); err == nil {
				fmt.Fprint(out, picture)
			}
			fmt.Fprintf(out, "\nOr enter this key by hand: %s\n\n", groupKey(enrollment.Secret))

			code, err := prompt(cmd, "Code from the app: ")
			if err != nil {
				return err
			}
			var confirmed struct {
				RecoveryCodes []string `json:"recovery_codes"`
			}
			if err := c.Do("POST", twoFactorPath+"/totp/confirm",
				map[string]string{"code": code}, &confirmed); err != nil {
				return err
			}
			fmt.Fprintln(out, "Two-factor authentication is on.")
			printRecoveryCodes(out, confirmed.RecoveryCodes)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "recovery-codes",
		Short: "Replace your recovery codes with new ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			password, err := promptSecret(cmd, currentPasswordPrompt)
			if err != nil {
				return err
			}
			var issued struct {
				RecoveryCodes []string `json:"recovery_codes"`
			}
			if err := c.Do("POST", twoFactorPath+"/recovery-codes",
				map[string]string{"current_password": password}, &issued); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Your old recovery codes no longer work.")
			printRecoveryCodes(cmd.OutOrStdout(), issued.RecoveryCodes)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "disable",
		Short: "Turn off two-factor authentication",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			password, err := promptSecret(cmd, currentPasswordPrompt)
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", twoFactorPath,
				map[string]string{"current_password": password}, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Two-factor authentication is off.")
			return nil
		},
	})
	return cmd
}

// userResetTwoFactorCmd is an administrator's reset of someone else's second
// factor (R-405).
func userResetTwoFactorCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "reset-two-factor <user-id>",
		Short: "Remove an account's two-factor authentication",
		Long: "Removes a local account's authenticator and recovery codes and ends every session it holds,\n" +
			"for someone who has lost both. They sign in with their password alone, and set it up again\n" +
			"there if host policy requires it.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", usersPath+args[0]+"/two-factor", nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Two-factor authentication removed. The account is signed out everywhere.")
			return nil
		},
	}
}

func printRecoveryCodes(out io.Writer, codes []string) {
	fmt.Fprintln(out, "\nRecovery codes. Each one signs in once in place of a code from your app, if you")
	fmt.Fprintln(out, "lose your phone. Save them somewhere safe now: Pando does not show them again.")
	fmt.Fprintln(out)
	for _, code := range codes {
		fmt.Fprintf(out, "  %s\n", code)
	}
}

// groupKey spaces an authenticator key in fours, the way apps show it, so it
// can be typed without losing one's place.
func groupKey(key string) string {
	var b strings.Builder
	for i, r := range key {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}
