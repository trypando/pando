package chat

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/secret"
)

// A webhook URL sent with a save is checked then; one kept from before was
// checked when it was saved.
func TestAWebhookURLIsCheckedWhenItIsSent(t *testing.T) {
	check := Teams.Info().Check
	require.NotNil(t, check)
	slack := map[string]secret.Value{"webhook_url": secret.New("https://hooks.slack.com/services/T0/B0/x")}
	require.Error(t, check(json.RawMessage(`{}`), slack, nil))
	require.NoError(t, check(json.RawMessage(`{}`), nil, []string{"webhook_url"}))
	// Removing it, or never giving one, leaves nothing to post to.
	require.Error(t, check(json.RawMessage(`{}`), map[string]secret.Value{"webhook_url": {}}, []string{"webhook_url"}))
	require.Error(t, check(json.RawMessage(`{}`), nil, nil))
}
