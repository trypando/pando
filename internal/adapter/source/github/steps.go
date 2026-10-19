package github

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps walks through one sign-in method's setup in GitHub, chosen first.
func steps() []api.Step {
	return []api.Step{
		forgekit.MethodStep(),
		{ID: "app", Label: "Create the App", Title: "Create a GitHub App", ShownWhen: forgekit.WhenMethod(forgekit.MethodApp),
			Do: []api.Action{
				{Text: "In GitHub, open **Settings** → **Developer settings** → **GitHub Apps** → **New GitHub App**. For an organization, start from its settings."},
				{Text: "Enter a **GitHub App name** and **Homepage URL**. Under **Webhook**, clear **Active**."},
				{Text: "Under **Repository permissions**, set **Contents** to **Read-only**."},
				{Text: "Choose **Create GitHub App** and copy the **App ID**. Under **Credentials** → **Key pairs**, choose **New key**."},
			},
			Links:  []api.Link{{Label: "Open GitHub Apps", URL: "https://{host}/settings/apps"}},
			Fields: []string{"app_id", FieldAppPrivateKey}},
		{ID: "install", Label: "Install it", Title: "Install the App", ShownWhen: forgekit.WhenMethod(forgekit.MethodApp),
			Do: []api.Action{
				{Text: "Choose **Install App**, then **Install** beside the owner."},
				{Text: "Pick the repositories and choose **Install**."},
			},
			Fields: []string{"installation_id"}},
		forgekit.OAuthStep("OAuth app", "Register an OAuth application",
			[]api.Link{{Label: "Open OAuth apps", URL: "https://{host}/settings/developers"}},
			"In GitHub, open **Settings** → **Developer settings** → **OAuth apps** → **New OAuth App**.",
			"Enter an **Application name** and **Homepage URL**. "+forgekit.SetCallback("Authorization callback URL"),
			"Check **Enable Device Flow** and choose **Register application**.",
			"Copy the **Client ID**. For browser sign-in, choose **Generate a new client secret**."),
		{ID: "token", Label: "Access token", Title: "Create a personal access token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "In GitHub, open **Settings** → **Developer settings** → **Personal access tokens** → **Fine-grained tokens** → **Generate new token**."},
				{Text: "Set **Resource owner** to the owner. Under **Repository access**, pick the repositories."},
				{Text: "Set **Contents** to **Read-only** and choose **Generate token**."},
			},
			Links:  []api.Link{{Label: "Open fine-grained tokens", URL: "https://{host}/settings/personal-access-tokens/new"}},
			Fields: []string{forgekit.FieldToken}},
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "Deploy key", Title: "Add a deploy key", File: "pando-deploy",
			Add: []string{"In the repository, open **Settings** → **Deploy keys** → **Add deploy key**. Paste `pando-deploy.pub` into **Key** and choose **Add key**."}}),
		forgekit.ScopeStep("Repositories", "Choose an owner"),
	}
}
