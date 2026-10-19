//go:build integration

package httpapi_test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// R-261: the kinds of adapter this build can run are on the API, with their
// settings, so the console and CLI can offer them; a kind the build lacks is
// refused rather than saved to be skipped at every startup.
func TestR261_AdapterKindsAreListedAndUnknownOnesRefused(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	var got struct {
		Kinds []struct {
			Category string `json:"category"`
			Kind     string `json:"kind"`
			Fields   []struct {
				Key        string `json:"key"`
				Credential bool   `json:"credential"`
			} `json:"fields"`
		} `json:"kinds"`
	}
	listed := i.do(admin, http.MethodGet, "/adapters/kinds", nil)
	require.Equal(t, http.StatusOK, listed.Code, listed.String())
	listed.JSON(t, &got)
	var anthropicKey bool
	for _, k := range got.Kinds {
		if k.Category == "ai" && k.Kind == "anthropic" {
			for _, f := range k.Fields {
				if f.Key == "api_key" {
					anthropicKey = f.Credential
				}
			}
		}
	}
	require.True(t, anthropicKey, "the Anthropic API key is marked as a credential")

	refused := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_other", "category": "ai", "kind": "openai", "name": "Other", "config": map[string]any{},
	})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "ai/anthropic")

	added := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic", "name": "Anthropic",
		"config": map[string]any{"model": "claude-sonnet-5"}, "credentials": map[string]string{"api_key": "sk-ant-test"},
	})
	require.Equal(t, http.StatusCreated, added.Code, added.String())
	require.NotContains(t, added.String(), "sk-ant-test")
	require.Contains(t, added.String(), "restart")

	// Its settings come back to someone who may change them — never the key —
	// and not to someone who may only look.
	listedAll := i.do(admin, http.MethodGet, "/adapters", nil)
	require.Contains(t, listedAll.String(), `"model":"claude-sonnet-5"`)
	require.NotContains(t, listedAll.String(), "sk-ant-test")
	viewer := i.user("viewer")
	viewRole := i.customRole(admin, "inventory", "install", "install.view")
	require.Equal(t, http.StatusOK,
		i.do(admin, http.MethodPut, "/users/"+i.userID(viewer)+"/role", map[string]string{"role_id": viewRole}).Code)
	onlyView := i.do(viewer, http.MethodGet, "/adapters", nil)
	require.Equal(t, http.StatusOK, onlyView.Code, onlyView.String())
	require.NotContains(t, onlyView.String(), "claude-sonnet-5", "settings name operator detail")

	// Someone who may not configure adapters may not add one.
	require.Equal(t, http.StatusForbidden, i.do(i.user("ordinary"), http.MethodPost, "/adapters", map[string]any{
		"id": "ai_x", "category": "ai", "kind": "anthropic", "name": "X", "config": map[string]any{},
	}).Code)
}

// TestR105_AConfigurationThatCannotStartIsRefusedAsItIsSaved asserts R-105 for
// a kind that checks itself as it is saved (KindInfo.Check): what would keep
// the adapter from starting is said in a sentence, and nothing is saved.
func TestR105_AConfigurationThatCannotStartIsRefusedAsItIsSaved(t *testing.T) {
	t.Parallel()
	i := newInstall(t)
	admin := i.admin()

	refused := i.do(admin, http.MethodPost, "/adapters", map[string]any{
		"id": "ai_anthropic", "category": "ai", "kind": "anthropic",
		"config": map[string]any{"workspace_id": "default"}, "credentials": map[string]string{"api_key": "sk-ant-x"},
	})
	require.Equal(t, http.StatusBadRequest, refused.Code, refused.String())
	require.Contains(t, refused.String(), "workspace")
	require.NotContains(t, refused.String(), "anthropic: ")

	listed := i.do(admin, http.MethodGet, "/adapters", nil)
	require.NotContains(t, listed.String(), "ai_anthropic")
}
