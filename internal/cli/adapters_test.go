package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var anthropicKind = map[string]any{"kinds": []map[string]any{{
	"category": "ai", "kind": "anthropic", "name": "Anthropic", "id_prefix": "ai_",
	"fields": []map[string]any{
		{"key": "api_key", "label": "API key", "type": "string", "credential": true},
		{"key": "model", "label": "Model", "type": "string"},
		{"key": "timeout_seconds", "label": "Timeout", "type": "int"},
	},
}}}

// R-261: an adapter can be added from the CLI as from the API — ordinary
// settings from --set, the API key read from stdin rather than typed on the
// command line, and the two kept apart in the request.
func TestAnAdapterCanBeAddedFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /adapters/kinds", anthropicKind)

	got := run(t, api, "sk-ant-secret\n", "adapter", "add", "ai/anthropic",
		"--set", "model=claude-sonnet-5", "--set", "timeout_seconds=90")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic", "name": "Anthropic",
		"config": {"model": "claude-sonnet-5", "timeout_seconds": 90},
		"credentials": {"api_key": "sk-ant-secret"},
		"is_default": true
	}`, api.bodyFor("POST /adapters"))
	require.Contains(t, got.out, "Restart Pando")
	require.NotContains(t, got.out, "sk-ant-secret")

	// A secret on the command line is refused; so is a setting the kind lacks,
	// and a kind the build lacks.
	require.ErrorContains(t, run(t, api, "", "adapter", "add", "ai/anthropic", "--set", "api_key=x").err, "is a secret")
	require.ErrorContains(t, run(t, api, "\n", "adapter", "add", "ai/anthropic", "--set", "colour=red").err, "no setting")
	require.ErrorContains(t, run(t, api, "", "adapter", "add", "ai/openai").err, "no ai adapter")
	require.ErrorContains(t, run(t, api, "\n", "adapter", "add", "ai/anthropic", "--set", "timeout_seconds=soon").err, "whole number")
}

var trivyKind = map[string]any{"kinds": []map[string]any{{
	"category": "scanner", "kind": "trivy", "name": "Trivy", "id_prefix": "scan_",
	"fields": []map[string]any{
		{"key": "severity", "label": "Severity", "type": "string"},
		{"key": "timeout_seconds", "label": "Timeout", "type": "int", "default": "600", "advanced": true},
	},
}}}

// R-261: advanced settings stay available from the CLI. They take --set like
// any other, `adapter kinds` lists them apart, and help for a named kind
// lists them under their own heading with their defaults (issue #85).
func TestR261_AdvancedSettingsAreListedApartAndStillSet(t *testing.T) {
	api := newAPI(t).reply("GET /adapters/kinds", trivyKind)

	kinds := run(t, api, "", "adapter", "kinds")
	require.NoError(t, kinds.err, kinds.errOut)
	require.Contains(t, kinds.out, "ADVANCED")
	require.Regexp(t, `scanner/trivy\s+severity\s+timeout_seconds`, kinds.out)

	help := run(t, api, "", "adapter", "add", "scanner/trivy", "--help")
	require.NoError(t, help.err, help.errOut)
	require.Regexp(t, `(?s)Settings, each --set KEY=VALUE:\s+severity.*Advanced settings.*:\s+timeout_seconds\s+Timeout \(default: 600\)`, help.out)

	got := run(t, api, "", "adapter", "add", "scanner/trivy", "--set", "timeout_seconds=900")
	require.NoError(t, got.err, got.errOut)
	require.Contains(t, api.bodyFor("POST /adapters"), `"config":{"timeout_seconds":900}`)
}
