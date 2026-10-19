package aikit_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// TestKeyAndModelSteps asserts the shared AI steps ask for the settings
// every AI kind names api_key and model.
func TestKeyAndModelSteps(t *testing.T) {
	link := api.Link{Label: "Open API keys", URL: "https://example.com/keys"}
	key := aikit.KeyStep(link, api.Action{Text: "Create a key."})
	require.Equal(t, api.Step{ID: "key", Label: "API key", Title: "Create an API key",
		Do: []api.Action{{Text: "Create a key."}}, Links: []api.Link{link}, Fields: []string{"api_key"}}, key)

	model := aikit.ModelStep(nil)
	require.Equal(t, api.Step{ID: "model", Label: "Model", Title: "Choose a model", Fields: []string{"model"}}, model)
}
