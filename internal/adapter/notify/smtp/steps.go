package smtp

import "github.com/trypando/pando/internal/adapter/api"

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "server", Label: "Server", Title: "Enter your SMTP server",
			Do:     []api.Action{{Text: "Find your email provider's SMTP server name and port."}},
			Fields: []string{"host"}},
		{ID: "sign-in", Label: "Sign-in", Title: "Enter SMTP credentials",
			Do:     []api.Action{{Text: "Create SMTP credentials with your email provider, if the server requires sign-in."}},
			Fields: []string{"username", "password"}},
		{ID: "sender", Label: "Sender", Title: "Choose the From address",
			Do:     []api.Action{{Text: "Verify the address or its domain with your email provider, if it requires a verified sender."}},
			Fields: []string{"from"}},
	}
}
