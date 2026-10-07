package multidocker

import (
	"strconv"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/hostagent"
)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category: api.CategoryRuntime,
		Kind:     Kind,
		Name:     "Docker on several hosts",
		Description: "Runs apps as containers on several Docker hosts. Each app is placed on one host and stays there: " +
			"a new app goes to the host with the most free memory that fits it. Pando reaches each host's apps " +
			"through a forwarding agent it runs there, which accepts only Pando's certificate. Built images reach " +
			"the hosts through the install's image registry.",
		IDPrefix: "rt_",
		Fields: []api.Field{
			{Key: "hosts", Label: "Hosts", Type: "string", Multiline: true, Required: true,
				Help: "The hosts as a JSON list. Each has a name; an endpoint (unix:///var/run/docker.sock, tcp://host:2376 " +
					"with the Docker TLS credential, or ssh://user@host with the SSH key and the host's ssh_host_key); an " +
					"agent_address where Pando reaches the host's agent; and optionally no_placement to keep new apps off it. " +
					"Exactly one is \"control\": true, the host Pando runs on.",
				Placeholder: `[{"name":"control","control":true,"agent_address":"10.0.0.5:7443"},{"name":"app-1","endpoint":"tcp://10.0.0.6:2376"}]`},
			{Key: "agent_authority", Label: "Agent certificate authority", Type: "string", Multiline: true, Credential: true, Required: true,
				Help: "The output of `pando host-agent new-authority`. Pando issues its own certificate and each agent's from it."},
			{Key: "docker_tls", Label: "Docker TLS client certificate", Type: "string", Multiline: true, Credential: true,
				Help: "For tcp:// hosts: the CA (ca.pem), the client certificate (cert.pem) and its key (key.pem), pasted together."},
			{Key: "ssh_key", Label: "SSH private key", Type: "string", Multiline: true, Credential: true,
				Help: "For ssh:// hosts: an unencrypted private key for a user in the docker group, or root."},
			{Key: "agent_image", Label: "Agent image", Type: "string", Advanced: true,
				Help:    "The image of Pando each host's agent and each restricted app's egress gateway run. Every host must be able to pull it.",
				Default: "The image Pando's own container runs"},
			{Key: "agent_port", Label: "Agent port", Type: "int", Advanced: true,
				Help:    "The one port each host publishes, for its agent. Restrict it to the control host's address with the host's firewall.",
				Default: strconv.Itoa(hostagent.DefaultPort)},
			{Key: "network_pool", Label: "App network range", Type: "string", Advanced: true,
				Help:    "The IPv4 range each host's app networks take their addresses from. Each host uses the whole range for its own apps. Cannot be \"off\" here.",
				Default: defaultNetworkPool},
			{Key: "network_block_bits", Label: "App network size", Type: "int", Advanced: true,
				Help:    "The size of each app's private network, as a prefix length from 24 to 29.",
				Default: "28"},
			{Key: "oci_runtime", Label: "Container runtime", Type: "string", Advanced: true,
				Help:    "The runtime every host's Docker starts apps with, by the name it is registered under in daemon.json, such as runsc.",
				Default: "Docker's default, runc"},
		},
	}
}
