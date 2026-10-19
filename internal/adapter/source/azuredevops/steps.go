package azuredevops

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps walks through one sign-in method's setup, chosen first. The two
// Microsoft Entra ID methods share an app registration, so they share its
// steps, with the actions only one of them needs marked as such.
func steps() []api.Step {
	entra := forgekit.WhenMethod(forgekit.MethodOAuth, forgekit.MethodServicePrincipal)
	return []api.Step{
		forgekit.MethodStep(),
		{ID: "token", Label: "Access token", Title: "Create a personal access token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "In Azure DevOps, open **User settings** → **Personal access tokens** → **New Token**."},
				{Text: "Enter a **Name**, select the **Organization**, and set an **Expiration**. On Azure DevOps Server, select the collection."},
				{Text: "Under **Scopes**, choose **Custom defined**, then select **Read** under **Code**."},
				{Text: "Choose **Create** and copy the token."},
			},
			Fields: []string{forgekit.FieldToken}},
		{ID: "app", Label: "App registration", Title: "Register an app in Microsoft Entra ID", ShownWhen: entra,
			Do: []api.Action{
				{Text: "In the Microsoft Entra admin center, open **Entra ID** → **App registrations** → **New registration**."},
				{Text: "Enter a **Name** and choose **Register**."},
				{Text: "For Entra ID sign-in, under **API permissions**, choose **Add a permission** → **Azure DevOps** → **Delegated permissions**, select **vso.code**, and choose **Add permissions**."},
				{Text: "For Entra ID sign-in, under **Authentication**, set **Allow public client flows** to **Yes** and choose **Save**."},
				{Text: "For browser sign-in, under **Authentication**, choose **Add Redirect URI** → **Web**, enter your Pando address plus `" + forgekit.CallbackPath + "`, and choose **Configure**."},
				{Text: "For a service principal, in Azure DevOps open **Organization settings** → **Users** → **Add users**. Enter the app's name, choose **Basic** access and the projects, and choose **Add**."},
				{Text: "On the app's **Overview**, copy the **Application (client) ID** and **Directory (tenant) ID**."},
			},
			Links:  []api.Link{{Label: "Open App registrations", URL: "https://entra.microsoft.com/#view/Microsoft_AAD_RegisteredApps/ApplicationsListBlade"}},
			Fields: []string{"client_id", "tenant_id"}},
		{ID: "secret", Label: "Client secret", Title: "Create a client secret", ShownWhen: entra,
			Do: []api.Action{
				{Text: "In the app registration, open **Certificates & secrets** → **Client secrets** → **New client secret**."},
				{Text: "Enter a description, choose an expiration, and choose **Add**."},
				{Text: "Copy the secret's **Value**."},
			},
			Fields: []string{forgekit.FieldClientSecret}},
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "SSH key", Title: "Add an SSH key", File: "pando-azure", Type: "-t rsa -b 3072",
			Add: []string{
				"In Azure DevOps, open **User settings** → **SSH public keys** → **New Key**.",
				"Paste `pando-azure.pub` into **Public Key Data**, enter a name, and choose **Save**.",
			},
			KeyScan: "`ssh-keyscan ssh.dev.azure.com`, or your server's host,"}),
		forgekit.ScopeStep("Organization", "Choose an organization"),
	}
}
