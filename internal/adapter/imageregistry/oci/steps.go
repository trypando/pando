package oci

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "registry", Label: "Registry", Title: "Enter the registry address",
			Fields: []string{"url"}},
		{ID: "credential", Label: "Credential", Title: "Create a credential to push with",
			Do: []api.Action{
				{Text: "Create a credential that can push, pull and delete images:", Table: &api.Table{
					Columns: []string{"Registry", "Credential", "Permissions", "Username", "Password"},
					Rows: [][]string{
						{"GitHub Container Registry", "Personal access token (classic)", "`write:packages`, `delete:packages`", "Your GitHub username", "The token"},
						{"Harbor", "Robot account in the project", "Push Repository, Pull Repository, Delete Artifact", "The robot's full name, such as `robot$project+pando`", "The secret"},
						{"Artifact Registry", "Service account key", "Artifact Registry Repository Administrator", "`_json_key_base64`", "The key file, base64-encoded"},
						{"Distribution, Zot", "A user in its htpasswd file", "", "The user", "The user's password"},
					},
				}},
			},
			Links: []api.Link{
				{Label: "Open GitHub tokens (classic)", URL: "https://github.com/settings/tokens/new?scopes=write:packages,delete:packages"},
				{Label: "Open Google Cloud service accounts", URL: "https://console.cloud.google.com/iam-admin/serviceaccounts"},
			},
			Fields: []string{"username", "password"}},
	}
}
