package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	adapterapi "github.com/trypando/pando/internal/adapter/api"
	servicesdocker "github.com/trypando/pando/internal/adapter/services/docker"
	"github.com/trypando/pando/internal/config"
	"github.com/trypando/pando/internal/core/assist"
)

// TestR190_DeclaredCredentialsAreReadFromWhereTheFileSays asserts R-190: a
// declared credential is read at startup from the variable or file named,
// and one that cannot be read stops startup naming where Pando looked.
func TestR190_DeclaredCredentialsAreReadFromWhereTheFileSays(t *testing.T) {
	t.Setenv("PANDO_TEST_API_KEY", "sk-from-env")
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("sk-from-file\n"), 0o600))

	got, err := declaredCredentials(map[string]config.CredentialRef{
		"api_key": {Env: "PANDO_TEST_API_KEY"},
		"token":   {File: path},
	})
	require.NoError(t, err)
	require.Equal(t, "sk-from-env", got["api_key"].Reveal())
	require.Equal(t, "sk-from-file", got["token"].Reveal(), "a mounted secret's trailing newline is not part of it")

	t.Setenv("PANDO_TEST_UNSET", "")
	_, err = declaredCredentials(map[string]config.CredentialRef{"api_key": {Env: "PANDO_TEST_UNSET"}})
	require.ErrorContains(t, err, "api_key")
	require.ErrorContains(t, err, "PANDO_TEST_UNSET, which is not set")

	missing := filepath.Join(t.TempDir(), "absent")
	_, err = declaredCredentials(map[string]config.CredentialRef{"token": {File: missing}})
	require.ErrorContains(t, err, "token")
	require.ErrorContains(t, err, missing+", which could not be read")
}

// TestR271_DeclaredAssignmentsComeFromEnabledAdaptersOnly asserts R-271: the
// AI functions a file assigns are carried with their model and the key that
// declared them, and a disabled adapter assigns nothing.
func TestR271_DeclaredAssignmentsComeFromEnabledAdaptersOnly(t *testing.T) {
	src := config.Source{Kind: "file", Name: "/etc/pando.yaml", Key: "adapters.ai_anthropic.functions.search_audit"}
	got := declaredAssignments([]config.AdapterDecl{
		{ID: "ai_anthropic", Category: "ai", Kind: "anthropic", Enabled: true, Functions: []config.FunctionDecl{
			{Function: "search_audit", Model: "claude-haiku-4-5", Source: src},
		}},
		{ID: "ai_openai", Category: "ai", Kind: "openai", Enabled: false, Functions: []config.FunctionDecl{
			{Function: "repair_plan"},
		}},
	})
	require.Equal(t, []assist.Declared{{
		Function: adapterapi.AIFunction("search_audit"), Adapter: "ai_anthropic", Model: "claude-haiku-4-5",
		Source: assist.Source{Kind: "file", Name: "/etc/pando.yaml", Key: src.Key},
	}}, got)

	require.Empty(t, declaredAssignments(nil))
}

// Every category and kind this build ships has an adapter; anything else is
// nil, which startup reports rather than registers.
func TestNewAdapterBuildsEachShippedKind(t *testing.T) {
	for _, tc := range []struct{ category, kind string }{
		{"runtime", "docker"},
		{"runtime", "docker-hosts"},
		{"routing", "loopback"},
		{"routing", "traefik"},
		{"secrets", "local"},
		{"builder", "buildkit"},
		{"backup", "local"},
		{"services", "docker"},
		{"scanner", "trivy"},
		{"ai", "anthropic"},
		{"ai", "openai"},
		{"ai", "local"},
		{"notify", "console"},
	} {
		a := newAdapter(tc.category, tc.kind, nil)
		require.NotNil(t, a, "%s/%s", tc.category, tc.kind)
		require.Equal(t, tc.kind, a.Kind())
		require.Equal(t, adapterapi.Category(tc.category), a.Category())
	}

	require.Nil(t, newAdapter("ai", "gemini", nil))
	require.Nil(t, newAdapter("runtime", "anthropic", nil), "a kind is only valid in its own category")
}

// TestR271_TwoDeclaredServicesAdaptersForOneSlotTypeStopStartup asserts R-271:
// two declared services adapters that provide the same kind of service stop
// startup naming both declarations.
func TestR271_TwoDeclaredServicesAdaptersForOneSlotTypeStopStartup(t *testing.T) {
	registry := adapterapi.NewRegistry()
	require.NoError(t, registry.Register("svc_one", servicesdocker.New()))
	require.NoError(t, registry.Register("svc_two", servicesdocker.New()))

	decl := func(id string, enabled bool) config.AdapterDecl {
		return config.AdapterDecl{ID: id, Category: "services", Kind: "docker", Enabled: enabled,
			Source: config.Source{Kind: "file", Name: "/etc/pando.yaml", Key: "adapters." + id}}
	}

	err := declaredServicesOverlap(registry, []config.AdapterDecl{decl("svc_one", true), decl("svc_two", true)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "/etc/pando.yaml")
	require.Contains(t, err.Error(), "adapters.svc_one")
	require.Contains(t, err.Error(), "adapters.svc_two")

	require.NoError(t, declaredServicesOverlap(registry, []config.AdapterDecl{decl("svc_one", true)}))
	require.NoError(t, declaredServicesOverlap(registry, []config.AdapterDecl{decl("svc_one", true), decl("svc_two", false)}),
		"a disabled declaration provides nothing")
	require.NoError(t, declaredServicesOverlap(registry, []config.AdapterDecl{decl("svc_one", true), decl("svc_absent", true)}),
		"a declaration that failed to register provides nothing")
	require.NoError(t, declaredServicesOverlap(registry, []config.AdapterDecl{
		decl("svc_one", true), {ID: "rt", Category: "runtime", Kind: "docker", Enabled: true},
	}), "only services adapters are compared")
}
