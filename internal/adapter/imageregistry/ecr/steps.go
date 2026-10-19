package ecr

import "github.com/trypando/pando/internal/adapter/api"

// theRepository is the resource of every action but GetAuthorizationToken.
const theRepository = "The repository"

// steps walk through the repository and an access key that can use it.
func steps() []api.Step {
	return []api.Step{
		{ID: "repository", Label: "Repository", Title: "Create a repository",
			Do: []api.Action{
				{Text: "In the Amazon ECR console, choose the Region from the navigation bar."},
				{Text: "Choose **Private repositories**, then **Create repository**."},
				{Text: "Enter a **Repository name**, such as `pando`, and choose **Create**."},
				{Text: "Copy the repository's **URI**."},
			},
			Links:  []api.Link{{Label: "Open Amazon ECR", URL: "https://console.aws.amazon.com/ecr/repositories"}},
			Fields: []string{"url"}},
		{ID: "access_key", Label: "Access key", Title: "Create an access key",
			Do: []api.Action{
				{Text: "In IAM, choose **Users**, then **Create user**."},
				{Text: "Enter a **User name**, then choose **Next** and **Create user**."},
				{Text: "On the user's **Permissions** tab, choose **Add permissions**, then **Create inline policy**."},
				{Text: "Allow these actions:", Table: &api.Table{
					Columns: []string{"Action", "Resource"},
					Rows: [][]string{
						{"ecr:GetAuthorizationToken", "All resources"},
						{"ecr:BatchCheckLayerAvailability", theRepository},
						{"ecr:InitiateLayerUpload", theRepository},
						{"ecr:UploadLayerPart", theRepository},
						{"ecr:CompleteLayerUpload", theRepository},
						{"ecr:PutImage", theRepository},
						{"ecr:BatchGetImage", theRepository},
						{"ecr:GetDownloadUrlForLayer", theRepository},
						{"ecr:ListImages", theRepository},
						{"ecr:BatchDeleteImage", theRepository},
					},
				}},
				{Text: "On the **Security credentials** tab, choose **Create access key**, then **Other** and **Next**."},
				{Text: "Choose **Create access key**, then **Show**."},
			},
			Links:  []api.Link{{Label: "Open IAM users", URL: "https://console.aws.amazon.com/iam/home#/users"}},
			Fields: []string{"access_key_id", "secret_access_key"}},
	}
}
