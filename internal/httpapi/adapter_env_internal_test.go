package httpapi

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	aianthropic "github.com/trypando/pando/internal/adapter/ai/anthropic"
	adapterapi "github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/errs"
	"github.com/trypando/pando/internal/secret"
)

// TestR080a_AnAdapterSetThroughTheAPIReadsOnlyItsOwnVariable asserts R-080a
// for finding 5 of the 2026-10-16 assessment, case by case: which variables
// and addresses an AI adapter configured through the API may name.
func TestR080a_AnAdapterSetThroughTheAPIReadsOnlyItsOwnVariable(t *testing.T) {
	s := &Server{AdapterKinds: []adapterapi.KindInfo{aianthropic.Info()}}
	check := func(cfg map[string]any, creds map[string]secret.Value) error {
		raw, err := json.Marshal(cfg)
		require.NoError(t, err)
		return s.envReferences(context.Background(), "ai_x", "ai", "anthropic", raw, creds)
	}

	refused := check(map[string]any{"api_key_env": "PANDO_DATABASE_URL"}, nil)
	require.Equal(t, errs.ValidInvalid, errs.CodeOf(refused))
	require.Contains(t, refused.Error(), "may name only ANTHROPIC_API_KEY")

	// A kind with no default variable may name none.
	none := s.envReferences(context.Background(), "ai_y", "ai", "local",
		json.RawMessage(`{"api_key_env":"OPENAI_API_KEY"}`), nil)
	require.Contains(t, none.Error(), "may name no variable")

	// Its own default, an empty name, or something that is not a name at all
	// reads nothing it should not.
	require.NoError(t, check(map[string]any{"api_key_env": "ANTHROPIC_API_KEY"}, nil))
	require.NoError(t, check(map[string]any{"api_key_env": ""}, nil))
	require.NoError(t, check(map[string]any{"api_key_env": 5}, nil))

	// Another address needs a key of its own; with no credential store there
	// is none.
	away := check(map[string]any{"base_url": "https://attacker.example"}, nil)
	require.Contains(t, away.Error(), "would read ANTHROPIC_API_KEY from Pando's environment and send it to https://attacker.example")
	require.NoError(t, check(map[string]any{"base_url": "https://attacker.example"},
		map[string]secret.Value{"api_key": secret.New("sk-ant-test")}))
	require.Error(t, check(map[string]any{"base_url": "https://attacker.example"},
		map[string]secret.Value{"api_key": secret.New("")}), "removing the key in the same request leaves none")
	require.NoError(t, check(map[string]any{"base_url": "https://api.anthropic.com/"}, nil), "the provider's own address")
	// encoding/json matches field names without regard to case, so every
	// spelling is the adapter's BaseURL, and an empty one does not hide another.
	require.Error(t, check(map[string]any{"BASE_URL": "https://attacker.example"}, nil))
	require.Error(t, check(map[string]any{"base_url": "", "Base_Url": "https://attacker.example"}, nil))
	require.Error(t, check(map[string]any{"API_KEY_ENV": "PANDO_ADMIN_PASSWORD"}, nil))

	// Not an object, or nothing at all: inlineCredential's to refuse.
	require.NoError(t, s.envReferences(context.Background(), "ai_x", "ai", "anthropic", json.RawMessage(`[1]`), nil))
	require.NoError(t, s.envReferences(context.Background(), "ai_x", "ai", "anthropic", nil, nil))
}
