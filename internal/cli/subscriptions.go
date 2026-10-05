package cli

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// Event subscriptions from a terminal (issue #50, R-261): the catalog, the
// subscriptions themselves, their deliveries, and which of Pando's own
// notifications reach you.

type subscriptionView struct {
	ID                  string   `json:"id"`
	OwnerName           string   `json:"owner_name"`
	AppID               string   `json:"app_id"`
	AppName             string   `json:"app_name"`
	Events              []string `json:"events"`
	Destination         string   `json:"destination"`
	URL                 string   `json:"url"`
	AdapterID           string   `json:"adapter_id"`
	Description         string   `json:"description"`
	Enabled             bool     `json:"enabled"`
	DisabledReason      string   `json:"disabled_reason"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	SigningKey          string   `json:"signing_key"`
}

func (s subscriptionView) where() string {
	if s.Destination == "webhook" {
		return s.URL
	}
	return "notify:" + s.AdapterID
}

func (s subscriptionView) about() string {
	if s.AppID == "" {
		return "install-wide"
	}
	if s.AppName != "" {
		return s.AppName + " (" + s.AppID + ")"
	}
	return s.AppID
}

type deliveryView struct {
	ID             string `json:"id"`
	EventID        string `json:"event_id"`
	Event          string `json:"event"`
	Status         string `json:"status"`
	Attempts       int    `json:"attempts"`
	NextAttemptAt  string `json:"next_attempt_at"`
	LastStatusCode *int   `json:"last_status_code"`
	LastError      string `json:"last_error"`
	CreatedAt      string `json:"created_at"`
	AttemptLog     []struct {
		Attempt     int    `json:"attempt"`
		AttemptedAt string `json:"attempted_at"`
		StatusCode  *int   `json:"status_code"`
		Error       string `json:"error"`
		DurationMS  int    `json:"duration_ms"`
	} `json:"attempt_log"`
}

func code(c *int) string {
	if c == nil {
		return "-"
	}
	return fmt.Sprint(*c)
}

func eventsCmd(client func() (*Client, error)) *cobra.Command {
	return &cobra.Command{
		Use:   "events",
		Short: "List the events a subscription can name",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Events []struct {
					Name    string `json:"name"`
					Scope   string `json:"scope"`
					Summary string `json:"summary"`
				} `json:"events"`
			}
			if err := c.Do("GET", "/events", nil, &out); err != nil {
				return err
			}
			t := table(cmd.OutOrStdout(), "EVENT", "ABOUT", "WHAT HAPPENED")
			for _, e := range out.Events {
				fmt.Fprintf(t, "%s\t%s\t%s\n", e.Name, e.Scope, e.Summary)
			}
			return t.Flush()
		},
	}
}

func subscriptionsCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "subscriptions",
		Aliases: []string{"subscription", "subs"},
		Short:   "Send Pando's events to a webhook, Slack, Teams, Discord, email or ntfy",
	}

	var app string
	var everyone bool
	list := &cobra.Command{
		Use:   "list",
		Short: "Your subscriptions, or everybody's with --everyone",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			q := url.Values{}
			if app != "" {
				q.Set("app_id", app)
			}
			if everyone {
				q.Set("everyone", "true")
			}
			path := "/subscriptions"
			if len(q) > 0 {
				path += "?" + q.Encode()
			}
			var out struct {
				Subscriptions []subscriptionView `json:"subscriptions"`
			}
			if err := c.Do("GET", path, nil, &out); err != nil {
				return err
			}
			if len(out.Subscriptions) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No subscriptions. Make one with `pando subscriptions create`.")
				return nil
			}
			t := table(cmd.OutOrStdout(), "ID", "ABOUT", "EVENTS", "SENT TO", "ON", "OWNER")
			for _, s := range out.Subscriptions {
				on := "yes"
				if !s.Enabled {
					on = "no"
				}
				fmt.Fprintf(t, "%s\t%s\t%s\t%s\t%s\t%s\n", s.ID, s.about(), strings.Join(s.Events, ","), s.where(), on, s.OwnerName)
			}
			return t.Flush()
		},
	}
	list.Flags().StringVar(&app, "app", "", "Only subscriptions about this app (its ID)")
	list.Flags().BoolVar(&everyone, "everyone", false, "Every person's subscriptions (needs install.events.manage)")
	cmd.AddCommand(list)

	var createApp, createURL, createAdapter, createDescription string
	var createEvents []string
	create := &cobra.Command{
		Use:   "create",
		Short: "Subscribe to events on one app, or install-wide",
		Long: "Subscribe to events and send them to a webhook (--url) or through a notification adapter (--adapter).\n\n" +
			"--events takes event names, prefixes such as deploy.*, or * for everything; `pando events` lists them.\n" +
			"Without --app the subscription is install-wide, which needs install.events.manage.\n\n" +
			"A webhook's signing key is printed once. Keep it: Pando signs every delivery with it and cannot show it again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if (createURL == "") == (createAdapter == "") {
				return fmt.Errorf("give exactly one of --url (a webhook) or --adapter (a notification adapter's ID, from `pando adapters list`)")
			}
			c, err := client()
			if err != nil {
				return err
			}
			body := map[string]any{"events": createEvents, "app_id": createApp, "description": createDescription}
			if createURL != "" {
				body["destination"], body["url"] = "webhook", createURL
			} else {
				body["destination"], body["adapter_id"] = "notify", createAdapter
			}
			var out subscriptionView
			if err := c.Do("POST", "/subscriptions", body, &out); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "Created %s: %s, sent to %s.\n", out.ID, out.about(), out.where())
			if out.SigningKey != "" {
				fmt.Fprintf(w, "\nSigning key (shown once):\n\n  %s\n\n", out.SigningKey)
				fmt.Fprintln(w, "Check each delivery's Pando-Signature header against it; docs/events.md shows how.")
			}
			return nil
		},
	}
	create.Flags().StringSliceVar(&createEvents, "events", nil, "Events to send: names, prefixes such as deploy.*, or *")
	create.Flags().StringVar(&createApp, "app", "", "The app's ID; omit for install-wide")
	create.Flags().StringVar(&createURL, "url", "", "Post each event to this webhook URL")
	create.Flags().StringVar(&createAdapter, "adapter", "", "Send each event through this notification adapter")
	create.Flags().StringVar(&createDescription, "description", "", "What this subscription is for")
	_ = create.MarkFlagRequired("events")
	cmd.AddCommand(create)

	cmd.AddCommand(&cobra.Command{
		Use:   "show <subscription-id>",
		Short: "One subscription",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var s subscriptionView
			if err := c.Do("GET", "/subscriptions/"+url.PathEscape(args[0]), nil, &s); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s\n  About:    %s\n  Events:   %s\n  Sent to:  %s\n  Owner:    %s\n",
				s.ID, s.about(), strings.Join(s.Events, ", "), s.where(), s.OwnerName)
			if s.Description != "" {
				fmt.Fprintf(w, "  For:      %s\n", s.Description)
			}
			if s.Enabled {
				fmt.Fprintln(w, "  On:       yes")
			} else {
				fmt.Fprintf(w, "  On:       no. %s\n", s.DisabledReason)
			}
			if s.ConsecutiveFailures > 0 {
				fmt.Fprintf(w, "  Failing:  the last %d attempts failed\n", s.ConsecutiveFailures)
			}
			return nil
		},
	})

	var updEvents []string
	var updURL, updAdapter, updDescription string
	var enable, disable bool
	update := &cobra.Command{
		Use:   "update <subscription-id>",
		Short: "Change a subscription, or turn it on or off",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			body := map[string]any{}
			if cmd.Flags().Changed("events") {
				body["events"] = updEvents
			}
			if cmd.Flags().Changed("url") {
				body["url"] = updURL
			}
			if cmd.Flags().Changed("adapter") {
				body["adapter_id"] = updAdapter
			}
			if cmd.Flags().Changed("description") {
				body["description"] = updDescription
			}
			if enable && disable {
				return fmt.Errorf("give --enable or --disable, not both")
			}
			if enable {
				body["enabled"] = true
			}
			if disable {
				body["enabled"] = false
			}
			if len(body) == 0 {
				return fmt.Errorf("nothing to change: give --events, --url, --adapter, --description, --enable or --disable")
			}
			c, err := client()
			if err != nil {
				return err
			}
			var s subscriptionView
			if err := c.Do("PATCH", "/subscriptions/"+url.PathEscape(args[0]), body, &s); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Updated %s.\n", s.ID)
			return nil
		},
	}
	update.Flags().StringSliceVar(&updEvents, "events", nil, "Replace the events it sends")
	update.Flags().StringVar(&updURL, "url", "", "A new webhook URL")
	update.Flags().StringVar(&updAdapter, "adapter", "", "A new notification adapter")
	update.Flags().StringVar(&updDescription, "description", "", "A new description")
	update.Flags().BoolVar(&enable, "enable", false, "Turn it on, clearing its record of failures")
	update.Flags().BoolVar(&disable, "disable", false, "Turn it off")
	cmd.AddCommand(update)

	cmd.AddCommand(&cobra.Command{
		Use:   "delete <subscription-id>",
		Short: "Delete a subscription and its delivery log",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if err := c.Do("DELETE", "/subscriptions/"+url.PathEscape(args[0]), nil, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted %s.\n", args[0])
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "rotate-key <subscription-id>",
		Short: "Replace a webhook's signing key and print the new one, once",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out subscriptionView
			if err := c.Do("POST", "/subscriptions/"+url.PathEscape(args[0])+"/signing-key", map[string]any{}, &out); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "New signing key for %s (shown once):\n\n  %s\n", out.ID, out.SigningKey)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "test <subscription-id>",
		Short: "Send a test event to one subscription",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var d deliveryView
			if err := c.Do("POST", "/subscriptions/"+url.PathEscape(args[0])+"/test", map[string]any{}, &d); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Queued %s. See how it went with `pando subscriptions delivery %s %s`.\n", d.ID, args[0], d.ID)
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "deliveries <subscription-id>",
		Short: "A subscription's recent deliveries",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out struct {
				Deliveries []deliveryView `json:"deliveries"`
			}
			if err := c.Do("GET", "/subscriptions/"+url.PathEscape(args[0])+"/deliveries", nil, &out); err != nil {
				return err
			}
			if len(out.Deliveries) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Nothing has been sent yet.")
				return nil
			}
			t := table(cmd.OutOrStdout(), "DELIVERY", "EVENT", "STATUS", "ATTEMPTS", "LAST ANSWER", "LAST ERROR")
			for _, d := range out.Deliveries {
				fmt.Fprintf(t, "%s\t%s\t%s\t%d\t%s\t%s\n", d.ID, d.Event, d.Status, d.Attempts, code(d.LastStatusCode), d.LastError)
			}
			return t.Flush()
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "delivery <subscription-id> <delivery-id>",
		Short: "One delivery and every attempt at it",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var d deliveryView
			if err := c.Do("GET", "/subscriptions/"+url.PathEscape(args[0])+"/deliveries/"+url.PathEscape(args[1]), nil, &d); err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "%s: %s (%s), %s after %d attempts\n", d.ID, d.Event, d.EventID, d.Status, d.Attempts)
			if d.Status == "pending" && d.NextAttemptAt != "" {
				fmt.Fprintf(w, "Next attempt: %s\n", d.NextAttemptAt)
			}
			if len(d.AttemptLog) > 0 {
				t := table(w, "ATTEMPT", "AT", "ANSWER", "TOOK", "ERROR")
				for _, a := range d.AttemptLog {
					fmt.Fprintf(t, "%d\t%s\t%s\t%dms\t%s\n", a.Attempt, a.AttemptedAt, code(a.StatusCode), a.DurationMS, a.Error)
				}
				return t.Flush()
			}
			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "redeliver <subscription-id> <delivery-id>",
		Short: "Send a delivery again now",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			path := "/subscriptions/" + url.PathEscape(args[0]) + "/deliveries/" + url.PathEscape(args[1]) + "/redeliver"
			if err := c.Do("POST", path, map[string]any{}, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Queued %s again.\n", args[1])
			return nil
		},
	})
	return cmd
}

// notificationsCmd is which of Pando's own notifications reach you, and on
// which channel (R-373).
func notificationsCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "notifications", Short: "Choose which of Pando's notifications reach you, and where"}

	var set []string
	prefs := &cobra.Command{
		Use:   "preferences",
		Short: "Show your notification preferences, or change them with --set",
		Long: "Show which of Pando's own notifications reach you on each channel.\n\n" +
			"--set kind:channel=on|off changes one, and may be repeated, for example:\n\n" +
			"  pando notifications preferences --set app_shared:ntf_smtp=on --set deploy_approval:ntf_console=off",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var view struct {
				Kinds []struct {
					Kind  string `json:"kind"`
					Label string `json:"label"`
				} `json:"kinds"`
				Channels []struct {
					ID   string `json:"id"`
					Kind string `json:"kind"`
				} `json:"channels"`
				Choices []struct {
					Kind    string `json:"kind"`
					Channel string `json:"channel"`
					Enabled bool   `json:"enabled"`
				} `json:"choices"`
			}
			if len(set) > 0 {
				var choices []map[string]any
				for _, s := range set {
					key, value, ok := strings.Cut(s, "=")
					kind, channel, ok2 := strings.Cut(key, ":")
					if !ok || !ok2 || (value != "on" && value != "off") {
						return fmt.Errorf("%q is not kind:channel=on or kind:channel=off, such as app_shared:ntf_smtp=on", s)
					}
					choices = append(choices, map[string]any{"kind": kind, "channel": channel, "enabled": value == "on"})
				}
				if err := c.Do("PUT", "/notification-preferences", map[string]any{"choices": choices}, &view); err != nil {
					return err
				}
			} else if err := c.Do("GET", "/notification-preferences", nil, &view); err != nil {
				return err
			}

			on := map[string]bool{}
			for _, ch := range view.Choices {
				on[ch.Kind+"\x00"+ch.Channel] = ch.Enabled
			}
			headers := []string{"NOTIFICATION", "KIND"}
			for _, ch := range view.Channels {
				headers = append(headers, strings.ToUpper(ch.ID))
			}
			t := table(cmd.OutOrStdout(), headers...)
			for _, k := range view.Kinds {
				row := []string{k.Label, k.Kind}
				for _, ch := range view.Channels {
					v := "off"
					if on[k.Kind+"\x00"+ch.ID] {
						v = "on"
					}
					row = append(row, v)
				}
				fmt.Fprintln(t, strings.Join(row, "\t"))
			}
			return t.Flush()
		},
	}
	prefs.Flags().StringArrayVar(&set, "set", nil, "kind:channel=on|off, repeatable")
	cmd.AddCommand(prefs)
	return cmd
}
