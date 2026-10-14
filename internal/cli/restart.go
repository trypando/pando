package cli

import (
	"errors"
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

// restartCmd restarts the server, as the console's Restart button does — so a
// saved adapter can be put into effect without a shell on the host (R-253,
// R-261).
func restartCmd(client func() (*Client, error)) *cobra.Command {
	var wait time.Duration
	cmd := &cobra.Command{
		Use:   "restart",
		Short: "Restart Pando, loading its adapters and configuration file again",
		Long: "Restart Pando. Requests in flight finish first, and apps behind Pando are unreachable " +
			"for the seconds it takes. Adapters and the configuration file are read again; " +
			"environment variables are not.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			before, err := startedAt(c)
			if err != nil {
				return err
			}
			if err := c.Do("POST", "/restart", nil, nil); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if wait <= 0 {
				fmt.Fprintln(out, "Pando is restarting.")
				return nil
			}
			fmt.Fprint(out, "Restarting Pando…")

			// Back when it answers with a different start time. Errors in
			// between are the restart itself and are expected.
			deadline := time.Now().Add(wait)
			for time.Now().Before(deadline) {
				time.Sleep(time.Second)
				if now, err := startedAt(c); err == nil && now != before {
					fmt.Fprintln(out, " done.")
					return nil
				}
			}
			fmt.Fprintln(out)
			return errors.New("Pando has not come back after " + wait.String() +
				". Check its logs where it runs, for example docker compose logs pando")
		},
	}
	cmd.Flags().DurationVar(&wait, "wait", 90*time.Second, "how long to wait for Pando to come back; 0 returns at once")
	return cmd
}

// startedAt is when the server process started, from the adapters list.
func startedAt(c *Client) (string, error) {
	var out struct {
		StartedAt string `json:"started_at"`
	}
	if err := c.Do("GET", adaptersPath, nil, &out); err != nil {
		return "", err
	}
	return out.StartedAt, nil
}
