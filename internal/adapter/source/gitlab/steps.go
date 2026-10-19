package gitlab

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps walks through one sign-in method's setup in GitLab, chosen first.
func steps() []api.Step {
	return []api.Step{
		forgekit.MethodStep(),
		forgekit.OAuthStep("OAuth app", "Create an OAuth application",
			[]api.Link{{Label: "Open applications", URL: "https://{host}/-/user_settings/applications"}},
			"In GitLab, select your avatar, then **Edit profile** → **Access** → **Applications** → **Add new application**. For a group, start from its **Settings** → **Applications**.",
			"Enter a **Name**. "+forgekit.SetCallback("Redirect URI"),
			"Clear **Confidential**. Under **Scopes**, select **read_repository** and **read_api**.",
			"Choose **Save application** and copy the **Application ID** and **Secret**."),
		{ID: "token", Label: "Token", Title: "Create a token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "Create one of these in GitLab:", Table: &api.Table{
					Columns: []string{"Token", "Where", "Settings"},
					Rows: [][]string{
						{"Fine-grained personal access token (GitLab 19.2 or later)", "Avatar → Edit profile → Access → Personal access tokens → Generate token → Fine-grained token", "Code: Download, Project: Read and Branch: Read; boundary User to list every project, or the group"},
						{"Personal access token", "Avatar → Edit profile → Access → Personal access tokens → Generate token → Legacy token", "Scopes read_repository, read_api"},
						{"Project access token", "Project → Settings → Access tokens → Add new token", reporterScopes},
						{"Group access token", "Group → Settings → Access tokens → Add new token", reporterScopes},
						{"Deploy token", "Project or group → Settings → Repository → Deploy tokens → Add token", "Scope read_repository"},
					},
				}},
				{Text: "Copy the token, and for a deploy token its username."},
			},
			Links: []api.Link{{Label: "Open personal access tokens",
				URL: "https://{host}/-/user_settings/personal_access_tokens?name=Pando&scopes=read_repository,read_api"}},
			Fields: []string{"token_type", "username", forgekit.FieldToken}},
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "Deploy key", Title: "Add a deploy key", File: "pando-deploy",
			Add: []string{
				"In the project, open **Settings** → **Repository**, expand **Deploy keys** and choose **Add new key**.",
				"Paste `pando-deploy.pub` into **Key** and choose **Add key**.",
			}}),
		forgekit.ScopeStep("Group", "Choose a group"),
	}
}

// reporterScopes is what a project or group access token needs.
const reporterScopes = "Role Reporter; scopes read_repository, read_api"
