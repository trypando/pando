package openai

import (
	"github.com/trypando/pando/internal/adapter/ai/aikit"
	"github.com/trypando/pando/internal/adapter/api"
)

// steps walk through a restricted key and the model.
func steps() []api.Step {
	return []api.Step{
		aikit.KeyStep(api.Link{Label: "Open API keys", URL: "https://platform.openai.com/settings/organization/api-keys"},
			api.Action{Text: "In the OpenAI platform, open **API keys** and choose **Create new secret key**."},
			api.Action{Text: "Under **Permissions**, choose **Restricted** and set:", Table: &api.Table{
				Columns: []string{"Resource", "Access"},
				Rows: [][]string{
					{"Models", "Read"},
					{"Model capabilities", "Request"},
					{"Responses API", "Write"},
				},
			}},
			api.Action{Text: "Create the key and copy it."},
		),
		aikit.ModelStep([]api.Link{{Label: "Open OpenAI models", URL: "https://developers.openai.com/api/docs/models"}}),
	}
}
