package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// TestR261_TheCLIWalksAKindsSteps asserts R-261 for guided setup: `pando
// adapter add` at a terminal asks a kind's settings step by step, as the
// console does — skipping a step another answer rules out, following a
// suggestion from an earlier answer, and leaving alone what --set gave.
func TestR261_TheCLIWalksAKindsSteps(t *testing.T) {
	var info kindInfo
	require.NoError(t, json.Unmarshal([]byte(`{
		"category": "routing", "kind": "example", "name": "Example",
		"fields": [
			{"key": "method", "label": "Method", "type": "select", "default": "token",
			 "options": [{"value": "token", "label": "Token"}, {"value": "key", "label": "Key"}]},
			{"key": "token", "label": "Token", "type": "string", "credential": true,
			 "shown_when": {"key": "method", "values": ["token"]}},
			{"key": "ssh_key", "label": "SSH key", "type": "string", "credential": true,
			 "shown_when": {"key": "method", "values": ["key"]}},
			{"key": "zone", "label": "Zone", "type": "string"},
			{"key": "console", "label": "Console", "type": "string", "suggest": "pando.{zone}"},
			{"key": "account", "label": "Account", "type": "string"}
		],
		"steps": [
			{"id": "method", "label": "Method", "title": "Choose a method", "fields": ["method"]},
			{"id": "token", "label": "Token", "title": "Create a token", "fields": ["token"],
			 "shown_when": {"key": "method", "values": ["token"]},
			 "do": [{"text": "Open **Tokens** and run `+"`make-token`"+`."}],
			 "links": [{"label": "Open tokens", "url": "https://{zone}/tokens"}]},
			{"id": "key", "label": "Key", "title": "Add a key", "fields": ["ssh_key"],
			 "shown_when": {"key": "method", "values": ["key"]}},
			{"id": "zones", "label": "Zones", "title": "Choose zones", "fields": ["zone", "console", "account"]}
		]}`), &info))

	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	// Method: Enter keeps Token. Token. Zone. Console: Enter keeps the suggestion.
	cmd.SetIn(strings.NewReader("\ns3cret\nexample.com\n\n"))
	values := map[string]string{"account": "given"}
	credentials := map[string]string{}

	asked, err := walkSteps(cmd, &info, "", values, credentials)
	require.NoError(t, err)

	require.Equal(t, map[string]string{"account": "given", "zone": "example.com", "console": "pando.example.com"}, values)
	require.Equal(t, map[string]string{"token": "s3cret"}, credentials)
	require.True(t, asked["token"])
	require.True(t, asked["ssh_key"], "the key step is for the other method, so nothing asks for it afterwards")

	out := stderr.String()
	require.Contains(t, out, "Create a token (step 2 of 4)")
	require.Contains(t, out, "1. Open Tokens and run make-token.")
	require.NotContains(t, out, "Add a key")
	require.Contains(t, out, "Console [pando.example.com]: ")
	// The link names a setting not yet answered when it is shown.
	require.NotContains(t, out, "Open tokens:")
}

// TestR261_TheCLIAsksWhichProviderFirst asserts R-261 for a kind with
// presets: the CLI asks which provider before its steps, fills in that
// provider's values, and shows only its steps; --preset answers it.
func TestR261_TheCLIAsksWhichProviderFirst(t *testing.T) {
	var info kindInfo
	require.NoError(t, json.Unmarshal([]byte(`{
		"category": "audit_sink", "kind": "example", "name": "Example",
		"fields": [
			{"key": "url", "label": "URL", "type": "string"},
			{"key": "token", "label": "Token", "type": "string", "credential": true},
			{"key": "body", "label": "Body", "type": "string", "default": "ndjson"}
		],
		"presets": [{"id": "splunk", "label": "Splunk", "values": {"body": "splunk_hec"}}],
		"other_preset": "Another SIEM",
		"steps": [
			{"id": "splunk_token", "label": "Token", "title": "Create an HEC token", "fields": ["token"], "presets": ["splunk"]},
			{"id": "token", "label": "Token", "title": "Enter the token", "fields": ["token", "body"], "presets": ["other"]},
			{"id": "url", "label": "URL", "title": "Enter the endpoint", "fields": ["url"]}
		]}`), &info))

	run := func(preset, input string) (map[string]string, string) {
		cmd := &cobra.Command{}
		var stderr bytes.Buffer
		cmd.SetErr(&stderr)
		cmd.SetIn(strings.NewReader(input))
		values := map[string]string{}
		if preset != "" {
			require.NoError(t, applyPreset(&info, preset, values))
		}
		_, err := walkSteps(cmd, &info, preset, values, map[string]string{})
		require.NoError(t, err)
		return values, stderr.String()
	}

	values, out := run("", "1\ntok\nhttps://splunk.example.com\n")
	require.Contains(t, out, "1) Splunk")
	require.Contains(t, out, "2) Another SIEM")
	require.Contains(t, out, "Create an HEC token (step 1 of 2)")
	require.NotContains(t, out, "Enter the token")
	require.Equal(t, map[string]string{"body": "splunk_hec", "url": "https://splunk.example.com"}, values)

	_, out = run("other", "tok\n\nhttps://siem.example.com\n")
	require.NotContains(t, out, "Choose a provider")
	require.Contains(t, out, "Enter the token (step 1 of 2)")

	require.ErrorContains(t, applyPreset(&info, "nope", map[string]string{}), "its providers are splunk, other")
}
