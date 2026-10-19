package local

import "github.com/trypando/pando/internal/adapter/api"

// Nothing to do outside Pando: one plain step for the one setting.
func steps() []api.Step {
	return []api.Step{
		{ID: "directory", Label: "Directory", Title: "Choose a directory", Fields: []string{"path"}},
	}
}
