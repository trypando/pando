// Package reference assembles Pando's own documentation from Pando.
//
// R-261 says the API is the product and that the console, the CLI and MCP are
// clients of it with no capability it lacks. A description of those three that
// is written by hand is a fourth artifact with its own drift: it is correct on
// the day it is written and wrong on the day after a route is added, and the
// people it is wrong for are the ones automating against it.
//
// So it is built from the things themselves. The CLI half is walked out of the
// cobra tree that `pando` actually runs; the MCP half is the tool list the MCP
// server actually announces; the error codes are the catalog `errs` is tested
// against. The API half is the one part that cannot be read from the router —
// chi knows a route's method and pattern but not what it is for — so httpapi
// keeps a table of summaries beside its routes, and a test walks the real
// router and fails when the two disagree in either direction.
//
// The endpoint serving this is the console's source, and `cmd/gen-reference`
// writes the same document to `docs/`. One assembly, three readers.
package reference

import (
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/trypando/pando/internal/cli"
	"github.com/trypando/pando/internal/core/events"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// Document is everything a person or an agent needs to use Pando without the
// console.
type Document struct {
	API     API            `json:"api"`
	CLI     []Command      `json:"cli"`
	MCP     []mcp.ToolDoc  `json:"mcp"`
	Errors  []errs.CodeDoc `json:"errors"`
	Events  []events.Def   `json:"events"`
	Connect Connect        `json:"connect"`
	Install Install        `json:"install"`
}

// Connect is how a machine authenticates, named from the constants the code
// reads rather than from what somebody wrote down once.
type Connect struct {
	ServerEnv string `json:"server_env"`
	TokenEnv  string `json:"token_env"`
	LoginCmd  string `json:"login_cmd"`
	MCPCmd    string `json:"mcp_cmd"`
}

// API describes the HTTP surface.
type API struct {
	BasePath string  `json:"base_path"`
	Auth     []Auth  `json:"auth"`
	Routes   []Route `json:"routes"`
}

// Auth is one way to authenticate.
type Auth struct {
	Name        string `json:"name"`
	How         string `json:"how"`
	Description string `json:"description"`
}

// Route is one endpoint.
//
// Verb is the authorization verb the endpoint asks for, in Pando's own
// vocabulary (design 06 §5) — empty where the endpoint asks for nothing beyond
// being signed in, because "no verb" is an answer somebody needs.
type Route struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Group   string `json:"group"`
	Summary string `json:"summary"`
	Verb    string `json:"verb,omitempty"`
}

// Command is one CLI command, as cobra has it.
type Command struct {
	Name     string    `json:"name"`
	Use      string    `json:"use"`
	Summary  string    `json:"summary"`
	Details  string    `json:"details,omitempty"`
	Flags    []Flag    `json:"flags,omitempty"`
	Children []Command `json:"children,omitempty"`
}

// Flag is one flag on a command.
type Flag struct {
	Name        string `json:"name"`
	Shorthand   string `json:"shorthand,omitempty"`
	Description string `json:"description"`
	Default     string `json:"default,omitempty"`
}

// Build assembles the document. Routes come from the caller because only the
// HTTP layer knows them.
func Build(routes []Route) Document {
	return Document{
		API: API{
			BasePath: "/api/v1",
			Auth: []Auth{
				{
					Name: "Token",
					How:  "Authorization: Bearer <token>",
					Description: "A token minted from the console or with `pando token create`. " +
						"A delegated token acts as the person who made it and is bounded by their " +
						"live grants (R-058, R-059). A service token is its own principal and " +
						"holds only what has been shared with it (R-060).",
				},
				{
					Name: "Session",
					How:  "POST /api/v1/sessions, then the session cookie",
					Description: "How the console authenticates. Cookies in Pando's namespace " +
						"never reach an app (R-173).",
				},
			},
			Routes: routes,
		},
		CLI:    Commands(),
		MCP:    mcp.Catalog(),
		Errors: errs.Catalog(),
		Events: events.Catalog(),
		Connect: Connect{
			ServerEnv: cli.EnvServer,
			TokenEnv:  cli.EnvToken,
			LoginCmd:  "pando login <server-url>",
			MCPCmd:    "pando mcp",
		},
		Install: install(),
	}
}

// Commands walks the CLI's own cobra tree.
func Commands() []Command {
	roots := cli.Commands()
	out := make([]Command, 0, len(roots))
	for _, c := range roots {
		out = append(out, describe(c, ""))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// nonEmpty drops blank parts before joining a command line.
func nonEmpty(parts ...string) []string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			out = append(out, strings.TrimSpace(p))
		}
	}
	return out
}

func describe(c *cobra.Command, prefix string) Command {
	name := strings.TrimSpace(prefix + " " + c.Name())

	doc := Command{
		Name: name,
		// Joined rather than concatenated: a root command has no prefix, and
		// gluing an empty one in left "pando  app" in the generated page.
		Use:     strings.Join(nonEmpty("pando", prefix, c.Use), " "),
		Summary: c.Short,
		Details: c.Long,
	}

	c.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		doc.Flags = append(doc.Flags, Flag{
			Name:        f.Name,
			Shorthand:   f.Shorthand,
			Description: f.Usage,
			Default:     f.DefValue,
		})
	})

	for _, child := range c.Commands() {
		if child.Hidden || child.Name() == "help" || child.Name() == "completion" {
			continue
		}
		doc.Children = append(doc.Children, describe(child, name))
	}
	return doc
}
