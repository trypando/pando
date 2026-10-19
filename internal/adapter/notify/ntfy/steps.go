package ntfy

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "server", Label: "Server", Title: "Choose a server",
			Fields: []string{"server_url"}},
		{ID: "topic", Label: "Topic", Title: "Subscribe to a topic",
			Do: []api.Action{
				{Text: "In the ntfy app, choose **Subscribe to topic**."},
				{Text: "Enter a **Topic name** nobody would guess."},
				{Text: "For your own server, turn on **Use another server** and enter its address."},
				{Text: "Choose **Subscribe**."},
			},
			Links:  []api.Link{{Label: "Open ntfy", URL: "{server_url}"}},
			Fields: []string{"topic"}},
		{ID: "token", Label: "Access token", Title: "Create an access token",
			Do: []api.Action{
				{Text: "Sign in to the ntfy web app and open **Account**."},
				{Text: "Under **Access tokens**, choose **Create access token**, then **Create token**."},
				{Text: "Copy the token."},
			},
			Links:  []api.Link{{Label: "Open your ntfy account", URL: "{server_url}/account"}},
			Fields: []string{"access_token"}},
	}
}
