package hosts

import (
	"context"
	"strings"

	"github.com/trypando/pando/internal/core/state"
)

// The install's own first host (R-414): every install is a cluster of one
// from its first start, so the machine it started on is in cluster_hosts like
// any host that joined, and the cluster's count is the rows.

// installHostName is the name the first host takes when its machine's name
// gives none Pando can use.
const installHostName = "control"

// unpinned stands in for the first host's certificate fingerprint until the
// host issues its own certificate (ControlHostCert) and records it, with
// where it answers pings, in its first round of liveness checks
// (ControlPeers, R-421). The runtime's host list leaves the first host out of
// the agent:// entries either way (RuntimeHosts).
var unpinned = strings.Repeat("0", 64)

// InstallHostStore records the first host: state.Cluster.
type InstallHostStore interface {
	RecordFirstHost(ctx context.Context, h state.Host) (state.Host, bool, error)
}

// InstallHostInfo is what the first host is recorded with.
type InstallHostInfo struct {
	// Hostname is the machine's; the host is named by its first label.
	Hostname string
	// Address is where its agent is reached, host:port.
	Address  string
	Platform string
	Version  string
}

// RecordInstallHost records the install's own machine as its first host, with
// both roles, unless a first host is recorded already. It reports whether it
// recorded one. A host that joined a cluster never calls it: it was recorded
// by its join.
func RecordInstallHost(ctx context.Context, store InstallHostStore, info InstallHostInfo) (state.Host, bool, error) {
	return store.RecordFirstHost(ctx, state.Host{
		Name:       InstallHostName(info.Hostname),
		Roles:      []string{state.HostRoleControl, state.HostRoleApp},
		Address:    info.Address,
		CertSHA256: unpinned,
		Platform:   info.Platform,
		Version:    info.Version,
	})
}

// InstallHostName is the first label of hostname, lower case, with anything a
// DNS label cannot hold turned into a hyphen: "Build-01.example.com" is
// build-01. A name that leaves nothing usable is "control".
func InstallHostName(hostname string) string {
	label, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(hostname)), ".")
	label = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return '-'
	}, label)
	if len(label) > 63 {
		label = label[:63]
	}
	label = strings.Trim(label, "-")
	if CheckHostName(label) != nil {
		return installHostName
	}
	return label
}
