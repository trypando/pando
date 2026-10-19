package incus

import "github.com/trypando/pando/internal/adapter/api"

// steps walks through reaching Incus, then what Pando's VMs need.
func steps() []api.Step {
	return []api.Step{
		{ID: "address", Label: "Address", Title: "Where is Incus?",
			Fields: []string{"address"}},
		{ID: "https", Label: "Certificates", Title: "Connect over HTTPS",
			Lede: "Skip this step for a unix:// address.",
			Do: []api.Action{
				{Text: "On the Incus server, run `incus config set core.https_address=:8443`."},
				{Text: "Run `openssl req -x509 -newkey rsa:3072 -nodes -days 3650 -subj /CN=pando -keyout pando.key -out pando.crt`."},
				{Text: "Run `incus config trust add-certificate pando.crt --restricted --projects default`."},
				{Text: "Paste `/var/lib/incus/server.crt` below, or `/var/lib/incus/cluster.crt` on a cluster."},
				{Text: "Paste `pando.crt` and `pando.key` together below."},
			},
			Links:  []api.Link{{Label: "Open the Incus docs", URL: "https://linuxcontainers.org/incus/docs/main/howto/server_expose/"}},
			Fields: []string{"server_certificate", "tls_client"}},
		{ID: "network", Label: "Network", Title: "Choose a network",
			Do:     []api.Action{{Text: "Run `incus network list` to see the networks."}},
			Fields: []string{"network", "proxy_sources"}},
		{ID: "agent", Label: "Agents", Title: "Set up the forwarding agents",
			Do:     []api.Action{{Text: "Run `pando host-agent new-authority` and paste its output below."}},
			Fields: []string{"agent_authority", "agent_image"}},
	}
}
