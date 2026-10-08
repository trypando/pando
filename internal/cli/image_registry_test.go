package cli_test

import (
	"testing"

	"github.com/stretchr/testify/require"
)

var ociRegistryKind = map[string]any{"kinds": []map[string]any{{
	"category": "image_registry", "kind": "oci", "name": "OCI registry", "id_prefix": "reg_",
	"fields": []map[string]any{
		{"key": "url", "label": "Registry address", "type": "string", "required": true},
		{"key": "username", "label": "Username", "type": "string"},
		{"key": "password", "label": "Password", "type": "string", "credential": true},
		{"key": "insecure", "label": "Allow plain HTTP", "type": "bool", "advanced": true, "default": "false"},
	},
}}}

// TestR261_TheImageRegistryIsSetFromTheCLI asserts R-261 for the image
// registry as an adapter (issue #153): `pando adapter add image_registry/oci`
// configures it like any adapter, with the password read from stdin rather
// than the command line, and says it is used from now on — no restart.
func TestR261_TheImageRegistryIsSetFromTheCLI(t *testing.T) {
	api := newAPI(t).reply("GET /adapters/kinds", ociRegistryKind)

	got := run(t, api, "registry-secret\n", "adapter", "add", "image_registry/oci",
		"--set", "url=http://registry.internal:5000", "--set", "username=pando", "--set", "insecure=true")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{
		"id": "reg_oci", "category": "image_registry", "kind": "oci", "name": "OCI registry",
		"config": {"url": "http://registry.internal:5000", "username": "pando", "insecure": true},
		"credentials": {"password": "registry-secret"},
		"is_default": true
	}`, api.bodyFor("POST /adapters"))
	require.Contains(t, got.out, "The next build that goes through a registry is pushed there")
	require.NotContains(t, got.out, "Restart")
	require.NotContains(t, got.out, "registry-secret")

	// Turned off with the same command, the stored password kept by leaving
	// it empty.
	api = newAPI(t).reply("GET /adapters/kinds", ociRegistryKind)
	got = run(t, api, "\n", "adapter", "add", "image_registry/oci", "--id", "reg_oci",
		"--set", "url=http://registry.internal:5000", "--disabled")
	require.NoError(t, got.err, got.errOut)
	require.JSONEq(t, `{
		"id": "reg_oci", "category": "image_registry", "kind": "oci", "name": "OCI registry",
		"config": {"url": "http://registry.internal:5000"}, "is_default": true, "enabled": false
	}`, api.bodyFor("POST /adapters"))
}
