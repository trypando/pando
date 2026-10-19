package cloudflare

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "token", Label: "API token", Title: "Create an API token",
			Do: []api.Action{
				{Text: "In Cloudflare, open **Manage account** → **Account API tokens** for an account token, or **My Profile** → **API Tokens** for a user token."},
				{Text: "Choose **Create Token**, then a custom token."},
				{Text: "Add these permissions:", Table: &api.Table{
					Columns: []string{"Scope", "Resource", "Access"},
					Rows: [][]string{
						{"Account", "Cloudflare Tunnel", "Edit"},
						{"Zone", "DNS", "Edit"},
						{"Zone", "Zone", "Read"},
					},
				}},
				{Text: "Under **Account Resources**, include your account. Under **Zone Resources**, include each zone apps will use."},
			},
			Links: []api.Link{
				{Label: "Open the Cloudflare dashboard", URL: "https://dash.cloudflare.com/"},
				{Label: "Open user API tokens", URL: "https://dash.cloudflare.com/profile/api-tokens"},
			},
			Fields: []string{"api_token"}},
		{ID: "account", Label: "Account", Title: "Find your account ID",
			Do:     []api.Action{{Text: "Open one of your domains and copy **Account ID** from the **API** section of its **Overview** page."}},
			Links:  []api.Link{{Label: "Open Cloudflare", URL: "https://dash.cloudflare.com/"}},
			Fields: []string{"account_id"}},
		{ID: "domain", Label: "Zones", Title: "Choose zones",
			Fields: []string{"zone", "other_zones", "console_hostname"}},
	}
}
