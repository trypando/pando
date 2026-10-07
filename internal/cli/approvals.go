package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

// approvalsCmd is deploy approval from a terminal (R-154, R-261): what is
// waiting, and approving or rejecting it. Its own command rather than
// subcommands of `pando deploy`, which takes an app or a directory as its
// argument — an app or a directory called "approve" would be ambiguous.
func approvalsCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "approvals", Short: "See and decide deploys waiting for approval"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Deploys waiting for approval on the apps you can see",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Approvals []waitingDeploy `json:"approvals"`
			}
			err = allPages(c, "/approvals", func(get func(any) error) (string, error) {
				var page struct {
					Approvals  []waitingDeploy `json:"approvals"`
					NextCursor string          `json:"next_cursor"`
				}
				if err := get(&page); err != nil {
					return "", err
				}
				out.Approvals = append(out.Approvals, page.Approvals...)
				return page.NextCursor, nil
			})
			if err != nil {
				return err
			}
			if len(out.Approvals) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing is waiting for approval.")
				return nil
			}
			t := table(cmd.OutOrStdout(), "APP", "DEPLOY", "APPROVALS", "EXPIRES", "YOU MAY DECIDE", "WHY")
			for _, d := range out.Approvals {
				approved := 0
				for _, a := range d.Approvals {
					if a.Decision == "approve" {
						approved++
					}
				}
				expires := "never"
				if d.ApprovalExpiresAt != "" {
					expires = d.ApprovalExpiresAt
				}
				decide := "no"
				if d.CanDecide {
					decide = "yes"
				}
				why := make([]string, 0, len(d.ApprovalReasons))
				for _, r := range d.ApprovalReasons {
					why = append(why, r.Reason)
				}
				fmt.Fprintf(t, "%s (%s)\t%s\t%d of %d\t%s\t%s\t%s\n",
					d.AppName, d.AppID, d.ID, approved, d.ApprovalsRequired, expires, decide, strings.Join(why, ", "))
			}
			return t.Flush()
		},
	})

	cmd.AddCommand(decideCmd(client, "approve",
		"Approve a deploy waiting for approval; the last approval it needs starts it"))
	cmd.AddCommand(decideCmd(client, "reject",
		"Reject a deploy waiting for approval, which ends the request"))
	return cmd
}

// waitingDeploy is the part of a deployment these commands print.
type waitingDeploy struct {
	ID                string `json:"id"`
	AppID             string `json:"app_id"`
	AppName           string `json:"app_name"`
	Status            string `json:"status"`
	ApprovalsRequired int    `json:"approvals_required"`
	ApprovalExpiresAt string `json:"approval_expires_at"`
	ApprovalReasons   []struct {
		Reason  string `json:"reason"`
		Message string `json:"message"`
	} `json:"approval_reasons"`
	Approvals []struct {
		Decision string `json:"decision"`
	} `json:"approvals"`
	CanDecide bool `json:"can_decide"`
}

func decideCmd(client func() (*Client, error), action, short string) *cobra.Command {
	var comment string
	cmd := &cobra.Command{
		Use:   action + " <app> <deployment>",
		Short: short,
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			body := map[string]any{}
			if comment != "" {
				body["comment"] = comment
			}
			var dep waitingDeploy
			if err := c.Do("POST", "/apps/"+args[0]+"/deployments/"+args[1]+"/"+action, body, &dep); err != nil {
				return err
			}
			switch dep.Status {
			case "awaiting_approval":
				approved := 0
				for _, a := range dep.Approvals {
					if a.Decision == "approve" {
						approved++
					}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Approved. It has %d of the %d approvals it needs, so it is still waiting.\n",
					approved, dep.ApprovalsRequired)
			case "rejected":
				fmt.Fprintln(cmd.OutOrStdout(), "Rejected. The deploy will not run.")
			default:
				fmt.Fprintf(cmd.OutOrStdout(), "Approved. Deploying; watch it with `pando logs %s -f`.\n", args[0])
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&comment, "comment", "", "a note recorded with the decision")
	return cmd
}

// printAwaiting says that a deploy is waiting for approval, why, and how it
// gets approved.
func printAwaiting(w io.Writer, appID string, dep waitingDeploy) {
	fmt.Fprintf(w, "This deploy needs approval before it runs (%d approval(s)).\n", dep.ApprovalsRequired)
	for _, r := range dep.ApprovalReasons {
		fmt.Fprintf(w, "  %s\n", r.Message)
	}
	fmt.Fprintf(w, "Someone who may approve it can run `pando approvals approve %s %s`.\n", appID, dep.ID)
}
