package traefik

import "github.com/trypando/pando/internal/adapter/api"

// steps asks who runs Traefik first, then branches: a Traefik somebody else
// runs needs its file provider pointed at Pando's routes; the one Pando runs
// needs certificates chosen, and for a wildcard, DNS provider credentials.
func steps() []api.Step {
	managed := &api.Condition{Key: "managed", Values: []string{"true"}}
	return []api.Step{
		{ID: "traefik", Label: "Traefik", Title: "Choose who runs Traefik",
			Fields: []string{"managed"}},
		{ID: "existing", Label: "Your Traefik", Title: "Point your Traefik at Pando's routes",
			ShownWhen: &api.Condition{Key: "managed", Values: []string{"false"}},
			Do: []api.Action{
				{Text: "Start Traefik with `--providers.file.directory` set to a directory, and `--providers.file.watch=true`."},
				{Text: "Mount that directory into Pando at `/etc/traefik/dynamic`."},
				{Text: "Copy the name of a resolver under `certificatesResolvers` in Traefik's static configuration."},
			},
			Links:  []api.Link{{Label: "Open Traefik's certificate resolver docs", URL: "https://doc.traefik.io/traefik/reference/install-configuration/tls/certificate-resolvers/acme/"}},
			Fields: []string{"cert_resolver"}},
		{ID: "domain", Label: "Domain", Title: "Choose a domain",
			Do: []api.Action{
				{Text: "At your DNS provider, add **A** records for the base domain, `*.` plus the base domain, and the console hostname, pointing at this machine's public IP address."},
			},
			Fields: []string{"base_domain", "console_hostname"}},
		{ID: "certificates", Label: "Certificates", Title: "Choose how certificates are issued", ShownWhen: managed,
			Fields: []string{"certificates", "acme_email"}},
		{ID: "dns", Label: "DNS provider", Title: "Create DNS provider credentials",
			ShownWhen: &api.Condition{Key: "certificates", Values: []string{CertsDNS}},
			Do: []api.Action{
				{Text: "At the provider that hosts the base domain, create a credential with this access:", Table: &api.Table{
					Columns: []string{"Provider", "Access", "Variables"},
					Rows: [][]string{
						{"Cloudflare", "API token with Zone / Zone / Read and Zone / DNS / Edit", "CF_DNS_API_TOKEN"},
						{"Amazon Route 53", "IAM user access key allowed route53:GetChange, route53:ListHostedZonesByName, route53:ListResourceRecordSets and route53:ChangeResourceRecordSets", "AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION"},
						{"DigitalOcean", "Personal access token with the domain:create, domain:read and domain:delete scopes", "DO_AUTH_TOKEN"},
						{"Porkbun", "API key, with API Access turned on in the domain's details", "PORKBUN_API_KEY, PORKBUN_SECRET_API_KEY"},
						{"Namecheap", "API access, with this machine's IPv4 address whitelisted", "NAMECHEAP_API_USER, NAMECHEAP_API_KEY"},
					},
				}},
				{Text: "For another provider, find its code and variables in the lego DNS provider list."},
				{Text: "Enter each variable below as `NAME=value`, one to a line. For Route 53, add `AWS_REGION=us-east-1`."},
			},
			Links: []api.Link{
				{Label: "Open Cloudflare API tokens", URL: "https://dash.cloudflare.com/profile/api-tokens"},
				{Label: "Open AWS IAM users", URL: "https://console.aws.amazon.com/iam/home#/users"},
				{Label: "Open DigitalOcean tokens", URL: "https://cloud.digitalocean.com/account/api/tokens"},
				{Label: "Open Porkbun API access", URL: "https://porkbun.com/account/api"},
				{Label: "Open Namecheap API access", URL: "https://ap.www.namecheap.com/settings/tools/apiaccess/"},
				{Label: "Open lego DNS providers", URL: "https://go-acme.github.io/lego/dns/"},
			},
			Fields: []string{"dns_provider", "dns_credentials"}},
	}
}
