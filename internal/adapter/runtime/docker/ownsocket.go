package docker

import (
	"context"
	"strings"
)

// OwnSocket is the container runtime socket Pando reaches this Docker
// through, as the daemon names it: the host path mounted into Pando's own
// container, and the group that may connect to it. The Docker hosts runtime
// gives it to the agent it keeps on the install's own machine, so the
// cluster's other control hosts reach this machine's runtime through that
// agent (R-416). The socket is never published: the agent carries runtime
// calls only for a control host's certificate.
type OwnSocket struct {
	// Source is the socket's path on the host, as a bind mount names it.
	Source string
	// Group is the numeric group ID that owns the socket, as Pando's own
	// container sees it; empty when it cannot be read.
	Group string
}

// OwnSocket reports the runtime socket Pando's own container reaches this
// Docker through, or false when Pando is not in a container here, or reaches
// Docker some other way (tcp://, ssh://).
func (a *Adapter) OwnSocket(ctx context.Context) (OwnSocket, bool) {
	if a.cli == nil {
		return OwnSocket{}, false
	}
	path, ok := strings.CutPrefix(a.cli.DaemonHost(), "unix://")
	if !ok || path == "" {
		return OwnSocket{}, false
	}
	self := a.inspectSelf(ctx)
	if self == nil {
		return OwnSocket{}, false
	}
	for _, m := range self.Mounts {
		if m.Destination == path && m.Type == "bind" && m.Source != "" {
			return OwnSocket{Source: m.Source, Group: socketGroup(path)}, true
		}
	}
	return OwnSocket{}, false
}
