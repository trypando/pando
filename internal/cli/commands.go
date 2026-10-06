package cli

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Commands returns the client half of the CLI (design 04 §4).
//
// Every one of these is a wrapper over an endpoint. That is the constraint
// R-261 puts on this package and the reason it reads repetitively: a command
// that did something the API cannot do would be a capability the console and
// MCP could never have.
func Commands() []*cobra.Command {
	var server string

	withServer := func(c *cobra.Command) *cobra.Command {
		c.PersistentFlags().StringVar(&server, "server", "", "Pando server URL (defaults to the one you logged in to)")
		return c
	}

	client := func() (*Client, error) { return New(server) }

	return []*cobra.Command{
		withServer(loginCmd(&server)),
		withServer(appCmd(client)),
		withServer(deployCmd(client)),
		withServer(execCmd(client)),
		withServer(slotCmd(client)),
		withServer(planCmd(client)),
		withServer(logsCmd(client)),
		withServer(secretCmd(client)),
		withServer(grantCmd(client)),
		withServer(userCmd(client)),
		withServer(groupCmd(client)),
		withServer(adapterCmd(client)),
		withServer(identityCmd(client)),
		withServer(aiCmd(client)),
		withServer(restartCmd(client)),
		withServer(sectionCmd(client)),
		withServer(auditCmd(client)),
		withServer(configCmd(client)),
		withServer(updatesCmd(client)),
		withServer(upgradeCmd(client)),
		withServer(rollbackCmd(client)),
		withServer(approvalsCmd(client)),
		withServer(eventsCmd(client)),
		withServer(subscriptionsCmd(client)),
		withServer(notificationsCmd(client)),
		withServer(exportCmd(client)),
		withServer(backupCmd(client)),
		withServer(policyCmd(client)),
		withServer(tokenCmd(client)),
		withServer(MCPCommand()),
	}
}

func loginCmd(server *string) *cobra.Command {
	var username string

	cmd := &cobra.Command{
		Use:   "login [server-url]",
		Short: "Sign in and store a token for this machine",
		Long: "Signs in with a username and password, then creates a token and stores it.\n" +
			"The token acts as you and holds nothing you do not.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			url := "http://localhost:8080"
			if len(args) == 1 {
				url = strings.TrimSuffix(args[0], "/")
			} else if *server != "" {
				url = *server
			}

			if username == "" {
				var err error
				username, err = prompt(cmd, "Username: ")
				if err != nil {
					return err
				}
			}

			// Read without echo. A password in a terminal's scrollback is a
			// password in the scrollback, and `--password` would put it in the
			// shell history as well.
			password, err := promptSecret(cmd, "Password: ")
			if err != nil {
				return err
			}

			// A cookie jar for exactly two calls: sign in, then mint a token
			// with the session. The token is what gets stored — a session is
			// bound to a browser's lifetime and dies on a password change,
			// which is right for a browser and wrong for a script at 3am.
			jar, err := cookiejar.New(nil)
			if err != nil {
				return err
			}
			anon := &Client{BaseURL: url, HTTP: &http.Client{Jar: jar, Timeout: time.Minute}}

			var session struct {
				UserID             string `json:"user_id"`
				MustChangePassword bool   `json:"must_change_password"`
			}
			if err := anon.Do("POST", "/sessions",
				map[string]string{"username": username, "password": password}, &session); err != nil {
				return err
			}
			if session.MustChangePassword {
				return fmt.Errorf("this account still has the password Pando generated for it.\n" +
					"Open the web console and choose your own first")
			}

			name, _ := os.Hostname()
			if name == "" {
				name = "cli"
			}
			var issued struct {
				Secret string `json:"secret"`
			}
			if err := anon.Do("POST", "/tokens",
				map[string]any{"name": name + " CLI"}, &issued); err != nil {
				return err
			}

			// The session has done its job. Revoking it means the only
			// credential left on this machine is the one in the file, with the
			// permissions that file has.
			_ = anon.Do("DELETE", "/sessions", nil, nil)

			if err := SaveCredentials(Credentials{URL: url, Token: issued.Secret, User: username}); err != nil {
				return err
			}

			path, _ := credentialsPath()
			fmt.Fprintf(cmd.OutOrStdout(), "Signed in to %s. Token stored in %s.\n", url, path)
			return nil
		},
	}
	cmd.Flags().StringVar(&username, "username", "", "username to sign in as")
	return cmd
}

// appIconCmd sets or removes the image on an app's launcher tile (R-340).
func appIconCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "icon", Short: "Set the image on an app's launcher tile"}

	cmd.AddCommand(&cobra.Command{
		Use:   "set <app> <image-file>",
		Short: "Set an app's image from a PNG, JPEG, WebP or GIF file",
		Long: "Sets the image shown on the app's tile in everyone's launcher.\n\n" +
			"PNG, JPEG, WebP or GIF, at most 256 KB. SVG is not accepted. A square image a few\n" +
			"hundred pixels across is plenty.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			image, err := os.ReadFile(args[1])
			if err != nil {
				return err
			}
			if err := c.UploadIcon(args[0], image); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Set the image for %s.\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "clear <app>",
		Short: "Remove an app's image, so its tile shows the map generated for it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/apps/"+args[0]+"/icon", nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the image for %s.\n", args[0])
			return nil
		},
	})

	return cmd
}

func appCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "app", Short: "Work with apps"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List the apps you can manage",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Apps []struct {
					ID        string `json:"id"`
					Name      string `json:"name"`
					Slug      string `json:"slug"`
					State     string `json:"state"`
					Detection *struct {
						Status string `json:"status"`
						Stage  string `json:"stage"`
					} `json:"detection"`
				} `json:"apps"`
			}
			if err := c.Do("GET", "/apps", nil, &out); err != nil {
				return err
			}

			t := table(cmd.OutOrStdout(), "NAME", "STATE", "ID")
			for _, a := range out.Apps {
				state := a.State
				// A draft says why it is still one (issue #80).
				if a.State == "draft" && a.Detection != nil {
					why := a.Detection.Status
					if a.Detection.Stage != "" {
						why += ", " + a.Detection.Stage
					}
					state += " (detection " + why + ")"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\n", a.Name, state, a.ID)
			}
			return t.Flush()
		},
	})

	add := &cobra.Command{
		Use:   "add <source-url> | --image <reference>",
		Short: "Create an app from a repository, or from an image that is already built",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			image, _ := cmd.Flags().GetString("image")
			if (len(args) == 1) == (image != "") {
				return fmt.Errorf("give a repository URL, or --image with an image reference such as ghcr.io/acme/web:1.4")
			}
			c, err := client()
			if err != nil {
				return err
			}
			name, _ := cmd.Flags().GetString("name")

			var src map[string]any
			if image != "" {
				if name == "" {
					name = nameFromImage(image)
				}
				src = map[string]any{"type": "image", "image": image}
				cred, err := registryCredential(cmd)
				if err != nil {
					return err
				}
				if cred != nil {
					src["credential"] = cred
				}
			} else {
				if name == "" {
					name = nameFromURL(args[0])
				}
				src = map[string]any{"type": "git", "url": args[0]}
			}

			var app map[string]any
			if err := c.Do("POST", "/apps", map[string]any{
				"name":   name,
				"source": src,
			}, &app); err != nil {
				return err
			}
			// 202: the app exists in draft and detection has been queued.
			// Saying so is the difference between waiting and wondering —
			// and pointing at the command that says when it is done, rather
			// than at one that does not (issue #80).
			id, _ := app["id"].(string)
			if wait, _ := cmd.Flags().GetBool("wait"); wait {
				fmt.Fprintf(cmd.ErrOrStderr(), "Created %s (%s). Working out how to run it...\n", name, id)
				timeout, _ := cmd.Flags().GetDuration("timeout")
				return c.waitAndReport(cmd, id, timeout)
			}
			fmt.Fprintf(cmd.OutOrStdout(),
				"Created %s (%s). Pando is working out how to run it — `pando app detection %s --wait` to wait for it.\n",
				name, id, id)
			return nil
		},
	}
	// Bound to `add` directly. Reaching for the last entry of cmd.Commands()
	// instead put the flag on whichever subcommand sorted last — cobra keeps
	// that slice in alphabetical order, not insertion order — so `app add
	// --name` was rejected as an unknown flag and `app list --name` quietly
	// accepted one it ignores.
	add.Flags().String("name", "", "name for the app (defaults to the repository or image name)")
	add.Flags().String("image", "", "run this prebuilt image instead of building a repository, such as ghcr.io/acme/web:1.4")
	registryCredentialFlags(add)
	add.Flags().Bool("wait", false, "wait for detection to finish, as `pando app detection --wait` does")
	add.Flags().Duration("timeout", detectionTimeout, "with --wait, how long to wait before giving up")
	cmd.AddCommand(add)
	cmd.AddCommand(registryCredentialCmd(client))
	cmd.AddCommand(appDetectionCmd(client))

	del := &cobra.Command{
		Use:   "delete <app>",
		Short: "Delete an app, keeping a final backup of its data",
		Long: "Deletes an app. Its storage is backed up first and the backup is kept until you\n" +
			"discard it — not aged out.\n\n" +
			"If the backup fails the app is not deleted, because a failed backup means the data\n" +
			"is not safe. Pass --discard-data to delete without one.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			discard, _ := cmd.Flags().GetBool("discard-data")

			// Backing up by default is R-205: a non-interactive delete keeps a
			// copy unless told not to. The API asks the caller to decide; this
			// is the CLI deciding, which is where the requirement puts it.
			query := "?backup=true"
			if discard {
				query = "?force=true"
			}

			if err := c.Do("DELETE", "/apps/"+args[0]+query, nil, nil); err != nil {
				return err
			}
			if discard {
				fmt.Fprintln(cmd.OutOrStdout(), "Deleted. Its storage was discarded.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(),
					"Deleted. Its storage is backed up — `pando backup list` to find it.")
			}
			return nil
		},
	}
	del.Flags().Bool("discard-data", false, "delete without keeping a backup of the app's storage")
	cmd.AddCommand(del)

	cmd.AddCommand(&cobra.Command{
		Use:   "show <app>",
		Short: "Show an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var app map[string]any
			if err := c.Do("GET", "/apps/"+args[0], nil, &app); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), app)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "stop <app>",
		Short: "Stop an app without deleting it",
		Long: "Stops an app.\n\n" +
			"Nothing is removed: its storage, its configuration and its address are kept, and\n" +
			"`pando app start` brings back the version that was running. A stopped app stays\n" +
			"stopped — it is the app's desired state, not a one-off act, so it survives Pando\n" +
			"itself restarting.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/stop", map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Stopping %s. `pando app start %s` brings it back.\n",
				args[0], args[0])
			return nil
		},
	})

	cmd.AddCommand(appIconCmd(client))

	cmd.AddCommand(&cobra.Command{
		Use:   "rename <app> <new-name>",
		Short: "Change an app's display name",
		Long: "Changes the name shown for the app in the console and on everyone's launcher.\n\n" +
			"The app's ID and address do not change.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("PATCH", "/apps/"+args[0], map[string]string{"name": args[1]}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Renamed %s to %q.\n", args[0], args[1])
			return nil
		},
	})

	// Favorites (R-341): pinned to the top of your own launcher.
	for _, f := range []struct {
		use, short, method, done string
	}{
		{"favorite <app>", "Pin an app to the top of your launcher", "PUT", "Pinned %s to the top of your launcher.\n"},
		{"unfavorite <app>", "Unpin an app from your launcher", "DELETE", "Unpinned %s.\n"},
	} {
		cmd.AddCommand(&cobra.Command{
			Use:   f.use,
			Short: f.short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := client()
				if err != nil {
					return err
				}
				if err := c.Do(f.method, "/me/favorites/"+args[0], nil, nil); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), f.done, args[0])
				return nil
			},
		})
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "start <app>",
		Short: "Start an app that was stopped",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/start", map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Starting %s.\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "restart <app>",
		Short: "Restart an app's workloads, changing nothing",
		Long: "Restarts the workloads in place.\n\n" +
			"Nothing is rebuilt and nothing is re-read: this is the same version, started again.\n" +
			"To ship a change, deploy.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/restart", map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Restarting %s.\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status <app>",
		Short: "What each part of an app is doing right now",
		Long: "What each part of an app is doing right now.\n\n" +
			"An app can be made of several parts, and \"degraded\" is the app's answer for all " +
			"of them together. This is the per-part answer: which are running, which are " +
			"restarting and how many times, and which health check is failing. The names are " +
			"also what `pando logs --workload` takes.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var status struct {
				State         string `json:"state"`
				DesiredState  string `json:"desired_state"`
				Observability string `json:"observability"`
				Workloads     []struct {
					Name         string `json:"name"`
					Primary      bool   `json:"primary"`
					Present      bool   `json:"present"`
					Running      bool   `json:"running"`
					Restarting   bool   `json:"restarting"`
					RestartCount int    `json:"restart_count"`
					Healthy      *bool  `json:"healthy"`
					ExitCode     *int   `json:"exit_code"`
				} `json:"workloads"`
			}
			if err := c.Do("GET", "/apps/"+args[0]+"/status", nil, &status); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s (wanted: %s)\n", status.State, status.DesiredState)
			if status.Observability == "unreachable" {
				fmt.Fprintln(out, "The runtime could not be reached, so what follows is unknown rather than false.")
				return nil
			}
			if len(status.Workloads) == 0 {
				fmt.Fprintln(out, "Nothing is running.")
				return nil
			}

			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PART\tSTATE\tHEALTH\tRESTARTS")
			for _, part := range status.Workloads {
				name := part.Name
				if part.Primary {
					name += " *"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%d\n",
					name, partState(part.Present, part.Running, part.Restarting, part.ExitCode),
					partHealth(part.Healthy), part.RestartCount)
			}
			_ = w.Flush()
			fmt.Fprintln(out, "\n* the part this app's address resolves to.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "usage <app>",
		Short: "What each part of an app is using right now: CPU, memory and disk",
		Long: "What each part of an app is using right now, beside its limits (R-245).\n\n" +
			"CPU is in cores; a part with no limit may use what the host has. A reading, not a history.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var usage struct {
				Supported bool `json:"supported"`
				Workloads []struct {
					Name        string `json:"name"`
					Primary     bool   `json:"primary"`
					Running     bool   `json:"running"`
					CPUMillis   int    `json:"cpu_millis"`
					CPULimit    int    `json:"cpu_limit_millis"`
					Memory      int64  `json:"memory_bytes"`
					MemoryLimit int64  `json:"memory_limit_bytes"`
					Disk        int64  `json:"disk_bytes"`
					Volumes     []struct {
						Name  string `json:"name"`
						Bytes int64  `json:"bytes"`
					} `json:"volumes"`
				} `json:"workloads"`
			}
			if err := c.Do("GET", "/apps/"+args[0]+"/usage", nil, &usage); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if !usage.Supported {
				fmt.Fprintln(out, "This app's runtime does not report what its parts are using.")
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "PART\tCPU\tMEMORY\tDISK\tVOLUMES")
			for _, p := range usage.Workloads {
				name := p.Name
				if p.Primary {
					name += " *"
				}
				cpu, mem := "stopped", "stopped"
				if p.Running {
					cpu = fmt.Sprintf("%.2f of %s", float64(p.CPUMillis)/1000, limitOr(p.CPULimit > 0, fmt.Sprintf("%.2f", float64(p.CPULimit)/1000)))
					mem = fmt.Sprintf("%s of %s", size(p.Memory), limitOr(p.MemoryLimit > 0, size(p.MemoryLimit)))
				}
				vols := make([]string, 0, len(p.Volumes))
				for _, v := range p.Volumes {
					vols = append(vols, v.Name+" "+size(v.Bytes))
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", name, cpu, mem, size(p.Disk), strings.Join(vols, ", "))
			}
			_ = w.Flush()
			fmt.Fprintln(out, "\n* the part this app's address resolves to.")
			return nil
		},
	})

	return cmd
}

