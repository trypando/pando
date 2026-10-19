package incus

import (
	"strconv"

	"github.com/trypando/pando/internal/adapter/api"
	"github.com/trypando/pando/internal/adapter/runtime/docker"
	"github.com/trypando/pando/internal/hostagent"
)

// Info describes this kind of adapter for the forms that configure one
// (api.KindInfo, R-261).
func Info() api.KindInfo {
	return api.KindInfo{
		Category: api.CategoryRuntime,
		Kind:     Kind,
		Name:     "Incus virtual machines",
		Description: "Runs each app in its own Incus virtual machine, so no two apps share a kernel. Incus decides " +
			"which cluster member a VM runs on. Pando reaches each VM through a forwarding agent it runs there, and " +
			"a network ACL admits nothing else. Built images reach the VMs through the install's image registry. " +
			"The edge keeps running on the Docker runtime.",
		IDPrefix: "rt_",
		Fields: []api.Field{
			{Key: "address", Label: "Incus address", Type: "string",
				Help:        "unix:///var/lib/incus/unix.socket when Pando runs on the Incus server, or https://host:8443 with a client certificate.",
				Placeholder: defaultAddress},
			{Key: "server_certificate", Label: "Server certificate", Type: "string", Multiline: true,
				Help: "For an https:// address: /var/lib/incus/server.crt on the Incus server, or cluster.crt on a cluster."},
			{Key: "tls_client", Label: "TLS client certificate", Type: "string", Multiline: true, Credential: true,
				Help: "For an https:// address: a client certificate and its key, as PEM, added with `incus config trust add-certificate`."},
			{Key: "network", Label: "Network", Type: "string", Required: true,
				Help:        "The Incus network each app's VM is attached to. Pando must be able to reach addresses on it. On a cluster, an OVN network, or a bridge with a target member.",
				Placeholder: "incusbr0"},
			{Key: "proxy_sources", Label: "Pando's addresses", Type: "string", Required: true, List: true,
				Help: "The addresses Pando connects to app VMs from, as seen on that network, as addresses or ranges."},
			{Key: "agent_authority", Label: "Agent certificate authority", Type: "string", Multiline: true, Credential: true, Required: true,
				Help: "The output of `pando host-agent new-authority`. Pando issues its own certificate and each VM's agent's from it."},
			{Key: "agent_image", Label: "Pando image", Type: "string", Required: true,
				Help:        "The image of this version of Pando, which each VM's forwarding agent and each restricted app's egress gateway run. Every VM must be able to pull it.",
				Placeholder: "trypando/pando:0.4.0"},
			{Key: "project", Label: "Project", Type: "string", Advanced: true,
				Help:    "The Incus project VMs, volumes and the guest image are made in. A restricted client certificate must be allowed it and the network's project.",
				Default: defaultProject},
			{Key: "storage_pool", Label: "Storage pool", Type: "string", Advanced: true,
				Help:    "The pool for each VM's disk and the apps' volumes.",
				Default: "The pool of the project's default profile's root disk"},
			{Key: "target", Label: "Cluster target", Type: "string", Advanced: true,
				Help:    "A cluster member, or @ and a cluster group, that VMs run on.",
				Default: "Any member, as Incus places them"},
			{Key: "profiles", Label: "Profiles", Type: "string", Advanced: true, List: true,
				Help:    "Incus profiles applied to each VM. Pando sets the VM's disk and NIC itself; a profile that adds another NIC gives the VM a way in that skips Pando.",
				Default: "None"},
			{Key: "guest_image", Label: "Guest image", Type: "string", Advanced: true,
				Help:    "An Incus image alias or fingerprint to start VMs from, with Podman serving " + podmanSocket + " and socat installed.",
				Default: "Debian 13 with Podman, prepared by Pando on first use"},
			{Key: "agent_port", Label: "Agent port", Type: "int", Advanced: true,
				Help:    "The port each VM's agent listens on, and the only one the network ACL opens, to Pando's addresses.",
				Default: strconv.Itoa(hostagent.DefaultPort)},
			{Key: "root_size_bytes", Label: "VM disk size", Type: "int", Advanced: true,
				Help:    "Each VM's disk, in bytes, for images and what workloads write outside their volumes.",
				Default: "21474836480 (20 GiB)"},
			{Key: "volume_size_bytes", Label: "Volume size", Type: "int", Advanced: true,
				Help:    "The size of each app volume, in bytes, unless the app asks for more.",
				Default: "10737418240 (10 GiB)"},
			{Key: "guest_overhead_bytes", Label: "VM memory overhead", Type: "int", Advanced: true,
				Help:    "Memory each VM has beyond its app's limits, in bytes, for the guest system and Podman.",
				Default: "536870912 (512 MiB)"},
			{Key: "default_cpus", Label: "Default CPUs", Type: "int", Advanced: true,
				Help:    "CPUs for the VM of an app that sets no limits.",
				Default: "1"},
			{Key: "default_memory_bytes", Label: "Default memory", Type: "int", Advanced: true,
				Help:    "Memory for the VM of an app that sets no limits, in bytes.",
				Default: "1073741824 (1 GiB)"},
			{Key: "network_pool", Label: "App network range", Type: "string", Advanced: true,
				Help:    "The IPv4 range app networks inside each VM take their addresses from. Must not overlap the Incus network.",
				Default: docker.DefaultNetworkPool()},
			{Key: "network_block_bits", Label: "App network size", Type: "int", Advanced: true,
				Help:    "The size of each app's private network inside its VM, as a prefix length from 24 to 29.",
				Default: "28"},
			{Key: "total_cpu_millis", Label: "CPU for Pando", Type: "int", Advanced: true,
				Help:    "Thousandths of a core Pando may use across the members, when it shares them with other work.",
				Default: "What the members have"},
			{Key: "total_memory_bytes", Label: "Memory for Pando", Type: "int", Advanced: true,
				Help:    "Bytes of memory Pando may use across the members, when it shares them with other work.",
				Default: "What the members have"},
		},
		Steps: steps(),
	}
}
