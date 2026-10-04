package cli

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// adapterCmd configures adapters from the command line, as the console's
// Adapters screen does — the API could always do it, and a capability only the
// API has is a gap in the other two surfaces (R-261).
func adapterCmd(client func() (*Client, error)) *cobra.Command {
	cmd := &cobra.Command{Use: "adapter", Short: "See and configure this installation's adapters"}

	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Show the adapters configured here",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out map[string]any
			if err := c.Do("GET", "/adapters", nil, &out); err != nil {
				return err
			}
			return printJSON(cmd.OutOrStdout(), out)
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "kinds",
		Short: "Show the kinds of adapter this build can run, and their settings",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			kinds, err := adapterKinds(c)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "KIND\tSETTINGS\tADVANCED\tWHAT IT IS")
			for _, k := range kinds {
				var basic, advanced []string
				for _, f := range k.Fields {
					key := f.Key
					if f.Credential {
						key += " (secret)"
					}
					if f.Required {
						key += "*"
					}
					if f.Advanced {
						advanced = append(advanced, key)
					} else {
						basic = append(basic, key)
					}
				}
				fmt.Fprintf(w, "%s/%s\t%s\t%s\t%s\n", k.Category, k.Kind, orDash(basic), orDash(advanced), k.Description)
			}
			_ = w.Flush()
			fmt.Fprintln(cmd.OutOrStdout(), "\n* required. A secret setting is asked for when you add the adapter, not typed on the command line.\n"+
				"Advanced settings have defaults most installations keep. `pando adapter add <category>/<kind> --help` lists each one's.")
			return nil
		},
	})

	var id, name string
	var set []string
	var isDefault bool
	add := &cobra.Command{
		Use:   "add <category>/<kind>",
		Short: "Add an adapter, or change one, by kind",
		Long: "Adds an adapter of a kind from `pando adapter kinds`. Ordinary settings go in --set KEY=VALUE;\n" +
			"a secret setting such as an API key is asked for without echoing it, so it never lands in\n" +
			"your shell history (piped in when stdin is not a terminal). Adding with an existing --id\n" +
			"changes that adapter. Pando loads adapters at startup: restart it afterwards.\n\n" +
			"With a kind named, --help lists its settings and their defaults, the advanced ones under\n" +
			"their own heading. Advanced settings take --set like any other.\n\n" +
			"  pando adapter add ai/anthropic --set model=claude-sonnet-5",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			category, kind, ok := strings.Cut(args[0], "/")
			if !ok || category == "" || kind == "" {
				return fmt.Errorf("say which kind as category/kind, for example ai/anthropic — `pando adapter kinds` lists them")
			}
			c, err := client()
			if err != nil {
				return err
			}
			kinds, err := adapterKinds(c)
			if err != nil {
				return err
			}
			var info *kindInfo
			for i := range kinds {
				if kinds[i].Category == category && kinds[i].Kind == kind {
					info = &kinds[i]
				}
			}
			if info == nil {
				return fmt.Errorf("this Pando has no %s adapter of kind %q — `pando adapter kinds` lists the ones it has", category, kind)
			}

			values := map[string]string{}
			for _, kv := range set {
				key, value, found := strings.Cut(kv, "=")
				if !found || key == "" {
					return fmt.Errorf("--set takes KEY=VALUE, and %q has no =", kv)
				}
				values[key] = value
			}

			config := map[string]any{}
			credentials := map[string]string{}
			for _, f := range info.Fields {
				if f.Credential {
					if _, typed := values[f.Key]; typed {
						return fmt.Errorf("%s is a secret: leave it off --set and Pando will ask for it", f.Key)
					}
					value, err := promptSecret(cmd, f.Label+" (empty to skip): ")
					if err != nil {
						return err
					}
					if value != "" {
						credentials[f.Key] = value
					}
					continue
				}
				raw, given := values[f.Key]
				delete(values, f.Key)
				if !given || raw == "" {
					if f.Required {
						return fmt.Errorf("%s needs %s: --set %s=…", args[0], f.Key, f.Key)
					}
					continue
				}
				switch f.Type {
				case "int":
					n, err := strconv.ParseInt(raw, 10, 64)
					if err != nil {
						return fmt.Errorf("%s is a whole number, and %q is not one", f.Key, raw)
					}
					config[f.Key] = n
				case "bool":
					b, err := strconv.ParseBool(raw)
					if err != nil {
						return fmt.Errorf("%s is true or false, and %q is neither", f.Key, raw)
					}
					config[f.Key] = b
				default:
					config[f.Key] = raw
				}
			}
			for key := range values {
				return fmt.Errorf("%s has no setting %q — `pando adapter kinds` lists its settings", args[0], key)
			}

			if id == "" {
				id = info.IDPrefix + info.Kind
			}
			if name == "" {
				name = info.Name
			}
			body := map[string]any{
				"id": id, "category": category, "kind": kind, "name": name,
				"config": config, "is_default": isDefault,
			}
			if len(credentials) > 0 {
				body["credentials"] = credentials
			}
			if err := c.Do("POST", "/adapters", body, nil); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Saved %s. Restart Pando to use it: pando restart\n", id)
			return nil
		},
	}
	add.Flags().StringVar(&id, "id", "", "the adapter's ID (default: the kind's usual prefix and name, e.g. ai_anthropic)")
	add.Flags().StringVar(&name, "name", "", "what the console calls it (default: the kind's name)")
	add.Flags().StringArrayVar(&set, "set", nil, "a setting, KEY=VALUE; repeat for more")
	add.Flags().BoolVar(&isDefault, "default", true, "make it the default adapter of its category")

	// Help for a named kind lists its settings after the flags, the basic ones
	// first and the advanced ones under their own heading, each with its
	// default. The settings come from the server, so without one this is the
	// ordinary help.
	usage := add.HelpFunc()
	add.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		usage(cmd, args)
		named := cmd.Flags().Args()
		if len(named) == 0 {
			return
		}
		category, kind, _ := strings.Cut(named[0], "/")
		c, err := client()
		if err != nil {
			return
		}
		kinds, err := adapterKinds(c)
		if err != nil {
			return
		}
		for i := range kinds {
			if kinds[i].Category == category && kinds[i].Kind == kind {
				printSettings(cmd.OutOrStdout(), &kinds[i])
			}
		}
	})
	cmd.AddCommand(add)
	return cmd
}