// limitOr is the limit, or "no limit" for a part that may use what the host has.
func limitOr(has bool, limit string) string {
	if has {
		return limit
	}
	return "no limit"
}

// size is bytes as people read them; "unknown" for -1, which the API sends
// when the runtime could not say.
func size(b int64) string {
	switch {
	case b < 0:
		return "unknown"
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

// partState reads the runtime's facts as one word somebody can act on.
//
// Restarting comes before running because a crash-looping container is running
// at almost every instant somebody looks at it, and that is the reading that
// makes a broken app look fine.
func partState(present, running, restarting bool, exit *int) string {
	switch {
	case restarting:
		return "restarting"
	case running:
		return "running"
	case present && exit != nil:
		return fmt.Sprintf("exited (%d)", *exit)
	case present:
		return "stopped"
	default:
		return "not running"
	}
}

func partHealth(healthy *bool) string {
	switch {
	case healthy == nil:
		// R-221: no health check is not a failing one.
		return "no check"
	case *healthy:
		return "healthy"
	default:
		return "failing"
	}
}

func deployCmd(client func() (*Client, error)) *cobra.Command {
	var asApp string
	var env []string

	cmd := &cobra.Command{
		Use:   "deploy <app|path>",
		Short: "Deploy an app, or a directory on this machine",
		Long: "With an app ID, deploys that app.\n" +
			"With a path, packs the directory, uploads it as the app's source, and deploys that.\n\n" +
			"The second form exists for R-262's agent workflow: something that has just\n" +
			"generated an app cannot commit and push, but it can run a command.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			target := args[0]

			// A path, not an app. Deciding by "does this exist on disk" rather
			// than by a flag, because `pando deploy ./` is the form the design
			// names and a flag would make the common case the verbose one.
			if info, statErr := os.Stat(target); statErr == nil && info.IsDir() {
				appID := asApp
				if appID == "" {
					// No app named, so make one from the directory. Named for
					// the directory, which is what a person would have called
					// it anyway.
					abs, _ := filepath.Abs(target)
					var created map[string]any
					if err := c.Do("POST", "/apps", map[string]any{
						"name":   filepath.Base(abs),
						"source": map[string]string{"type": "upload"},
					}, &created); err != nil {
						return err
					}
					appID, _ = created["id"].(string)
					fmt.Fprintf(cmd.ErrOrStderr(), "Created %s (%s).\n", filepath.Base(abs), appID)
				}

				archive, files, err := PackDirectory(target)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "Uploading %d files (%s)...\n", files, humanBytes(len(archive)))
				if err := c.UploadSource(appID, archive); err != nil {
					return err
				}

				// The source only exists now, so detection has to run against
				// it — it could not have run at creation, when there was
				// nothing to look at. Explicit, which is R-022: detection never
				// re-runs on its own.
				if err := c.prepareUploadedApp(cmd, appID, env); err != nil {
					return err
				}
				target = appID
			}

			var dep waitingDeploy
			if err := c.Do("POST", "/apps/"+target+"/deployments", map[string]any{}, &dep); err != nil {
				return err
			}
			if dep.Status == "awaiting_approval" {
				printAwaiting(cmd.OutOrStdout(), target, dep)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deploying. Watch it with `pando logs %s -f`.\n", target)
			return nil
		},
	}
	cmd.Flags().StringVar(&asApp, "app", "", "deploy a directory as an existing app, instead of creating one")
	cmd.Flags().StringArrayVar(&env, "env", nil,
		"KEY=VALUE, set when a new directory's setup is accepted; repeat for more (e.g. --env API_URL=https://api)")
	return cmd
}

// prepareUploadedApp runs detection over a freshly uploaded directory and pins
// the result, or prints the questions and stops.
//
// The questions are printed **verbatim** (R-105). They are written to be
// self-contained and pasteable into the assistant that wrote the app, which is
// the whole intended workflow for R-262 — and paraphrasing them here would undo
// that at the last step, exactly as it would in the console.
func (c *Client) prepareUploadedApp(cmd *cobra.Command, appID string, env []string) error {
	if err := c.Do("POST", "/apps/"+appID+"/detection/rerun", map[string]any{}, nil); err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "Working out how to run it...")

	// Waited on the way `pando app detection --wait` waits, reporting each
	// stage as it is reached (issue #80).
	d, err := c.awaitDetection(appID, detectionTimeout, func(stage string) {
		if stage != "" {
			fmt.Fprintf(cmd.ErrOrStderr(), "  %s...\n", stagePhrase(stage))
		}
	})
	if err != nil {
		return err
	}

	if d.Status == "failed" || d.Status == "blocked" {
		printDetection(cmd.ErrOrStderr(), appID, d)
		return fmt.Errorf("pando could not work out how to run this directory — "+
			"run `pando app detection %s` to see what it found", appID)
	}

	if open := d.openQuestions(); len(open) > 0 {
		fmt.Fprintln(cmd.ErrOrStderr())
		fmt.Fprintln(cmd.ErrOrStderr(), "Pando needs to know a few things before it can deploy this:")
		for _, q := range open {
			fmt.Fprintf(cmd.ErrOrStderr(), "\n  %s\n", q)
		}
		fmt.Fprintln(cmd.ErrOrStderr(),
			"\nAnswer them in the web console, or paste a question into whatever wrote this app.")
		return fmt.Errorf("%d question(s) still to answer", len(open))
	}

	// Nothing outstanding, so accept the proposal and pin revision 1. Accepting
	// does not deploy — that is the next call, and keeping them separate is
	// what makes "accepted but not deployed" a state someone can sit in.
	// With the variables given on the command line written into the accepted
	// setup, as the console's onboarding does — one step, not a setup and
	// then an edit.
	values := make([]map[string]any, 0, len(env))
	for _, kv := range env {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || key == "" {
			return fmt.Errorf("--env takes KEY=VALUE, and %q has no =", kv)
		}
		values = append(values, map[string]any{"key": key, "value": value})
	}
	if err := c.Do("POST", "/apps/"+appID+"/detection/accept", map[string]any{"values": values}, nil); err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Recognized it: %s, built with %s.\n",
		d.Detection.Winner.Detector, d.Detection.Winner.Strategy)
	return nil
}

