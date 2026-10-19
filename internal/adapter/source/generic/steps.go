package generic

import (
	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/source/forgekit"
)

// steps walk through a connection: the method and host, then the method's
// credential, then the path prefix.
func steps() []api.Step {
	return []api.Step{
		forgekit.MethodStep(),
		{ID: "token", Label: "Access token", Title: "Create an access token", ShownWhen: forgekit.WhenMethod(forgekit.MethodToken),
			Do: []api.Action{
				{Text: "On the git host, create an access token with read access to the repositories."},
			},
			Fields: []string{"username", forgekit.FieldToken}},
		forgekit.SSHKeyStep(forgekit.SSHKey{Label: "SSH key", Title: "Add an SSH key", File: "pando-deploy",
			Add:     []string{"On the git host, add `pando-deploy.pub` to the repository as a read-only deploy key."},
			KeyScan: forgekit.KeyScanAnyHost}),
		forgekit.ScopeStep("Repositories", "Choose a path prefix"),
	}
}
