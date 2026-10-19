package multidocker

import (
	"strconv"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/hostagent"
)

// steps walk through setting up this kind (api.KindInfo.Steps).
func steps() []api.Step {
	return []api.Step{
		{ID: "access", Label: "Docker access", Title: "Open each host's Docker API",
			Do: []api.Action{
				{Text: "Install Docker Engine on each host. Use hosts of one CPU architecture."},
				{Text: "For a `tcp://` host, follow **Use TLS (HTTPS) to protect the Docker daemon socket** to run `dockerd --tlsverify` on port `2376`, and paste `ca.pem`, `cert.pem` and `key.pem` below."},
				{Text: "For an `ssh://` host, use rootful Docker: over SSH, Pando reaches only `/var/run/docker.sock`, not a rootless daemon's socket."},
				{Text: "For an `ssh://` host, run `ssh-keygen -t ed25519 -f pando-docker -N \"\"`."},
				{Text: "On each `ssh://` host, add a user to the `docker` group with `sudo usermod -aG docker <user>` and add `pando-docker.pub` to its `~/.ssh/authorized_keys`."},
				{Text: "Paste `pando-docker` below."},
			},
			Links: []api.Link{
				{Label: "Open Docker's install guide", URL: "https://docs.docker.com/engine/install/"},
				{Label: "Open Docker's TLS guide", URL: "https://docs.docker.com/engine/security/protect-access/#use-tls-https-to-protect-the-docker-daemon-socket"},
			},
			Fields: []string{"docker_tls", "ssh_key"}},
		{ID: "hosts", Label: "Hosts", Title: "List the hosts",
			Do: []api.Action{
				{Text: "On each host, allow port `" + strconv.Itoa(hostagent.DefaultPort) + "` from the control host's address only."},
				{Text: "On each host, let Docker pull from the install's image registry: trust its certificate, or list it under `insecure-registries` in `/etc/docker/daemon.json`."},
				{Text: "Give each host a `name` and an `endpoint`: `tcp://<host>:2376` or `ssh://<user>@<host>`."},
				{Text: "Mark the host Pando runs on with `\"control\": true` and give it an `agent_address`, such as `10.0.0.5:" + strconv.Itoa(hostagent.DefaultPort) + "`."},
				{Text: "For each `ssh://` host, set `ssh_host_key` to its line from `ssh-keyscan -t ed25519 <host>`."},
			},
			Fields: []string{"hosts"}},
		{ID: "authority", Label: "Authority", Title: "Create the agent certificate authority",
			Do: []api.Action{
				{Text: "On the host Pando runs on, run `pando host-agent new-authority`, or `docker compose exec pando pando host-agent new-authority` with Docker Compose."},
				{Text: "Paste its output below."},
			},
			Fields: []string{"agent_authority"}},
	}
}
