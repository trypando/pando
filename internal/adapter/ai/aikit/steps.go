package aikit

import "github.com/trypando/pando/internal/adapter/api"

// Guided-setup steps every AI adapter asks the same way (api.KindInfo.Steps):
// the API key, and the model each function uses by default.

// KeyStep creates an API key at the provider and asks for it.
func KeyStep(link api.Link, do ...api.Action) api.Step {
	return api.Step{ID: "key", Label: "API key", Title: "Create an API key",
		Do: do, Links: []api.Link{link}, Fields: []string{"api_key"}}
}

// ModelStep asks for the default model.
func ModelStep(links []api.Link, do ...api.Action) api.Step {
	return api.Step{ID: "model", Label: "Model", Title: "Choose a model",
		Do: do, Links: links, Fields: []string{"model"}}
}
