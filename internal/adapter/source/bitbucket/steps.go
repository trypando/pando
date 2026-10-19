package bitbucket

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps is guided setup: the method first, then one step for it.
func steps() []api.Step {
	return []api.Step{
		forgekit.MethodStep(),
		{ID: "token", Label: "Token", Title: "Create a token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "Create one of these tokens:", Table: &api.Table{
					Columns: []string{"Token", "Where", "Permission"},
					Rows: [][]string{
						{"Cloud access token", "Workspace, project or repository settings → Security → Access tokens → Create access token", "Repositories: Read"},
						{"Cloud API token", "Account settings → Security → Create API token with scopes → Bitbucket", "read:repository:bitbucket"},
						{"Data Center HTTP access token", "Manage account, Project settings or Repository settings → HTTP access tokens → Create token", "Repository read"},
					},
				}},
				{Text: "For a Cloud API token or a Data Center personal token, enter your username below."},
			},
			Links: []api.Link{
				{Label: "Open Bitbucket", URL: "https://{host}/"},
				{Label: "Open Atlassian API tokens", URL: "https://id.atlassian.com/manage-profile/security/api-tokens"},
			},
			Fields: []string{"token_type", "username", "email", forgekit.FieldToken}},
		forgekit.OAuthStep("OAuth consumer", "Add an OAuth consumer",
			[]api.Link{{Label: "Open workspaces", URL: "https://bitbucket.org/account/workspaces/"}},
			"In the workspace, open **Settings** → **Workspace settings** → **OAuth consumers** → **Add consumer**.",
			"Enter a **Name**. "+forgekit.SetCallback("Callback URL"),
			"Under **Permissions**, check **Repositories** **Read** and choose **Save**.",
			"Choose the consumer's name and copy its **Key** and **Secret**."),
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "Access key", Title: "Add an access key", File: "pando-access",
			Add: []string{
				"On Bitbucket Cloud, open **Repository settings** → **Access keys** → **Add key**. Enter a **Label**, paste `pando-access.pub` into **Key** and save it.",
				"On Data Center, open the project's or repository's **Settings** → **Access keys** → **Add key**. Paste `pando-access.pub`, choose **Read** and choose **Add key**.",
			},
			KeyScan: "`ssh-keyscan bitbucket.org`, or `ssh-keyscan -p 7999 HOST` for Data Center,"}),
		forgekit.ScopeStep("Workspace or project", "Choose a workspace or project"),
	}
}