// printSettings lists a kind's settings for `pando adapter add <kind> --help`.
func printSettings(out io.Writer, k *kindInfo) {
	section := func(title string, advanced bool) {
		var rows []string
		for _, f := range k.Fields {
			if f.Advanced != advanced {
				continue
			}
			var notes []string
			switch {
			case f.Credential:
				notes = append(notes, "secret, asked for when you add it")
			case f.Required:
				notes = append(notes, "required")
			}
			if f.Default != "" {
				notes = append(notes, "default: "+f.Default)
			}
			row := "  " + f.Key + "\t" + f.Label
			if len(notes) > 0 {
				row += " (" + strings.Join(notes, "; ") + ")"
			}
			rows = append(rows, row)
		}
		if len(rows) == 0 {
			return
		}
		fmt.Fprintf(out, "\n%s:\n", title)
		w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, r := range rows {
			fmt.Fprintln(w, r)
		}
		_ = w.Flush()
	}
	fmt.Fprintf(out, "\n%s/%s: %s\n", k.Category, k.Kind, k.Name)
	section("Settings, each --set KEY=VALUE", false)
	section("Advanced settings, each left at its default unless set", true)
}

func orDash(keys []string) string {
	if len(keys) == 0 {
		return "-"
	}
	return strings.Join(keys, ", ")
}

type kindInfo struct {
	Category    string `json:"category"`
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Description string `json:"description"`
	IDPrefix    string `json:"id_prefix"`
	Fields      []struct {
		Key        string `json:"key"`
		Label      string `json:"label"`
		Type       string `json:"type"`
		Required   bool   `json:"required"`
		Credential bool   `json:"credential"`
		Advanced   bool   `json:"advanced"`
		Default    string `json:"default"`
	} `json:"fields"`
}

func adapterKinds(c *Client) ([]kindInfo, error) {
	var out struct {
		Kinds []kindInfo `json:"kinds"`
	}
	if err := c.Do("GET", "/adapters/kinds", nil, &out); err != nil {
		return nil, err
	}
	return out.Kinds, nil
}
