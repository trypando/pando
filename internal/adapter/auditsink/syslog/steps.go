package syslog

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "collector", Label: "Collector", Title: "Connect to your collector",
			Fields: []string{"address", "tls"}},
		{ID: "certificates", Label: "Certificates", Title: "Add certificates",
			ShownWhen: &api.Condition{Key: "tls", Values: []string{"on", ""}},
			Fields:    []string{"ca_certificate", "client_certificate", "client_key"}},
		{ID: "events", Label: "Events", Title: "Choose events",
			Fields: []string{"actions", "exclude"}},
	}
}
