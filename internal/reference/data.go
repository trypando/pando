package reference

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// The machine-readable reference: docs/cli.json, mcp.json, adapters.json and
// config.json, for a site that renders Pando's documentation rather than
// reading the Markdown. Built from the same things the Markdown is — the cobra
// tree, the MCP tool list, each adapter package's KindInfo, Config — and held
// to them the same way, by `make reference-check`.
//
// Deterministic: sorted, with nothing in it that differs between builds, so a
// diff means the code changed.

// CLIData is docs/cli.json.
type CLIData struct {
	Commands []CLICommand `json:"commands"`
}

// CLICommand is one command as cobra has it.
type CLICommand struct {
	Path           string    `json:"path"`
	Use            string    `json:"use"`
	Short          string    `json:"short"`
	Long           string    `json:"long"`
	Example        string    `json:"example"`
	Aliases        []string  `json:"aliases"`
	Hidden         bool      `json:"hidden"`
	Deprecated     string    `json:"deprecated"`
	Flags          []CLIFlag `json:"flags"`
	InheritedFlags []CLIFlag `json:"inherited_flags"`
	Subcommands    []string  `json:"subcommands"`
}

// CLIFlag is one flag. Default is as cobra renders it in --help.
type CLIFlag struct {
	Name      string `json:"name"`
	Shorthand string `json:"shorthand"`
	Type      string `json:"type"`
	Default   string `json:"default"`
	Usage     string `json:"usage"`
	Required  bool   `json:"required"`
}

// CLI walks the command tree under root. Hidden commands — plumbing Pando
// starts itself — and cobra's own help and completion commands are left out,
// as cli.md leaves them out.
func CLI(root *cobra.Command) CLIData {
	var out []CLICommand
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		cmd := CLICommand{
			Path:           c.CommandPath(),
			Use:            c.Use,
			Short:          c.Short,
			Long:           c.Long,
			Example:        c.Example,
			Aliases:        append([]string{}, c.Aliases...),
			Deprecated:     c.Deprecated,
			Flags:          flags(c.LocalFlags()),
			InheritedFlags: flags(c.InheritedFlags()),
			Subcommands:    []string{},
		}
		for _, child := range c.Commands() {
			if !documented(child) {
				continue
			}
			cmd.Subcommands = append(cmd.Subcommands, child.CommandPath())
			walk(child)
		}
		sort.Strings(cmd.Subcommands)
		out = append(out, cmd)
	}
	walk(root)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return CLIData{Commands: out}
}

func documented(c *cobra.Command) bool {
	return !c.Hidden && c.Name() != "help" && c.Name() != "completion"
}

// flags lists a flag set sorted by name. --help is left out: cobra adds it to
// whichever command runs, so whether it is there depends on what ran.
func flags(set *pflag.FlagSet) []CLIFlag {
	out := []CLIFlag{}
	set.VisitAll(func(f *pflag.Flag) {
		if f.Hidden || f.Name == "help" {
			return
		}
		required := false
		if v, ok := f.Annotations[cobra.BashCompOneRequiredFlag]; ok && len(v) > 0 && v[0] == "true" {
			required = true
		}
		out = append(out, CLIFlag{
			Name:      f.Name,
			Shorthand: f.Shorthand,
			Type:      f.Value.Type(),
			Default:   f.DefValue,
			Usage:     f.Usage,
			Required:  required,
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// MCPData is docs/mcp.json.
type MCPData struct {
	Tools []MCPTool `json:"tools"`
}

// MCPTool is one tool as tools/list announces it.
type MCPTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// MCP is the server's tool list, sorted by name.
func MCP() MCPData {
	tools := mcp.Catalog()
	out := make([]MCPTool, 0, len(tools))
	for _, t := range tools {
		out = append(out, MCPTool{Name: t.Name, Description: t.Description, InputSchema: t.Schema})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return MCPData{Tools: out}
}

// AdaptersData is docs/adapters.json.
type AdaptersData struct {
	Categories []AdapterCategory `json:"categories"`
}

// AdapterCategory is one category's kinds, sorted by kind.
type AdapterCategory struct {
	Category api.Category   `json:"category"`
	Kinds    []api.KindInfo `json:"kinds"`
}

// Adapters groups kinds by category, in api.Categories' order. A kind in a
// category that list does not name is an error, so a new category cannot be
// left out of the reference by forgetting it there.
func Adapters(kinds []api.KindInfo) (AdaptersData, error) {
	byCat := map[api.Category][]api.KindInfo{}
	for _, k := range kinds {
		byCat[k.Category] = append(byCat[k.Category], k)
	}
	out := AdaptersData{Categories: []AdapterCategory{}}
	for _, c := range api.Categories {
		ks := byCat[c]
		delete(byCat, c)
		if len(ks) == 0 {
			continue
		}
		sort.Slice(ks, func(i, j int) bool { return ks[i].Kind < ks[j].Kind })
		out.Categories = append(out.Categories, AdapterCategory{Category: c, Kinds: ks})
	}
	for c := range byCat {
		return AdaptersData{}, fmt.Errorf("adapter category %q is not in api.Categories", c)
	}
	return out, nil
}

// ConfigData is docs/config.json.
type ConfigData struct {
	Settings []config.SettingDoc `json:"settings"`
}

// Config is every startup setting, then every host policy field, which can
// also be fixed at startup as PANDO_POLICY_<FIELD> or under policy: in the
// config file (R-271).
func Config() ConfigData {
	settings := config.Reference()
	defaults := map[string]any{}
	raw, _ := json.Marshal(policy.Default())
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // 1000000, not 1e+06
	_ = dec.Decode(&defaults)
	for _, key := range policy.Fields() {
		words, _ := policy.FieldType(key)
		typ := map[string]string{"list": "list", "true or false": "bool", "whole number": "int"}[words]
		if typ == "" {
			typ = "string"
		}
		settings = append(settings, config.SettingDoc{
			Env:         config.PolicyEnvPrefix + strings.ToUpper(key),
			Key:         "policy." + key,
			Type:        typ,
			Default:     policyDefault(typ, defaults[key]),
			Description: policy.Describe(key),
			Group:       "policy",
		})
	}
	return ConfigData{Settings: settings}
}

// policyDefault writes a policy default as its variable takes it: a list
// comma-separated, as PANDO_POLICY_* reads one.
func policyDefault(typ string, v any) string {
	switch v := v.(type) {
	case nil:
		switch typ {
		case "bool":
			return "false"
		case "int":
			return "0"
		}
		return ""
	case []any:
		parts := make([]string, 0, len(v))
		for _, p := range v {
			parts = append(parts, fmt.Sprint(p))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprint(v)
}

// ErrorsData is docs/errors.json.
type ErrorsData struct {
	Errors []ErrorDoc `json:"errors"`
}

// ErrorDoc is one error code: what a client branches on, the HTTP status it
// comes with, and what it means. Status is omitted for a code the error
// package gives none.
type ErrorDoc struct {
	Code    errs.Code `json:"code"`
	Status  int       `json:"status,omitempty"`
	Meaning string    `json:"meaning"`
}

// Errors is errs.Catalog, sorted by code rather than by status, so a reader
// looking a code up finds it where the alphabet says.
func Errors() ErrorsData {
	out := []ErrorDoc{}
	for _, e := range errs.Catalog() {
		out = append(out, ErrorDoc{Code: e.Code, Status: e.Status, Meaning: e.Meaning})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return ErrorsData{Errors: out}
}

// JSON renders a reference file: indented, with a trailing newline, and with
// <, > and & left as they are — the text is read by people as often as by
// programs.
func JSON(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
