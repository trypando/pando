package reference

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/core/policy"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/mcp"
)

// traceID matches a requirement (R-059) or open-decision (O-12) ID, which mean
// nothing to the people the reference is written for.
var traceID = regexp.MustCompile(`\b[RO]-\d+`)

// TestR261_TheCLIReferenceWalksTheTreeAsCobraHasIt asserts R-261 for
// docs/cli.json: every command a person can run is listed, root first, with
// its own flags and the ones it inherits, and hidden plumbing is not.
func TestR261_TheCLIReferenceWalksTheTreeAsCobraHasIt(t *testing.T) {
	root := &cobra.Command{Use: "pando", Short: "Root"}
	root.PersistentFlags().String("server", "", "Pando server URL")
	app := &cobra.Command{Use: "app", Short: "Apps", Aliases: []string{"apps"}}
	add := &cobra.Command{Use: "add <name>", Short: "Add one", Example: "pando app add web", Run: func(*cobra.Command, []string) {}}
	add.Flags().StringP("image", "i", "", "an image")
	add.Flags().Int("port", 8080, "a port")
	require.NoError(t, add.MarkFlagRequired("image"))
	add.Flags().String("secret-flag", "", "hidden")
	require.NoError(t, add.Flags().MarkHidden("secret-flag"))
	plumbing := &cobra.Command{Use: "plumbing", Hidden: true, Run: func(*cobra.Command, []string) {}}
	app.AddCommand(add)
	root.AddCommand(app, plumbing)
	root.InitDefaultHelpCmd()

	data := CLI(root)
	paths := make([]string, 0, len(data.Commands))
	for _, c := range data.Commands {
		paths = append(paths, c.Path)
	}
	require.Equal(t, []string{"pando", "pando app", "pando app add"}, paths)
	require.Equal(t, []string{"pando app"}, data.Commands[0].Subcommands)
	require.Equal(t, []string{"apps"}, data.Commands[1].Aliases)

	got := data.Commands[2]
	require.Equal(t, "add <name>", got.Use)
	require.Equal(t, "pando app add web", got.Example)
	require.Equal(t, []CLIFlag{
		{Name: "image", Shorthand: "i", Type: "string", Usage: "an image", Required: true},
		{Name: "port", Type: "int", Default: "8080", Usage: "a port"},
	}, got.Flags)
	require.Equal(t, []CLIFlag{{Name: "server", Type: "string", Usage: "Pando server URL"}}, got.InheritedFlags)
	require.Empty(t, got.Subcommands)
	require.NotNil(t, got.Subcommands, "an empty list, not null")
}

// TestR261_TheMCPReferenceIsTheToolList asserts R-261 for docs/mcp.json: the
// tools are the ones the server announces, each with its input schema.
func TestR261_TheMCPReferenceIsTheToolList(t *testing.T) {
	data := MCP()
	require.Len(t, data.Tools, len(mcp.Catalog()))
	for i, tool := range data.Tools {
		require.NotEmpty(t, tool.Description, tool.Name)
		require.Equal(t, "object", tool.InputSchema["type"], tool.Name)
		// An agent and the person reading docs/mcp.md see this text; an R- or
		// O- ID means nothing to either.
		text, err := json.Marshal(tool)
		require.NoError(t, err)
		require.NotRegexp(t, traceID, string(text), tool.Name)
		if i > 0 {
			require.Less(t, data.Tools[i-1].Name, tool.Name)
		}
	}
}

// TestR252_TheAdapterReferenceListsCategoriesInOrder asserts R-252 for
// docs/adapters.json: categories in the requirement's order, kinds sorted, and
// a category the list does not name refused rather than dropped.
func TestR252_TheAdapterReferenceListsCategoriesInOrder(t *testing.T) {
	data, err := Adapters([]api.KindInfo{
		{Category: api.CategoryRuntime, Kind: "kubernetes"},
		{Category: api.CategoryIdentity, Kind: "saml"},
		{Category: api.CategoryRuntime, Kind: "docker"},
		{Category: api.CategoryIdentity, Kind: "oidc"},
	})
	require.NoError(t, err)
	require.Len(t, data.Categories, 2)
	require.Equal(t, api.CategoryIdentity, data.Categories[0].Category)
	require.Equal(t, "oidc", data.Categories[0].Kinds[0].Kind)
	require.Equal(t, api.CategoryRuntime, data.Categories[1].Category)
	require.Equal(t, "docker", data.Categories[1].Kinds[0].Kind)

	_, err = Adapters([]api.KindInfo{{Category: "telepathy", Kind: "x"}})
	require.ErrorContains(t, err, "telepathy")
}

// TestR271_EveryPolicySettingIsInTheConfigReference asserts R-271 for
// docs/config.json: each host policy field, which can be fixed at startup, is
// listed with the variable that fixes it and what it does.
func TestR271_EveryPolicySettingIsInTheConfigReference(t *testing.T) {
	byKey := map[string]bool{}
	for _, s := range Config().Settings {
		require.NotEmpty(t, s.Description, s.Key)
		require.NotRegexp(t, traceID, s.Description, "%s's description is read by users: no requirement or decision IDs", s.Key)
		byKey[s.Key] = true
		switch s.Key {
		case "policy.min_security_score":
			require.Equal(t, "PANDO_POLICY_MIN_SECURITY_SCORE", s.Env)
			require.Equal(t, "int", s.Type)
			require.Equal(t, "0", s.Default)
		case "policy.allow_anonymous_grants":
			require.Equal(t, "bool", s.Type)
			require.Equal(t, "true", s.Default)
		case "policy.agent_disabled_verbs":
			require.Equal(t, "list", s.Type)
			require.Contains(t, s.Default, "app.exec,")
		}
	}
	for _, key := range policy.Fields() {
		require.True(t, byKey["policy."+key], key)
	}
	require.True(t, byKey["server.external_url"])
}

// The files are read by people as well as programs: <, > and & stay as they
// are, and each ends in a newline.
func TestReferenceJSONIsReadable(t *testing.T) {
	body, err := JSON(map[string]string{"use": "add <name> & more"})
	require.NoError(t, err)
	require.Equal(t, "{\n  \"use\": \"add <name> & more\"\n}\n", string(body))
	var back map[string]string
	require.NoError(t, json.Unmarshal(body, &back))

	require.Equal(t, "", policyDefault("string", nil))
	require.Equal(t, "false", policyDefault("bool", nil))
	require.Equal(t, "0", policyDefault("int", nil))
	require.Equal(t, "a,b", policyDefault("list", []any{"a", "b"}))
}

// TestR105_EveryErrorCodeHasAMeaning asserts R-105 for docs/errors.json: every
// code a client can branch on is listed, once, in code order, with the HTTP
// status it comes with and a sentence saying what it means.
func TestR105_EveryErrorCodeHasAMeaning(t *testing.T) {
	data := Errors()
	require.Len(t, data.Errors, len(errs.Catalog()))
	for i, e := range data.Errors {
		require.NotEmpty(t, e.Meaning, "%s has no meaning in errs' catalog", e.Code)
		require.NotRegexp(t, traceID, e.Meaning, "%s's meaning is read by users: no requirement or decision IDs", e.Code)
		require.NotZero(t, e.Status, e.Code)
		if i > 0 {
			require.Less(t, data.Errors[i-1].Code, e.Code)
		}
	}
}
