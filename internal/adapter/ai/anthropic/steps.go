package anthropic

import (
	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// steps walk through the key and the model. The workspace ID is asked with
// the key, and is sent as anthropic-workspace-id only when given.
func steps() []api.Step {
	key := aikit.KeyStep(api.Link{Label: "Open API keys", URL: "https://platform.claude.com/settings/keys"},
		api.Action{Text: "In the Claude Console, open **API keys** and choose **Create key**."},
		api.Action{Text: "Name the key and choose an expiration."},
		api.Action{Text: "Set **Linked account** to a service account, or to yourself."},
		api.Action{Text: "Copy the key, which starts with `sk-ant-`."},
	)
	key.Fields = append(key.Fields, "workspace_id")
	return []api.Step{
		key,
		aikit.ModelStep([]api.Link{{Label: "Compare models", URL: "https://platform.claude.com/docs/en/models/overview"}}),
	}
}
