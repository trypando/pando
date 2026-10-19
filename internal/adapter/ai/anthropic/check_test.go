package anthropic

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A workspace ID that is not one is refused as it is saved, with the key
// already stored.
func TestAWorkspaceIDIsCheckedAsItIsSaved(t *testing.T) {
	check := Info().Check
	require.NotNil(t, check)
	require.Error(t, check(json.RawMessage(`{"workspace_id":"default"}`), nil, []string{"api_key"}))
	require.NoError(t, check(json.RawMessage(`{"workspace_id":"wrkspc_01JwQvzr7rXLA5AGx3HKfFUJ"}`), nil, []string{"api_key"}))
	// A key Pando may read from its environment at startup is not asked for.
	require.NoError(t, check(json.RawMessage(`{}`), nil, nil))
}