func execCmd(client func() (*Client, error)) *cobra.Command {
	var workload string

	cmd := &cobra.Command{
		Use:   "exec <app> [--workload <name>] [-- <command>...]",
		Short: "Run a command inside a running app",
		Long: "Opens a terminal inside a running workload.\n\n" +
			"This is the most privileged thing you can do to an app: what runs here can read\n" +
			"the app's database directly and read its injected environment, including secrets.\n" +
			"The command is recorded in the audit log; what happens inside the session is not.",
		Args: cobra.MinimumNArgs(1),

		// The command after -- is the container's, not cobra's. Without this,
		// `pando exec notes -- ls -la` loses -la to flag parsing.
		DisableFlagsInUseLine: true,

		// A non-zero exit inside the container is the command's answer, not a
		// usage problem, so cobra must not print the help text over it.
		SilenceUsage: true,

		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			command := args[1:]
			if at := cmd.ArgsLenAtDash(); at >= 0 {
				command = args[at:]
			}

			in, ok := cmd.InOrStdin().(*os.File)
			if !ok {
				in = os.Stdin
			}

			err = c.Exec(cmd.Context(), ExecOptions{
				AppID: args[0], Workload: workload, Command: command,
				In: in, Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr(),
			})

			// A command that exited non-zero inside the container is reported
			// the way ssh reports it: this process takes the same status and
			// prints nothing. Anything else and `pando exec app -- test -f x`
			// cannot be used in a script.
			var exit *exitError
			if errors.As(err, &exit) {
				cmd.SilenceErrors = true
				os.Exit(exit.ExitCode())
			}
			return err
		},
	}

	cmd.Flags().StringVar(&workload, "workload", "",
		"which part of the app to open a terminal in (default: the one its URL points at)")
	return cmd
}

func slotCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "slot", Short: "Fill an app's service slots"}

	set := &cobra.Command{
		Use:   "set <app> <key>",
		Short: "Say how a slot is filled",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			provision, _ := cmd.Flags().GetBool("provision")
			bind, _ := cmd.Flags().GetString("bind")
			literal, _ := cmd.Flags().GetString("literal")

			chosen := 0
			body := map[string]any{"key": args[1]}
			if provision {
				chosen++
				body["mode"] = "provision"
			}
			if bind != "" {
				chosen++
				body["mode"] = "bind"
				body["target"] = bind
			}
			if literal != "" {
				chosen++
				body["mode"] = "literal"
				body["value"] = literal
			}
			if chosen != 1 {
				return fmt.Errorf("choose exactly one of --provision, --bind or --literal")
			}

			if err := c.Do("PUT", "/apps/"+args[0]+"/slots/"+args[1], body, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Set %s.\n", args[1])
			return nil
		},
	}
	set.Flags().Bool("provision", false, "let Pando create the service")
	set.Flags().String("bind", "", "bind to an existing service")
	set.Flags().String("literal", "", "use this value directly")
	cmd.AddCommand(set)
	return cmd
}

func humanBytes(n int) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func planCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "plan <app>",
		Short: "Show what a deploy would do, without doing it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var plan map[string]any
			if err := c.Do("POST", "/apps/"+args[0]+"/plan", map[string]any{}, &plan); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), plan)
		},
	}
}

func logsCmd(client func() (*Client, error)) *cobra.Command {
	var (
		follow   bool
		workload string
	)

	cmd := &cobra.Command{
		Use:   "logs <app>",
		Short: "Read an app's logs",
		Long: "Read an app's logs.\n\n" +
			"An app made of several parts — a web service, a worker, a database it " +
			"brought with it — has a log per part. Without --workload this is the " +
			"primary one, which is the part the app's address resolves to. " +
			"`pando app status <app>` lists the names.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			query := url.Values{}
			if follow {
				query.Set("follow", "true")
			}
			if workload != "" {
				query.Set("workload", workload)
			}
			path := "/apps/" + args[0] + "/logs"
			if len(query) > 0 {
				path += "?" + query.Encode()
			}
			body, err := c.Stream("GET", path, nil)
			if err != nil {
				return err
			}
			defer func() { _ = body.Close() }()

			_, err = io.Copy(cmd.OutOrStdout(), body)
			return err
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "keep the connection open and print new lines")
	cmd.Flags().StringVarP(&workload, "workload", "w", "",
		"which part of the app to read (default: the primary one)")
	return cmd
}

func secretCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "secret", Short: "Manage an app's secrets"}

	cmd.AddCommand(&cobra.Command{
		Use:   "set <app> <key>",
		Short: "Set a secret value, read from the terminal",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			// Read without echo and never from a flag. A secret passed as an
			// argument is in the shell history and in /proc for every process
			// on the machine to read.
			value, err := promptSecret(cmd, "Value: ")
			if err != nil {
				return err
			}
			if err := c.Do("PUT", "/apps/"+args[0]+"/secrets/"+args[1],
				map[string]string{"value": value}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Set %s.\n", args[1])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "list <app>",
		Short: "List which secrets are set, without their values",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/secrets", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})
	return cmd
}

func grantCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "grant", Short: "Share an app, or stop sharing it"}

	add := &cobra.Command{
		Use:   "add <app>",
		Short: "Give someone access to an app",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			user, _ := cmd.Flags().GetString("user")
			group, _ := cmd.Flags().GetString("group")
			anyone, _ := cmd.Flags().GetBool("anyone")
			passcode, _ := cmd.Flags().GetString("passcode")
			plane, _ := cmd.Flags().GetString("plane")
			role, _ := cmd.Flags().GetString("role")

			// Exactly one of the three: a grant is to one principal.
			chosen := 0
			for _, set := range []bool{user != "", group != "", anyone} {
				if set {
					chosen++
				}
			}
			if chosen != 1 {
				return fmt.Errorf("say who to share with: one of --user=<id>, --group=<id> or --anyone")
			}
			if passcode != "" && !anyone {
				return fmt.Errorf("--passcode goes with --anyone: only sharing with everyone has a passcode")
			}

			body := map[string]any{"plane": plane}
			switch {
			case anyone:
				// Anyone on the internet, without signing in (R-077) — or
				// with the passcode, if one is given (R-075a).
				body["plane"] = "data"
				body["principal_kind"] = "anonymous"
				if passcode != "" {
					body["passcode"] = passcode
				}
			case group != "":
				body["principal_kind"] = "group"
				body["principal_id"] = group
			default:
				body["principal_kind"] = "user"
				body["principal_id"] = user
			}
			if role != "" {
				body["role_id"] = role
			}
			if err := c.Do("POST", "/apps/"+args[0]+"/grants", body, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Shared.")
			return nil
		},
	}
	add.Flags().String("user", "", "user ID to share with")
	add.Flags().String("group", "", "group ID to share with")
	add.Flags().Bool("anyone", false, "share with anyone on the internet, without signing in")
	add.Flags().String("passcode", "", "with --anyone: only those who enter this passcode")
	// "data" by default: sharing an app normally means letting someone use it,
	// not letting them redeploy it. The dangerous one has to be asked for.
	add.Flags().String("plane", "data", "data (use the app) or control (manage it)")
	add.Flags().String("role", "", "role ID, for control-plane grants")
	cmd.AddCommand(add)

	cmd.AddCommand(&cobra.Command{
		Use:   "list <app>",
		Short: "Show who an app is shared with",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/grants", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "passcode <app> <grant-id> [passcode]",
		Short: "Change the passcode on an app shared with everyone, or remove it",
		Long: "Sets a new passcode on the app's grant to everyone; everyone let in by the old one\n" +
			"is asked again. With no passcode, removes it: the app is then open to anyone.",
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			passcode := ""
			if len(args) == 3 {
				passcode = args[2]
			}
			if err := c.Do("PATCH", "/apps/"+args[0]+"/grants/"+args[1], map[string]any{"passcode": passcode}, nil); err != nil {
				return err
			}
			if passcode == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "Passcode removed. Anyone on the internet can open it without signing in.")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "Passcode changed.")
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "role <app> <grant-id> <role-id>",
		Short: "Change the role a grant for managing an app carries",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("PATCH", "/apps/"+args[0]+"/grants/"+args[1], map[string]any{"role_id": args[2]}, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Role changed.")
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "remove <app> <grant-id>",
		Short: "Take a grant away",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/apps/"+args[0]+"/grants/"+args[1], nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed.")
			return nil
		},
	})
	return cmd
}

func userCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Work with accounts"}

	// generated asks the server for a password, so the CLI's are the same
	// strength and alphabet as the console's (R-046).
	generated := func(c *Client) (string, error) {
		var out struct {
			Password string `json:"password"`
		}
		if err := c.Do("POST", "/passwords/generate", nil, &out); err != nil {
			return "", err
		}
		return out.Password, nil
	}

	var name, email string
	var keep bool
	create := &cobra.Command{
		Use:   "create <username>",
		Short: "Create a local account with a generated password",
		Long: "Creates a local account and prints the password Pando generated for it, once.\n" +
			"Give it to the account holder yourself; by default they choose their own at first sign-in.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			password, err := generated(c)
			if err != nil {
				return err
			}
			body := map[string]any{
				"username": args[0], "display_name": name, "email": email,
				"password": password, "must_change_password": !keep,
			}
			if err := c.Do("POST", "/users", body, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Created %s. Password: %s\n", args[0], password)
			return nil
		},
	}
	create.Flags().StringVar(&name, "name", "", "the name shown in the console and the audit log")
	create.Flags().StringVar(&email, "email", "", "the account's email address")
	create.Flags().BoolVar(&keep, "no-change-required", false, "do not require a new password at first sign-in")
	cmd.AddCommand(create)

	var keepReset bool
	reset := &cobra.Command{
		Use:   "reset-password <user-id>",
		Short: "Give an account a new generated password",
		Long: "Sets a new generated password on a local account, ends every session it holds, and prints\n" +
			"the password once. By default its holder chooses their own at the next sign-in.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			password, err := generated(c)
			if err != nil {
				return err
			}
			body := map[string]any{"password": password, "must_change_password": !keepReset}
			if err := c.Do("POST", "/users/"+args[0]+"/password", body, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Password reset. New password: %s\n", password)
			return nil
		},
	}
	reset.Flags().BoolVar(&keepReset, "no-change-required", false, "do not require a new password at the next sign-in")
	cmd.AddCommand(reset)

	update := &cobra.Command{
		Use:   "update <user-id>",
		Short: "Change an account's username, name or email",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			// Only the flags given: an unset flag leaves that field alone.
			body := map[string]any{}
			for flag, field := range map[string]string{"username": "username", "name": "display_name", "email": "email"} {
				if cmd.Flags().Changed(flag) {
					v, _ := cmd.Flags().GetString(flag)
					body[field] = v
				}
			}
			if len(body) == 0 {
				return fmt.Errorf("say what to change: --username, --name or --email")
			}
			if err := c.Do("PATCH", "/users/"+args[0], body, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Updated.")
			return nil
		},
	}
	update.Flags().String("username", "", "the name the account signs in with")
	update.Flags().String("name", "", "the name shown in the console and the audit log")
	update.Flags().String("email", "", "the account's email address")
	cmd.AddCommand(update)

	cmd.AddCommand(&cobra.Command{
		Use:   "apps <user-id>",
		Short: "Show the apps an account has access to, and its role on each",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/users/"+args[0]+"/apps", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})
	return cmd
}

func groupCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "group", Short: "Work with groups and what they hold"}

	do := func(method, path string, body any, done string) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do(method, path, body, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), done)
			return nil
		}
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show every group, its members and its installation role",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/groups", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "add-member <group-id> <user-id>",
		Short: "Add an account to a group; it then holds what the group holds",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return do("PUT", "/groups/"+args[0]+"/members/"+args[1], nil, "Added.")(cmd, args)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "remove-member <group-id> <user-id>",
		Short: "Remove an account from a group",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return do("DELETE", "/groups/"+args[0]+"/members/"+args[1], nil, "Removed.")(cmd, args)
		},
	})
	var clear bool
	role := &cobra.Command{
		Use:   "role <group-id> [role-id]",
		Short: "Give a group an installation role, or take it away with --clear",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if clear {
				return do("DELETE", "/groups/"+args[0]+"/role", nil, "Role removed.")(cmd, args)
			}
			if len(args) < 2 {
				return fmt.Errorf("say which role, or pass --clear to take the group's role away")
			}
			return do("PUT", "/groups/"+args[0]+"/role", map[string]any{"role_id": args[1]}, "Role set.")(cmd, args)
		},
	}
	role.Flags().BoolVar(&clear, "clear", false, "take the group's installation role away")
	cmd.AddCommand(role)
	cmd.AddCommand(&cobra.Command{
		Use:   "apps <group-id>",
		Short: "Show the apps a group has access to, and its role on each",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/groups/"+args[0]+"/apps", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})
	return cmd
}

func rollbackCmd(client func() (*Client, error)) *cobra.Command {
	var to int

	cmd := &cobra.Command{
		Use:   "rollback <app>",
		Short: "Roll an app back to an earlier spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if to > 0 {
				body["spec_revision"] = to
			}
			var out waitingDeploy
			if err := c.Do("POST", "/apps/"+args[0]+"/deployments/rollback", body, &out); err != nil {
				return err
			}
			// A revision that never ran successfully is not a rollback R-157
			// exempts, so it can wait for approval like any other deploy.
			if out.Status == "awaiting_approval" {
				printAwaiting(cmd.OutOrStdout(), args[0], out)
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Rolling back.")
			return nil
		},
	}
	cmd.Flags().IntVar(&to, "to", 0, "spec revision to roll back to (defaults to the previous one)")
	return cmd
}

func exportCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "export <app>",
		Short: "Print an app's spec",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var spec map[string]any
			if err := c.Do("GET", "/apps/"+args[0]+"/export", nil, &spec); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), spec)
		},
	}
}

func backupCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Back up and restore this installation"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List backups",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Backups []struct {
					ID         string `json:"id"`
					Kind       string `json:"kind"`
					AdapterRef string `json:"adapter_ref"`
					CreatedAt  string `json:"created_at"`
					SizeBytes  int64  `json:"size_bytes"`
				} `json:"backups"`
			}
			if err := c.Do("GET", "/backups", nil, &out); err != nil {
				return err
			}
			t := table(cmd.OutOrStdout(), "TAKEN", "SIZE", "DESTINATION", "ID")
			for _, b := range out.Backups {
				fmt.Fprintf(t, "%s\t%d\t%s\t%s\n", b.CreatedAt, b.SizeBytes, b.AdapterRef, b.ID)
			}
			return t.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "create",
		Short: "Take a backup of the whole installation",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			// R-214, said before the passphrase is chosen rather than after.
			fmt.Fprintln(cmd.ErrOrStderr(),
				"Pando doesn't keep this passphrase. If you lose it, nothing in this backup\n"+
					"can be read again — not by you, and not by anyone who takes the file.")

			passphrase, err := promptSecret(cmd, "Passphrase: ")
			if err != nil {
				return err
			}
			again, err := promptSecret(cmd, "Passphrase again: ")
			if err != nil {
				return err
			}
			if passphrase != again {
				return fmt.Errorf("those two passphrases are different")
			}

			var out map[string]any
			if err := c.Do("POST", "/backups", map[string]any{"passphrase": passphrase}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Backed up: %s\n", out["id"])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "verify <backup-id>",
		Short: "Check a backup is complete, without restoring it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			passphrase, err := promptSecret(cmd, "Passphrase: ")
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("POST", "/backups/"+args[0]+"/verify",
				map[string]any{"passphrase": passphrase}, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "This backup is complete and can be restored.")
			return nil
		},
	})

	restore := &cobra.Command{
		Use:   "restore <backup-id>",
		Short: "Replace this installation from a backup",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			yes, _ := cmd.Flags().GetBool("yes")

			// Typed confirmation, like the console. A --yes flag alone would
			// make the most destructive action in the system a thing that fits
			// in a shell alias.
			if !yes {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"This replaces everything in this installation: every app, every account,\n"+
						"every secret. Anything created since the backup is gone.")
				answer, err := prompt(cmd, "Type replace to confirm: ")
				if err != nil {
					return err
				}
				if answer != "replace" {
					return fmt.Errorf("not confirmed, so nothing was changed")
				}
			}

			passphrase, err := promptSecret(cmd, "Passphrase: ")
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("POST", "/backups/"+args[0]+"/restore",
				map[string]any{"passphrase": passphrase, "confirm": true}, &out); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Restored.")
			return nil
		},
	}
	restore.Flags().Bool("yes", false, "skip the typed confirmation (for scripts that already have one)")
	cmd.AddCommand(restore)

	return cmd
}

func policyCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "policy", Short: "Read and set host policy"}

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Print the installation's policy",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var doc map[string]any
			if err := c.Do("GET", "/policy", nil, &doc); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), doc)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "set",
		Short: "Replace the policy with a document read from stdin",
		Long: "Reads a whole policy document as JSON on stdin and replaces the current one.\n" +
			"Replaces rather than merges: a merge would make it impossible to remove a rule.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return err
			}
			var doc map[string]any
			if err := json.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("that is not valid JSON: %w", err)
			}
			if err := c.Do("PUT", "/policy", doc, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Policy saved. Apps that are already running are unchanged.")
			return nil
		},
	})
	return cmd
}

func tokenCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage your API tokens"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "List your tokens",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Tokens []struct {
					ID        string  `json:"id"`
					Name      string  `json:"name"`
					RevokedAt *string `json:"revoked_at"`
				} `json:"tokens"`
			}
			if err := c.Do("GET", "/tokens", nil, &out); err != nil {
				return err
			}
			t := table(cmd.OutOrStdout(), "NAME", "STATE", "ID")
			for _, tok := range out.Tokens {
				state := "active"
				if tok.RevokedAt != nil {
					state = "revoked"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\n", tok.Name, state, tok.ID)
			}
			return t.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <token-id>",
		Short: "Revoke a token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/tokens/"+args[0], nil, nil); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Revoked. It stops working immediately.")
			return nil
		},
	})
	return cmd
}

// --- helpers ---------------------------------------------------------------

func table(w io.Writer, headers ...string) *tabwriter.Writer {
	t := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(t, strings.Join(headers, "\t"))
	return t
}

func printJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// buffered returns one reader per underlying stdin, reused across prompts.
//
// A fresh bufio.Reader per prompt reads ahead and keeps what it buffered, so
// asking for a username and then a password loses the password to the first
// reader's buffer — which presents as EOF on the second prompt and is
// thoroughly confusing. One reader, remembered.
var buffers = map[io.Reader]*bufio.Reader{}

func buffered(r io.Reader) *bufio.Reader {
	if b, ok := buffers[r]; ok {
		return b
	}
	b := bufio.NewReader(r)
	buffers[r] = b
	return b
}

func prompt(cmd *cobra.Command, label string) (string, error) {
	fmt.Fprint(cmd.ErrOrStderr(), label)
	line, err := buffered(cmd.InOrStdin()).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

// promptSecret reads without echoing.
//
// Falls back to a plain read when stdin is not a terminal, so a script can pipe
// a passphrase in — but it says so on stderr, because a secret that ends up in
// a log because someone did not realize it was being echoed is a secret that
// has leaked.
func promptSecret(cmd *cobra.Command, label string) (string, error) {
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		fmt.Fprint(cmd.ErrOrStderr(), label)
		raw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.ErrOrStderr())
		return string(raw), err
	}
	// Not a terminal, so there is nothing to turn echo off on. A script piping
	// a secret in is a legitimate thing to do; it is worth saying that the
	// value was not hidden, because a secret nobody realized was echoed is a
	// secret that has leaked into a log.
	fmt.Fprintln(cmd.ErrOrStderr(), "(reading from a pipe, so this value is not hidden)")
	return prompt(cmd, label)
}

func nameFromURL(raw string) string {
	trimmed := strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	if i := strings.LastIndex(trimmed, "/"); i >= 0 {
		trimmed = trimmed[i+1:]
	}
	if trimmed == "" {
		return "app"
	}
	return trimmed
}
