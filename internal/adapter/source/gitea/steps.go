package gitea

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps walk through each sign-in method. The names in Do are Forgejo's, as
// Codeberg shows them; Gitea title-cases the same labels ("Token Name").
func steps() []api.Step {
	applications := []api.Link{{Label: "Open Applications", URL: "https://{host}/user/settings/applications"}}
	return []api.Step{
		forgekit.MethodStep(),
		{ID: "token", Label: "Access token", Title: "Create an access token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "Open **Settings** → **Applications**. On Forgejo, choose **New access token**."},
				{Text: "Enter a **Token name**. Under **Repository and organization access**, choose **All (public, private, and limited)**."},
				{Text: "Under **Select permissions**, set these:", Table: &api.Table{
					Columns: []string{"Permission", "Access"},
					Rows:    [][]string{{"repository", "Read"}, {"user", "Read"}},
				}},
				{Text: "Choose **Generate token** and copy the token."},
			},
			Links:  applications,
			Fields: []string{forgekit.FieldToken, "username"}},
		forgekit.OAuthStep("OAuth2 app", "Create an OAuth2 application", applications,
			"Open **Settings** → **Applications**.",
			"Under **Create a new OAuth2 application**, enter an **Application name**.",
			forgekit.SetCallback("Redirect URIs"),
			"Leave **Confidential client** checked and choose **Create application**.",
			"Copy the **Client ID** and **Client secret**."),
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "Deploy key", Title: "Add a deploy key", File: "pando-deploy",
			Add: []string{
				"In the repository, open **Settings** → **Deploy keys** → **Add deploy key** (**Add SSH key** on Gitea).",
				"Enter a **Title**, paste `pando-deploy.pub` into **Content**, and add the key.",
			},
			KeyScan: forgekit.KeyScanAnyHost}),
		forgekit.ScopeStep("Owner", "Choose an owner"),
	}
}
