package cli

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// The commands under `pando app detection`, one per detection endpoint
// (R-261): answering, revising, comparing, re-running and accepting.

func detectionPath(appID, rest string) string {
	return "/apps/" + url.PathEscape(appID) + "/detection" + rest
}

func detectionAnswerCmd(client func() (*Client, error)) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "answer <app> <key> <answer>",
		Short: "Answer one of detection's questions",
		Long: "Answers a question detection asked. The key is the one printed with the\n" +
			"question by `pando app detection <app>`.\n\n" +
			"An answer that changes what is built, such as a build argument, has Pando build\n" +
			"and scan the image again, so the plan shows the score of what will run. This\n" +
			"waits for that, printing each stage, then exits as `pando app detection --wait`\n" +
			"does: 0 when the app is ready to accept, 2 when there are questions left, and 1\n" +
			"when detection failed or was blocked.",
		Args: cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			appID, key, answer := args[0], args[1], args[2]
			if err := c.Do("POST", detectionPath(appID, "/answers"),
				map[string]any{"answers": map[string]string{key: answer}}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Answered %s.\n", key)
			return c.waitAndReport(cmd, appID, timeout)
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", detectionTimeout, "how long to wait for a rebuild before giving up")
	return cmd
}

func detectionReviseCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "revise <app> <message>...",
		Short: "Tell the AI adapter what is wrong with the plan",
		Long: "Tells the install's AI adapter what is wrong with the plan, in your own words,\n" +
			"for example \"The app serves on port 8080.\" It checks the repository, changes\n" +
			"what it can show, and replies. Prints the reply, what it changed and what Pando\n" +
			"refused, then where detection stands. Needs an AI adapter.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			appID, message := args[0], strings.Join(args[1:], " ")
			var d detectionStatus
			if err := c.Do("POST", detectionPath(appID, "/revise"), map[string]any{"message": message}, &d); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if turns := d.Detection.Conversation; len(turns) > 0 && turns[len(turns)-1].From == "ai" {
				reply := turns[len(turns)-1]
				fmt.Fprintln(w, reply.Text)
				for _, change := range reply.Changes {
					fmt.Fprintf(w, "  Changed: %s\n", change)
				}
				for _, refused := range reply.Refused {
					fmt.Fprintf(w, "  Refused: %s\n", refused)
				}
				fmt.Fprintln(w)
			}
			printDetection(w, appID, d)
			return nil
		},
	}
}

// detectionDiff is GET /detection/diff as the CLI reads it.
type detectionDiff struct {
	Pinned  *int `json:"pinned"`
	IsFirst bool `json:"is_first"`
	Changes *struct {
		Changes []struct {
			Class   string `json:"class"`
			Summary string `json:"summary"`
		} `json:"changes"`
	} `json:"changes"`
}

func detectionDiffCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "diff <app>",
		Short: "Show what accepting the proposal would change",
		Long: "Compares detection's proposal with the configuration the app is pinned to, and\n" +
			"lists each change with what applying it takes: benign, restart, rebuild or\n" +
			"destructive. A destructive change loses data, such as removing a volume.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			appID := args[0]
			var d detectionDiff
			if err := c.Do("GET", detectionPath(appID, "/diff"), nil, &d); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if d.IsFirst {
				fmt.Fprintln(w, "This app is not configured yet, so accepting sets it up from the proposal.")
				fmt.Fprintf(w, "Next: `pando app detection accept %s`.\n", appID)
				return nil
			}
			pinned := "the pinned revision"
			if d.Pinned != nil {
				pinned = fmt.Sprintf("revision %d", *d.Pinned)
			}
			if d.Changes == nil || len(d.Changes.Changes) == 0 {
				fmt.Fprintf(w, "Accepting changes nothing: the proposal matches %s.\n", pinned)
				return nil
			}
			fmt.Fprintf(w, "Accepting would change %s:\n\n", pinned)
			for _, ch := range d.Changes.Changes {
				fmt.Fprintf(w, "  %-11s  %s\n", ch.Class, ch.Summary)
			}
			fmt.Fprintf(w, "\nNext: `pando app detection accept %s --confirm` accepts it.\n", appID)
			return nil
		},
	}
}

func detectionRerunCmd(client func() (*Client, error)) *cobra.Command {
	var wait bool
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "rerun <app>",
		Short: "Run detection again, against the source as it is now",
		Long: "Works out again how to run an app, from its source as it is now: after the\n" +
			"repository changed, after uploading new files, or after fixing what blocked it.\n" +
			"Nothing about the app changes until the new proposal is accepted.\n\n" +
			"With --wait, waits for it and exits as `pando app detection --wait` does.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			appID := args[0]
			if err := c.Do("POST", detectionPath(appID, "/rerun"), map[string]any{}, nil); err != nil {
				return err
			}
			if wait {
				return c.waitAndReport(cmd, appID, timeout)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Detection is running again. `pando app detection %s --wait` waits for it.\n", appID)
			return nil
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "wait for detection to finish, and exit non-zero unless it is ready")
	cmd.Flags().DurationVar(&timeout, "timeout", detectionTimeout, "with --wait, how long to wait before giving up")
	return cmd
}

// acceptValues turns KEY=VALUE flags into the variables POST
// /detection/accept takes.
func acceptValues(flag string, pairs []string, secret bool) ([]map[string]any, error) {
	values := make([]map[string]any, 0, len(pairs))
	for _, kv := range pairs {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return nil, fmt.Errorf("%s takes KEY=VALUE, and %q has no =", flag, kv)
		}
		v := map[string]any{"key": key, "value": value}
		if secret {
			v["secret"] = true
		}
		values = append(values, v)
	}
	return values, nil
}

func detectionAcceptCmd(client func() (*Client, error)) *cobra.Command {
	var env, secrets, optional []string
	var hostname string
	var confirm bool

	cmd := &cobra.Command{
		Use:   "accept <app>",
		Short: "Accept detection's proposal as the app's configuration",
		Long: "Accepts what detection worked out and pins it as the app's configuration. This\n" +
			"does not deploy: `pando deploy <app>` does that.\n\n" +
			"--env and --secret set variables in the same step; a secret is stored by the\n" +
			"install's secrets adapter. --optional names a dependency the app runs without.\n\n" +
			"Accepting over an app that is already configured replaces how it is built and\n" +
			"what it runs, and needs --confirm. Variables you set, how each dependency is\n" +
			"filled, and storage you added are carried over. `pando app detection diff <app>`\n" +
			"shows what would change.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			plain, err := acceptValues("--env", env, false)
			if err != nil {
				return err
			}
			hidden, err := acceptValues("--secret", secrets, true)
			if err != nil {
				return err
			}
			c, err := client()
			if err != nil {
				return err
			}
			appID := args[0]
			body := map[string]any{"values": append(plain, hidden...), "confirm": confirm}
			if len(optional) > 0 {
				body["optional"] = optional
			}
			if hostname != "" {
				body["hostname"] = hostname
			}
			var out struct {
				Revision int `json:"revision"`
			}
			if err := c.Do("POST", detectionPath(appID, "/accept"), body, &out); err != nil {
				// The server's remedy asks for `confirm: true`; here that is a
				// flag.
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Details["pinned_spec_id"] != nil {
					return fmt.Errorf("%s\nVariables you set, how each dependency is filled, and storage you added "+
						"are carried over. `pando app detection diff %s` shows what would change; add --confirm to go ahead",
						apiErr.Message, appID)
				}
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Accepted as revision %d. Nothing is deployed yet.\n", out.Revision)
			fmt.Fprintf(cmd.OutOrStdout(), "Next: `pando deploy %s`.\n", appID)
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&env, "env", nil, "set a variable, KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&secrets, "secret", nil, "set a secret variable, KEY=VALUE (repeatable)")
	cmd.Flags().StringArrayVar(&optional, "optional", nil, "a dependency the app runs without, by its key (repeatable)")
	cmd.Flags().StringVar(&hostname, "hostname", "", "the address to serve the app at, where its routing gives it a hostname of its own")
	cmd.Flags().BoolVar(&confirm, "confirm", false, "replace the configuration of an app that is already configured")
	return cmd
}
